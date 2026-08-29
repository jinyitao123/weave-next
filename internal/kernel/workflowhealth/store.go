package workflowhealth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const RulesetVersion = 1

type Policy struct {
	WindowSize         int           `json:"window_size"`
	MinSamples         int           `json:"min_samples"`
	WarningFailureRate float64       `json:"warning_failure_rate"`
	WarningSlowRate    float64       `json:"warning_slow_rate"`
	SlowRunThreshold   time.Duration `json:"slow_run_threshold_ns"`
}

func DefaultPolicy() Policy {
	return Policy{WindowSize: 20, MinSamples: 3, WarningFailureRate: 0.25, WarningSlowRate: 0.50, SlowRunThreshold: 10 * time.Minute}
}

type Store struct {
	pool   *pgxpool.Pool
	policy Policy
	now    func() time.Time
}

func New(pool *pgxpool.Pool, policy Policy) (*Store, error) {
	if pool == nil {
		return nil, errors.New("workflow health store requires a pool")
	}
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	return &Store{pool: pool, policy: policy, now: time.Now}, nil
}

type Health struct {
	Conclusion          string    `json:"conclusion"`
	WorkflowID          string    `json:"workflow_id"`
	WorkflowVersion     int       `json:"workflow_version"`
	ArtifactContentHash string    `json:"artifact_content_hash"`
	ObservationID       string    `json:"observation_id,omitempty"`
	ObservedAt          time.Time `json:"observed_at,omitempty"`
	SampleCount         int       `json:"sample_count"`
	SucceededCount      int       `json:"succeeded_count"`
	FailedCount         int       `json:"failed_count"`
	SlowCount           int       `json:"slow_count"`
	HumanCompletedCount int       `json:"human_completed_count"`
	HumanTimeoutCount   int       `json:"human_timeout_count"`
	ReasonCodes         []string  `json:"reason_codes"`
}

type versionKey struct {
	WorkspaceID string
	WorkflowID  string
	Version     int
	ContentHash string
}

