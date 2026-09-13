package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type artifactTaskReader struct {
	ExecutorTaskStore
	tasks map[string]*taskqueue.Task
	err   error
}

func (s artifactTaskReader) Get(_ context.Context, _ string, id string) (*taskqueue.Task, error) {
	return s.tasks[id], s.err
}

func artifactTask(id, contentType string) *taskqueue.Task {
	raw, _ := json.Marshal(map[string]any{"status": "completed", "artifacts": []fileartifact.File{{Path: "report.md", ContentType: contentType, Content: "saved body"}}})
	return &taskqueue.Task{ID: id, WorkspaceID: "workspace-1", RunSnapshotID: "snapshot-1", Kind: "engine_exec", Status: taskqueue.StatusCompleted, Result: raw}
}

func TestWorkflowArtifactLoaderRetainsExactSourcesAndMergesIdenticalFiles(t *testing.T) {
	run := TeamRun{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1"}
	first, second := artifactTask("task-1", "text/markdown"), artifactTask("task-2", "text/markdown")
	runtime := &WorkflowSerialRuntime{Tasks: artifactTaskReader{tasks: map[string]*taskqueue.Task{first.ID: first, second.ID: second}}}
	artifacts, err := runtime.workflowArtifactLoader(run)(t.Context(), []string{first.ID, second.ID, first.ID})
	if err != nil || len(artifacts) != 1 || len(artifacts[0].Sources) != 2 {
		t.Fatalf("artifacts=%#v err=%v", artifacts, err)
	}
	for index, source := range artifacts[0].Sources {
		expected := []string{first.ID, second.ID}[index]
		digest, _ := workflowResultDigest(first.Result)
		if source.TaskID != expected || source.MemberRunID != "" || source.ResultDigest != digest || source.RunSnapshotID != run.RunSnapshotID || source.ParentRunID != run.RunID {
			t.Fatalf("wrong source: %#v", source)
		}
	}
	sources, err := runtime.workflowArtifactObservationLoader(run)(t.Context(), []string{first.ID, second.ID, first.ID})
	if err != nil || len(sources) != 2 || !reflect.DeepEqual([]any{sources[0].Source, sources[1].Source}, []any{artifacts[0].Sources[0], artifacts[0].Sources[1]}) {
		t.Fatalf("summary source mismatch: %#v, %v", sources, err)
	}
	second.Result = json.RawMessage(`{"status":"completed","output":"summary only"}`)
	sources, err = runtime.workflowArtifactObservationLoader(run)(t.Context(), []string{second.ID})
	if err != nil || len(sources) != 1 || sources[0].Source.TaskID != second.ID || len(sources[0].Source.ResultDigest) != 64 {
		t.Fatalf("fileless result source lost: %#v %v", sources, err)
	}
}

func TestWorkflowArtifactLoaderRejectsInvalidPhysicalResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*taskqueue.Task)
	}{
		{"workspace", func(task *taskqueue.Task) { task.WorkspaceID = "elsewhere" }},
		{"snapshot", func(task *taskqueue.Task) { task.RunSnapshotID = "old" }},
		{"identity", func(task *taskqueue.Task) { task.ID = "other" }},
		{"kind", func(task *taskqueue.Task) { task.Kind = "workflow" }},
		{"task state", func(task *taskqueue.Task) { task.Status = taskqueue.StatusFailed }},
		{"engine state", func(task *taskqueue.Task) {
			task.Result = json.RawMessage(`{"status":"failed","error":"original failure"}`)
		}},
		{"invalid result", func(task *taskqueue.Task) { task.Result = json.RawMessage(`{"artifacts":`) }},
		{"null result", func(task *taskqueue.Task) { task.Result = json.RawMessage(`null`) }},
		{"invalid files", func(task *taskqueue.Task) {
			task.Result = json.RawMessage(`{"artifacts":[{"path":"../host","content_type":"text/plain","content":"a"}]}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := artifactTask("task-1", "text/markdown")
			test.mutate(task)
			runtime := &WorkflowSerialRuntime{Tasks: artifactTaskReader{tasks: map[string]*taskqueue.Task{"task-1": task}}}
			_, err := runtime.workflowArtifactLoader(TeamRun{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1"})(t.Context(), []string{"task-1"})
			if err == nil {
				t.Fatal("invalid physical result was promoted")
			}
		})
	}
	run := TeamRun{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1"}
	runtime := &WorkflowSerialRuntime{Tasks: artifactTaskReader{tasks: map[string]*taskqueue.Task{"task-1": artifactTask("task-1", "text/markdown"), "task-2": artifactTask("task-2", "text/plain")}}}
	if _, err := runtime.workflowArtifactLoader(run)(t.Context(), []string{"task-1", "task-2"}); err == nil {
		t.Fatal("content type conflict hidden by content deduplication")
	}
	runtime.Tasks = artifactTaskReader{err: errors.New("storage unavailable")}
	if _, err := runtime.workflowArtifactLoader(run)(t.Context(), []string{"task-1"}); err == nil || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("storage error swallowed: %v", err)
	}
}

type artifactMemberTx struct {
	pgx.Tx
	raw      []byte
	wantArgs []any
	t        *testing.T
}

func (tx artifactMemberTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if !strings.Contains(query, "workspace_id=$1 AND parent_run_id=$2 AND run_snapshot_id=$3 AND member_run_id=$4") || !reflect.DeepEqual(args, tx.wantArgs) {
		tx.t.Fatalf("member provenance query is unscoped: %s %#v", query, args)
	}
	return artifactMemberRow{raw: tx.raw}
}
func (tx artifactMemberTx) Rollback(context.Context) error { return nil }

type artifactMemberRow struct{ raw []byte }

func (row artifactMemberRow) Scan(dest ...any) error {
	*dest[0].(*[]byte) = append([]byte(nil), row.raw...)
	return nil
}

type artifactMemberBeginner struct{ tx pgx.Tx }

func (b artifactMemberBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

func TestWorkflowArtifactLoaderPreservesLoomMemberResultProvenance(t *testing.T) {
	run := TeamRun{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1"}
	for _, reason := range []loom.StopReason{loom.StopCompleted, loom.StopError} {
		result := loom.RunResult{RunID: "member-1", StopReason: reason, State: map[string]any{fileartifact.MemberStateKey: []fileartifact.File{{Path: "report.md", ContentType: "text/markdown", Content: "saved member body"}}}}
		raw, _ := json.Marshal(map[string]any{"result": result, "error": ""})
		runtime := &WorkflowSerialRuntime{Transactions: artifactMemberBeginner{tx: artifactMemberTx{raw: raw, t: t, wantArgs: []any{run.WorkspaceID, run.RunID, run.RunSnapshotID, "member-1"}}}}
		artifacts, err := runtime.workflowArtifactLoader(run)(t.Context(), []string{"member:member-1"})
		if reason != loom.StopCompleted {
			if err == nil {
				t.Fatal("non-completed member promoted")
			}
			continue
		}
		if err != nil || len(artifacts) != 1 || len(artifacts[0].Sources) != 1 {
			t.Fatalf("member artifacts=%#v err=%v", artifacts, err)
		}
		source := artifacts[0].Sources[0]
		digest, _ := workflowResultDigest(raw)
		if source.MemberRunID != "member-1" || source.TaskID != "" || source.ResultDigest != digest || source.ParentRunID != run.RunID || source.RunSnapshotID != run.RunSnapshotID {
			t.Fatalf("member source lost: %#v", source)
		}
	}
}

func TestWorkflowResultDigestCanonicalizesWithoutLosingPrecision(t *testing.T) {
	one, err := workflowResultDigest(json.RawMessage(`{"b":9007199254740993,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	two, err := workflowResultDigest(json.RawMessage("{\n\"a\":\"x\",\"b\":9007199254740993}"))
	if err != nil || one != two {
		t.Fatalf("jsonb formatting changed source: %s %s %v", one, two, err)
	}
	other, err := workflowResultDigest(json.RawMessage(`{"b":9007199254740992,"a":"x"}`))
	if err != nil || one == other {
		t.Fatalf("distinct source collapsed: %s %s %v", one, other, err)
	}
	if _, err := workflowResultDigest(json.RawMessage(`{} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}
