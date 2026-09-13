package deliverable

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ConversationOwner resolves a root conversation inside the caller's transaction.
// Product storage and tenant validation belong to the injected implementation.
type ConversationOwner func(context.Context, pgx.Tx, string, string) (userID string, found bool, err error)

var ErrConversationOwnerUnavailable = errors.New("conversation owner resolver is unavailable")

type StoreOption func(*Store)

func WithConversationOwner(resolve ConversationOwner) StoreOption {
	return func(s *Store) { s.conversationOwner = resolve }
}

// AgentLabel resolves optional product presentation text in the caller's
// transaction. It never supplies execution or delivery ownership facts.
type AgentLabel func(context.Context, pgx.Tx, string, string) (string, error)

func WithAgentLabel(resolve AgentLabel) StoreOption {
	return func(s *Store) { s.agentLabel = resolve }
}
