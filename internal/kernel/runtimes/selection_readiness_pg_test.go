package runtimes

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

func TestSelectSkipsEngineWithUnavailableAuthenticationRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws')`); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	runtimePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	store := NewStore(runtimePool)

	blocked, _, err := store.Create(ctx, "ws", "Blocked provider runtime")
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err := store.Create(ctx, "ws", "Ready subscription runtime")
	if err != nil {
		t.Fatal(err)
	}
	base := EngineCapability{Engine: engine.Codex, BinaryPath: "/fixture/codex", BinaryVersion: "fixture", ProtocolVersion: "1", EndpointClass: "fixture"}
	blockedCapability := base
	blockedCapability.AuthMode = AuthModeProvider
	blockedCapability.Availability = EngineAvailabilityUnavailable
	blockedCapability.UnavailableReason = "provider_credentials_missing"
	if err := store.HelloWithCapabilities(ctx, "ws", blocked.ID, []string{engine.Codex}, []EngineCapability{blockedCapability}, 1); err != nil {
		t.Fatal(err)
	}
	readyCapability := base
	readyCapability.AuthMode = AuthModeChatGPT
	readyCapability.Availability = EngineAvailabilityReady
	if err := store.HelloWithCapabilities(ctx, "ws", ready.ID, []string{engine.Codex}, []EngineCapability{readyCapability}, 1); err != nil {
		t.Fatal(err)
	}

	assignment, err := store.Select(ctx, "ws", engine.Codex, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.RuntimeID != ready.ID {
		t.Fatalf("selected runtime = %q, want %q", assignment.RuntimeID, ready.ID)
	}
	foundBlocked := false
	for _, fact := range assignment.CapabilityFacts {
		if fact.RuntimeID == blocked.ID {
			foundBlocked = true
			if fact.Eligible || fact.UnavailableReason != "provider_credentials_missing" || fact.EngineAvailability != EngineAvailabilityUnavailable {
				t.Fatalf("blocked fact = %#v", fact)
			}
		}
	}
	if !foundBlocked {
		t.Fatal("blocked runtime fact missing")
	}
}
