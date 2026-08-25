package registry

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidChannelOrder indicates that a reorder request is not an exact
// permutation of an agent's channels with the default channel first.
var ErrInvalidChannelOrder = errors.New("invalid channel order")

// Channel is an agent-scoped conversation channel.
type Channel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Position  int    `json:"position"`
	IsDefault bool   `json:"is_default"`
}

// ListChannels returns an agent's channels in display order, with the default first.
func (r *AgentRegistry) ListChannels(ctx context.Context, workspace, agentName string) ([]Channel, error) {
	agentID, err := r.channelAgentID(ctx, r.pool, workspace, agentName)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, position, is_default
		FROM weave_channels
		WHERE workspace_id=$1 AND agent_id=$2
		ORDER BY is_default DESC, position, id
	`, workspace, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	channels := make([]Channel, 0)
	for rows.Next() {
		var channel Channel
		if err := scanChannel(rows, &channel); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

// CreateChannel appends a custom channel to an agent's channel list.
func (r *AgentRegistry) CreateChannel(ctx context.Context, workspace, agentName, name string) (*Channel, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	agentID, err := r.channelAgentIDForUpdate(ctx, tx, workspace, agentName)
	if err != nil {
		return nil, err
	}
	var channel Channel
	err = scanChannel(tx.QueryRow(ctx, `
		INSERT INTO weave_channels (workspace_id, agent_id, id, name, position, is_default)
		SELECT $1, $2, $3, $4, COALESCE(MAX(position), -1) + 1, false
		FROM weave_channels
		WHERE workspace_id=$1 AND agent_id=$2
		RETURNING id, name, position, is_default
	`, workspace, agentID, uuid.NewString(), name), &channel)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &channel, nil
}

// RenameChannel renames a custom channel in the requested workspace and agent scope.
func (r *AgentRegistry) RenameChannel(ctx context.Context, workspace, agentName, id, name string) (*Channel, error) {
	agentID, err := r.channelAgentID(ctx, r.pool, workspace, agentName)
	if err != nil {
		return nil, err
	}
	var channel Channel
	err = scanChannel(r.pool.QueryRow(ctx, `
		UPDATE weave_channels
		SET name=$4
		WHERE workspace_id=$1 AND agent_id=$2 AND id=$3 AND is_default=false
		RETURNING id, name, position, is_default
	`, workspace, agentID, id, name), &channel)
	if err != nil {
		return nil, err
	}
	return &channel, nil
}

// DeleteChannel removes a custom channel in the requested workspace and agent scope.
func (r *AgentRegistry) DeleteChannel(ctx context.Context, workspace, agentName, id string) error {
	agentID, err := r.channelAgentID(ctx, r.pool, workspace, agentName)
	if err != nil {
		return err
	}
	var deleted string
	return r.pool.QueryRow(ctx, `
		DELETE FROM weave_channels
		WHERE workspace_id=$1 AND agent_id=$2 AND id=$3 AND is_default=false
		RETURNING id
	`, workspace, agentID, id).Scan(&deleted)
}

// ReorderChannels atomically replaces every position for an agent's complete channel list.
func (r *AgentRegistry) ReorderChannels(ctx context.Context, workspace, agentName string, orderedIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	agentID, err := r.channelAgentID(ctx, tx, workspace, agentName)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM weave_channels
		WHERE workspace_id=$1 AND agent_id=$2
		FOR UPDATE
	`, workspace, agentID)
	if err != nil {
		return err
	}
	existing := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(orderedIDs) != len(existing) || len(orderedIDs) == 0 || orderedIDs[0] != "default" {
		return ErrInvalidChannelOrder
	}
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, id := range orderedIDs {
		if _, ok := existing[id]; !ok {
			return ErrInvalidChannelOrder
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrInvalidChannelOrder
		}
		seen[id] = struct{}{}
	}

	for position, id := range orderedIDs {
		if _, err := tx.Exec(ctx, `
			UPDATE weave_channels
			SET position=$4
			WHERE workspace_id=$1 AND agent_id=$2 AND id=$3
		`, workspace, agentID, id, position); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type channelQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (r *AgentRegistry) channelAgentID(ctx context.Context, q channelQuerier, workspace, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, workspace, name).Scan(&id)
	return id, err
}

func (r *AgentRegistry) channelAgentIDForUpdate(ctx context.Context, q channelQuerier, workspace, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
		FOR UPDATE
	`, workspace, name).Scan(&id)
	return id, err
}

func scanChannel(row rowScanner, channel *Channel) error {
	return row.Scan(&channel.ID, &channel.Name, &channel.Position, &channel.IsDefault)
}
