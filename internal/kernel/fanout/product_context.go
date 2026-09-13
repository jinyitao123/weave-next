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
