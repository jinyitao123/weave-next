package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	forgeEmployeeEventURLEnv    = "WEAVE_FORGE_EVENT_URL"
	forgeEmployeeEventSecretEnv = "WEAVE_FORGE_EVENT_SECRET"
	employeeEventBatchSize      = 32
)

type employeeRunEvent struct {
	EventID  string
	Payload  json.RawMessage
	Attempts int
}

type employeeRunEventWorker struct {
	Pool         *pgxpool.Pool
	Endpoint     string
	Secret       string
	Client       *http.Client
	PollInterval time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newEmployeeRunEventWorker(pool *pgxpool.Pool) *employeeRunEventWorker {
	endpoint := strings.TrimSpace(os.Getenv(forgeEmployeeEventURLEnv))
	secret := strings.TrimSpace(os.Getenv(forgeEmployeeEventSecretEnv))
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		endpoint = ""
	}
	return &employeeRunEventWorker{
		Pool: pool, Endpoint: endpoint, Secret: secret,
		Client: &http.Client{Timeout: 10 * time.Second}, PollInterval: 2 * time.Second,
	}
}

func (worker *employeeRunEventWorker) configured() bool {
	return worker != nil && worker.Pool != nil && worker.Endpoint != "" && worker.Secret != ""
}

func (worker *employeeRunEventWorker) Start() {
	if !worker.configured() {
		return
	}
	worker.mu.Lock()
	if worker.cancel != nil {
		worker.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker.cancel = cancel
	worker.wg.Add(1)
	worker.mu.Unlock()
	go func() {
		defer worker.wg.Done()
		for {
			processed, err := worker.Sweep(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error("employee run event delivery failed", "error", err)
			}
			if processed == 0 {
				timer := time.NewTimer(worker.pollInterval())
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
		}
	}()
	slog.Info("employee run event worker started")
}

func (worker *employeeRunEventWorker) Stop() {
	if worker == nil {
		return
	}
	worker.mu.Lock()
	cancel := worker.cancel
	worker.cancel = nil
	worker.mu.Unlock()
	if cancel != nil {
		cancel()
		worker.wg.Wait()
	}
}

func (worker *employeeRunEventWorker) pollInterval() time.Duration {
	if worker.PollInterval > 0 {
		return worker.PollInterval
	}
	return 2 * time.Second
}

func (worker *employeeRunEventWorker) Sweep(ctx context.Context) (int, error) {
	if !worker.configured() {
		return 0, nil
	}
	if err := worker.materialize(ctx); err != nil {
		return 0, err
	}
	event, err := worker.claim(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	status, notificationID, deliveryErr := worker.deliver(ctx, event)
	if err := worker.finish(ctx, event, status, notificationID, deliveryErr); err != nil {
		return 0, err
	}
	return 1, nil
}

func (worker *employeeRunEventWorker) materialize(ctx context.Context) error {
	_, err := worker.Pool.Exec(ctx, `WITH candidates AS (
		SELECT run.workspace_id,run.run_id,run.status,run.terminal_at,run.cause_summary,
			input.input_revision_id,input.workbench_session_id,input.project_id,
			identity.subject AS assignee_account_id,identity.workspace_id AS external_organization,
			COALESCE(NULLIF(team.name,''),'团队工作') AS team_name,
			COALESCE(NULLIF(deliverable.content,''),'') AS deliverable_content
		FROM weave_team_runs AS run
		JOIN weave_dispatch_input_revisions AS input
		  ON input.workspace_id=run.workspace_id AND input.consumed_run_id=run.run_id
		JOIN weave_external_identities AS identity
		  ON identity.workspace_id=input.workspace_id AND identity.user_id=input.user_id
		LEFT JOIN weave_teams AS team
		  ON team.workspace_id=run.workspace_id AND team.id=run.team_id
		LEFT JOIN LATERAL (
		  SELECT content FROM weave_final_deliverables
		  WHERE workspace_id=run.workspace_id AND run_id=run.run_id
		    AND COALESCE(metadata->>'artifact_kind','final')='final'
		  ORDER BY created_at DESC,id DESC LIMIT 1
		) AS deliverable ON true
		WHERE run.status IN ('succeeded','failed','cancelled','abandoned')
	)
	INSERT INTO weave_employee_run_event_outbox(event_id,workspace_id,run_id,input_revision_id,payload)
	SELECT (
		substr(hash,1,8)||'-'||substr(hash,9,4)||'-5'||substr(hash,14,3)||'-8'||substr(hash,18,3)||'-'||substr(hash,21,12)
	  )::uuid,workspace_id,run_id,input_revision_id,
	  jsonb_build_object(
		'version','1','eventId',(
		  substr(hash,1,8)||'-'||substr(hash,9,4)||'-5'||substr(hash,14,3)||'-8'||substr(hash,18,3)||'-'||substr(hash,21,12)
		),'kind',CASE status WHEN 'succeeded' THEN 'result' WHEN 'cancelled' THEN 'cancelled' ELSE 'failure' END,
		'organizationId',external_organization,'assigneeAccountId',assignee_account_id,
		'title',team_name||CASE status WHEN 'succeeded' THEN '已完成' WHEN 'cancelled' THEN '已取消' ELSE '处理失败' END,
		'summary',left(CASE
		  WHEN status='succeeded' AND deliverable_content<>'' THEN deliverable_content
		  WHEN status='succeeded' THEN '团队工作已完成，可在桌面查看结果。'
		  WHEN status='cancelled' THEN '本次团队工作已取消。'
		  WHEN cause_summary IS NOT NULL THEN '团队处理失败：'||cause_summary
		  ELSE '团队处理失败，请在桌面查看并重试。' END,4000),
		'occurredAt',terminal_at,
		'source',jsonb_build_object(
		  'workReference',input_revision_id,'runReference',run_id,
		  'sessionReference',workbench_session_id,
		  'idempotencyKey','weave-team-run-terminal:'||run_id
		)
	  )
	FROM (SELECT candidates.*,md5('weave-team-run-event'||chr(31)||workspace_id||chr(31)||run_id) AS hash FROM candidates) AS fixed
	ON CONFLICT (workspace_id,run_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("materialize employee run events: %w", err)
	}
	return nil
}

func (worker *employeeRunEventWorker) claim(ctx context.Context) (employeeRunEvent, error) {
	var event employeeRunEvent
	err := worker.Pool.QueryRow(ctx, `WITH candidate AS (
		SELECT event_id FROM weave_employee_run_event_outbox
		WHERE (delivery_state='pending' AND next_attempt_at<=statement_timestamp())
		   OR (delivery_state='delivering' AND claimed_at<=statement_timestamp()-interval '1 minute')
		ORDER BY next_attempt_at,created_at,event_id
		FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE weave_employee_run_event_outbox AS event SET
		delivery_state='delivering',delivery_attempts=event.delivery_attempts+1,
		claimed_at=statement_timestamp(),updated_at=statement_timestamp()
	FROM candidate WHERE event.event_id=candidate.event_id
	RETURNING event.event_id::text,event.payload,event.delivery_attempts`).Scan(&event.EventID, &event.Payload, &event.Attempts)
	if err != nil {
		return employeeRunEvent{}, err
	}
	return event, nil
}

func (worker *employeeRunEventWorker) deliver(ctx context.Context, event employeeRunEvent) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, worker.Endpoint, bytes.NewReader(event.Payload))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+worker.Secret)
	response, err := worker.Client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		return response.StatusCode, "", readErr
	}
	var receipt struct {
		NotificationID string `json:"notificationId"`
		Accepted       bool   `json:"accepted"`
	}
	_ = json.Unmarshal(body, &receipt)
	if (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusAccepted) && receipt.Accepted {
		return response.StatusCode, strings.TrimSpace(receipt.NotificationID), nil
	}
	return response.StatusCode, "", fmt.Errorf("Forge event ingress returned status %d", response.StatusCode)
}

func (worker *employeeRunEventWorker) finish(ctx context.Context, event employeeRunEvent, status int, notificationID string, deliveryErr error) error {
	if deliveryErr == nil {
		_, err := worker.Pool.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET
			delivery_state='delivered',claimed_at=NULL,last_error=NULL,forge_notification_id=NULLIF($2,''),
			delivered_at=statement_timestamp(),updated_at=statement_timestamp()
			WHERE event_id=$1 AND delivery_state='delivering'`, event.EventID, notificationID)
		return err
	}
	message := deliveryErr.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	permanent := status == http.StatusBadRequest || status == http.StatusUnauthorized ||
		status == http.StatusForbidden || status == http.StatusConflict || status == http.StatusUnprocessableEntity
	if permanent {
		_, err := worker.Pool.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET
			delivery_state='permanent_failure',claimed_at=NULL,last_error=$2,updated_at=statement_timestamp()
			WHERE event_id=$1 AND delivery_state='delivering'`, event.EventID, message)
		return err
	}
	delaySeconds := 1 << min(event.Attempts, 8)
	_, err := worker.Pool.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET
		delivery_state='pending',claimed_at=NULL,last_error=$2,
		next_attempt_at=statement_timestamp()+($3::text||' seconds')::interval,updated_at=statement_timestamp()
		WHERE event_id=$1 AND delivery_state='delivering'`, event.EventID, message, strconv.Itoa(delaySeconds))
	return err
}
