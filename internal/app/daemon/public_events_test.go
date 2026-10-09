package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func TestPublicJournalReplaysLostResponseAndRestartWithoutReexecution(t *testing.T) {
	spool, err := newPublicSpool(t.TempDir(), "private-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.begin("task-original"); err != nil {
		t.Fatal(err)
	}
	if err := spool.append("task-original", engine.Event{Kind: "text", Text: "public draft"}, false); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var batch runtimeprotocol.PublicEventsRequest
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		for _, event := range batch.Events {
			if previous, present := seen[event.Seq]; present && previous != event.Event.Text {
				t.Error("replay changed its event identity")
			}
			seen[event.Seq] = event.Event.Text
		}
		if requests == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // Backend applied it, but the daemon received no ACK.
			return
		}
		_ = json.NewEncoder(w).Encode(runtimeprotocol.PublicEventsResponse{Versioned: runtimeprotocol.NewVersioned(), AckSeq: batch.Events[len(batch.Events)-1].Seq})
	}))
	defer server.Close()
	client, _ := newRuntimeClient(server.URL, "private-token", server.Client())
	if err := spool.flush(context.Background(), client); err == nil {
		t.Fatal("lost response was treated as acknowledged")
	}
	if err := spool.flush(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(seen) != 1 {
		t.Fatalf("unexpected replay requests=%d events=%d", requests, len(seen))
	}
	journal, err := spool.read(spool.path("task-original"))
	if err != nil || journal.Next != 2 || len(journal.Events) != 0 {
		t.Fatalf("ACK discarded active sequence: %+v %v", journal, err)
	}
	if err := spool.append("task-original", engine.Event{Kind: "text", Text: "next public draft"}, false); err != nil {
		t.Fatal(err)
	}
	// A fresh daemon sees a pending journal, but never runs that model task.
	restarted, err := newPublicSpool(filepath.Dir(spool.dir), "private-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.flush(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[2] != "next public draft" {
		t.Fatal("restart lost or resequenced public progress")
	}
	if _, err := os.Stat(spool.path("task-original")); !os.IsNotExist(err) {
		t.Fatalf("acknowledged orphan journal was not cleaned: %v", err)
	}
}

func TestPublicJournalUploadsWhileExecutionIsStillActive(t *testing.T) {
	spool, err := newPublicSpool(t.TempDir(), "token")
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch runtimeprotocol.PublicEventsRequest
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(runtimeprotocol.PublicEventsResponse{Versioned: runtimeprotocol.NewVersioned(), AckSeq: batch.Events[len(batch.Events)-1].Seq})
		select {
		case arrived <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	client, _ := newRuntimeClient(server.URL, "token", server.Client())
	d := &service{publicSpool: spool, client: client}
	publish, finish := d.publicEventCapture(&runtimeprotocol.ExecutionClaim{TaskID: "task-live"}, true)
	publish(engine.Event{Kind: "tool_call", Tool: "shell", CallID: "call", Status: "running"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); d.publicEventsLoop(ctx) }()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("live event waited for executeTask to finish")
	}
	cancel()
	workers.Wait()
	result := engine.RunResult{Status: "completed", Output: "real final"}
	finish(&result)
	if err := spool.flush(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool.path("task-live")); !os.IsNotExist(err) {
		t.Fatal("finished acknowledged journal retained")
	}
	if result.Output != "real final" || len(result.Diagnostics) != 0 {
		t.Fatalf("progress changed final output: %+v", result)
	}
}

func TestPublicJournalCapacityAndDiskFailureDegradeExplicitly(t *testing.T) {
	spool, err := newPublicSpool(t.TempDir(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.begin("task-bound"); err != nil {
		t.Fatal(err)
	}
	// Seed the real persisted counter near its limit to exercise the final slot
	// without doing hundreds of unrelated fsync operations in this regression.
	journal := publicJournal{TaskID: "task-bound", Next: runtimeprotocol.PublicEventLimit - 1}
	if err := spool.save(journal); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := spool.append("task-bound", engine.Event{Kind: "text", Text: "bounded"}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.finish("task-bound", false); err != nil {
		t.Fatal(err)
	}
	journal, err = spool.read(spool.path("task-bound"))
	if err != nil || len(journal.Events) != 2 || journal.Events[1].Seq != runtimeprotocol.PublicEventLimit || !journal.Events[1].Truncated {
		t.Fatalf("event bound lost its explicit end: %+v %v", journal, err)
	}
	info, _ := os.Stat(spool.path("task-bound"))
	// Windows has no Unix permission bits; privacy there comes from the
	// per-user profile directory.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("public journal is not private")
	}
	for index := 0; index < publicJournalMaxFiles-1; index++ {
		if err := spool.begin(fmt.Sprintf("pending-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := spool.begin("overflow"); err == nil {
		t.Fatal("unacknowledged journals exceeded file bound")
	}
	byteBound, err := newPublicSpool(t.TempDir(), "byte-bound")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(byteBound.dir, "other-runtime.json"), make([]byte, publicJournalMaxBytes), 0600); err != nil {
		t.Fatal(err)
	}
	if err := byteBound.begin("overflow"); err == nil {
		t.Fatal("journals exceeded their global byte bound")
	}
	broken := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(broken, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	d := &service{publicSpool: &publicSpool{dir: broken, prefix: "test-", active: map[string]bool{}}}
	publish, finish := d.publicEventCapture(&runtimeprotocol.ExecutionClaim{TaskID: "disk-failed"}, true)
	publish(engine.Event{Kind: "text", Text: "public"}, false)
	result := engine.RunResult{Status: "completed", Output: "retained final"}
	finish(&result)
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "public_events_unavailable" || result.Output != "retained final" {
		t.Fatalf("disk error silently lost progress or final output: %+v", result)
	}
	if strings.Contains(result.Diagnostics[0].Message, broken) {
		t.Fatal("diagnostic leaked local path")
	}
}
