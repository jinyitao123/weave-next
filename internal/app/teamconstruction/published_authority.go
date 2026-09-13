package teamconstruction

import (
	"context"
	"errors"
	"strings"

	"github.com/jinyitao123/weave/internal/app/workflowadmission"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// AuthorizePublished rechecks a server-persisted delivery intent and live
// product access. It does not reuse a request-supplied grant or share its read
// transaction with the kernel's acceptance transaction.
func (a *PublicationAuthority) AuthorizePublished(ctx context.Context, request publication.PublishedRunRequest, envelope frozen.ArtifactEnvelopeV1) error {
	if a == nil || a.pool == nil {
		return errors.New("published authority unavailable")
	}
	subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
	if err != nil {
		return err
	}
	record, err := workflowadmission.New(a.pool, nil).Get(ctx, envelope.WorkspaceID, request.RequestID)
	if err != nil {
		return err
	}
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	storedDigest, err := record.Request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	if digest != storedDigest || record.Subject != subject || publication.CandidateRevision(envelope) != request.Revision {
		return publication.ErrRequestConflict
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	if record.Target.TeamID != payload.Team.TeamID {
		return publication.ErrRequestConflict
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = authorizePublicationActorTx(ctx, tx, subject.WorkspaceID, payload.Team.TeamID); err != nil {
		return err
	}
	var teamID, workflowStatus, versionStatus, teamStatus, leadID string
	err = tx.QueryRow(ctx, `SELECT w.team_id,w.status,v.status,t.status,t.lead_avatar_id FROM weave_team_workflows w JOIN weave_team_workflow_versions v ON v.workspace_id=w.workspace_id AND v.workflow_id=w.id JOIN weave_teams t ON t.workspace_id=w.workspace_id AND t.id=w.team_id WHERE w.workspace_id=$1 AND w.id=$2 AND v.version=$3 FOR SHARE OF w,v,t`, subject.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion).Scan(&teamID, &workflowStatus, &versionStatus, &teamStatus, &leadID)
	if err != nil {
		return err
	}
	if teamID != payload.Team.TeamID || workflowStatus != "active" || versionStatus != "published" || teamStatus != "active" || leadID != payload.Team.LeadAgentID {
		return workflow.ErrWorkflowScheduleAdmissionDenied
	}
	if err = workflowcatalog.EvaluatePublishedRosterTx(ctx, tx, subject.WorkspaceID, teamID, payload.GraphDefinition, envelope.WorkflowVersion); err != nil {
		return err
	}
	if request.Trigger.Type == "schedule" {
		if subject.ServiceID != "workflow-schedule:"+record.Target.ScheduleID || record.Target.ScheduleID != request.Trigger.SourceRef || record.Target.OccurrenceKey != request.Trigger.OccurrenceKey {
			return execution.ErrSubjectMismatch
		}
		var enabled bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_schedule WHERE workspace_id=$1 AND id=$2 AND enabled AND target_kind='team_workflow' AND target_workflow_id=$3)`, subject.WorkspaceID, record.Target.ScheduleID, envelope.WorkflowID).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return errors.New("schedule authorization revoked")
		}
	} else if strings.HasPrefix(subject.ServiceID, "workflow-schedule:") {
		return execution.ErrSubjectMismatch
	}
	if record.Target.ConversationID != "" {
		var allowed bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_conversations WHERE workspace_id=$1 AND user_id=$2 AND id=$3 AND project_id=$4)`, subject.WorkspaceID, subject.UserID, record.Target.ConversationID, request.ProjectID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return publication.ErrRequestConflict
		}
	}
	if record.Target.ChatRequestID != "" {
		var allowed bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_chat_requests WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3 AND user_message_id=$4 AND conversation_id=$5 AND project_id=$6)`, subject.WorkspaceID, subject.UserID, record.Target.ChatRequestID, request.InputVersion, record.Target.ConversationID, request.ProjectID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return publication.ErrRequestConflict
		}
	}
	if record.Target.InputRevisionID != "" {
		var current, closed bool
		var runID, taskID string
		err = tx.QueryRow(ctx, `SELECT is_current,closed_at IS NOT NULL,COALESCE(consumed_run_id,''),COALESCE(consumed_task_id,'') FROM weave_dispatch_input_revisions WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 FOR SHARE`, subject.WorkspaceID, subject.UserID, record.Target.InputRevisionID).Scan(&current, &closed, &runID, &taskID)
		if err != nil {
			return err
		}
		if closed {
			return publication.ErrAdmissionClosed
		}
		if (!current && runID == "") || (runID != "" && (runID != request.RunID || taskID != request.TaskID)) {
			return publication.ErrRequestConflict
		}
	}
	return nil
}