type runFact struct {
	RunID          string    `json:"run_id"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	TerminalAt     time.Time `json:"terminal_at"`
	FactsComplete  bool      `json:"facts_complete"`
	UsageComplete  bool      `json:"usage_complete"`
	HumanCompleted bool      `json:"human_completed"`
	HumanTimedOut  bool      `json:"human_timed_out"`
}

type observation struct {
	Health
	workspaceID     string
	FactHash        string
	ConfigHash      string
	ConfigJSON      json.RawMessage
	CutoffAt        time.Time
	WindowStartedAt *time.Time
	WindowEndedAt   *time.Time
	SelectedRunIDs  []string
}

func validatePolicy(policy Policy) error {
	if policy.WindowSize <= 0 || policy.MinSamples <= 0 || policy.MinSamples > policy.WindowSize || policy.SlowRunThreshold <= 0 {
		return errors.New("workflow health policy has invalid window or duration")
	}
	for _, rate := range []float64{policy.WarningFailureRate, policy.WarningSlowRate} {
		if rate <= 0 || rate > 1 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return errors.New("workflow health policy rates must be in (0,1]")
		}
	}
	return nil
}

func (s *Store) ObserveBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 32
	}
	configHash := hashJSON(struct {
		Ruleset int    `json:"ruleset"`
		Policy  Policy `json:"policy"`
	}{RulesetVersion, s.policy})
	rows, err := s.pool.Query(ctx, `
		SELECT run.workspace_id, run.workflow_id, run.workflow_version, artifact.content_hash
		FROM weave_team_runs AS run
		JOIN weave_team_run_snapshots AS snapshot
		  ON snapshot.workspace_id=run.workspace_id AND snapshot.run_id=run.run_snapshot_id
		JOIN weave_published_artifact_contents AS artifact
		  ON artifact.workspace_id=run.workspace_id AND artifact.workflow_id=run.workflow_id
		 AND artifact.workflow_version=run.workflow_version
		LEFT JOIN weave_run_terminal_markers AS marker
		  ON marker.workspace_id=run.workspace_id AND marker.run_id=run.run_id
		LEFT JOIN loom_store AS audit
		  ON audit.namespace='audit:'||run.workspace_id AND audit.key=run.run_id
		LEFT JOIN weave_workflow_current_health AS current
		  ON current.workspace_id=run.workspace_id AND current.workflow_id=run.workflow_id
		 AND current.workflow_version=run.workflow_version AND current.artifact_content_hash=artifact.content_hash
		LEFT JOIN weave_workflow_health_observations AS observed
		  ON observed.workspace_id=current.workspace_id AND observed.observation_id=current.observation_id
		WHERE run.status IN ('succeeded','failed','cancelled','abandoned')
		  AND snapshot.mode='fixed_workflow' AND snapshot.build_run_id IS NULL
		GROUP BY run.workspace_id,run.workflow_id,run.workflow_version,artifact.content_hash,
		         current.observed_at,observed.config_hash
		HAVING current.observed_at IS NULL OR observed.config_hash<>$2 OR
		  MAX(GREATEST(run.terminal_at,COALESCE(marker.updated_at,run.terminal_at),COALESCE(audit.updated_at,run.terminal_at)))>current.observed_at
		ORDER BY MAX(run.terminal_at),run.workspace_id,run.workflow_id,run.workflow_version,artifact.content_hash
		LIMIT $1
	`, limit, configHash)
	if err != nil {
		return 0, fmt.Errorf("list workflow health versions: %w", err)
	}
	defer rows.Close()
	keys := make([]versionKey, 0)
	for rows.Next() {
		var key versionKey
		if err := rows.Scan(&key.WorkspaceID, &key.WorkflowID, &key.Version, &key.ContentHash); err != nil {
			return 0, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	written := 0
	for _, key := range keys {
		changed, err := s.observeVersion(ctx, key)
		if err != nil {
			return written, err
		}
		if changed {
			written++
		}
	}
	return written, nil
}

func (s *Store) observeVersion(ctx context.Context, key versionKey) (bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT run.run_id, run.status, run.created_at, run.terminal_at,
		       COALESCE(marker.audit_state='materialized' AND marker.lineage_state='complete'
		         AND audit.value IS NOT NULL,FALSE) AS facts_complete,
		       CASE WHEN audit.value IS NULL OR marker.audit_state<>'materialized' THEN FALSE
		         ELSE COALESCE((convert_from(audit.value,'UTF8')::jsonb->>'usage_complete')::boolean,TRUE)
		       END AS usage_complete,
		       EXISTS (SELECT 1 FROM weave_team_run_transitions AS transition
		         WHERE transition.workspace_id=run.workspace_id AND transition.run_id=run.run_id
		           AND transition.source='human_resume' AND transition.actor<>'teamrun-human-timeout-sweeper') AS human_completed,
		       EXISTS (SELECT 1 FROM weave_team_run_transitions AS transition
		         WHERE transition.workspace_id=run.workspace_id AND transition.run_id=run.run_id
		           AND transition.source='human_resume' AND transition.actor='teamrun-human-timeout-sweeper') AS human_timed_out
		FROM weave_team_runs AS run
		JOIN weave_team_run_snapshots AS snapshot
		  ON snapshot.workspace_id=run.workspace_id AND snapshot.run_id=run.run_snapshot_id
		LEFT JOIN weave_run_terminal_markers AS marker
		  ON marker.workspace_id=run.workspace_id AND marker.run_id=run.run_id
		LEFT JOIN loom_store AS audit
		  ON audit.namespace='audit:'||run.workspace_id AND audit.key=run.run_id
		WHERE run.workspace_id=$1 AND run.workflow_id=$2 AND run.workflow_version=$3
		  AND run.status IN ('succeeded','failed','cancelled','abandoned')
		  AND snapshot.mode='fixed_workflow' AND snapshot.build_run_id IS NULL
		ORDER BY run.terminal_at DESC, run.run_id DESC
		LIMIT $4
	`, key.WorkspaceID, key.WorkflowID, key.Version, s.policy.WindowSize)
	if err != nil {
		return false, fmt.Errorf("read workflow health facts: %w", err)
	}
	defer rows.Close()
	facts := make([]runFact, 0, s.policy.WindowSize)
	for rows.Next() {
		var fact runFact
		if err := rows.Scan(&fact.RunID, &fact.Status, &fact.CreatedAt, &fact.TerminalAt, &fact.FactsComplete, &fact.UsageComplete, &fact.HumanCompleted, &fact.HumanTimedOut); err != nil {
			return false, err
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	observed := evaluate(key, facts, s.policy, s.now().UTC())
	return s.persist(ctx, observed)
}

func evaluate(key versionKey, facts []runFact, policy Policy, cutoff time.Time) observation {
	config := struct {
		Ruleset int    `json:"ruleset"`
		Policy  Policy `json:"policy"`
	}{RulesetVersion, policy}
	configJSON, _ := json.Marshal(config)
	configHash := hashJSON(config)
	result := observation{workspaceID: key.WorkspaceID, Health: Health{
		Conclusion: "unknown", WorkflowID: key.WorkflowID, WorkflowVersion: key.Version,
		ArtifactContentHash: key.ContentHash, ReasonCodes: []string{},
	}, ConfigHash: configHash, ConfigJSON: configJSON, CutoffAt: cutoff, SelectedRunIDs: []string{}}
	if len(facts) > 0 {
		oldest, newest := facts[0].TerminalAt, facts[0].TerminalAt
		for _, fact := range facts {
			result.SelectedRunIDs = append(result.SelectedRunIDs, fact.RunID)
			if fact.TerminalAt.Before(oldest) {
				oldest = fact.TerminalAt
			}
			if fact.TerminalAt.After(newest) {
				newest = fact.TerminalAt
			}
			if fact.Status == "succeeded" {
				result.SucceededCount++
			} else {
				result.FailedCount++
			}
			if fact.TerminalAt.Sub(fact.CreatedAt) >= policy.SlowRunThreshold {
				result.SlowCount++
			}
			if fact.HumanCompleted {
				result.HumanCompletedCount++
			}
			if fact.HumanTimedOut {
				result.HumanTimeoutCount++
			}
			if !fact.FactsComplete || !fact.UsageComplete {
				result.ReasonCodes = append(result.ReasonCodes, "facts_incomplete")
			}
		}
		result.SampleCount = len(facts)
		result.WindowStartedAt, result.WindowEndedAt = &oldest, &newest
	}
	result.ReasonCodes = uniqueSorted(result.ReasonCodes)
	if result.SampleCount < policy.MinSamples {
		result.ReasonCodes = uniqueSorted(append(result.ReasonCodes, "insufficient_samples"))
	} else if len(result.ReasonCodes) == 0 {
		failureRate := float64(result.FailedCount) / float64(result.SampleCount)
		slowRate := float64(result.SlowCount) / float64(result.SampleCount)
		humanTimeoutRate := float64(result.HumanTimeoutCount) / float64(result.SampleCount)
		switch {
		case failureRate >= policy.WarningFailureRate:
			result.Conclusion = "warning"
			result.ReasonCodes = []string{"failure_rate"}
		case slowRate >= policy.WarningSlowRate:
			result.Conclusion = "warning"
			result.ReasonCodes = []string{"slow_run_rate"}
		case humanTimeoutRate >= policy.WarningFailureRate:
			result.Conclusion = "warning"
			result.ReasonCodes = []string{"human_timeout_rate"}
		default:
			result.Conclusion = "healthy"
		}
	}
	result.FactHash = hashJSON(struct {
		Key   versionKey `json:"key"`
		Facts []runFact  `json:"facts"`
	}{key, facts})
	return result
}

func (s *Store) persist(ctx context.Context, value observation) (bool, error) {
	runIDs, _ := json.Marshal(value.SelectedRunIDs)
	reasons, _ := json.Marshal(value.ReasonCodes)
	observationID := "health-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(value.ConfigHash+":"+value.FactHash)).String()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `
		INSERT INTO weave_workflow_health_observations (
		  workspace_id,workflow_id,workflow_version,artifact_content_hash,observation_id,
		  ruleset_version,config_json,config_hash,fact_hash,cutoff_at,window_started_at,window_ended_at,
		  sample_count,succeeded_count,failed_count,slow_count,human_completed_count,human_timeout_count,
		  selected_run_ids,reason_codes,conclusion
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (workspace_id,workflow_id,workflow_version,artifact_content_hash,ruleset_version,config_hash,fact_hash)
		DO NOTHING
	`, value.workspaceID, value.WorkflowID, value.WorkflowVersion, value.ArtifactContentHash, observationID,
		RulesetVersion, value.ConfigJSON, value.ConfigHash, value.FactHash, value.CutoffAt, value.WindowStartedAt, value.WindowEndedAt,
		value.SampleCount, value.SucceededCount, value.FailedCount, value.SlowCount, value.HumanCompletedCount, value.HumanTimeoutCount,
		json.RawMessage(runIDs), json.RawMessage(reasons), value.Conclusion)
	if err != nil {
		return false, fmt.Errorf("insert workflow health observation: %w", err)
	}
	if command.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO weave_workflow_current_health (
		  workspace_id,workflow_id,workflow_version,artifact_content_hash,observation_id,conclusion,observed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id,workflow_id,workflow_version,artifact_content_hash)
		DO UPDATE SET observation_id=EXCLUDED.observation_id,conclusion=EXCLUDED.conclusion,observed_at=EXCLUDED.observed_at
		WHERE weave_workflow_current_health.observed_at <= EXCLUDED.observed_at
	`, value.workspaceID, value.WorkflowID, value.WorkflowVersion, value.ArtifactContentHash, observationID, value.Conclusion, value.CutoffAt)
	if err != nil {
		return false, fmt.Errorf("project workflow health observation: %w", err)
	}
	return true, tx.Commit(ctx)
}

func (s *Store) Current(ctx context.Context, workspaceID, workflowID string, version int, contentHash string) (Health, error) {
	value := Health{Conclusion: "unknown", WorkflowID: workflowID, WorkflowVersion: version, ArtifactContentHash: contentHash, ReasonCodes: []string{"not_observed"}}
	var reasons []byte
	err := s.pool.QueryRow(ctx, `
		SELECT current.observation_id,current.conclusion,current.observed_at,
		       observed.sample_count,observed.succeeded_count,observed.failed_count,observed.slow_count,
		       observed.human_completed_count,observed.human_timeout_count,observed.reason_codes
		FROM weave_workflow_current_health AS current
		JOIN weave_workflow_health_observations AS observed
		  ON observed.workspace_id=current.workspace_id AND observed.observation_id=current.observation_id
		WHERE current.workspace_id=$1 AND current.workflow_id=$2 AND current.workflow_version=$3
		  AND current.artifact_content_hash=$4
	`, workspaceID, workflowID, version, contentHash).Scan(&value.ObservationID, &value.Conclusion, &value.ObservedAt,
		&value.SampleCount, &value.SucceededCount, &value.FailedCount, &value.SlowCount,
		&value.HumanCompletedCount, &value.HumanTimeoutCount, &reasons)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return Health{}, err
	}
	if err := json.Unmarshal(reasons, &value.ReasonCodes); err != nil {
		return Health{}, err
	}
	return value, nil
}

func hashJSON(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	values = values[:0]
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
