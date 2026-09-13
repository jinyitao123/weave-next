package loomruntime

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
)

// TerminalActivityProjector projects an accepted candidate's product activity
// inside the terminal transaction. It does not choose or change terminal state.
// Runtime-only deployments may omit this product projection.
type TerminalActivityProjector interface {
	ProjectTerminalActivityTx(context.Context, pgx.Tx, string, string, time.Time) error
}

func NewPGTerminalStateStoreWithActivity(projector TerminalActivityProjector) *PGTerminalStateStore {
	return &PGTerminalStateStore{activityProjector: projector}
}

func terminalStateStoreFor(records any) *PGTerminalStateStore {
	projector, _ := records.(TerminalActivityProjector)
	return NewPGTerminalStateStoreWithActivity(projector)
}
