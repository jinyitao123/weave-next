package teamrun

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

// Once a resume advances the lease, the parked attempt's yielded marker stays
// until the resumed attempt writes its own. The resumed attempt is read as
// having no marker yet, not as a conflict, so a reconciler does not take it
// for a vanished creator and close the group under it.
func TestCreatorLeaseStateReadsAnEarlierAttemptsMarkerAsAbsent(t *testing.T) {
	parked, resumed := uuid.New(), uuid.New()
	lease := loomruntime.RunAttemptLease{WorkspaceID: "w", RunID: "r", AttemptGeneration: 2, AttemptID: resumed}
	for _, tc := range []struct {
		name     string
		marker   loomruntime.TerminalMarkerV1
		present  bool
		phase    string
		conflict bool
	}{
		{name: "no marker", phase: "absent"},
		{name: "parked attempt's marker", present: true, phase: "absent",
			marker: loomruntime.TerminalMarkerV1{AttemptGeneration: 1, AttemptID: parked, Phase: loomruntime.TerminalMarkerPhaseYielded}},
		{name: "own marker", present: true, phase: string(loomruntime.TerminalMarkerPhaseYielded),
			marker: loomruntime.TerminalMarkerV1{AttemptGeneration: 2, AttemptID: resumed, Phase: loomruntime.TerminalMarkerPhaseYielded}},
		{name: "same generation, other attempt", present: true, conflict: true,
			marker: loomruntime.TerminalMarkerV1{AttemptGeneration: 2, AttemptID: parked, Phase: loomruntime.TerminalMarkerPhaseYielded}},
		{name: "later generation", present: true, conflict: true,
			marker: loomruntime.TerminalMarkerV1{AttemptGeneration: 3, AttemptID: uuid.New(), Phase: loomruntime.TerminalMarkerPhaseYielded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := creatorLeaseStateOf(lease, tc.marker, tc.present)
			if tc.conflict {
				if fanout.ErrorCodeOf(err) != fanout.ErrorResumeConflict {
					t.Fatalf("err = %v, want a resume conflict", err)
				}
				return
			}
			if err != nil || state.MarkerPhase != tc.phase || state.AttemptGeneration != 2 || state.AttemptID != resumed.String() {
				t.Fatalf("state = %+v, err = %v, want phase %q for the resumed attempt", state, err, tc.phase)
			}
		})
	}
}
