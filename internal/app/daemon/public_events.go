package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

const publicJournalMaxBytes = 16 * 1024 * 1024
const publicJournalMaxFiles = 64

type publicJournal struct {
	Subject    execution.Subject      `json:"subject"`
	ClaimEpoch int64                  `json:"claim_epoch"`
	TaskID     string                 `json:"task_id"`
	Next       int64                  `json:"next"`
	Events     []runtimes.PublicEvent `json:"events"`
	Done       bool                   `json:"done"`
	Truncated  bool                   `json:"truncated"`
}

// Each atomic file contains both the unacknowledged events and the next
// sequence. An acknowledged active task keeps its sequence until execution
// exits. On restart, orphan journals are drained without restarting execution.
type publicSpool struct {
	mu          sync.Mutex
	dir, prefix string
	active      map[string]bool
}

func newPublicSpool(root, token string) (*publicSpool, error) {
	dir := filepath.Join(root, ".weave-public-events")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(token))
	spool := &publicSpool{dir: dir, prefix: hex.EncodeToString(sum[:16]) + "-", active: map[string]bool{}}
	probe, err := os.CreateTemp(dir, spool.prefix+"*.tmp")
	if err != nil {
		return nil, err
	}
	_, writeErr := probe.Write([]byte{1})
	if writeErr == nil {
		writeErr = probe.Sync()
	}
	closeErr := probe.Close()
	_ = os.Remove(probe.Name())
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Fixed-name scratch files can remain after a crash, but cannot accumulate.
	files, err := filepath.Glob(filepath.Join(dir, spool.prefix+"*.tmp"))
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		if err := os.Remove(name); err != nil {
			return nil, err
		}
	}
	return spool, nil
}

func (s *publicSpool) path(taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return filepath.Join(s.dir, s.prefix+hex.EncodeToString(sum[:16])+".json")
}

func (s *publicSpool) read(path string) (publicJournal, error) {
	var journal publicJournal
	info, err := os.Lstat(path)
	if err != nil {
		return journal, err
	}
	if !info.Mode().IsRegular() || info.Size() > publicJournalMaxBytes {
		return journal, errors.New("invalid public journal file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return journal, err
	}
	if journal.TaskID == "" || s.path(journal.TaskID) != path || journal.Next < 1 || journal.Next > runtimes.PublicEventLimit+1 || len(journal.Events) > runtimes.PublicEventLimit {
		return journal, errors.New("invalid public journal identity")
	}
	return journal, nil
}

// save is called under mu. Both file contents and the rename are synced before
// the caller may treat a public event as durably captured.
func (s *publicSpool) save(journal publicJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	path := s.path(journal.TaskID)
	var size int64
	files := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if filepath.Join(s.dir, entry.Name()) != path {
			size += info.Size()
			files++
		}
	}
	if size+int64(len(data)) > publicJournalMaxBytes || files >= publicJournalMaxFiles {
		return errors.New("public event journal capacity exceeded")
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *publicSpool) begin(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	journal, err := s.read(s.path(taskID))
	if errors.Is(err, os.ErrNotExist) {
		journal = publicJournal{TaskID: taskID, Next: 1}
	} else if err != nil {
		return err
	}
	if journal.Done {
		return errors.New("public journal already finished")
	}
	if err := s.save(journal); err != nil {
		return err
	}
	s.active[taskID] = true
	return nil
}

func (s *publicSpool) append(taskID string, event engine.Event, truncated bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	journal, err := s.read(s.path(taskID))
	if err != nil {
		return err
	}
	if journal.Done {
		return errors.New("public journal already finished")
	}
	journal.Truncated = journal.Truncated || truncated
	if journal.Next >= runtimes.PublicEventLimit && event.Kind != "stream_end" {
		journal.Truncated = true
		return s.save(journal)
	}
	item := runtimes.PublicEvent{Seq: journal.Next, OccurredAt: time.Now().UTC(), Event: event, Truncated: truncated}
	if event.Kind == "stream_end" {
		item.Truncated = journal.Truncated
		journal.Done = true
	}
	if err := runtimes.ValidatePublicEvent(item); err != nil {
		return err
	}
	journal.Events = append(journal.Events, item)
	journal.Next++
	return s.save(journal)
}

func (s *publicSpool) finish(taskID string, partial bool) error {
	err := s.append(taskID, engine.Event{Kind: "stream_end"}, partial)
	s.mu.Lock()
	delete(s.active, taskID)
	s.mu.Unlock()
	return err
}

func (c *runtimeClient) publicEvents(ctx context.Context, taskID string, events []runtimes.PublicEvent) (int64, error) {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/tasks/"+url.PathEscape(taskID)+"/events", map[string]any{"events": events})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := expectStatus(response, http.StatusOK); err != nil {
		return 0, err
	}
	var ack struct {
		AckSeq int64 `json:"ack_seq"`
	}
	if err := json.NewDecoder(response.Body).Decode(&ack); err != nil {
		return 0, err
	}
	if len(events) == 0 || ack.AckSeq != events[len(events)-1].Seq {
		return 0, errors.New("invalid public event acknowledgement")
	}
	return ack.AckSeq, nil
}

