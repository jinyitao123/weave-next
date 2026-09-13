package agentcatalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/kernel/registry"

	"github.com/jackc/pgx/v5"
)

// ListLinks returns every relationship in a workspace.
func (r *AgentRegistry) ListLinks(ctx context.Context, tenant string) ([]registry.Link, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, from_agent_id, to_agent_id, type, instruction, kind
		FROM weave_agent_links
		WHERE workspace_id=$1
		ORDER BY created_at, id
	`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []registry.Link
	for rows.Next() {
		var link registry.Link
		if err := scanLink(rows, &link); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// CreateLink creates a workspace-scoped relationship between two active agents.
func (r *AgentRegistry) CreateLink(ctx context.Context, tenant, sourceID, targetID, linkType, instruction string, kinds ...string) (*registry.Link, error) {
	if linkType == "peer" && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	kind, err := linkKind(kinds)
	if err != nil {
		return nil, err
	}
	if linkType == "manages" {
		return r.createLegacyManagesLink(ctx, tenant, sourceID, targetID, instruction, kind)
	}

	var link registry.Link
	err = scanLink(r.pool.QueryRow(ctx, `
		INSERT INTO weave_agent_links (
			workspace_id, from_agent_id, to_agent_id, type, instruction, kind
		)
		SELECT $1, source.id, target.id, $4, $5, $6
		FROM weave_agents AS source
		CROSS JOIN weave_agents AS target
		WHERE source.workspace_id=$1 AND source.id=$2 AND source.deleted=false
		  AND target.workspace_id=$1 AND target.id=$3 AND target.deleted=false
		RETURNING id::text, from_agent_id, to_agent_id, type, instruction, kind
	`, tenant, sourceID, targetID, linkType, instruction, kind), &link)
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func (r *AgentRegistry) createLegacyManagesLink(
	ctx context.Context,
	tenant, sourceID, targetID, instruction, kind string,
) (*registry.Link, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return nil, err
	}
	roles, err := lockActiveAgents(ctx, tx, tenant, sourceID, targetID)
	if err != nil {
		return nil, err
	}
	if _, ok := roles[sourceID]; !ok {
		return nil, fmt.Errorf("source agent %q not found for tenant %q", sourceID, tenant)
	}
	if _, ok := roles[targetID]; !ok {
		return nil, fmt.Errorf("target agent %q not found for tenant %q", targetID, tenant)
	}
	var teamContext bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_agents AS source
			JOIN weave_teams AS team
			  ON team.workspace_id=source.workspace_id
			 AND (
				team.lead_avatar_id=source.id OR
				(team.lead_avatar_id IS NULL AND team.id=source.team_id)
			 )
			WHERE source.workspace_id=$1 AND source.id=$2
		)
	`, tenant, sourceID).Scan(&teamContext); err != nil {
		return nil, err
	}
	if teamContext {
		return nil, fmt.Errorf("%w: lead %q", registry.ErrTeamRosterWriteRequired, sourceID)
	}
	var link registry.Link
	if err := scanLink(tx.QueryRow(ctx, `
		INSERT INTO weave_agent_links (
			workspace_id, from_agent_id, to_agent_id, type, instruction, kind
		) VALUES ($1, $2, $3, 'manages', $4, $5)
		RETURNING id::text, from_agent_id, to_agent_id, type, instruction, kind
	`, tenant, sourceID, targetID, instruction, kind), &link); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &link, nil
}

