package db

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestMergedMigrationsFreshAndExistingDatabase(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "already_applied_through_0164"
		}
		t.Run(name, func(t *testing.T) {
			pool := testutil.PostgresPool(t)
			ctx := context.Background()
			if existing {
				files := fstest.MapFS{}
				paths, err := migrationFiles(migrations)
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					if path >= "migrations/0165_" {
						continue
					}
					data, err := fs.ReadFile(migrations, path)
					if err != nil {
						t.Fatal(err)
					}
					files[path] = &fstest.MapFile{Data: data}
				}
				if err := migrateFS(ctx, pool, files); err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			// Both branches' tables must exist; retaining a duplicate version would
			// otherwise make one branch disappear from the version-only ledger.
			for _, table := range []string{"weave_capability_definitions", "weave_capability_invocations", "weave_game_decision_bindings", "weave_game_decision_admissions", "weave_game_decision_cancellations"} {
				var exists bool
				if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
					t.Fatalf("missing %s: %v", table, err)
				}
			}
			var appliedBefore, appliedAfter int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_schema_migrations`).Scan(&appliedBefore); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_schema_migrations`).Scan(&appliedAfter); err != nil {
				t.Fatal(err)
			}
			if appliedBefore != appliedAfter {
				t.Fatalf("migration replay changed ledger: %d -> %d", appliedBefore, appliedAfter)
			}
		})
	}
}

func TestNativeTaskAuthorityUpgradeFromPublished0173RealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := t.Context()
	files := fstest.MapFS{}
	paths, err := migrationFiles(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path >= "migrations/0176_" {
			continue
		}
		data, err := fs.ReadFile(migrations, path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = &fstest.MapFile{Data: data}
	}
	if err := migrateFS(ctx, pool, files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES('legacy-ws','legacy-ws','Legacy');
		INSERT INTO weave_users(id,tenant_id,username,password,role)
		  VALUES('legacy-user','legacy-ws','legacy-user','!','member');
		INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		  VALUES('http://old-forge.example','native-user','legacy-ws','legacy-user');
		INSERT INTO weave_dispatch_input_revisions(workspace_id,user_id,workbench_session_id,input_revision_id,
		  registration_id,registration_sha256,source_messages,task,task_sha256,team_id,mode,workflow_id,
		  workflow_version,client_request_id,execution_task,revision_kind,root_input_revision_id)
		  VALUES('legacy-ws','legacy-user','legacy-session','legacy-input','legacy-registration',repeat('a',64),
		    '[{"id":"message"}]','legacy task',repeat('b',64),'legacy-team','workflow','legacy-flow',1,
		    'legacy-request','legacy task','initial','legacy-input');
		INSERT INTO weave_task_business_delegations(workspace_id,user_id,input_revision_id,delegation_id,
		  credential_ref,issuer,external_subject,external_organization,credential_ciphertext,credential_sha256,
		  allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at,forge_base_url,forge_delegation_id,
		  revoked_at,revocation_reason)
		  VALUES('legacy-ws','legacy-user','legacy-input','11111111-1111-1111-1111-111111111111','legacy-ref',
		    'http://old-forge.example','native-user','legacy-ws','legacy-encrypted-session',repeat('c',64),
		    '[]','[]','legacy-flow',1,now()-interval '1 hour',now()+interval '1 hour',
		    'http://old-forge.example','legacy-forge-id',now(),'expired')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var user, organization, ciphertext, digest, grant, reason, credentialRef string
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT identity.user_id,identity.native_organization,
		delegation.credential_ciphertext,delegation.credential_sha256,delegation.grant_id,
		delegation.revocation_reason,delegation.revoked_at IS NOT NULL,delegation.credential_ref
		FROM weave_external_identities identity JOIN weave_task_business_delegations delegation
		  ON delegation.workspace_id=identity.workspace_id AND delegation.user_id=identity.user_id
		WHERE identity.workspace_id='legacy-ws'`).Scan(&user, &organization, &ciphertext, &digest, &grant, &reason, &revoked, &credentialRef); err != nil {
		t.Fatal(err)
	}
	if user != "legacy-user" || organization != "" || ciphertext != "" || digest != strings.Repeat("0", 64) ||
		grant != "" || !revoked || reason != "expired" || credentialRef != "legacy-ref" {
		t.Fatalf("legacy account/audit or credential retirement differs: user=%s org=%s grant=%s revoked=%v reason=%s ref=%s",
			user, organization, grant, revoked, reason, credentialRef)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM weave_task_business_delegations WHERE workspace_id='legacy-ws'`); err == nil {
		t.Fatal("upgraded audit guard allowed deletion")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
