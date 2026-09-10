package execution

// MemberBudgetPause identifies a persisted, unfinished member invocation.
// The host keeps the token private and projects only counters and reason.
type MemberBudgetPause struct {
	MemberRunID           string `json:"member_run_id"`
	Graph                 string `json:"graph"`
	CheckpointSeq         int64  `json:"checkpoint_seq"`
	YieldToken            string `json:"yield_token"`
	Slice                 uint64 `json:"slice"`
	RoundsUsed            uint64 `json:"rounds_used"`
	AuthorizedTotalRounds uint64 `json:"authorized_total_rounds"`
	Reason                string `json:"reason"`
}