func (s *publicSpool) flush(ctx context.Context, client *runtimeClient) error {
	paths, err := filepath.Glob(filepath.Join(s.dir, s.prefix+"*.json"))
	if err != nil {
		return err
	}
	var failures []error
	for _, path := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		journal, err := s.read(path)
		if err == nil && len(journal.Events) == 0 && !s.active[journal.TaskID] {
			err = os.Remove(path)
		}
		s.mu.Unlock()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if len(journal.Events) == 0 {
			continue
		}
		batch := journal.Events[:min(len(journal.Events), runtimes.PublicEventBatchLimit)]
		reportCtx, cancel := context.WithTimeout(withTaskProof(ctx, journal.Subject, journal.ClaimEpoch), 5*time.Second)
		ack, err := client.publicEvents(reportCtx, journal.TaskID, batch)
		cancel()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		s.mu.Lock()
		current, err := s.read(path)
		if err == nil {
			pending := current.Events[:0]
			for _, event := range current.Events {
				if event.Seq > ack {
					pending = append(pending, event)
				}
			}
			current.Events = pending
			if len(pending) == 0 && !s.active[current.TaskID] {
				err = os.Remove(path)
			} else {
				err = s.save(current)
			}
		}
		s.mu.Unlock()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (d *service) publicEventsLoop(ctx context.Context) {
	if d.publicSpool == nil {
		return
	}
	for {
		if err := d.publicSpool.flush(ctx, d.client); err != nil && ctx.Err() == nil {
			slog.Warn("runtime public progress not acknowledged; retaining journal", "error", err)
		}
		if !waitFor(ctx, 500*time.Millisecond) {
			return
		}
	}
}

func (d *service) publicEventCapture(task *taskqueue.Task, enabled bool) (func(engine.Event, bool), func(*engine.RunResult)) {
	if !enabled || d.publicSpool == nil {
		return nil, func(*engine.RunResult) {}
	}
	taskID := task.ID
	err := d.publicSpool.begin(taskID)
	if err == nil {
		err = d.publicSpool.bindTask(task)
	}
	started := err == nil
	if err != nil {
		slog.Warn("runtime public progress unavailable for this execution", "task_id", taskID, "error", err)
	}
	publish := func(event engine.Event, truncated bool) {
		if err == nil {
			err = d.publicSpool.append(taskID, event, truncated)
			if err != nil {
				slog.Warn("runtime public progress capture failed", "task_id", taskID, "error", err)
			}
		}
	}
	finish := func(result *engine.RunResult) {
		if started {
			err = errors.Join(err, d.publicSpool.finish(taskID, err != nil || result.Status != "completed"))
		}
		if err != nil {
			// A full or unwritable journal degrades progress only. Preserve the
			// authoritative result, and expose the missing-progress diagnosis.
			slog.Warn("runtime public progress incomplete", "task_id", taskID, "error", err)
			result.Diagnostics = append(result.Diagnostics, engine.Diagnostic{Code: "public_events_unavailable", Message: "Some public progress could not be saved; the final result remains authoritative."})
		}
	}
	return publish, finish
}

func (s *publicSpool) bindTask(task *taskqueue.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	journal, err := s.read(s.path(task.ID))
	if err != nil {
		return err
	}
	if journal.ClaimEpoch != 0 && (journal.ClaimEpoch != task.ClaimEpoch || journal.Subject != task.Subject) {
		return execution.ErrSubjectMismatch
	}
	journal.ClaimEpoch = task.ClaimEpoch
	journal.Subject = task.Subject
	return s.save(journal)
}
