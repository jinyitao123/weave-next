package deliverable

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/fileartifact"
)

func recheckRequest(report VerificationReport) RecheckRequest {
	return RecheckRequest{WorkspaceID: report.Candidate.WorkspaceID, RunID: report.Candidate.RunID, RevisionID: report.RevisionID, ContractDigest: report.ContractDigest}
}

func (h *verificationHarness) setTerminal(t *testing.T, runID, status string) {
	t.Helper()
	var errorCode any
	if status == "cancelled" {
		errorCode = "team_run_cancelled"
	}
	_, err := h.pool.Exec(context.Background(), `UPDATE weave_team_runs SET status=$2,current_executor_id=NULL,error_code=$3,terminal_at=now(),updated_at=now(),team_run_generation=team_run_generation+1 WHERE run_id=$1`, runID, status, errorCode)
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerificationRecheckSameVersionAfterTerminal(t *testing.T) {
	var extra atomic.Bool
	var calls atomic.Int32
	registry := NewVerifierRegistry()
	_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(context.Context, VerificationInput) (CheckResult, error) {
		calls.Add(1)
		status := VerificationPassed
		if extra.Load() {
			status = VerificationFailed
		}
		evidence, _ := json.Marshal(map[string]any{"complete": true, "unexpected_write": extra.Load()})
		return CheckResult{Status: status, Reason: "observed_current_effects", Evidence: evidence}, nil
	})
	h := newVerificationHarness(t, registry)
	for _, engine := range []string{"cli", "loom"} {
		t.Run(engine, func(t *testing.T) {
			extra.Store(false)
			h.seedSnapshot(t, engine)
			h.freeze(t, engine, fixtureContract())
			fence := h.seedRun(t, engine, "run-"+engine)
			files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
			source := h.source(t, fence, engine, "source-"+engine, files, nil)
			original, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence)
			if err != nil {
				t.Fatal(err)
			}
			h.setTerminal(t, fence.RunID, "succeeded")
			beforeCalls := calls.Load()
			for _, field := range []string{"revision", "contract"} {
				req := recheckRequest(original)
				if field == "revision" {
					req.RevisionID = "other"
				} else {
					req.ContractDigest = strings.Repeat("0", 64)
				}
				if _, err := h.store.RecheckCurrentDelivery(context.Background(), req); !errors.Is(err, ErrVerificationConflict) {
					t.Fatalf("wrong %s accepted: %v", field, err)
				}
			}
			if calls.Load() != beforeCalls {
				t.Fatal("invalid recheck invoked verifier")
			}
			extra.Store(true)
			checked, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(original))
			if err != nil {
				t.Fatal(err)
			}
			if !checked.Current || checked.Report.Status != VerificationFailed || checked.Report.RevisionID != original.RevisionID || checked.Report.ID == original.ID || checked.Report.Recheck == nil {
				t.Fatalf("invalid recheck result: %+v", checked)
			}
			if checked.Report.Fence != original.Fence {
				t.Fatal("recheck invented a new producer fence")
			}
			if count, reports := h.counts(t, fence.RunID); count != 2 || reports != 2 {
				t.Fatalf("recheck changed output count: %d/%d", count, reports)
			}
			state, err := h.store.GetDeliveryState(context.Background(), fence.WorkspaceID, fence.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if state.SelectionSequence != 2 || state.RevisionID != original.RevisionID || state.VerificationID != checked.Report.ID {
				t.Fatal("same-version report did not advance")
			}
			old, err := h.store.GetVerificationReport(context.Background(), fence.WorkspaceID, fence.RunID, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if old.Status != VerificationPassed {
				t.Fatal("old verification was rewritten")
			}
			var executionStatus string
			if err := h.pool.QueryRow(context.Background(), `SELECT status FROM weave_team_runs WHERE run_id=$1`, fence.RunID).Scan(&executionStatus); err != nil {
				t.Fatal(err)
			}
			if executionStatus != "succeeded" {
				t.Fatal("recheck changed execution state")
			}
		})
	}
}

