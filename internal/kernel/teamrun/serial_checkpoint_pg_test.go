package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestCLIProgressCheckpointAllowsFirstStageReclaimAndFencesOldWriterRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, run := h.seedRunningWorkflowTask(t, "run-progress-reclaim")
	runtime := &WorkflowSerialRuntime{Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore()}
	writer := runtime.progressCheckpointWriter(run)
	if writer == nil {
		t.Fatal("CLI checkpoint writer unavailable")
	}
	checkpoint := checkpointFromPark(run, RuntimePark{NodeID: "lead", CompletedOutputs: map[string]json.RawMessage{}, UsageComplete: true}, h.now)
	if err := writer(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"reconciled":true}`), UsageComplete: true}
	processed, err := h.executor.ProcessNext(context.Background(), "worker-after-restart")
	if err != nil || !processed {
		t.Fatalf("reclaim processed=%v err=%v", processed, err)
	}
	if h.runtime.resumeCalls != 1 || h.runtime.executeCalls != 0 {
		t.Fatalf("resume=%d execute=%d", h.runtime.resumeCalls, h.runtime.executeCalls)
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, run.RunID, "")
	h.assertRun(t, run.RunID, StatusSucceeded, nil)
	if err := writer(context.Background(), checkpoint); !errors.Is(err, errTaskLeaseLost) {
		t.Fatalf("old writer=%v", err)
	}
}
