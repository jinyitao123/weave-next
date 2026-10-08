package fanout

type TaskLegMapping struct {
	State    LegDecisionState
	Terminal *LegTerminal
	Reason   string
}
