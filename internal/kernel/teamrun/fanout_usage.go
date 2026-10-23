package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

// nodeUsageAttempt is one physical model attempt a non-durable node reports,
// ready to be confirmed under a logical call of the run that owns its usage.
type nodeUsageAttempt struct {
	AttemptID string         `json:"attempt_id"`
	Usage     contract.Usage `json:"usage"`
	ToolCalls int            `json:"tool_calls"`
	HasTokens bool           `json:"has_tokens"`
	HasCost   bool           `json:"has_cost"`
	Source    string         `json:"source,omitempty"`
}

// nodeUsageAttempts lists the attempts a non-durable node invocation reports.
// key names the invocation: a serial node uses its logical call ID, a parallel
// branch step its group, leg and step, so attempt IDs stay stable on replay.
// coverageLost reports member receipts that do not cover tokens and cost.
func nodeUsageAttempts(key string, report nodeUsageReport) (attempts []nodeUsageAttempt, coverageLost bool) {
	cliAttempts := func() []nodeUsageAttempt {
		out := make([]nodeUsageAttempt, 0, len(report.CLIAttempts))
		for index, physical := range report.CLIAttempts {
			out = append(out, nodeUsageAttempt{
				AttemptID: nodePhysicalUsageAttemptID(key, physical.AttemptID, index),
				Usage:     contract.Usage{InputTokens: physical.InputTokens, OutputTokens: physical.OutputTokens, CostUSD: physical.CostUSD},
				ToolCalls: physical.ToolCalls, HasTokens: physical.HasTokens, HasCost: physical.HasCost, Source: physical.Source,
			})
		}
		return out
	}
	switch {
	case len(report.MemberReceipts) > 0:
		// Each confirmed Loom attempt is kept instead of one synthetic receipt:
		// a later unreported attempt can make coverage incomplete while earlier
		// attempts still carry real token counts.
		for _, receipt := range report.MemberReceipts {
			attempts = append(attempts, nodeUsageAttempt{
				AttemptID: nodePhysicalUsageAttemptID(key, receipt.AttemptID, 0),
				Usage:     receipt.Usage, ToolCalls: receipt.ToolCalls,
				HasTokens: receipt.Metadata.HasTokens, HasCost: receipt.Metadata.HasCost, Source: receipt.Metadata.Source,
			})
		}
		return attempts, !report.Coverage.HasTokens || !report.Coverage.HasCost
	case report.CLIExecution:
		return cliAttempts(), false
	case report.UsageIncompleteReason != "":
		return []nodeUsageAttempt{{AttemptID: nodeUsageAttemptID(key)}}, false
	case len(report.CLIAttempts) == 0:
		return []nodeUsageAttempt{{
			AttemptID: nodeUsageAttemptID(key),
			Usage:     contract.Usage{InputTokens: report.Totals.InputTokens, OutputTokens: report.Totals.OutputTokens, CostUSD: report.Totals.CostUSD},
			ToolCalls: report.Totals.ToolCalls, HasTokens: report.Coverage.HasTokens, HasCost: report.Coverage.HasCost,
		}}, false
	default:
		return cliAttempts(), false
	}
}

// nodeUsageIncompleteReason names why a node's own report cannot be fully
// confirmed: an explicit reason, or a CLI receipt without every dimension.
func nodeUsageIncompleteReason(report nodeUsageReport) string {
	if report.UsageIncompleteReason != "" {
		return report.UsageIncompleteReason
	}
	if report.CLIExecution && (!report.Coverage.HasTokens || !report.Coverage.HasCost) {
		if report.Coverage.HasTokens || report.Coverage.HasCost {
			return UsageIncompleteReasonCLIDimensions
		}
		return UsageIncompleteReasonCLINode
	}
	return ""
}

func confirmNodeUsageAttempts(usage *loomruntime.UsageAccumulator, callID string, attempts []nodeUsageAttempt) error {
	for _, attempt := range attempts {
		if err := usage.StartAttempt(callID, attempt.AttemptID); err != nil {
			return err
		}
		if err := usage.ConfirmAttemptWithMetadata(callID, attempt.AttemptID, attempt.Usage, attempt.ToolCalls,
			loomruntime.UsageAttemptMetadata{HasTokens: attempt.HasTokens, HasCost: attempt.HasCost, Source: attempt.Source}); err != nil {
			return err
		}
	}
	return nil
}

