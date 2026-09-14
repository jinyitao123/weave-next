package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/accesschange"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/labstack/echo/v4"
)

func accessChangeKey(c echo.Context) (string, error) {
	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" || key != strings.TrimSpace(key) || len(key) > 256 || strings.ContainsRune(key, 0) {
		return "", echo.NewHTTPError(http.StatusBadRequest, "无法确认这次权限变更，请重新操作。")
	}
	return key, nil
}
func newAccessChangeIntent(c echo.Context, kind, target string, mutation any, block, grant, scope []admissionfence.Resource) (accesschange.Intent, error) {
	key, err := accessChangeKey(c)
	if err != nil {
		return accesschange.Intent{}, err
	}
	raw, err := json.Marshal(mutation)
	if err != nil {
		return accesschange.Intent{}, err
	}
	digest, err := accesschange.SourceDigest(mutation)
	if err != nil {
		return accesschange.Intent{}, err
	}
	return accesschange.Intent{WorkspaceID: getTenant(c), OperationID: key, Kind: kind, TargetID: target, Mutation: raw, SourceDigest: digest, Block: block, Grant: grant, Scope: scope}, nil
}
func (s *Server) accessChangeFlow() (accesschange.Flow, error) {
	kernel, ok := s.KernelPublication.(admissionfence.Service)
	if !ok || s.GetPool() == nil {
		return accesschange.Flow{}, errors.New("permission change service unavailable")
	}
	return accesschange.Flow{Store: accesschange.NewStore(s.GetPool()), Kernel: kernel}, nil
}
func (s *Server) applyAccessChange(ctx context.Context, intent accesschange.Intent, mutation accesschange.Mutation) (accesschange.Result, error) {
	flow, err := s.accessChangeFlow()
	if err != nil {
		return accesschange.Result{}, err
	}
	// Validate with the owner's exact product policy before taking any Kernel
	// action. Mutation is contractually limited to this transaction's SQL; the
	// savepoint discards all prospective product facts and generated receipts.
	flow.Validate = func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SAVEPOINT access_change_validation"); err != nil {
			return err
		}
		_, validationErr := mutation(ctx, tx)
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT access_change_validation"); err != nil {
			return err
		}
		return validationErr
	}
	return flow.Apply(ctx, intent, mutation)
}

// applyExternalAccessChange records the same durable operation for a product
// store whose mutation owns its own transaction. The operation fence is still
// the only admission authority; retries replay the store operation by the same
// Idempotency-Key before completing the server record.
func (s *Server) applyExternalAccessChange(c echo.Context, kind, target string, mutation any, block, grant, scope []admissionfence.Resource, apply func(context.Context) (json.RawMessage, error), successStatus int) error {
	intent, err := newAccessChangeIntent(c, kind, target, mutation, block, grant, scope)
	if err != nil {
		return err
	}
	flow, err := s.accessChangeFlow()
	if err != nil {
		return writeAccessChangeOutcome(c, accesschange.Result{}, err, successStatus)
	}
	result, err := flow.ApplyReceiptMutation(c.Request().Context(), intent, apply)
	return writeAccessChangeOutcome(c, result, err, successStatus)
}

// Completed keeps the existing endpoint response. Pending has one stable
// operation reference and remains retryable with the same request identity.
func writeAccessChangeOutcome(c echo.Context, result accesschange.Result, err error, successStatus int) error {
	if err == nil {
		c.Response().Header().Set("X-Weave-Operation-State", "completed")
		if successStatus == http.StatusNoContent {
			return c.NoContent(successStatus)
		}
		return c.JSONBlob(successStatus, result.Value)
	}
	if errors.Is(err, accesschange.ErrUnauthorized) {
		return c.JSON(http.StatusForbidden, map[string]any{"error": "你没有权限执行这项变更。", "retryable": false})
	}
	if errors.Is(err, accesschange.ErrConflict) || errors.Is(err, accesschange.ErrInvalid) {
		return c.JSON(http.StatusConflict, map[string]any{"error": "这项变更的内容已发生变化，请重新操作。", "retryable": false})
	}
	var rejected *accesschange.RejectedError
	if errors.As(err, &rejected) {
		return c.JSON(http.StatusConflict, map[string]any{"error": "权限对象已发生变化，请刷新后重新操作。", "retryable": false})
	}
	operationID := result.OperationID
	var pending *accesschange.PendingError
	if errors.As(err, &pending) {
		operationID = pending.OperationID
	}
	if operationID == "" {
		operationID = c.Request().Header.Get("Idempotency-Key")
	}
	c.Response().Header().Set("Retry-After", "1")
	c.Response().Header().Set("X-Weave-Operation-State", "pending")
	return c.JSON(http.StatusAccepted, map[string]any{"state": "pending", "operation_id": operationID, "retryable": true, "message": "权限变更正在确认，请稍后重试。"})
}

func (s *Server) handleGetAccessChange(c echo.Context) error {
	result, err := accesschange.NewStore(s.GetPool()).Get(c.Request().Context(), getTenant(c), c.Param("operationID"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "找不到这项权限变更。"})
	}
	state := "pending"
	if result.State == "completed" {
		state = "completed"
	}
	return c.JSON(http.StatusOK, map[string]any{"operation_id": result.OperationID, "state": state, "retryable": state != "completed", "result": result.Value})
}
