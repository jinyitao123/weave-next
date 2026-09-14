package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// ProviderCredentials are resolved anew for the authenticated physical claim.
// Managed hosts never discover an operator's ambient provider or OAuth login.
type ProviderCredentials struct{ BaseURL, APIKey string }
type ClaimCredentialResolver func(context.Context, *runtimeprotocol.ExecutionClaim) (ProviderCredentials, error)

type Host interface {
	Run(context.Context) error
	Capabilities() []runtimeprotocol.EngineCapability
}

type HostConfig struct {
	Server, Token, WorkspacesRoot string
	SubjectBindingRoot            string
	Concurrency                   int
	HTTPClient                    *http.Client
	Credentials                   ClaimCredentialResolver
}

// NewManagedHost reuses the normal versioned claim/receipt/recovery loop.
// No provider key is persisted in its registration or result journal.
func NewManagedHost(ctx context.Context, cfg HostConfig) (Host, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("runtime subject credentials are required")
	}
	if err := os.MkdirAll(cfg.WorkspacesRoot, 0700); err != nil {
		return nil, err
	}
	guard, err := newSubjectGuard(ctx, cfg.WorkspacesRoot)
	if err != nil {
		return nil, err
	}
	if cfg.SubjectBindingRoot != "" {
		guard.bindingRoot = cfg.SubjectBindingRoot
	}
	detected := detectEngines()
	capabilities := make([]runtimeprotocol.EngineCapability, 0, len(detected))
	for _, name := range detected {
		version := engine.BinaryVersion(ctx, config.ResolveEngineCLIPath(name))
		availability, reason := runtimeprotocol.EngineAvailabilityUnknown, ""
		if version == "unavailable" {
			availability, reason = runtimeprotocol.EngineAvailabilityUnavailable, "engine_version_unavailable"
		}
		capabilities = append(capabilities, runtimeprotocol.EngineCapability{
			Engine: name, BinaryPath: config.ResolveEngineCLIPath(name), BinaryVersion: version,
			AuthMode: runtimeprotocol.AuthModeProvider, ProtocolVersion: engineProtocolVersion(name), EndpointClass: "subject_provider",
			ConfigurationSource: "subject_credentials", Availability: availability, UnavailableReason: reason, SubjectIsolation: guard.mode,
		})
	}
	d, err := newDaemon(daemonConfig{server: cfg.Server, token: cfg.Token, workspacesRoot: cfg.WorkspacesRoot, concurrency: cfg.Concurrency,
		httpClient: cfg.HTTPClient, detectedEngines: detected, engineCapabilities: capabilities, renewInterval: time.Second})
	if err != nil {
		return nil, err
	}
	d.subjectGuard, d.credentials = guard, cfg.Credentials
	return d, nil
}

func (d *service) Capabilities() []runtimeprotocol.EngineCapability {
	return append([]runtimeprotocol.EngineCapability(nil), d.engineCapabilities...)
}

type subjectBinding struct {
	Version  int                 `json:"version"`
	Subjects []execution.Subject `json:"subjects"`
}
type subjectGuard struct {
	root, mode  string
	bindingRoot string
}

// Embedded workspace Hosts share one physical machine binding. The Manager
// holds the cross-process root lock; this serializes concurrent workspace claims.
var subjectBindingLocks sync.Map

func newSubjectGuard(ctx context.Context, root string) (*subjectGuard, error) {
	guard := &subjectGuard{root: root, mode: runtimeprotocol.SubjectIsolationSingleUser}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if engine.ProbeIsolation(probe, root) == nil {
		guard.mode = runtimeprotocol.SubjectIsolationStrong
	}
	return guard, nil
}

func (g *subjectGuard) bind(subject execution.Subject) (string, *engine.ProcessIsolation, error) {
	if err := subject.Validate(); err != nil {
		return "", nil, err
	}
	bindingRoot := g.bindingRoot
	if bindingRoot == "" {
		bindingRoot = g.root
	}
	bindingRoot, err := filepath.Abs(bindingRoot)
	if err != nil {
		return "", nil, err
	}
	value, _ := subjectBindingLocks.LoadOrStore(bindingRoot, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	path := filepath.Join(bindingRoot, ".subject-bindings.json")
	binding := subjectBinding{Version: 1}
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &binding) != nil || binding.Version != 1 {
			return "", nil, errors.New("runtime subject binding is invalid")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if g.mode != runtimeprotocol.SubjectIsolationStrong && (subject.UserID == "" || len(binding.Subjects) > 1 || len(binding.Subjects) == 1 && binding.Subjects[0] != subject) {
		return "", nil, errors.New("runtime_single_user_bound: this host cannot isolate another account")
	}
	if !slices.Contains(binding.Subjects, subject) {
		binding.Subjects = append(binding.Subjects, subject)
		data, _ = json.Marshal(binding)
		file, err := os.CreateTemp(bindingRoot, ".subject-binding-*")
		if err != nil {
			return "", nil, err
		}
		defer os.Remove(file.Name())
		_, err = file.Write(data)
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
		if err == nil {
			err = os.Rename(file.Name(), path)
		}
		if err != nil {
			return "", nil, err
		}
		dir, err := os.Open(bindingRoot)
		if err != nil {
			return "", nil, err
		}
		err = errors.Join(dir.Sync(), dir.Close())
		if err != nil {
			return "", nil, err
		}
	}
	root := filepath.Join(g.root, ".subjects", subject.Digest())
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", nil, err
	}
	if g.mode != runtimeprotocol.SubjectIsolationStrong {
		return root, nil, nil
	}
	protected := []string{g.root}
	if home, err := os.UserHomeDir(); err == nil {
		protected = append(protected, home)
	}
	return root, &engine.ProcessIsolation{SubjectRoot: root, ProtectedRoots: protected}, nil
}

func managedEnvironment(root string, credentials ProviderCredentials) (map[string]string, error) {
	if credentials.APIKey == "" || credentials.BaseURL == "" {
		return nil, errors.New("runtime_credentials_missing: configure a provider for this account")
	}
	env := map[string]string{"WEAVE_SUBJECT_SANDBOX": "1", "OPENAI_BASE_URL": credentials.BaseURL, "OPENAI_API_KEY": credentials.APIKey, "ONEAPI_API_KEY": credentials.APIKey,
		"ANTHROPIC_BASE_URL": credentials.BaseURL, "ANTHROPIC_API_KEY": credentials.APIKey, "ANTHROPIC_AUTH_TOKEN": credentials.APIKey}
	for name, dir := range map[string]string{"HOME": "home", "USERPROFILE": "home", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "CLAUDE_CONFIG_DIR": "claude", "TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp"} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, fmt.Errorf("create account directory: %w", err)
		}
		env[name] = path
	}
	return env, nil
}
