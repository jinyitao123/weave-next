package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/labstack/echo/v4"
)

const (
	deliveryStoreUnavailableCode = "workflow_delivery_store_unavailable"
	deliveryStoreFailedCode      = "workflow_delivery_store_failed"
	maxDeliveryTargetRequestBody = int64(1 << 20)
)

var errDeliveryTargetRequestBodyTooLarge = errors.New("delivery target request body too large")

type createDeliveryTargetRequest struct {
	ID             string            `json:"id"`
	Kind           delivery.Kind     `json:"kind"`
	URL            string            `json:"url"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	Headers        map[string]string `json:"headers"`
}

type updateDeliveryTargetRequest struct {
	Kind           delivery.Kind     `json:"kind"`
	URL            string            `json:"url"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	Headers        map[string]string `json:"headers"`
}

type rotateDeliveryTargetHeadersRequest struct {
	Headers map[string]string `json:"headers"`
}

type deliveryTargetView struct {
	ID             string     `json:"id"`
	LatestRevision int64      `json:"latest_revision"`
	Enabled        bool       `json:"enabled"`
	RevokedAt      *time.Time `json:"revoked_at"`
	DeletedAt      *time.Time `json:"deleted_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type deliveryTargetRevisionView struct {
	TargetID       string        `json:"target_id"`
	Revision       int64         `json:"revision"`
	Kind           delivery.Kind `json:"kind"`
	Transport      string        `json:"transport"`
	URL            string        `json:"url"`
	Method         string        `json:"method"`
	ContentType    string        `json:"content_type"`
	TimeoutSeconds int64         `json:"timeout_seconds"`
	HeaderNames    []string      `json:"header_names"`
	ContentHash    string        `json:"content_hash"`
	CreatedAt      time.Time     `json:"created_at"`
}

type deliveryTargetMutationResponse struct {
	Target   deliveryTargetView         `json:"target"`
	Revision deliveryTargetRevisionView `json:"revision"`
	Advanced bool                       `json:"advanced"`
}

type deliveryTargetListResponse struct {
	Targets []deliveryTargetView `json:"targets"`
}

type deliveryTargetDetailResponse struct {
	Target   deliveryTargetView         `json:"target"`
	Revision deliveryTargetRevisionView `json:"revision"`
}

type deliveryTargetRevisionResponse struct {
	Revision deliveryTargetRevisionView `json:"revision"`
}

func (s *Server) handleListDeliveryTargets(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targets, err := s.DeliveryTargets.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	views := make([]deliveryTargetView, 0, len(targets))
	for _, target := range targets {
		views = append(views, projectDeliveryTarget(target))
	}
	return c.JSON(http.StatusOK, deliveryTargetListResponse{Targets: views})
}

func (s *Server) handleCreateDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	var request *createDeliveryTargetRequest
	if err := decodeDeliveryTargetJSON(c, &request); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if request == nil || !validDeliveryTargetID(request.ID) || request.Headers == nil {
		return deliveryTargetInvalidRequest(c)
	}
	result, err := s.DeliveryTargets.Create(
		c.Request().Context(), getTenant(c), request.ID,
		delivery.Config{
			Kind: request.Kind, URL: request.URL,
			TimeoutSeconds: request.TimeoutSeconds, Headers: request.Headers,
		},
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.JSON(http.StatusCreated, projectDeliveryMutation(result))
}

func (s *Server) handleGetDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	target, err := s.DeliveryTargets.Get(
		c.Request().Context(), getTenant(c), targetID,
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	revision, err := s.DeliveryTargets.GetRevision(
		c.Request().Context(), getTenant(c), targetID, target.LatestRevision,
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.JSON(http.StatusOK, deliveryTargetDetailResponse{
		Target: projectDeliveryTarget(target), Revision: projectDeliveryRevision(revision),
	})
}

func (s *Server) handleUpdateDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	var request *updateDeliveryTargetRequest
	if err := decodeDeliveryTargetJSON(c, &request); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if request == nil || request.Headers == nil {
		return deliveryTargetInvalidRequest(c)
	}
	result, err := s.DeliveryTargets.Update(
		c.Request().Context(), getTenant(c), targetID,
		delivery.Config{
			Kind: request.Kind, URL: request.URL,
			TimeoutSeconds: request.TimeoutSeconds, Headers: request.Headers,
		},
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.JSON(http.StatusOK, projectDeliveryMutation(result))
}

func (s *Server) handleGetDeliveryTargetRevision(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	revisionNumber, ok := deliveryTargetRevisionParam(c.Param("revision"))
	if !ok {
		return deliveryTargetInvalidRequest(c)
	}
	revision, err := s.DeliveryTargets.GetRevision(
		c.Request().Context(), getTenant(c), targetID, revisionNumber,
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.JSON(http.StatusOK, deliveryTargetRevisionResponse{
		Revision: projectDeliveryRevision(revision),
	})
}

func (s *Server) handleRotateDeliveryTargetHeaders(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	var request *rotateDeliveryTargetHeadersRequest
	if err := decodeDeliveryTargetJSON(c, &request); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if request == nil || request.Headers == nil {
		return deliveryTargetInvalidRequest(c)
	}
	result, err := s.DeliveryTargets.RotateHeaders(
		c.Request().Context(), getTenant(c), targetID, request.Headers,
	)
	if err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.JSON(http.StatusOK, projectDeliveryMutation(result))
}

func (s *Server) handleDisableDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	if err := decodeEmptyDeliveryTargetJSON(c); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if err := s.DeliveryTargets.Disable(
		c.Request().Context(), getTenant(c), targetID,
	); err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleRevokeDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	if err := decodeEmptyDeliveryTargetJSON(c); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if err := s.DeliveryTargets.Revoke(
		c.Request().Context(), getTenant(c), targetID,
	); err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleDeleteDeliveryTarget(c echo.Context) error {
	if s.DeliveryTargets == nil {
		return deliveryTargetStoreUnavailable(c)
	}
	targetID := c.Param("id")
	if !validDeliveryTargetID(targetID) {
		return deliveryTargetInvalidRequest(c)
	}
	if err := decodeEmptyDeliveryTargetJSON(c); err != nil {
		return mapDeliveryTargetDecodeError(c, err)
	}
	if err := s.DeliveryTargets.Delete(
		c.Request().Context(), getTenant(c), targetID,
	); err != nil {
		return mapDeliveryTargetError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func decodeDeliveryTargetJSON(c echo.Context, destination any) error {
	raw, err := readDeliveryTargetRequestBody(c)
	if err != nil {
		return err
	}
	return decodeDeliveryTargetJSONBytes(raw, destination)
}

func decodeDeliveryTargetJSONBytes(raw []byte, destination any) error {
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("delivery target request contains multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeEmptyDeliveryTargetJSON(c echo.Context) error {
	raw, err := readDeliveryTargetRequestBody(c)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var request *struct{}
	if err := decodeDeliveryTargetJSONBytes(raw, &request); err != nil {
		return err
	}
	if request == nil {
		return errors.New("delivery target action request must be an empty object")
	}
	return nil
}

func readDeliveryTargetRequestBody(c echo.Context) ([]byte, error) {
	request := c.Request()
	if request.ContentLength > maxDeliveryTargetRequestBody {
		return nil, errDeliveryTargetRequestBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxDeliveryTargetRequestBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxDeliveryTargetRequestBody {
		return nil, errDeliveryTargetRequestBodyTooLarge
	}
	return body, nil
}

func deliveryTargetRevisionParam(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	return revision, err == nil
}

func validDeliveryTargetID(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '-', '.', '_', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func projectDeliveryTarget(target delivery.Target) deliveryTargetView {
	return deliveryTargetView{
		ID: target.ID, LatestRevision: target.LatestRevision, Enabled: target.Enabled,
		RevokedAt: target.RevokedAt, DeletedAt: target.DeletedAt,
		CreatedAt: target.CreatedAt, UpdatedAt: target.UpdatedAt,
	}
}

func projectDeliveryRevision(revision delivery.Revision) deliveryTargetRevisionView {
	return deliveryTargetRevisionView{
		TargetID: revision.TargetID, Revision: revision.Revision, Kind: revision.Kind,
		Transport: revision.Transport, URL: revision.URL, Method: revision.Method,
		ContentType: revision.ContentType, TimeoutSeconds: revision.TimeoutSeconds,
		HeaderNames: append([]string{}, revision.HeaderNames...),
		ContentHash: revision.ContentHash, CreatedAt: revision.CreatedAt,
	}
}

func projectDeliveryMutation(result delivery.MutationResult) deliveryTargetMutationResponse {
	return deliveryTargetMutationResponse{
		Target:   projectDeliveryTarget(result.Target),
		Revision: projectDeliveryRevision(result.Revision),
		Advanced: result.Advanced,
	}
}

func mapDeliveryTargetError(c echo.Context, err error) error {
	switch {
	case isDeliveryTargetContextError(err):
		return err
	case errors.Is(err, delivery.ErrInvalidRequest):
		return deliveryTargetInvalidRequest(c)
	case errors.Is(err, delivery.ErrTargetNotFound):
		return deliveryTargetError(
			c, http.StatusNotFound,
			delivery.CodeTargetNotFound, "workflow delivery target not found",
		)
	case errors.Is(err, delivery.ErrTargetClosed):
		return deliveryTargetError(
			c, http.StatusConflict,
			delivery.CodeTargetClosed, "workflow delivery target closed",
		)
	case errors.Is(err, delivery.ErrCredentialUnavailable):
		return deliveryTargetError(
			c, http.StatusServiceUnavailable,
			delivery.CodeCredentialUnavailable, "workflow credential unavailable",
		)
	case errors.Is(err, delivery.ErrFrozenManifestMismatch):
		return deliveryTargetError(
			c, http.StatusInternalServerError,
			delivery.CodeFrozenManifestMismatch, "workflow frozen manifest mismatch",
		)
	default:
		return deliveryTargetError(
			c, http.StatusInternalServerError,
			deliveryStoreFailedCode, "workflow delivery store failed",
		)
	}
}

func mapDeliveryTargetDecodeError(c echo.Context, err error) error {
	if isDeliveryTargetContextError(err) {
		return err
	}
	return deliveryTargetInvalidRequest(c)
}

func isDeliveryTargetContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func deliveryTargetStoreUnavailable(c echo.Context) error {
	return deliveryTargetError(
		c, http.StatusServiceUnavailable,
		deliveryStoreUnavailableCode, "workflow delivery store unavailable",
	)
}

func deliveryTargetInvalidRequest(c echo.Context) error {
	return deliveryTargetError(
		c, http.StatusBadRequest,
		delivery.CodeInvalidRequest, "workflow delivery request invalid",
	)
}

func deliveryTargetError(c echo.Context, status int, code, message string) error {
	return c.JSON(status, map[string]string{"code": code, "error": message})
}
