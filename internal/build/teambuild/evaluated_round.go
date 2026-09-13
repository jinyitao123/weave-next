package teambuild

import (
	"context"
	"fmt"
)

// RecordEvaluatedRound commits a report and its referencing round as one command.
// Callers cannot commit either half or retain a database transaction.
func (s *Store) RecordEvaluatedRound(ctx context.Context, workspaceID, buildRunID string, report EvaluationReport, result RoundResult) (Round, error) {
	reportHash, err := report.Hash()
	if err != nil {
		return Round{}, fmt.Errorf("hash evaluated round report: %w", err)
	}
	if result.ReportRef != reportHash {
		return Round{}, fmt.Errorf("evaluated round report reference differs from report hash")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Round{}, fmt.Errorf("begin round persistence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := s.SaveRoundReportTx(ctx, tx, workspaceID, buildRunID, report); err != nil {
		return Round{}, fmt.Errorf("save round report: %w", err)
	}
	round, err := s.RecordRoundResultTx(ctx, tx, workspaceID, buildRunID, result)
	if err != nil {
		return Round{}, fmt.Errorf("record round result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Round{}, fmt.Errorf("commit round persistence: %w", err)
	}
	return round, nil
}
