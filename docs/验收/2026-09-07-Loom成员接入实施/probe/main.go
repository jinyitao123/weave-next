// Opt-in acceptance driver. All member execution, publication, recovery and
// delivery use production Weave code. The gateway supplies inference and a
// scoped engineering tool; it contains no engineering model or answer files.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/app/api"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/skills"
)

type settings struct {
	Root        string `json:"root"`
	DatabaseURL string `json:"database_url"`
	Repo        string `json:"repo"`
	Workspace   string `json:"workspace"`
	APIPort     string `json:"api_port"`
	GatewayPort string `json:"gateway_port"`
	SecretKey   string `json:"secret_key"`
	JWTSecret   string `json:"jwt_secret"`
	CLIPath     string `json:"cli_path"`
	CLIVersion  string `json:"cli_version"`
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func write(path string, v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	must(err)
	must(os.MkdirAll(filepath.Dir(path), 0700))
	must(os.WriteFile(path, append(raw, '\n'), 0600))
}
func main() {
	if len(os.Args) != 3 {
		panic("usage: probe init|serve|gateway settings.json")
	}
	var cfg settings
	raw, err := os.ReadFile(os.Args[2])
	must(err)
	must(json.Unmarshal(raw, &cfg))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "gateway" {
		serveGateway(ctx, cfg)
		return
	}
	store, err := pgstore.New(cfg.DatabaseURL)
	must(err)
	defer store.Close()
	key, err := hex.DecodeString(cfg.SecretKey)
	must(err)
	switch os.Args[1] {
	case "init":
		must(store.Migrate(ctx))
		must(db.Migrate(ctx, store.Pool()))
		_, err := store.Pool().Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Loom 成员恢复验收')`, cfg.Workspace)
		must(err)
		_, err = store.Pool().Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('acceptance-user',$1,'acceptance-user','unusable-password','admin')`, cfg.Workspace)
		must(err)
		_, err = store.Pool().Exec(ctx, `INSERT INTO weave_members(workspace_id,user_id,role) VALUES($1,'acceptance-user','owner')`, cfg.Workspace)
		must(err)
		_, token, err := apikeys.NewStore(store.Pool()).Create(ctx, cfg.Workspace, "isolated-workbench", "admin", "acceptance-user", apikeys.BootstrapScopes(), nil)
		must(err)
		must(os.WriteFile(filepath.Join(cfg.Root, "api-key"), []byte(token), 0600))
		mcp := mcpregistry.New(store.Pool(), key)
		server, err := mcp.Create(ctx, cfg.Workspace, "acceptance-user", mcpregistry.UpsertServerRequest{Slug: "engineering", DisplayName: "本轮工程工具", Transport: mcpregistry.TransportStreamableHTTP, URL: "http://127.0.0.1:" + cfg.GatewayPort + "/mcp", Enabled: true})
		must(err)
		_, err = mcp.RecordProbeSuccess(ctx, cfg.Workspace, server.ID, "2025-03-26", json.RawMessage(`{}`), []mcpregistry.Tool{{Name: "run_python", Description: toolDescription, InputSchema: toolSchema}})
		must(err)
		prompt, err := os.ReadFile(filepath.Join(cfg.Root, "stage-d/prompt.txt"))
		must(err)
		bundle, _, published := publishMemberSample(ctx, store.Pool(), key, cfg.Workspace, "http://127.0.0.1:"+cfg.GatewayPort, server.ID, server.FunctionalRevision, "run_python", string(prompt))
		write(filepath.Join(cfg.Root, "stage-d/published-artifact.json"), published)
		write(filepath.Join(cfg.Root, "stage-d/identity.json"), map[string]any{"workspace_id": cfg.Workspace, "team_id": "team", "workflow_id": "flow", "worker_id": bundle.Agent.AgentID, "published_hash": published.ContentHash})
		fmt.Println("isolated publication initialized")
	case "serve":
		conf, err := config.Load()
		must(err)
		conf.Port = cfg.APIPort
		conf.DatabaseURL = cfg.DatabaseURL
		conf.JWTSecret = cfg.JWTSecret
		conf.CORSOrigins = "*"
		conf.MetaTeamEnabled = false
		models := llmrouter.NewResolver(llmrouter.New("acceptance-model"))
		s := api.NewServer(conf, store, models)
		s.Descriptors = descriptors()
		s.OrgStore = org.NewStore(store.Pool())
		s.UserStore = users.NewStore(store.Pool())
		s.KeyStore = apikeys.NewStore(store.Pool())
		s.Credentials = credentials.New(store.Pool(), key)
		s.DeliveryTargets = delivery.New(store.Pool(), key)
		s.MCPRegistry = mcpregistry.New(store.Pool(), key)
		s.Runtimes = runtimes.NewStore(store.Pool())
		s.Skills = skills.New(store.Pool())
		s.Projects = projects.New(store.Pool(), projects.RealClock{})
		s.Conversations = conversation.New(store.Pool(), conversation.RealClock{})
		s.Deliverables = deliverable.New(store.Pool())
		s.Tasks = taskqueue.New(store.Pool(), nil, 60*time.Second)
		s.Fanout = fanout.New(store.Pool(), fanout.RealClock{})
		s.ConfigureTeamRunWorkers()
		_, err = s.Tasks.RecoverStale(ctx)
		must(err)
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_, err := s.Tasks.RecoverStale(ctx)
					if err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
				}
			}
		}()
		write(filepath.Join(cfg.Root, "stage-d/api-process.json"), map[string]any{"pid": os.Getpid(), "started_at": time.Now().UTC()})
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.Echo.Shutdown(shutdown)
		}()
		err = s.Start()
		if err != nil && err != http.ErrServerClosed {
			must(err)
		}
	default:
		panic("unknown mode")
	}
}
func descriptors() *compiler.DescriptorRegistry {
	r := compiler.NewDescriptorRegistry()
	for _, d := range []compiler.GraphFactoryDescriptor{compiler.NewStandardFrozenDescriptor(), compiler.NewStandardFrozenToolsDescriptor()} {
		must(r.Register(d))
	}
	return r
}
