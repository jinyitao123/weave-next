package fanout

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// ConversationProject resolves an optional product association under the same
// transaction as group creation; the kernel does not read conversation storage.
type ConversationProject func(context.Context, pgx.Tx, string, string) (string, error)

var ErrConversationProjectUnavailable = errors.New("conversation project resolver is unavailable")

type StoreOption func(*Store)

func WithConversationProject(resolve ConversationProject) StoreOption {
	return func(s *Store) { s.conversationProject = resolve }
}

// CompletionLead is the directory fact needed only when a group has no frozen
// source snapshot. Frozen execution identities always take precedence.
type CompletionLead struct {
	AgentID        string
	AgentVersion   int
	TeamFreeCollab bool
}

type CompletionLeadResolver func(context.Context, pgx.Tx, string, string) (CompletionLead, error)

var ErrCompletionLeadUnavailable = errors.New("task group completion lead resolver is unavailable")
var ErrCompletionLeadAmbiguous = errors.New("task group has conflicting frozen completion identities")

func WithCompletionLeadResolver(resolve CompletionLeadResolver) StoreOption {
	return func(s *Store) { s.completionLead = resolve }
}