// fanoutLegStepUsageV1 is what one finished step of a parallel branch spent.
// It is stored with the step, so a leg that runs again reuses it, and the
// parent adds it to its own usage when the group joins.
type fanoutLegStepUsageV1 struct {
	SchemaVersion    int                `json:"schema_version"`
	Attempts         []nodeUsageAttempt `json:"attempts"`
	IncompleteReason string             `json:"incomplete_reason,omitempty"`
}

func newFanoutLegStepUsage(key string, report nodeUsageReport) fanoutLegStepUsageV1 {
	attempts, coverageLost := nodeUsageAttempts(key, report)
	if attempts == nil {
		attempts = []nodeUsageAttempt{}
	}
	reason := ""
	if coverageLost {
		reason = UsageIncompleteReasonAttemptLost
	}
	if reason == "" {
		reason = nodeUsageIncompleteReason(report)
	}
	return fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: attempts, IncompleteReason: reason}
}

// mergeFanoutLegUsageTx adds what every leg of a joined group spent to the
// parent checkpoint's usage, in branch and step order. Steps that finished
// count even when their leg did not succeed. A leg that did not succeed, or a
// step without a stored receipt, leaves the parent usage-incomplete instead of
// being counted as free.
func mergeFanoutLegUsageTx(ctx context.Context, tx pgx.Tx, workspaceID string, checkpoint *WorkflowCheckpointV1, join fanout.JoinResultV1) error {
	usage := loomruntime.NewUsageAccumulator()
	if len(checkpoint.Usage) != 0 {
		restored, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			return fmt.Errorf("read parent usage before fanout join: %w", err)
		}
		usage = restored
	}
	rows, err := tx.Query(ctx, `SELECT leg_id,node_id,usage FROM weave_fanout_leg_steps
		WHERE workspace_id=$1 AND group_id=$2 ORDER BY leg_id,step_ordinal`, workspaceID, join.GroupID)
	if err != nil {
		return fmt.Errorf("read finished branch step usage: %w", err)
	}
	type stepUsage struct {
		nodeID string
		usage  []byte
	}
	steps := map[string][]stepUsage{}
	for rows.Next() {
		var legID string
		var step stepUsage
		if err := rows.Scan(&legID, &step.nodeID, &step.usage); err != nil {
			rows.Close()
			return err
		}
		steps[legID] = append(steps[legID], step)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	incomplete := ""
	for _, leg := range join.Legs {
		if leg.DecisionState != fanout.LegDecisionSucceeded {
			// The step that stopped the leg kept no receipt.
			incomplete = UsageIncompleteReasonParallelLegs
		}
		for _, step := range steps[leg.LegID] {
			if step.usage == nil {
				incomplete = UsageIncompleteReasonParallelLegs
				continue
			}
			var recorded fanoutLegStepUsageV1
			if err := decodeExact(step.usage, &recorded); err != nil || recorded.SchemaVersion != 1 || recorded.Attempts == nil {
				return errors.New("finished branch step usage is invalid")
			}
			callID, err := usage.NextCall(checkpoint.RunID, step.nodeID)
			if err != nil {
				return err
			}
			if err := confirmNodeUsageAttempts(&usage, callID, recorded.Attempts); err != nil {
				return fmt.Errorf("add branch step usage: %w", err)
			}
			if recorded.IncompleteReason != "" && incomplete == "" {
				incomplete = recorded.IncompleteReason
			}
		}
	}
	encoded, err := usage.MarshalCheckpoint()
	if err != nil {
		return err
	}
	checkpoint.Usage = encoded
	if incomplete != "" {
		checkpoint.UsageComplete = false
		if checkpoint.UsageIncompleteReason == "" {
			checkpoint.UsageIncompleteReason = incomplete
		}
	}
	return nil
}

func encodeFanoutLegStepUsage(usage fanoutLegStepUsageV1) (string, error) {
	encoded, err := json.Marshal(usage)
	return string(encoded), err
}
