// Package localruntime assembles the same Runtime Host beside a single-node
// Server. Queue ownership, claims, receipts and recovery remain in their
// existing platform and Host implementations.
package localruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/app/daemon"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

type Config struct {
	Root         string
	Handler      http.Handler
	Runtimes     *runtimes.Store
	Executor     *runtimes.Executor
	WorkspaceIDs func(context.Context) ([]string, error)
	Credentials  func(context.Context, string, *runtimeprotocol.ExecutionClaim) (daemon.ProviderCredentials, error)
	Concurrency  int
}
type registration struct {
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
	RuntimeID   string `json:"runtime_id"`
	Token       string `json:"token"`
}
type Manager struct {
	config  Config
	ctx     context.Context
	cancel  context.CancelFunc
	server  *http.Server
	url     string
	mu      sync.Mutex
	hosts   map[string]registration
	workers sync.WaitGroup
	release func()
}

func Start(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Root == "" || cfg.Handler == nil || cfg.Runtimes == nil || cfg.Executor == nil || cfg.WorkspaceIDs == nil || cfg.Credentials == nil {
		return nil, errors.New("local runtime assembly is incomplete")
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 2
	}
	if err := os.MkdirAll(cfg.Root, 0700); err != nil {
		return nil, err
	}
	release, err := lockHostRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		release()
		return nil, err
	}
	base, cancel := context.WithCancel(ctx)
	m := &Manager{config: cfg, ctx: base, cancel: cancel, server: &http.Server{Handler: cfg.Handler, ReadHeaderTimeout: 10 * time.Second}, url: "http://" + listener.Addr().String(), hosts: map[string]registration{}, release: release}
	go func() { _ = m.server.Serve(listener) }()
	if err := m.refresh(ctx); err != nil {
		_ = m.Close()
		return nil, err
	}
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-base.Done():
				return
			case <-ticker.C:
				if err := m.refresh(base); err != nil {
					slog.Warn("local runtime workspace registration pending", "error", err)
				}
			}
		}
	}()
	return m, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.workers.Wait() // keep receipt endpoints alive until Host processes stop
	err := m.server.Close()
	if m.release != nil {
		m.release()
		m.release = nil
	}
	return err
}

func (m *Manager) refresh(ctx context.Context) error {
	workspaces, err := m.config.WorkspaceIDs(ctx)
	if err != nil {
		return err
	}
	for _, workspace := range workspaces {
		if _, err = m.ensure(ctx, workspace); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensure(ctx context.Context, workspace string) (registration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return registration{}, m.ctx.Err()
	}
	if existing, ok := m.hosts[workspace]; ok {
		return existing, nil
	}
	sum := sha256.Sum256([]byte(workspace))
	root := filepath.Join(m.config.Root, hex.EncodeToString(sum[:16]))
	if err := os.MkdirAll(root, 0700); err != nil {
		return registration{}, err
	}
	path := filepath.Join(root, "host.json")
	var identity registration
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id, token, generateErr := runtimes.NewRegistrationIdentity()
		if generateErr != nil {
			return identity, generateErr
		}
		identity = registration{Version: 1, WorkspaceID: workspace, RuntimeID: id, Token: token}
		if err := writeRegistration(root, path, identity); err != nil {
			return identity, err
		}
	} else if err != nil {
		return identity, err
	} else if json.Unmarshal(data, &identity) != nil || identity.Version != 1 || identity.WorkspaceID != workspace {
		return identity, errors.New("local runtime identity is invalid")
	}
	registered, err := m.config.Runtimes.EnsureManagedRegistration(ctx, workspace, identity.RuntimeID, "本机运行环境", identity.Token)
	if err != nil {
		return identity, err
	}
	if registered.ID != identity.RuntimeID {
		identity.RuntimeID = registered.ID
		if err := writeRegistration(root, path, identity); err != nil {
			return identity, err
		}
	}
	host, err := daemon.NewManagedHost(ctx, daemon.HostConfig{Server: m.url, Token: identity.Token, WorkspacesRoot: root, SubjectBindingRoot: m.config.Root, Concurrency: m.config.Concurrency, Credentials: func(ctx context.Context, claim *runtimeprotocol.ExecutionClaim) (daemon.ProviderCredentials, error) {
		if claim.WorkspaceID != identity.WorkspaceID {
			return daemon.ProviderCredentials{}, execution.ErrSubjectMismatch
		}
		return m.config.Credentials(ctx, identity.RuntimeID, claim)
	}})
	if err != nil {
		return identity, err
	}
	m.hosts[workspace] = identity
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		if err := host.Run(m.ctx); err != nil {
			slog.Error("local runtime stopped", "error", err)
		}
	}()
	// Host hello owns observed capabilities. Wait only for that authenticated
	// receipt, never fabricate readiness from registration alone.
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		runtime, err := m.config.Runtimes.Get(readyCtx, workspace, identity.RuntimeID)
		if err == nil && runtime.Online {
			return identity, nil
		}
		select {
		case <-readyCtx.Done():
			return identity, fmt.Errorf("local runtime did not connect: %w", readyCtx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func writeRegistration(root, path string, identity registration) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".host-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// ExecRemote selects the explicitly assembled machine only for mutable
// definitions without a runtime. Frozen definitions retain their binding.
func (m *Manager) ExecRemote(ctx context.Context, workspace string, record *registry.AgentRecord, stamp execution.AgentExecutionStamp, prompt string, attachments []execspec.Attachment) (engine.RunResult, error) {
	bound, err := m.bindRecord(ctx, workspace, record, stamp)
	if err != nil {
		return engine.RunResult{}, err
	}
	return m.config.Executor.ExecRemote(ctx, workspace, bound, stamp, prompt, attachments)
}

func (m *Manager) ExecRemoteStructured(ctx context.Context, workspace string, record *registry.AgentRecord, stamp execution.AgentExecutionStamp, prompt string, attachments []execspec.Attachment, schema json.RawMessage) (engine.RunResult, error) {
	bound, err := m.bindRecord(ctx, workspace, record, stamp)
	if err != nil {
		return engine.RunResult{}, err
	}
	return m.config.Executor.ExecRemoteStructured(ctx, workspace, bound, stamp, prompt, attachments, schema)
}

func (m *Manager) bindRecord(ctx context.Context, workspace string, record *registry.AgentRecord, stamp execution.AgentExecutionStamp) (*registry.AgentRecord, error) {
	if record == nil {
		return nil, errors.New("agent definition is required")
	}
	if record.RuntimeID != "" {
		return record, nil
	}
	if stamp.RunSnapshotID != "" || stamp.ExecutionScope == execution.ScopeTeamWorkerLeaf {
		return nil, errors.New("published agent requires its frozen runtime")
	}
	if _, err := execution.RequireSubject(ctx, workspace); err != nil {
		return nil, err
	}
	workspaces, err := m.config.WorkspaceIDs(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(workspaces, workspace) {
		return nil, errors.New("workspace is unavailable")
	}
	identity, err := m.ensure(ctx, workspace)
	if err != nil {
		return nil, err
	}
	bound := *record
	bound.RuntimeID = identity.RuntimeID
	return &bound, nil
}