// UpdateLinkInstruction replaces a link's dispatch guidance.
func (r *AgentRegistry) UpdateLinkInstruction(ctx context.Context, tenant, id, instruction string) (*registry.Link, error) {
	var link registry.Link
	err := scanLink(r.pool.QueryRow(ctx, `
		UPDATE weave_agent_links
		SET instruction=$3
		WHERE workspace_id=$1 AND id=$2
		RETURNING id::text, from_agent_id, to_agent_id, type, instruction, kind
	`, tenant, id, instruction), &link)
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// DeleteLink removes a relationship without deleting either agent.
func (r *AgentRegistry) DeleteLink(ctx context.Context, tenant, id string) (*registry.DeletedLink, error) {
	var link registry.Link
	if err := scanLink(r.pool.QueryRow(ctx, `
		DELETE FROM weave_agent_links
		WHERE workspace_id=$1 AND id=$2
		RETURNING id::text, from_agent_id, to_agent_id, type, instruction, kind
	`, tenant, id), &link); err != nil {
		return nil, err
	}
	return &registry.DeletedLink{Link: link}, nil
}

// ListOrphanWorkers returns active workers with neither a TeamWorker relation
// nor a legacy incoming manages link.
func (r *AgentRegistry) ListOrphanWorkers(ctx context.Context, tenant string) ([]registry.OrphanWorker, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT worker.id, worker.name
		FROM weave_agents AS worker
		WHERE worker.workspace_id=$1
		  AND worker.role='worker'
		  AND worker.deleted=false
		  AND NOT EXISTS (
			SELECT 1
			FROM weave_team_workers AS team_worker
			WHERE team_worker.workspace_id=$1
			  AND team_worker.worker_agent_id=worker.id
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM weave_agent_links AS link
			WHERE link.workspace_id=$1
			  AND link.to_agent_id=worker.id
			  AND link.type='manages'
		  )
		ORDER BY worker.id
	`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []registry.OrphanWorker
	for rows.Next() {
		var worker registry.OrphanWorker
		if err := rows.Scan(&worker.ID, &worker.Name); err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	return workers, rows.Err()
}

func scanLink(row rowScanner, link *registry.Link) error {
	return row.Scan(&link.ID, &link.FromAgentID, &link.ToAgentID, &link.Type, &link.Instruction, &link.Kind)
}

// ListManaged returns a team's enabled workers for active team leads. Agents
// outside team context retain the legacy manages view.
func (r *AgentRegistry) ListManaged(ctx context.Context, tenant, fromName string) ([]registry.ManagedAgent, error) {
	fromID, err := r.agentID(ctx, tenant, fromName)
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		WITH source_agent AS (
			SELECT id, team_id
			FROM weave_agents
			WHERE workspace_id=$1 AND id=$2 AND deleted=false
		), team_context AS (
			SELECT team.id, team.status,
			       COALESCE(team.lead_avatar_id=$2, false) AS authoritative_lead
			FROM weave_teams AS team
			JOIN source_agent AS source
			  ON team.lead_avatar_id=source.id
			  OR (team.lead_avatar_id IS NULL AND team.id=source.team_id)
			WHERE team.workspace_id=$1
		), active_team AS (
			SELECT id FROM team_context
			WHERE status='active' AND authoritative_lead=true
		)
		SELECT a.id, a.name, worker.team_id, a.owner_user_id, a.display_name,
		       a.role, a.spec, a.version, worker.duty, worker.default_kind
		FROM active_team AS team
		JOIN weave_team_workers AS worker
		  ON worker.workspace_id=$1 AND worker.team_id=team.id AND worker.enabled=true
		JOIN weave_agents AS a
		  ON a.workspace_id=worker.workspace_id
		 AND a.id=worker.worker_agent_id
		 AND a.role='worker'
		 AND a.deleted=false
		UNION ALL
		SELECT a.id, a.name, a.team_id, a.owner_user_id, a.display_name,
		       a.role, a.spec, a.version, link.instruction, link.kind
		FROM weave_agent_links AS link
		JOIN weave_agents AS a
		  ON a.workspace_id=link.workspace_id AND a.id=link.to_agent_id
		WHERE link.workspace_id=$1
		  AND link.from_agent_id=$2
		  AND link.type='manages'
		  AND a.deleted=false
		  AND NOT EXISTS (SELECT 1 FROM team_context)
		ORDER BY name
	`, tenant, fromID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []registry.ManagedAgent
	for rows.Next() {
		var instruction string
		var kind string
		rec, err := scanAgentRecord(managedAgentRow{
			rowScanner:  rows,
			instruction: &instruction,
			kind:        &kind,
		}, tenant)
		if err != nil {
			return nil, err
		}
		records = append(records, registry.ManagedAgent{AgentRecord: *rec, Instruction: instruction, Kind: kind})
	}
	return records, rows.Err()
}

type managedAgentRow struct {
	rowScanner
	instruction *string
	kind        *string
}

func (r managedAgentRow) Scan(dest ...any) error {
	return r.rowScanner.Scan(append(dest, r.instruction, r.kind)...)
}

// LinkManages remains available only for agents outside team context. Team
// authorization must use the complete, atomic roster contract.
func (r *AgentRegistry) LinkManages(ctx context.Context, tenant, fromName, toName, instruction string, kinds ...string) error {
	kind, err := linkKind(kinds)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}

	var fromID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, fromName).Scan(&fromID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", fromName, tenant)
	}
	if err != nil {
		return err
	}

	var toID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, toName).Scan(&toID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", toName, tenant)
	}
	if err != nil {
		return err
	}

	type leadTeam struct {
		id     string
		status string
	}
	teamRows, err := tx.Query(ctx, `
		SELECT team.id, team.status
		FROM weave_agents AS source
		JOIN weave_teams AS team
		  ON team.workspace_id=source.workspace_id
		 AND (
			team.lead_avatar_id=source.id OR
			(team.lead_avatar_id IS NULL AND team.id=source.team_id)
		 )
		WHERE source.workspace_id=$1 AND source.id=$2
		ORDER BY team.id
		FOR UPDATE OF team
	`, tenant, fromID)
	if err != nil {
		return err
	}
	teams := make([]leadTeam, 0, 1)
	for teamRows.Next() {
		var team leadTeam
		if err := teamRows.Scan(&team.id, &team.status); err != nil {
			teamRows.Close()
			return err
		}
		teams = append(teams, team)
	}
	if err := teamRows.Err(); err != nil {
		teamRows.Close()
		return err
	}
	teamRows.Close()
	roles, lockErr := lockActiveAgents(ctx, tx, tenant, fromID, toID)
	if lockErr != nil {
		return lockErr
	}
	if _, exists := roles[fromID]; !exists {
		return fmt.Errorf("agent %q not found for tenant %q", fromName, tenant)
	}
	toRole, exists := roles[toID]
	if !exists {
		return fmt.Errorf("agent %q not found for tenant %q", toName, tenant)
	}
	switch len(teams) {
	case 0:
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_agent_links (
				workspace_id, from_agent_id, to_agent_id, type, instruction, kind
			)
			VALUES ($1, $2, $3, 'manages', $4, $5)
			ON CONFLICT (workspace_id, from_agent_id, to_agent_id, type)
			DO UPDATE SET instruction=EXCLUDED.instruction, kind=EXCLUDED.kind
		`, tenant, fromID, toID, instruction, kind); err != nil {
			return err
		}
	default:
		_ = toRole
		return fmt.Errorf("%w: lead %q", registry.ErrTeamRosterWriteRequired, fromName)
	}

	return tx.Commit(ctx)
}

func linkKind(kinds []string) (string, error) {
	if len(kinds) == 0 || kinds[0] == "" {
		return "dispatch", nil
	}
	if kinds[0] != "consult" && kinds[0] != "dispatch" && kinds[0] != "handoff" {
		return "", fmt.Errorf("link kind must be consult, dispatch, or handoff")
	}
	return kinds[0], nil
}

// UnlinkManages removes a management relationship between two agents.
func (r *AgentRegistry) UnlinkManages(ctx context.Context, tenant, fromName, toName string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrganizationWorkspace(ctx, tx, tenant); err != nil {
		return err
	}

	var fromID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, fromName).Scan(&fromID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", fromName, tenant)
	}
	if err != nil {
		return err
	}

	var toID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, toName).Scan(&toID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent %q not found for tenant %q", toName, tenant)
	}
	if err != nil {
		return err
	}

	type leadTeam struct {
		id            string
		status        string
		authoritative bool
	}
	teamRows, err := tx.Query(ctx, `
		SELECT team.id, team.status,
		       COALESCE(team.lead_avatar_id=$2, false)
		FROM weave_agents AS source
		JOIN weave_teams AS team
		  ON team.workspace_id=source.workspace_id
		 AND (
			team.lead_avatar_id=source.id OR
			(team.lead_avatar_id IS NULL AND team.id=source.team_id)
		 )
		WHERE source.workspace_id=$1 AND source.id=$2
		ORDER BY team.id
		FOR UPDATE OF team
	`, tenant, fromID)
	if err != nil {
		return err
	}
	teams := make([]leadTeam, 0, 1)
	for teamRows.Next() {
		var team leadTeam
		if err := teamRows.Scan(&team.id, &team.status, &team.authoritative); err != nil {
			teamRows.Close()
			return err
		}
		teams = append(teams, team)
	}
	if err := teamRows.Err(); err != nil {
		teamRows.Close()
		return err
	}
	teamRows.Close()
	roles, lockErr := lockActiveAgents(ctx, tx, tenant, fromID, toID)
	if lockErr != nil {
		return lockErr
	}
	if _, exists := roles[fromID]; !exists {
		return fmt.Errorf("agent %q not found for tenant %q", fromName, tenant)
	}
	if _, exists := roles[toID]; !exists {
		return fmt.Errorf("agent %q not found for tenant %q", toName, tenant)
	}
	switch len(teams) {
	case 0:
		_, err = tx.Exec(ctx, `
			DELETE FROM weave_agent_links
			WHERE workspace_id=$1 AND from_agent_id=$2
			  AND to_agent_id=$3 AND type='manages'
		`, tenant, fromID, toID)
	default:
		return fmt.Errorf("%w: lead %q", registry.ErrTeamRosterWriteRequired, fromName)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockActiveAgents(
	ctx context.Context,
	tx pgx.Tx,
	tenant string,
	agentIDs ...string,
) (map[string]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, role
		FROM weave_agents
		WHERE workspace_id=$1 AND deleted=false AND id=ANY($2::text[])
		ORDER BY id
		FOR UPDATE
	`, tenant, agentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roles := make(map[string]string, len(agentIDs))
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		roles[id] = role
	}
	return roles, rows.Err()
}

func (r *AgentRegistry) agentID(ctx context.Context, tenant, name string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND name=$2 AND deleted=false
	`, tenant, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("agent %q not found for tenant %q", name, tenant)
	}
	return id, err
}
