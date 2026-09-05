package teamrun

import (
	"context"
	"testing"
)

func TestActivityListRetainsLatestRecoveryInChronologicalOrder(t *testing.T) {
	h := newProcessNextHarness(t)
	_, _ = h.seedRunningWorkflowTaskBeforeAdmission(t, "long-run")
	ctx := context.Background()
	_, err := h.pool.Exec(ctx, `INSERT INTO weave_team_run_activity_events
		(workspace_id,run_id,event_id,kind,node_id,member_id,detail,occurred_at)
		SELECT 'workspace-1','long-run','event-' || n,
			CASE WHEN n=502 THEN 'member_started' ELSE 'member_failed' END,
			'verify','reviewer','{}'::jsonb,$1::timestamptz
		FROM generate_series(1,502) AS n ORDER BY n`, h.now)
	if err != nil {
		t.Fatal(err)
	}
	store := &PGActivityStore{Transactions: h.pool}
	events, err := store.List(ctx, "workspace-1", "long-run", 500)
	if err != nil || len(events) != 500 {
		t.Fatalf("list latest events: count=%d err=%v", len(events), err)
	}
	if events[0].EventID != "event-3" || events[499].EventID != "event-502" || events[499].Kind != "member_started" {
		t.Fatalf("recovery event missing: first=%s last=%s", events[0].EventID, events[499].EventID)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].Seq >= events[i].Seq {
			t.Fatal("recent activity is not chronological")
		}
	}
	for _, identity := range [][2]string{{"workspace-2", "long-run"}, {"workspace-1", "another-run"}} {
		if hidden, err := store.List(ctx, identity[0], identity[1], 500); err != nil || len(hidden) != 0 {
			t.Fatalf("activity crossed run identity: count=%d err=%v", len(hidden), err)
		}
	}
}
