package teamrun

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
)

// MemberBudgetCoordinator joins the member's grant to the existing parent
// resume transaction; its implementation owns member checkpoint details.
type MemberBudgetCoordinator interface {
	Authorize(context.Context, pgx.Tx, string, string, string, string, execution.MemberBudgetPause, uint64) error
	HasProgress(context.Context, pgx.Tx, string, execution.MemberBudgetPause) (bool, error)
}

type ActiveMemberInvocation struct {
	ResumeGrantID string `json:"resume_grant_id,omitempty"`
	NodeID        string `json:"node_id"`
	CallID        string `json:"call_id"`
	EntryOrdinal  uint64 `json:"entry_ordinal"`
}
