package teamrun

type ActiveMemberInvocation struct {
	NodeID       string `json:"node_id"`
	CallID       string `json:"call_id"`
	EntryOrdinal uint64 `json:"entry_ordinal"`
}
