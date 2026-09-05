package schedule

import (
	"testing"
	"time"
)

func TestRetiredAgentScheduleNeverBecomesDue(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, target := range []string{"agent", ""} {
		item := Schedule{TargetKind: target, Enabled: true, Kind: KindDaily,
			TimeOfDay: "00:00", Timezone: "Legacy/Missing"}
		if due, err := DueScheduledFor(item, now); due != nil || err != nil {
			t.Fatalf("retired target %q entered scheduling: due=%+v err=%v", target, due, err)
		}
	}
	item := Schedule{TargetKind: TargetTeamWorkflow, Enabled: true, Kind: KindDaily,
		TimeOfDay: "09:00", Timezone: "UTC"}
	due, err := DueScheduledFor(item, now)
	if err != nil || due == nil || due.LocalDate != "2026-09-05" || due.ScheduledFor.Hour() != 9 {
		t.Fatalf("team workflow lost its daily occurrence: due=%+v err=%v", due, err)
	}
}
