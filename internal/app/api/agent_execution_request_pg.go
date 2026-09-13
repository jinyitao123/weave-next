package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

type agentExecutionPublicationPlan struct {
	Subject               execution.Subject                     `json:"subject"`
	RequestID             string                                `json:"request_id"`
	RequestDigest         string                                `json:"request_digest"`
	AgentName             string                                `json:"agent_name"`
	Request               agentExecutionRequest                 `json:"request"`
	AgentVersion          int                                   `json:"agent_version"`
	PublicationCommands   []teamconstruction.PublicationCommand `json:"publication_commands"`
	CompletedPublications []string                              `json:"completed_publications"`
	State                 string                                `json:"state"`
	Response              json.RawMessage                       `json:"response,omitempty"`
}

func findAgentExecutionPlan(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, workspaceID, requestID, digest string, subject execution.Subject) (agentExecutionPublicationPlan, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return agentExecutionPublicationPlan{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, found, err := loadAgentExecutionPlan(ctx, tx, workspaceID, requestID, digest, subject)
	if err != nil || !found {
		return plan, found, err
	}
	if err = tx.Commit(ctx); err != nil {
		return agentExecutionPublicationPlan{}, false, err
	}
	return plan, true, nil
}

func newAgentExecutionRequestIdentity(ctx context.Context, workspaceID, agentName string, request agentExecutionRequest) (string, string, execution.Subject, error) {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return "", "", execution.Subject{}, err
	}
	raw, err := json.Marshal(struct {
		AgentName string                `json:"agent_name"`
		Request   agentExecutionRequest `json:"request"`
	}{agentName, request})
	if err != nil {
		return "", "", execution.Subject{}, err
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", "", execution.Subject{}, err
	}
	digestBytes := sha256.Sum256(append([]byte("weave.agent-execution/v1\x00"), canonical...))
	digest := hex.EncodeToString(digestBytes[:])
	return "agent-execution:" + digest, digest, subject, nil
}

func loadAgentExecutionPlan(ctx context.Context, tx pgx.Tx, workspaceID, requestID, digest string, subject execution.Subject) (agentExecutionPublicationPlan, bool, error) {
	var plan agentExecutionPublicationPlan
	var subjectRaw, requestRaw, commandsRaw, completedRaw, responseRaw []byte
	err := tx.QueryRow(ctx, `SELECT actor_subject,request_digest,agent_name,request,agent_version,
 publication_commands,completed_publications,state,response
 FROM weave_agent_execution_requests WHERE workspace_id=$1 AND request_id=$2 FOR UPDATE`, workspaceID, requestID).
		Scan(&subjectRaw, &plan.RequestDigest, &plan.AgentName, &requestRaw, &plan.AgentVersion,
			&commandsRaw, &completedRaw, &plan.State, &responseRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return plan, false, nil
	}
	if err != nil {
		return plan, false, err
	}
	plan.RequestID = requestID
	if err = json.Unmarshal(subjectRaw, &plan.Subject); err != nil {
		return plan, false, err
	}
	if plan.Subject != subject {
		return plan, false, execution.ErrSubjectMismatch
	}
	if plan.RequestDigest != digest {
		return plan, false, publication.ErrRequestConflict
	}
	if err = json.Unmarshal(requestRaw, &plan.Request); err != nil {
		return plan, false, err
	}
	if err = json.Unmarshal(commandsRaw, &plan.PublicationCommands); err != nil {
		return plan, false, err
	}
	if err = json.Unmarshal(completedRaw, &plan.CompletedPublications); err != nil {
		return plan, false, err
	}
	plan.Response = append(json.RawMessage(nil), responseRaw...)
	return plan, true, nil
}

func insertAgentExecutionPlan(ctx context.Context, tx pgx.Tx, plan agentExecutionPublicationPlan) error {
	subjectRaw, _ := json.Marshal(plan.Subject)
	requestRaw, _ := json.Marshal(plan.Request)
	commandsRaw, _ := json.Marshal(plan.PublicationCommands)
	_, err := tx.Exec(ctx, `INSERT INTO weave_agent_execution_requests(
 workspace_id,request_id,actor_subject,request_digest,agent_name,request,agent_version,publication_commands)
 VALUES($1,$2,$3::jsonb,$4,$5,$6::jsonb,$7,$8::jsonb)`, plan.Subject.WorkspaceID, plan.RequestID,
		string(subjectRaw), plan.RequestDigest, plan.AgentName, string(requestRaw), plan.AgentVersion, string(commandsRaw))
	return err
}

func recordAgentExecutionPublication(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, plan agentExecutionPublicationPlan, childRequestID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, found, err := loadAgentExecutionPlan(ctx, tx, plan.Subject.WorkspaceID, plan.RequestID, plan.RequestDigest, plan.Subject)
	if err != nil || !found {
		if err == nil {
			err = errors.New("agent execution publication plan not found")
		}
		return err
	}
	for _, completed := range current.CompletedPublications {
		if completed == childRequestID {
			return tx.Commit(ctx)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE weave_agent_execution_requests
 SET completed_publications=completed_publications || to_jsonb($3::text),updated_at=now()
 WHERE workspace_id=$1 AND request_id=$2 AND state='prepared'`, plan.Subject.WorkspaceID, plan.RequestID, childRequestID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func completeAgentExecutionPlan(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, plan agentExecutionPublicationPlan, response json.RawMessage) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, found, err := loadAgentExecutionPlan(ctx, tx, plan.Subject.WorkspaceID, plan.RequestID, plan.RequestDigest, plan.Subject)
	if err != nil || !found {
		if err == nil {
			err = errors.New("agent execution publication plan not found")
		}
		return err
	}
	if current.State == "completed" {
		return tx.Commit(ctx)
	}
	if len(current.CompletedPublications) != len(current.PublicationCommands) {
		return fmt.Errorf("agent execution publications incomplete: %d/%d", len(current.CompletedPublications), len(current.PublicationCommands))
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_agent_execution_requests SET state='completed',response=$3::jsonb,updated_at=now()
 WHERE workspace_id=$1 AND request_id=$2 AND state='prepared'`, plan.Subject.WorkspaceID, plan.RequestID, string(response))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("agent execution request completion conflict")
	}
	return tx.Commit(ctx)
}
