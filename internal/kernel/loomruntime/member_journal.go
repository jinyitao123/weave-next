package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

var ErrMemberOutcomeUnknown = execution.ErrMemberOutcomeUnknown

type memberOperation struct {
	Kind              string          `json:"kind"`
	Input             json.RawMessage `json:"input"`
	InputHash         string          `json:"input_hash"`
	Response          json.RawMessage `json:"response,omitempty"`
	Usage             json.RawMessage `json:"usage,omitempty"`
	AttemptGeneration int64           `json:"attempt_generation"`
	Attempts          int64           `json:"attempts"`
	UsageIncomplete   bool            `json:"usage_incomplete,omitempty"`
}

// InstallFrozenMemberJournal is applied after InstallFrozenUsageTracking, so
// replay restores the exact original usage receipts without allocating new
// logical or physical calls. Outside a MemberRunner it remains transparent.
func InstallFrozenMemberJournal(opts compiler.FrozenBuildOpts) compiler.FrozenBuildOpts {
	journal := memberExecutionJournal{}
	caller := opts.ExecutionLLMWrapper
	opts.ExecutionLLMWrapper = func(inner contract.LLM) contract.LLM {
		if caller != nil {
			inner = caller(inner)
		}
		return stdlib.NewJournaledLLM(inner, journal)
	}
	opts.Tools = stdlib.NewJournaledToolDispatcher(memberOperationTools{inner: opts.Tools}, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true})
	opts.Hooks.BeforeStepHooks = append([]loom.StepHook{memberBeforeStep}, opts.Hooks.BeforeStepHooks...)
	opts.Hooks.AfterStepHooks = append(opts.Hooks.AfterStepHooks, memberAfterStep)
	return opts
}

type memberExecutionJournal struct{}

func (memberExecutionJournal) Active(ctx context.Context) bool {
	_, active := ctx.Value(memberExecutionKey{}).(*memberExecution)
	return active
}

func (memberExecutionJournal) Execute(ctx context.Context, operation stdlib.JournalOperation, perform stdlib.JournalPerform) (json.RawMessage, error) {
	member, active := ctx.Value(memberExecutionKey{}).(*memberExecution)
	if !active {
		response, err := perform()
		if err != nil {
			return nil, err
		}
		return json.Marshal(response)
	}
	input := operation.Input
	switch operation.Kind {
	case stdlib.OperationModel:
	case stdlib.OperationTool:
		call, ok := input.(contract.ToolCall)
		if !ok {
			return nil, errors.New("member tool journal input is invalid")
		}
		args, err := frozen.CanonicalizeJSON([]byte(call.Args))
		if err != nil {
			member.fatal = err
			return nil, err
		}
		call.Args = string(args)
		input = call
	default:
		return nil, errors.New("member journal operation kind is invalid")
	}
	raw, err := member.operation(ctx, string(operation.Kind), input, func() (any, error) {
		return perform()
	})
	if err != nil {
		if operation.Kind == stdlib.OperationTool {
			if errors.Is(err, ErrMemberOutcomeUnknown) {
				err = errors.Join(stdlib.ErrJournalOutcomeUnknown, err)
			}
			member.fatal = err
		}
		return nil, err
	}
	if operation.Kind == stdlib.OperationTool {
		var result contract.ToolResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if !result.IsError {
			files, present, err := fileartifact.DecodeMemberReceipt(result.Content)
			if err != nil {
				member.fatal = err
				return nil, err
			}
			if present {
				member.state[fileartifact.MemberStateKey] = files
			}
		}
	}
	return raw, nil
}

func memberBeforeStep(ctx context.Context, step string, state loom.State) error {
	member, ok := ctx.Value(memberExecutionKey{}).(*memberExecution)
	if !ok {
		return nil
	}
	member.step, member.state, member.cursor = step, state, 0
	segment, _ := state["__member_step_segment"].(string)
	previousStep, _ := state["__member_step_name"].(string)
	complete, _ := state["__member_step_complete"].(bool)
	if segment == "" || previousStep != step || complete {
		segment = fmt.Sprintf("%012d/%s", member.checkpointSeq+1, step)
	}
	member.segment = segment
	state["__member_step_segment"] = segment
	state["__member_step_name"] = step
	state["__member_step_complete"] = false
	state["__yield_phase"] = "mid_step"
	delete(state, "__error")
	delete(state, "__failed_step")
	// Persist the segment before any model, memory or tool effect. Replaying
	// its journal then reconstructs the original ToolLoop counters and queue.
	tx, err := member.runner.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := member.guardTx(ctx, tx); err != nil {
		return err
	}
	if err := member.consumeBudgetGrantTx(ctx, tx); err != nil {
		return err
	}
	seq, err := member.writeBoundaryTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	member.budgetGrant = nil
	member.checkpointSeq, state["__seq"] = seq, seq
	return nil
}