func TestVerificationRecheckKeepsCancelledExecutionAndStaleHistory(t *testing.T) {
	for _, mode := range []string{"cancel during recheck", "new delivery during recheck"} {
		t.Run(mode, func(t *testing.T) {
			registry := NewVerifierRegistry()
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(ctx context.Context, _ VerificationInput) (CheckResult, error) {
				if calls.Add(1) == 2 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return CheckResult{}, ctx.Err()
					}
				}
				return CheckResult{Status: VerificationPassed, Reason: "observed", Evidence: json.RawMessage(`{"scope":"isolated","complete":true}`)}, nil
			})
			h := newVerificationHarness(t, registry)
			h.seedSnapshot(t, "snapshot")
			h.freeze(t, "snapshot", fixtureContract())
			fence := h.seedRun(t, "snapshot", "run")
			files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "verified content"}}
			source := h.source(t, fence, "loom", "source", files, nil)
			original, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result RecheckResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(original))
				done <- outcome{result, err}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("recheck did not start")
			}
			newReport := original
			if mode == "cancel during recheck" {
				h.setTerminal(t, "run", "cancelled")
			} else {
				revised := append([]fileartifact.File(nil), files...)
				revised[0].Content += " new revision"
				newSource := h.source(t, fence, "loom", "new-source", revised, nil)
				newReport, err = h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, newSource, revised), fence)
				if err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			fileCountBefore, _ := h.counts(t, "run")
			close(release)
			observed := <-done
			if observed.err != nil {
				t.Fatal(observed.err)
			}
			fileCountAfter, _ := h.counts(t, "run")
			if fileCountAfter != fileCountBefore {
				t.Fatal("recheck wrote artifacts")
			}
			state, err := h.store.GetDeliveryState(context.Background(), fence.WorkspaceID, fence.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel during recheck" {
				if !observed.result.Current || state.RevisionID != original.RevisionID {
					t.Fatal("cancelled run lost existing delivery")
				}
				var status string
				if err := h.pool.QueryRow(context.Background(), `SELECT status FROM weave_team_runs WHERE run_id='run'`).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "cancelled" {
					t.Fatal("recheck revived cancellation")
				}
				// An already-cancelled run may still inspect its existing final version.
				if again, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(original)); err != nil || !again.Current {
					t.Fatalf("cancelled current delivery cannot be read-only rechecked: %v", err)
				}
			} else {
				if observed.result.Current || state.RevisionID != newReport.RevisionID || state.VerificationID != newReport.ID {
					t.Fatal("old recheck replaced the new delivery")
				}
				old, err := h.store.GetVerificationReport(context.Background(), fence.WorkspaceID, fence.RunID, observed.result.Report.ID)
				if err != nil {
					t.Fatal(err)
				}
				if old.RevisionID != original.RevisionID || old.Recheck == nil {
					t.Fatal("stale recheck history was lost")
				}
			}
		})
	}
}

func TestVerificationRecheckPreservesExactJSONAndRejectsOldReports(t *testing.T) {
	h := newVerificationHarness(t, fixtureEffectsRegistry(t, VerificationPassed))
	h.seedSnapshot(t, "snapshot")
	contract := fixtureContract()
	contract.RequiredArtifacts = nil
	contract.Output.Type = "json"
	h.freeze(t, "snapshot", contract)
	fence := h.seedRun(t, "snapshot", "run")
	output := map[string]any{"large": json.Number("9007199254740993"), "exponent": json.Number("1e3")}
	exact, _ := json.Marshal(output)
	outputs := []WorkflowOutput{{WorkspaceID: fence.WorkspaceID, RunID: fence.RunID, RunSnapshotID: fence.RunSnapshotID, NodeID: "deliver", NodeType: "deliver", Final: true, Output: output, Selection: &OutputSelection{Kind: "literal", ValueDigest: digest(exact)}}}
	original, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), outputs, fence)
	if err != nil {
		t.Fatal(err)
	}
	if original.OutputJSON != string(exact) {
		t.Fatalf("final JSON changed in JSONB: %q != %q", original.OutputJSON, exact)
	}
	checked, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(original))
	if err != nil {
		t.Fatal(err)
	}
	if checked.Report.OutputJSON != string(exact) || checked.Report.RevisionID != original.RevisionID || checked.Report.Status != VerificationPassed {
		t.Fatal("literal JSON was rewritten during reconstruction")
	}
	// Simulate a pre-output_json report without mutating an existing immutable row.
	legacy := original
	legacy.ID += "_legacy"
	legacy.OutputJSON = ""
	tx, err := h.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := insertVerificationReportTx(context.Background(), tx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE weave_run_delivery_state SET current_verification_id=$1,selection_sequence=selection_sequence+1 WHERE run_snapshot_id='snapshot'`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, before := h.counts(t, "run")
	if _, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(legacy)); !errors.Is(err, ErrVerificationUnavailable) {
		t.Fatalf("legacy missing value was reconstructed from zero value: %v", err)
	}
	_, after := h.counts(t, "run")
	if after != before {
		t.Fatal("unavailable old value created a report")
	}
}

func TestVerificationStoreRetainsEmptyFile(t *testing.T) {
	h := newVerificationHarness(t, fixtureEffectsRegistry(t, VerificationPassed))
	h.seedSnapshot(t, "snapshot")
	contract := fixtureContract()
	contract.RequiredArtifacts[0].Contains = nil
	h.freeze(t, "snapshot", contract)
	fence := h.seedRun(t, "snapshot", "run")
	files := []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: ""}}
	source := h.source(t, fence, "loom", "source", files, nil)
	report, err := h.store.RecordVerifiedWorkflowOutputs(context.Background(), bundleFor(fence, source, files), fence)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != VerificationPassed {
		t.Fatal("valid empty required file rejected")
	}
	items, err := h.store.List(context.Background(), fence.WorkspaceID, ListFilter{RunID: fence.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("empty file was hidden: %d outputs", len(items))
	}
	for _, item := range items {
		if item.Title == "report.md" {
			read, err := h.store.Get(context.Background(), fence.WorkspaceID, item.ID)
			if err != nil || read.Content != "" {
				t.Fatalf("empty file cannot be read: %v", err)
			}
		}
	}
	if checked, err := h.store.RecheckCurrentDelivery(context.Background(), recheckRequest(report)); err != nil || !checked.Current || checked.Report.Status != VerificationPassed {
		t.Fatalf("empty file cannot be rechecked: %v", err)
	}
}
