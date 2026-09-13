package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func TestRestartReplaysDurableResultWithoutExecutingCLI(t *testing.T) {
	root := t.TempDir()
	first, err := newResultSpool(root, "runtime-credential")
	if err != nil {
		t.Fatal(err)
	}
	journal := resultJournal{TaskID: "task-one", Result: runtimeprotocol.ExecutionReceipt{Versioned: runtimeprotocol.NewVersioned(), SchemaVersion: runtimeprotocol.ReceiptSchemaV1, TaskID: "task-one", Status: "completed", Output: "complete answer"}}
	if err := first.save(journal); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(first.path(journal.TaskID))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("result file permissions are not private")
	}
	var reports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/tasks/task-one/complete" {
			t.Errorf("unexpected execution endpoint: %s", r.URL.Path)
		}
		var got runtimeprotocol.ExecutionReceipt
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Output != journal.Result.Output {
			t.Error("lost result body")
		}
		if reports.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := newRuntimeClient(server.URL, "runtime-credential", server.Client())
	restarted, err := newResultSpool(root, "runtime-credential")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.replay(context.Background(), client); err == nil {
		t.Fatal("interrupted receipt was discarded")
	}
	restartedAgain, err := newResultSpool(root, "runtime-credential")
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedAgain.replay(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.path(journal.TaskID)); !os.IsNotExist(err) {
		t.Fatal("acknowledged result still pending")
	}
	if reports.Load() != 2 {
		t.Fatal("unexpected replay count")
	}
}
