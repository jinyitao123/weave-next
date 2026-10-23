package deliverable

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type StoreOption func(*Store)

// AgentLabel resolves optional product presentation text in the caller's
// transaction. It never supplies execution or delivery ownership facts.
type AgentLabel func(context.Context, pgx.Tx, string, string) (string, error)

func WithAgentLabel(resolve AgentLabel) StoreOption {
	return func(s *Store) { s.agentLabel = resolve }
}
