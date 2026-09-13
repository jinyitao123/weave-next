package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RecordRoundResult appends the next round ledger row in a new transaction.
// The run must be in round_running; the round number is derived from the
// ledger. Callers that persist an evaluation report in the same transaction
// should use RecordRoundResultTx instead.
func (s *Store) RecordRoundResult(
	ctx context.Context,
	workspaceID, buildRunID string,
	result RoundResult,
) (Round, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Round{}, fmt.Errorf("begin record round result: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	round, err := s.RecordRoundResultTx(ctx, tx, workspaceID, buildRunID, result)
	if err != nil {
		return Round{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Round{}, fmt.Errorf("commit record round result: %w", err)
	}
	return round, nil
}

// RecordRoundResultTx appends the next round ledger row inside the
// caller-owned transaction. The run must be in round_running; the round
// number is derived from the ledger. Controllers write the evaluation report
// and the round row in the same transaction so the ledger's report_ref always
// resolves to a report row.
func (s *Store) RecordRoundResultTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	result RoundResult,
) (Round, error) {
	if tx == nil {
		return Round{}, errors.New("record round result: transaction is required")
	}
	if strings.TrimSpace(result.CandidateRef) == "" || strings.TrimSpace(result.ReportRef) == "" {
		return Round{}, errors.New("record round result: candidate_ref and report_ref are required")
	}
	switch result.Conclusion {
	case ConclusionPass, ConclusionRevise, ConclusionBlocked:
	default:
		return Round{}, fmt.Errorf("record round result: %w", ErrInvalidRoundConclusion)
	}

	now := s.clock.Now()

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Round{}, fmt.Errorf("record round result: %w", ErrBuildRunNotFound)
		}
		return Round{}, fmt.Errorf("record round result: %w", err)
	}
	if status != StatusRoundRunning {
		return Round{}, fmt.Errorf("record round result: %w", ErrBuildRunNotRoundRunning)
	}

	var roundNo int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(round_no),0)+1
		FROM weave_team_build_run_rounds
		WHERE workspace_id=$1 AND build_run_id=$2
	`, workspaceID, buildRunID).Scan(&roundNo); err != nil {
		return Round{}, fmt.Errorf("record round result: derive round number: %w", err)
	}

	var round Round
	if err := tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_run_rounds (
			workspace_id, build_run_id, round_no, candidate_ref, report_ref,
			conclusion, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING workspace_id, build_run_id, round_no, candidate_ref,
			report_ref, conclusion, created_at
	`, workspaceID, buildRunID, roundNo, result.CandidateRef,
		result.ReportRef, result.Conclusion, now).Scan(
		&round.WorkspaceID, &round.BuildRunID, &round.RoundNo,
		&round.CandidateRef, &round.ReportRef, &round.Conclusion, &round.CreatedAt,
	); err != nil {
		return Round{}, fmt.Errorf("record round result: %w", err)
	}
	return round, nil
}

