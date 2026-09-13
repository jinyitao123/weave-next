package taskqueue

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// admissionSubject joins trusted request identity with durable parent facts.
// A task body cannot switch the actor of a child, retry, or resumed snapshot.
func (s *Store) admissionSubject(ctx context.Context, tx pgx.Tx, task *Task) (execution.Subject, error) {
	subject := task.Subject
	merge := func(inherited execution.Subject) error {
		if inherited.Validate() != nil || inherited.WorkspaceID != task.WorkspaceID {
			return execution.ErrSubjectMismatch
		}
		if subject != (execution.Subject{}) && subject != inherited {
			return execution.ErrSubjectMismatch
		}
		subject = inherited
		return nil
	}
	if authenticated, ok := execution.SubjectFromContext(ctx); ok {
		if err := merge(authenticated); err != nil {
			return execution.Subject{}, err
		}
	}
	for _, parent := range []struct {
		value, query string
	}{
		{task.ParentTaskID, `SELECT actor_subject FROM weave_task_queue WHERE workspace_id=$1 AND id=$2 FOR SHARE`},
		{task.RunSnapshotID, `SELECT actor_subject FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$2 FOR SHARE`},
	} {
		if parent.value == "" {
			continue
		}
		var raw []byte
		if err := tx.QueryRow(ctx, parent.query, task.WorkspaceID, parent.value).Scan(&raw); err != nil {
			return execution.Subject{}, err
		}
		var inherited execution.Subject
		if err := json.Unmarshal(raw, &inherited); err != nil {
			return execution.Subject{}, err
		}
		if err := merge(inherited); err != nil {
			return execution.Subject{}, err
		}
	}
	if err := subject.Validate(); err != nil {
		return execution.Subject{}, err
	}
	if subject.WorkspaceID != task.WorkspaceID {
		return execution.Subject{}, execution.ErrSubjectMismatch
	}
	return subject, nil
}

// BindTaskSubject restores the immutable task actor for handlers and nested calls.
func BindTaskSubject(ctx context.Context, task *Task) (context.Context, error) {
	if task == nil || task.Subject.WorkspaceID != task.WorkspaceID {
		return nil, execution.ErrSubjectMismatch
	}
	return execution.BindSubject(ctx, task.Subject)
}

func subjectFilter(ctx context.Context) any {
	subject, ok := execution.SubjectFromContext(ctx)
	if !ok {
		return nil
	}
	encoded, _ := json.Marshal(subject)
	return string(encoded)
}
