package kernelbindings

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type workspaceMirrorSource struct{}

func (workspaceMirrorSource) SystemProvider(id string) (llmrouter.ProviderConfig, bool) {
	return llmrouter.ProviderConfig{ID: id, Name: "Configured model", BaseURL: "https://models.invalid/v1", APIKey: "fixture-secret", Models: []string{"model"}}, true
}

// Resource stores cannot create a product workspace as a side effect. The
// normal Server account creation establishes it before resource admission.
func TestKernelResourcesNeverCreateProductWorkspaceRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	prepared, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	pool = prepared
	key := []byte(strings.Repeat("k", 32))
	provider := credentials.New(pool, key)
	cases := []struct {
		name  string
		write func(context.Context, string, string) error
	}{
		{"dispatch", func(ctx context.Context, ws, user string) error {
			return taskqueue.New(pool, nil, time.Minute).RecordDispatch(ctx, ws, "lead", "worker", "input", "result", true)
		}},
		{"fanout", func(ctx context.Context, ws, user string) error {
			_, err := fanout.New(pool, nil).CreateGroup(ctx, fanout.Group{WorkspaceID: ws, UserID: user, AvatarAgent: "lead", OriginalRequest: "input"})
			return err
		}},
		{"provider", func(ctx context.Context, ws, user string) error {
			return provider.Upsert(ctx, ws, llmrouter.ProviderConfig{ID: "model", Name: "Model", BaseURL: "https://models.invalid/v1", APIKey: "fixture-secret", Models: []string{"model"}, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: user})
		}},
		{"embedder", func(ctx context.Context, ws, user string) error {
			return provider.UpsertEmbedder(ctx, ws, credentials.EmbedderConfig{BaseURL: "https://models.invalid/v1", APIKey: "fixture-secret", Model: "embedding", Dimension: 8})
		}},
		{"mirror", func(ctx context.Context, ws, user string) error {
			ctx = execution.WithSubject(ctx, execution.Subject{WorkspaceID: ws, ServiceID: "system-provider:fixture"})
			_, err := provider.MirrorSystemProvider(ctx, ws, user, "fixture", workspaceMirrorSource{})
			return err
		}},
		{"mcp", func(ctx context.Context, ws, user string) error {
			_, err := mcpregistry.New(pool, key).Create(ctx, ws, user, mcpregistry.UpsertServerRequest{Slug: "tools", DisplayName: "Tools", Transport: mcpregistry.TransportStreamableHTTP, URL: "https://tools.invalid/mcp", Enabled: true})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := "ownership-" + tc.name
			ctx := execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: ws, UserID: "unprovisioned"})
			err := tc.write(ctx, ws, "unprovisioned")
			var foreignKey *pgconn.PgError
			if err != nil && (!errors.As(err, &foreignKey) || foreignKey.Code != "23503") {
				t.Fatalf("unexpected resource error: %v", err)
			}
			if tc.name != "dispatch" && err == nil {
				t.Fatal("product foreign key was not enforced")
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_workspaces WHERE id=$1`, ws).Scan(&count); err != nil || count != 0 {
				t.Fatalf("resource created product workspace: count=%d err=%v", count, err)
			}
			user, err := users.NewStore(pool).Create(t.Context(), ws, "owner", "test-password", "Owner", "admin")
			if err != nil {
				t.Fatal(err)
			}
			ctx = execution.WithSubject(t.Context(), execution.Subject{WorkspaceID: ws, UserID: user.ID})
			if err := tc.write(ctx, ws, user.ID); err != nil {
				t.Fatalf("resource failed after product account creation: %v", err)
			}
		})
	}
}
