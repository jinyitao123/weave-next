package schedule

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CommitAcceptedOccurrenceTx records an already accepted execution. If an
// operator changed the schedule while acceptance was in flight, its new cursor
// is preserved; the original occurrence still retains its exact receipt.
func (s *Store) CommitAcceptedOccurrenceTx(ctx context.Context, tx pgx.Tx, occurrence Occurrence, captured Schedule) error {
	current, err := scanSchedule(tx.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM weave_schedule WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, captured.WorkspaceID, captured.ID))
	if err != nil {
		return err
	}
	if captured.WorkspaceID != occurrence.WorkspaceID || captured.ID != occurrence.ScheduleID || captured.TargetWorkflowID != occurrence.TargetWorkflowID || OccurrenceKey(captured, occurrence.ScheduledFor) != occurrence.OccurrenceKey {
		return fmt.Errorf("captured schedule differs from accepted occurrence")
	}
	if err = s.CommitOccurrenceTx(ctx, tx, occurrence); err != nil {
		return err
	}
	if !sameLockedSchedule(current, captured) {
		return nil
	}
	return s.AdvanceCursorTx(ctx, tx, current, occurrence.ScheduledFor)
}