func memberAfterStep(ctx context.Context, _ string, state loom.State) error {
	member, ok := ctx.Value(memberExecutionKey{}).(*memberExecution)
	if !ok {
		return nil
	}
	if member.fatal != nil {
		return member.fatal
	}
	if yielded, _ := state["__yield"].(bool); yielded {
		outcome, present, err := stdlib.ReadToolLoopOutcome(state)
		if err != nil {
			return err
		}
		if present && (outcome.Reason == stdlib.ToolLoopSliceLimit || outcome.Reason == stdlib.ToolLoopTotalLimit || outcome.Reason == stdlib.ToolLoopRepeatLimit || outcome.Reason == stdlib.ToolLoopProviderLength || outcome.Reason == stdlib.ToolLoopProviderStop) {
			state["__member_step_complete"] = false
			state["__yield_phase"] = "mid_step"
			return nil
		}
		return errors.New("frozen member journal does not support interactive tool yields")
	}
	state["__member_step_complete"] = true
	state["__yield_phase"] = "after_step"
	return nil
}

func (member *memberExecution) operation(ctx context.Context, kind string, input any, perform func() (any, error)) (responseData json.RawMessage, operationErr error) {
	effectStarted, receiptCommitted := false, false
	defer func() {
		if kind == "tool" && effectStarted && !receiptCommitted && operationErr != nil {
			operationErr = errors.Join(ErrMemberOutcomeUnknown, operationErr)
		}
	}()
	if member.fatal != nil {
		return nil, member.fatal
	}
	if member.state == nil || member.segment == "" {
		return nil, errors.New("member journal step is unbound")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	hash, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	member.cursor++
	ns := "member-operation:" + member.request.WorkspaceID
	key := fmt.Sprintf("%s/%s/%012d", member.runID, member.segment, member.cursor)
	tx, err := member.runner.store.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	// Capture this transaction now. The response phase opens another one;
	// cancellation can make that later BeginTx return a nil transaction.
	defer tx.Rollback(ctx)
	if err := member.guardTx(ctx, tx); err != nil {
		return nil, err
	}
	prior, present, err := member.runner.store.ReadValueTx(ctx, tx, ns, key)
	if err != nil {
		return nil, err
	}
	op := memberOperation{Kind: kind, Input: raw, InputHash: hash, AttemptGeneration: member.lease.AttemptGeneration, Attempts: 1}
	if present {
		if err := json.Unmarshal(prior, &op); err != nil {
			return nil, err
		}
		if op.Kind != kind || op.InputHash != hash {
			return nil, ErrMemberIdentityConflict
		}
		if len(op.Response) > 0 {
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			if err := member.restoreOperationUsage(ctx, op); err != nil {
				return nil, err
			}
			return op.Response, nil
		}
		if kind != "model" {
			// A business receipt may have committed just before the process died
			// without saving this journal response. Reconcile only that trusted
			// receipt; ordinary tools and unknown effects remain stopped.
			recovered, confirmed, reconcileErr := execution.ReconcileOperation(ctx, key, raw)
			if reconcileErr != nil {
				return nil, errors.Join(ErrMemberOutcomeUnknown, reconcileErr)
			}
			if !confirmed || len(recovered) == 0 || !json.Valid(recovered) {
				return nil, fmt.Errorf("%w: %s", ErrMemberOutcomeUnknown, key)
			}
			op.Response = recovered
			op.UsageIncomplete = true
			encoded, err := json.Marshal(op)
			if err != nil {
				return nil, err
			}
			if err := member.runner.store.PutValueTx(ctx, tx, ns, key, encoded); err != nil {
				return nil, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			if err := member.restoreOperationUsage(ctx, op); err != nil {
				return nil, err
			}
			return op.Response, nil
		}
		if op.AttemptGeneration >= member.lease.AttemptGeneration {
			return nil, ErrMemberBusy
		}
		// A lost model response is safe to request again, but its unreported
		// spend cannot be silently called zero. Preserve that gap on the run.
		op.Attempts++
		if op.Attempts > 8 {
			return nil, errors.New("member model recovery attempt budget exhausted")
		}
		op.AttemptGeneration, op.UsageIncomplete = member.lease.AttemptGeneration, true
	}
	if present {
		if err := member.restoreOperationUsage(ctx, op); err != nil {
			return nil, err
		}
	}
	currentUsage, err := LoadUsageAccumulator(member.state)
	if err != nil {
		return nil, err
	}
	totals, limits := currentUsage.Totals(), member.request.Bundle.Agent.Limits
	if (limits.MaxCostUSD > 0 && totals.CostUSD >= limits.MaxCostUSD) ||
		(limits.MaxTokens > 0 && int64(totals.InputTokens+totals.OutputTokens) >= limits.MaxTokens) {
		return nil, errors.New("member cumulative execution budget exhausted")
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	if err := member.runner.store.PutValueTx(ctx, tx, ns, key, encoded); err != nil {
		return nil, err
	}
	seq, err := member.writeBoundaryTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	member.checkpointSeq, member.state["__seq"] = seq, seq
	// No effect precedes the durable intent. The transport itself may outlive
	// cancellation; a stale owner cannot commit its eventual response.
	if err := member.check(ctx); err != nil {
		return nil, err
	}
	effectStarted = true
	response, performErr := perform()
	if performErr != nil && kind == "tool" {
		performErr = errors.Join(ErrMemberOutcomeUnknown, performErr)
	}
	if performErr == nil {
		op.Response, err = json.Marshal(response)
		if err != nil {
			return nil, err
		}
		if string(op.Response) == "null" {
			return nil, errors.New("member operation returned no response")
		}
	}
	usage, err := LoadUsageAccumulator(member.state)
	if err != nil {
		return nil, err
	}
	op.Usage, err = usage.MarshalCheckpoint()
	if err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(op)
	if err != nil {
		return nil, err
	}
	tx, err = member.runner.store.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := member.guardTx(ctx, tx); err != nil {
		return nil, err
	}
	if err := member.runner.store.PutValueTx(ctx, tx, ns, key, encoded); err != nil {
		return nil, err
	}
	seq, err = member.writeBoundaryTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	receiptCommitted = len(op.Response) > 0
	member.checkpointSeq, member.state["__seq"] = seq, seq
	if op.UsageIncomplete {
		member.state["__member_usage_incomplete"] = true
	}
	return op.Response, performErr
}

func (member *memberExecution) restoreOperationUsage(ctx context.Context, op memberOperation) error {
	if len(op.Usage) > 0 {
		usage, err := UnmarshalUsageAccumulator(op.Usage)
		if err != nil {
			return err
		}
		if usage.OwnedRunID() != "" && usage.OwnedRunID() != member.runID {
			return ErrMemberIdentityConflict
		}
		if err := StoreUsageAccumulator(member.state, usage); err != nil {
			return err
		}
		if scope, ok := usageScopeFromContext(ctx); ok {
			scope.mu.Lock()
			scope.accumulator = usage
			scope.mu.Unlock()
		}
	}
	if op.UsageIncomplete {
		member.state["__member_usage_incomplete"] = true
	}
	return nil
}

// memberOperationTools is invoked only after the journal has durably reserved
// the operation. Its cursor is serialized by the journaled tool loop.
type memberOperationTools struct{ inner contract.ToolDispatcher }

func (tools memberOperationTools) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return tools.inner.ListTools(ctx)
}

func (tools memberOperationTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if member, active := ctx.Value(memberExecutionKey{}).(*memberExecution); active {
		if member.segment == "" || member.cursor < 1 {
			return nil, errors.New("member tool operation has no durable journal slot")
		}
		slot := fmt.Sprintf("%s/%s/%012d", member.runID, member.segment, member.cursor)
		ctx = execution.WithOperationID(ctx, slot)
	}
	return tools.inner.Dispatch(ctx, call)
}
