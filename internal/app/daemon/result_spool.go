package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

type resultJournal struct {
	Subject    execution.Subject         `json:"subject"`
	ClaimEpoch int64                     `json:"claim_epoch"`
	TaskID     string                    `json:"task_id"`
	Result     runtimes.EngineExecResult `json:"result"`
	Failure    string                    `json:"failure,omitempty"`
}

type resultSpool struct {
	dir, prefix string
	mu          sync.Mutex
	active      map[string]bool
}

func newResultSpool(root, token string) (*resultSpool, error) {
	dir := filepath.Join(root, ".weave-results")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(token))
	return &resultSpool{dir: dir, prefix: hex.EncodeToString(hash[:16]) + "-", active: map[string]bool{}}, nil
}
func (s *resultSpool) path(id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, s.prefix+hex.EncodeToString(hash[:16])+".pending.json")
}
func (s *resultSpool) save(journal resultJournal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(data) > 16*1024*1024 {
		return fmt.Errorf("runtime result journal exceeds size bound")
	}
	files, err := filepath.Glob(filepath.Join(s.dir, s.prefix+"*.pending.json"))
	if err != nil {
		return err
	}
	if len(files) >= 128 {
		if _, err := os.Stat(s.path(journal.TaskID)); err != nil {
			return fmt.Errorf("runtime result journal is full")
		}
	}
	file, err := os.CreateTemp(s.dir, s.prefix+"*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), s.path(journal.TaskID)); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	s.active[journal.TaskID] = true
	return nil
}
func (s *resultSpool) release(id string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.active, id) }
func (s *resultSpool) acknowledge(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (s *resultSpool) retain(id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := s.path(id)
	_ = os.Rename(name, strings.TrimSuffix(name, ".pending.json")+"."+reason+".json")
}
func (s *resultSpool) replay(ctx context.Context, client *runtimeClient) error {
	files, err := filepath.Glob(filepath.Join(s.dir, s.prefix+"*.pending.json"))
	if err != nil {
		return err
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var journal resultJournal
		if len(data) > 16*1024*1024 || json.Unmarshal(data, &journal) != nil || s.path(journal.TaskID) != name {
			return fmt.Errorf("invalid result journal")
		}
		s.mu.Lock()
		active := s.active[journal.TaskID]
		s.mu.Unlock()
		if active {
			continue
		}
		reportCtx := withTaskProof(ctx, journal.Subject, journal.ClaimEpoch)
		if journal.Failure != "" {
			err = client.fail(reportCtx, journal.TaskID, journal.Failure)
		} else {
			err = client.complete(reportCtx, journal.TaskID, journal.Result)
		}
		if err == nil {
			if err = s.acknowledge(journal.TaskID); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, errLeaseLost) || isRejectedRuntimeResult(err) {
			s.retain(journal.TaskID, "unaccepted")
			slog.Error("saved runtime result could not be reattached; retained for recovery", "task_id", journal.TaskID, "error", err)
			continue
		}
		return err
	}
	return nil
}
func (d *service) resultRecoveryLoop(ctx context.Context) {
	if d.resultSpool == nil {
		return
	}
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := d.resultSpool.replay(attempt, d.client)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("saved runtime result replay pending", "error", err)
		}
		if !waitFor(ctx, 5*time.Second) {
			return
		}
	}
}
