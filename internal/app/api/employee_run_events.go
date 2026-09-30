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
			COALESCE(NULLIF(workflow.name,''),NULLIF(team.name,''),'团队工作') AS team_name,
		COALESCE(NULLIF(deliverable.content,''),'') AS deliverable_content,
		deliverable.workbench_result AS workbench_result
		FROM weave_team_runs AS run
		JOIN weave_dispatch_input_revisions AS input
		  ON input.workspace_id=run.workspace_id AND input.consumed_run_id=run.run_id
		JOIN weave_external_identities AS identity
		  ON identity.workspace_id=input.workspace_id AND identity.user_id=input.user_id
		LEFT JOIN weave_teams AS team
		  ON team.workspace_id=run.workspace_id AND team.id=run.team_id
		LEFT JOIN weave_team_workflows AS workflow
		  ON workflow.workspace_id=run.workspace_id AND workflow.team_id=run.team_id AND workflow.id=run.workflow_id
		LEFT JOIN LATERAL (
		  SELECT content,metadata->'workbench_result' AS workbench_result FROM weave_final_deliverables
		  WHERE workspace_id=run.workspace_id AND run_id=run.run_id
		    AND COALESCE(metadata->>'artifact_kind','final')='final'
		  ORDER BY (metadata->'workbench_result' IS NOT NULL) DESC,created_at DESC,id DESC LIMIT 1
		) AS deliverable ON true
		WHERE run.status IN ('succeeded','failed','cancelled','abandoned')
	), business_action_receipts AS (
		SELECT DISTINCT ON (started.workspace_id,started.run_id,started.node_id,started.member_id,
			started.detail->>'invocation_id',started.detail->>'tool_call_id')
			started.workspace_id,started.run_id,started.seq,
			left(COALESCE(NULLIF(started.detail->>'action_label',''),NULLIF(started.detail->>'action_name',''),'业务动作'),128) AS action_label,
			COALESCE(outcome.status,'unknown') AS status
		FROM weave_team_run_activity_events AS started
		LEFT JOIN LATERAL (
			SELECT result.detail->>'status' AS status
			FROM weave_team_run_activity_events AS result
			WHERE result.workspace_id=started.workspace_id AND result.run_id=started.run_id
			  AND result.kind='business_action_result'
			  AND result.node_id IS NOT DISTINCT FROM started.node_id
			  AND result.member_id IS NOT DISTINCT FROM started.member_id
			  AND result.detail->>'invocation_id'=started.detail->>'invocation_id'
			  AND result.detail->>'tool_call_id'=started.detail->>'tool_call_id'
			ORDER BY result.seq DESC LIMIT 1
		) AS outcome ON true
		WHERE started.kind='business_action_started' AND started.detail->>'source'='forge_mcp.run_action'
		ORDER BY started.workspace_id,started.run_id,started.node_id,started.member_id,
			started.detail->>'invocation_id',started.detail->>'tool_call_id',started.seq DESC
	), business_action_summary AS (
		SELECT workspace_id,run_id,count(*) AS action_count,
			count(*) FILTER (WHERE status='succeeded') AS succeeded_count,
			count(*) FILTER (WHERE status='failed') AS failed_count,
			count(*) FILTER (WHERE status NOT IN ('succeeded','failed')) AS unknown_count,
			string_agg('业务动作“'||action_label||'”'||CASE status
				WHEN 'succeeded' THEN '调用返回成功。'
				WHEN 'failed' THEN '调用返回失败，请先核对业务记录后再处理。'
				ELSE '结果未知，请先核对业务记录后再处理。' END,'；' ORDER BY seq) AS summary
		FROM business_action_receipts
		GROUP BY workspace_id,run_id
	)
	INSERT INTO weave_employee_run_event_outbox(event_id,workspace_id,run_id,input_revision_id,payload)
	SELECT (
		substr(hash,1,8)||'-'||substr(hash,9,4)||'-5'||substr(hash,14,3)||'-8'||substr(hash,18,3)||'-'||substr(hash,21,12)
	  )::uuid,fixed.workspace_id,fixed.run_id,fixed.input_revision_id,
	  jsonb_build_object(
		'version','1','eventId',(
		  substr(hash,1,8)||'-'||substr(hash,9,4)||'-5'||substr(hash,14,3)||'-8'||substr(hash,18,3)||'-'||substr(hash,21,12)
		),'kind',CASE
		  WHEN status='succeeded' AND workbench_result->>'disposition'='needs_input'
		    AND COALESCE(business_action_summary.succeeded_count,0)=0 THEN 'revision_required'
		  WHEN status='succeeded' THEN 'result'
		  WHEN status='cancelled' THEN 'cancelled'
		  ELSE 'failure' END,
		'organizationId',external_organization,'assigneeAccountId',assignee_account_id,
		'title',left('团队运行'||CASE
		  WHEN status='succeeded' AND workbench_result->>'disposition'='needs_input'
		    AND COALESCE(business_action_summary.succeeded_count,0)=0 THEN '需要补充材料'
		  WHEN status='succeeded' THEN '已完成'
		  WHEN status='cancelled' THEN '已取消'
		  WHEN status='abandoned' THEN '已放弃'
		  ELSE '失败' END||CASE
		  WHEN COALESCE(business_action_summary.failed_count,0)>0 OR COALESCE(business_action_summary.unknown_count,0)>0
		    THEN '（业务动作需核对）' ELSE '' END||'：'||team_name,300),
		'summary',left(CASE
			WHEN business_action_summary.action_count IS NULL
				THEN '平台回执：本轮 Forge 业务动作调用记录为 0 条。模型摘要或团队成果不证明业务写入或正式业务状态。'
			ELSE '' END||CASE
		  WHEN status='succeeded' AND workbench_result->>'disposition'='needs_input' THEN
			CASE WHEN COALESCE(business_action_summary.succeeded_count,0)>0
			  THEN '团队检查意见（模型输出）：' ELSE '团队检查摘要（模型输出）：' END||
			COALESCE(NULLIF(workbench_result->>'summary',''),'本轮检查发现需要补充的信息。')||CASE
			  WHEN CASE WHEN jsonb_typeof(workbench_result->'missing_items')='array' THEN jsonb_array_length(workbench_result->'missing_items') ELSE 0 END>0 THEN
			    CASE WHEN COALESCE(business_action_summary.succeeded_count,0)>0 THEN ' 模型意见提及：' ELSE ' 需要补充：' END||(
				SELECT string_agg(item.value,'；') FROM jsonb_array_elements_text(workbench_result->'missing_items') AS item(value)
			  ) ELSE '' END
			||CASE
			  WHEN business_action_summary.action_count>12 THEN
				'。业务动作调用结果：成功 '||business_action_summary.succeeded_count||' 项，失败 '||business_action_summary.failed_count||
				' 项，结果未知 '||business_action_summary.unknown_count||' 项。失败或未知结果请先核对 Forge 业务记录后再决定下一步。'||
				CASE WHEN COALESCE(business_action_summary.succeeded_count,0)>0
				  THEN '后续正式业务事项由 Forge 原生业务状态决定。' ELSE '' END
			  WHEN business_action_summary.summary IS NOT NULL THEN
				'。业务动作调用结果：'||business_action_summary.summary||CASE
				  WHEN COALESCE(business_action_summary.succeeded_count,0)>0
				    THEN '本消息中的动作结果只反映调用回执；后续正式业务事项由 Forge 原生业务状态决定。'
				  ELSE '本消息中的动作结果只反映调用回执；正式审批状态请以 Forge 业务记录为准。' END
			  ELSE '' END
		  WHEN business_action_summary.action_count>12 THEN
			'团队运行状态：'||CASE status WHEN 'succeeded' THEN '已完成' WHEN 'cancelled' THEN '已取消' WHEN 'abandoned' THEN '已放弃' ELSE '失败' END||
			'。业务动作调用结果：成功 '||business_action_summary.succeeded_count||' 项，失败 '||business_action_summary.failed_count||
			' 项，结果未知 '||business_action_summary.unknown_count||' 项。失败或未知结果请先核对 Forge 业务记录后再决定下一步；本消息不代表正式审批状态。'
		  WHEN business_action_summary.summary IS NOT NULL THEN
			'团队运行状态：'||CASE status WHEN 'succeeded' THEN '已完成' WHEN 'cancelled' THEN '已取消' WHEN 'abandoned' THEN '已放弃' ELSE '失败' END||
			'。业务动作调用结果：'||business_action_summary.summary||
			'本消息中的动作结果只反映调用回执；正式审批状态请以 Forge 业务记录为准。'
		  WHEN status='succeeded' AND workbench_result->>'disposition'='complete' THEN
			'团队检查摘要（模型输出）：'||COALESCE(NULLIF(workbench_result->>'summary',''),'本轮检查已完成。')
		  WHEN status='succeeded' AND deliverable_content<>'' THEN '团队运行状态：已完成。团队成果（模型输出）：'||deliverable_content
		  WHEN status='succeeded' THEN '团队运行状态：已完成。团队工作已结束，可在桌面查看结果。'
		  WHEN status='cancelled' THEN '团队运行状态：已取消。'
		  WHEN status='abandoned' THEN '团队运行状态：已放弃。'
		  WHEN cause_summary IS NOT NULL THEN '团队运行状态：失败。团队处理失败：'||cause_summary
			ELSE '团队运行状态：失败。团队处理失败，请在桌面查看运行记录。' END||CASE
			WHEN status='failed' AND business_action_summary.action_count IS NOT NULL AND cause_summary IS NOT NULL
			  THEN '团队处理失败：'||cause_summary ELSE '' END,4000),
		'occurredAt',terminal_at,
		'source',jsonb_build_object(
		  'workReference',input_revision_id,'runReference',fixed.run_id,
		  'sessionReference',workbench_session_id,
		  'idempotencyKey','weave-team-run-terminal:'||fixed.run_id
		)
	  )
	FROM (SELECT candidates.*,md5('weave-team-run-event'||chr(31)||workspace_id||chr(31)||run_id) AS hash FROM candidates) AS fixed
	LEFT JOIN business_action_summary ON business_action_summary.workspace_id=fixed.workspace_id AND business_action_summary.run_id=fixed.run_id
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