// SaveRoundReport persists one immutable evaluation report in a new
// transaction. It is the standalone path used for infrastructure-error stops,
// which persist the report without appending a round ledger row; normal
// rounds persist the report and the round row atomically via
// SaveRoundReportTx + RecordRoundResultTx.
func (s *Store) SaveRoundReport(
	ctx context.Context,
	workspaceID, buildRunID string,
	report EvaluationReport,
) (RoundReport, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RoundReport{}, fmt.Errorf("begin save round report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	saved, err := s.SaveRoundReportTx(ctx, tx, workspaceID, buildRunID, report)
	if err != nil {
		return RoundReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RoundReport{}, fmt.Errorf("commit save round report: %w", err)
	}
	return saved, nil
}

// SaveRoundReportTx persists one immutable evaluation report inside the
// caller-owned transaction. The round controller writes the report row and
// RecordRoundResultTx in the same transaction, so the round ledger's
// report_ref is always resolvable.
func (s *Store) SaveRoundReportTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	report EvaluationReport,
) (RoundReport, error) {
	if tx == nil {
		return RoundReport{}, errors.New("save round report: transaction is required")
	}
	if workspaceID == "" || buildRunID == "" {
		return RoundReport{}, errors.New("save round report: workspace_id and build_run_id are required")
	}
	reportHash, err := report.Hash()
	if err != nil {
		return RoundReport{}, fmt.Errorf("save round report: %w", err)
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return RoundReport{}, fmt.Errorf("save round report: encode report: %w", err)
	}

	var saved RoundReport
	var storedJSON []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_run_reports (
			workspace_id, build_run_id, round_no, report_hash, report_json, created_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		RETURNING workspace_id, build_run_id, round_no, report_hash, report_json, created_at
	`, workspaceID, buildRunID, report.RoundNo, reportHash, string(reportJSON),
		s.clock.Now().UTC()).Scan(
		&saved.WorkspaceID, &saved.BuildRunID, &saved.RoundNo,
		&saved.ReportHash, &storedJSON, &saved.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RoundReport{}, fmt.Errorf(
				"save round report: %w: round %d of build run %q",
				ErrRoundReportExists, report.RoundNo, buildRunID,
			)
		}
		return RoundReport{}, fmt.Errorf("save round report: %w", err)
	}
	saved.Report = report
	return saved, nil
}

// GetRoundReport returns one persisted evaluation report and verifies that
// the stored JSON re-hashes to the stored report_hash, so a tampered or
// corrupt row never reads back as a valid report.
func (s *Store) GetRoundReport(
	ctx context.Context,
	workspaceID, buildRunID string,
	roundNo int,
) (RoundReport, error) {
	var saved RoundReport
	var reportJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, build_run_id, round_no, report_hash, report_json, created_at
		FROM weave_team_build_run_reports
		WHERE workspace_id=$1 AND build_run_id=$2 AND round_no=$3
	`, workspaceID, buildRunID, roundNo).Scan(
		&saved.WorkspaceID, &saved.BuildRunID, &saved.RoundNo,
		&saved.ReportHash, &reportJSON, &saved.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoundReport{}, fmt.Errorf(
			"get round report: %w: round %d of build run %q",
			ErrRoundReportNotFound, roundNo, buildRunID,
		)
	}
	if err != nil {
		return RoundReport{}, fmt.Errorf("get round report: %w", err)
	}
	var report EvaluationReport
	if err := json.Unmarshal(reportJSON, &report); err != nil {
		return RoundReport{}, fmt.Errorf("get round report: %w: %w", ErrRoundReportInvalid, err)
	}
	recomputed, err := report.Hash()
	if err != nil {
		return RoundReport{}, fmt.Errorf("get round report: %w: %w", ErrRoundReportInvalid, err)
	}
	if recomputed != saved.ReportHash {
		return RoundReport{}, fmt.Errorf(
			"get round report: %w: stored hash does not match report content",
			ErrRoundReportInvalid,
		)
	}
	saved.Report = report
	return saved, nil
}

// ListRounds returns the recorded round ledger for one build run in round
// order. The round controller uses it to derive the next round number and to
// compare consecutive reports for the early-stop rule.
func (s *Store) ListRounds(
	ctx context.Context,
	workspaceID, buildRunID string,
) ([]Round, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, build_run_id, round_no, candidate_ref, report_ref,
			conclusion, created_at
		FROM weave_team_build_run_rounds
		WHERE workspace_id=$1 AND build_run_id=$2
		ORDER BY round_no
	`, workspaceID, buildRunID)
	if err != nil {
		return nil, fmt.Errorf("list round results: %w", err)
	}
	defer rows.Close()
	var rounds []Round
	for rows.Next() {
		var round Round
		if err := rows.Scan(
			&round.WorkspaceID, &round.BuildRunID, &round.RoundNo,
			&round.CandidateRef, &round.ReportRef, &round.Conclusion, &round.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list round results: %w", err)
		}
		rounds = append(rounds, round)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list round results: %w", err)
	}
	return rounds, nil
}
