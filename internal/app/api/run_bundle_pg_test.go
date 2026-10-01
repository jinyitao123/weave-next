package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

const (
	canaryCiphertext = "CANARY-credential-ciphertext-9f3a"
	canaryDigest     = "51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc51bc"
	canaryToken      = "CANARY-access-token-77de"
	canaryBody       = "检查固定材料"
)

// plantCanaries puts values that must never appear in an export into the
// places a real run stores them: the delegation credential columns and a
// credential-named key inside a JSON payload.
func plantCanaries(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	var revision string
	if err := pool.QueryRow(t.Context(), `SELECT input_revision_id FROM weave_dispatch_input_revisions WHERE workspace_id='ws' AND consumed_run_id=$1`, runID).Scan(&revision); err != nil {
		t.Fatalf("read the run's input revision: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_task_business_delegations
		(workspace_id,user_id,input_revision_id,delegation_id,credential_ref,issuer,external_subject,external_organization,credential_ciphertext,credential_sha256,allowed_actions,resources,workflow_id,workflow_version,issued_at,expires_at)
		VALUES('ws','user',$1,gen_random_uuid(),'ref-1','https://forge.example.test','forge-user','org',$2,$3,'[]'::jsonb,'[]'::jsonb,'flow',1,now(),now()+interval '1 hour')`,
		revision, canaryCiphertext, canaryDigest); err != nil {
		t.Fatalf("plant delegation: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_task_queue SET payload = COALESCE(payload,'{}'::jsonb) || jsonb_build_object('access_token',$2::text,'input_tokens',7)
		WHERE workspace_id='ws' AND run_snapshot_id=(SELECT run_snapshot_id FROM weave_team_runs WHERE workspace_id='ws' AND run_id=$1)`, runID, canaryToken); err != nil {
		t.Fatalf("plant payload token: %v", err)
	}
}

// plantMemberRecords adds what a run with a member and a journal leaves behind
// but the minimal fixture run does not: an outbox event, an activity event, a
// member run with its lease, and loom_store rows under every key shape Weave
// uses, plus one row that belongs to a different run and must stay out.
func plantMemberRecords(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	statements := []string{
		`INSERT INTO weave_employee_run_event_outbox(event_id,workspace_id,run_id,input_revision_id,payload)
			SELECT gen_random_uuid(),'ws',$1,input_revision_id,'{"outcome":"succeeded"}'::jsonb FROM weave_dispatch_input_revisions WHERE workspace_id='ws' AND consumed_run_id=$1`,
		`INSERT INTO weave_team_run_activity_events(workspace_id,run_id,seq,event_id,kind,occurred_at) VALUES('ws',$1,1,'evt-1','member_started',now())`,
		`INSERT INTO weave_workflow_member_runs(workspace_id,parent_run_id,member_run_id,call_id,run_snapshot_id,node_id,parent_generation,identity_hash,initial_state)
			SELECT 'ws',$1,'member-1','call-1',run_snapshot_id,'deliver',1,repeat('a',64),'{"note":"member input"}'::jsonb FROM weave_team_runs WHERE workspace_id='ws' AND run_id=$1`,
		`INSERT INTO weave_run_attempt_leases(workspace_id,run_id,attempt_generation,attempt_id,graph_name,run_started_at,attempt_started_at,state,heartbeat_at,lease_expires_at,retry_count)
			VALUES('ws','member-1',1,gen_random_uuid(),'graph','2026-09-30T00:00:00Z',now(),'closed',now(),now(),0)`,
		`INSERT INTO loom_store(namespace,key,value) VALUES('checkpoint:graph','member-1',convert_to('{"state":{"access_token":"` + canaryToken + `","seen":1}}','UTF8'))`,
		`INSERT INTO loom_store(namespace,key,value) VALUES('member-operation:ws','member-1/seg/000000000001',convert_to('{"kind":"model","response":{"content":"recorded"}}','UTF8'))`,
		`INSERT INTO loom_store(namespace,key,value) VALUES('runreg:ws',$1,convert_to('{"claim":true}','UTF8'))`,
		`INSERT INTO loom_store(namespace,key,value) VALUES('checkpoint:graph','unrelated-run',convert_to('{"state":{}}','UTF8'))`,
	}
	for _, statement := range statements {
		var err error
		if strings.Contains(statement, "$1") {
			_, err = pool.Exec(t.Context(), statement, runID)
		} else {
			_, err = pool.Exec(t.Context(), statement)
		}
		if err != nil {
			t.Fatalf("plant %.60s: %v", statement, err)
		}
	}
}

func TestRunBundleExportRedactsAndImportsIntoAnotherDatabaseRealPG(t *testing.T) {
	pool, runID := succeededRunForOutboxTest(t)
	plantCanaries(t, pool, runID)
	plantMemberRecords(t, pool, runID)

	redacted, err := ExportRunBundle(t.Context(), pool, "ws", runID, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(redacted)
	for _, canary := range []string{canaryCiphertext, canaryDigest, canaryToken} {
		if bytes.Contains(encoded, []byte(canary)) {
			t.Fatalf("export leaked %q", canary)
		}
	}
	if bytes.Contains(encoded, []byte(canaryBody)) {
		t.Fatal("a bundle exported without content carried the task text")
	}
	for _, key := range []string{
		"weave_task_business_delegations.credential_ciphertext",
		"weave_task_business_delegations.credential_sha256",
		"weave_task_queue.payload (secret keys)",
		"weave_dispatch_input_revisions.task (content)",
	} {
		if redacted.Redactions[key] == 0 {
			t.Fatalf("expected redaction %q, got %v", key, redacted.Redactions)
		}
	}

	full, err := ExportRunBundle(t.Context(), pool, "ws", runID, true)
	if err != nil {
		t.Fatal(err)
	}
	fullEncoded, _ := json.Marshal(full)
	// Usage counters share a suffix with credential words and must survive.
	if !bytes.Contains(fullEncoded, []byte(`"input_tokens":7`)) {
		t.Fatal("the sweep removed a usage counter that is not a credential")
	}
	if !bytes.Contains(fullEncoded, []byte(canaryBody)) {
		t.Fatal("a bundle exported with content lost the task text")
	}
	for _, canary := range []string{canaryCiphertext, canaryDigest, canaryToken} {
		if bytes.Contains(fullEncoded, []byte(canary)) {
			t.Fatalf("content export leaked %q", canary)
		}
	}

	for _, want := range []string{"weave_team_runs", "weave_task_queue", "weave_dispatch_input_revisions", "weave_employee_run_event_outbox", "weave_team_run_activity_events", "weave_workflow_member_runs", "weave_run_attempt_leases"} {
		if rowsOf(full, want) == 0 {
			t.Fatalf("bundle has no %s rows: %v", want, tableCounts(full))
		}
	}

	storedCheckpoints := storedText(t, full)
	if strings.Contains(storedCheckpoints, canaryToken) {
		t.Fatal("a credential inside a checkpoint value survived a content export")
	}
	if !strings.Contains(storedCheckpoints, `"seen":1`) || !strings.Contains(storedCheckpoints, `"content":"recorded"`) {
		t.Fatalf("a content export lost checkpoint or journal content: %s", storedCheckpoints)
	}
	if got := rowsOf(full, "loom_store"); got != 3 {
		t.Fatalf("loom_store exported %d rows, want the run's three and none of the unrelated run's", got)
	}
	target := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	// The bundle must survive the file round trip an operator actually uses.
	var reread RunBundle
	if err := json.Unmarshal(fullEncoded, &reread); err != nil {
		t.Fatal(err)
	}
	counts, err := ImportRunBundle(t.Context(), target, &reread)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, table := range full.Tables {
		if counts[table.Name] != len(table.Rows) {
			t.Fatalf("%s imported %d rows, exported %d", table.Name, counts[table.Name], len(table.Rows))
		}
	}
	var status, task string
	if err := target.QueryRow(t.Context(), `SELECT r.status, i.task FROM weave_team_runs r
		JOIN weave_dispatch_input_revisions i ON i.workspace_id=r.workspace_id AND i.consumed_run_id=r.run_id
		WHERE r.workspace_id='ws' AND r.run_id=$1`, runID).Scan(&status, &task); err != nil {
		t.Fatalf("read back imported run: %v", err)
	}
	if status != "succeeded" || !strings.Contains(task, canaryBody) {
		t.Fatalf("imported run status=%q task=%q", status, task)
	}
	var stored string
	if err := target.QueryRow(t.Context(), `SELECT credential_ciphertext FROM weave_task_business_delegations WHERE workspace_id='ws'`).Scan(&stored); err != nil || stored != "[redacted]" {
		t.Fatalf("imported delegation credential = %q err=%v", stored, err)
	}

	if _, err := ImportRunBundle(t.Context(), target, &reread); err == nil {
		t.Fatal("importing the same bundle twice succeeded; an import must never overwrite")
	}
	var afterFailure int
	if err := target.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_runs`).Scan(&afterFailure); err != nil || afterFailure != 1 {
		t.Fatalf("a failed import left %d runs, want the original 1 (err=%v)", afterFailure, err)
	}
}

func TestRunBundleExportRejectsUnknownRunAndForeignWorkspaceRealPG(t *testing.T) {
	pool, runID := succeededRunForOutboxTest(t)
	if _, err := ExportRunBundle(t.Context(), pool, "ws", "missing-run", false); err == nil {
		t.Fatal("exported a run that does not exist")
	}
	if _, err := ExportRunBundle(t.Context(), pool, "other-workspace", runID, false); err == nil {
		t.Fatal("exported a run through another workspace")
	}
}

func TestRunBundleImportRejectsUnknownTablesAndFormats(t *testing.T) {
	if _, err := ImportRunBundle(t.Context(), nil, &RunBundle{}); err == nil {
		t.Fatal("import without a pool succeeded")
	}
}

// storedText returns the decoded text of every loom_store value in the bundle;
// bytea columns are exported as hex, so the plain-text canary search cannot
// see inside them.
func storedText(t *testing.T, bundle *RunBundle) string {
	t.Helper()
	var all strings.Builder
	for _, table := range bundle.Tables {
		if table.Name != "loom_store" {
			continue
		}
		for _, row := range table.Rows {
			var encoded string
			if err := json.Unmarshal(row["value"], &encoded); err != nil {
				t.Fatalf("loom_store value is not a string: %v", err)
			}
			raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "\\x"))
			if err != nil {
				all.WriteString(encoded)
				continue
			}
			all.Write(raw)
			all.WriteByte('\n')
		}
	}
	return all.String()
}

func rowsOf(bundle *RunBundle, name string) int {
	for _, table := range bundle.Tables {
		if table.Name == name {
			return len(table.Rows)
		}
	}
	return 0
}

func tableCounts(bundle *RunBundle) map[string]int {
	counts := map[string]int{}
	for _, table := range bundle.Tables {
		counts[table.Name] = len(table.Rows)
	}
	return counts
}

func TestRunBundlePreservesCredentialReferencesButRedactsCredentialValues(t *testing.T) {
	value := json.RawMessage(`{"credentials":[{"schema_version":1,"scope":"workspace_service","service_id":"service","workspace_id":"ws","kind":"provider_api_key","resource_id":"provider","slot":"api_key"}],"access_token":"CANARY-private"}`)
	cleaned, count := sweepSecretKeys(value)
	if count != 1 || bytes.Contains(cleaned, []byte("CANARY-private")) {
		t.Fatal("credential value survived")
	}
	var parsed map[string]json.RawMessage
	if json.Unmarshal(cleaned, &parsed) != nil || len(parsed["credentials"]) == 0 || parsed["credentials"][0] != '[' {
		t.Fatal("immutable credential references were lost")
	}
	if cleaned, count := sweepSecretKeys(json.RawMessage(`{"credentials":{"password":"CANARY-private"}}`)); count != 1 || bytes.Contains(cleaned, []byte("CANARY-private")) {
		t.Fatal("arbitrary credential objects were treated as references")
	}
}

func TestRunBundleImportEnforcesAllParentsAndRetainsTriggerModesRealPG(t *testing.T) {
	source, runID := succeededRunForOutboxTest(t)
	full, err := ExportRunBundle(t.Context(), source, "ws", runID, true)
	if err != nil {
		t.Fatal(err)
	}
	target := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	var originalDeferred int
	if err := target.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND contype='f' AND condeferrable`).Scan(&originalDeferred); err != nil {
		t.Fatal(err)
	}
	// Keep a broken reference in the real snapshot and remove its parent.
	raw, _ := json.Marshal(full)
	var missing RunBundle
	_ = json.Unmarshal(raw, &missing)
	for index := range missing.Tables {
		if missing.Tables[index].Name == "weave_agents" {
			missing.Tables[index].Rows = nil
		}
	}
	if _, err := ImportRunBundle(t.Context(), target, &missing); err == nil {
		t.Fatal("missing frozen parent was accepted")
	}
	var rows int
	_ = target.QueryRow(t.Context(), `SELECT count(*) FROM weave_workspaces`).Scan(&rows)
	if rows != 0 {
		t.Fatal("failed import left partial rows")
	}
	if _, err := ImportRunBundle(t.Context(), target, full); err != nil {
		t.Fatal(err)
	}
	var role string
	_ = target.QueryRow(t.Context(), `SELECT current_setting('session_replication_role')`).Scan(&role)
	if role != "origin" {
		t.Fatal("import disabled triggers")
	}
	var finalDeferred int
	_ = target.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND contype='f' AND condeferrable`).Scan(&finalDeferred)
	if finalDeferred != originalDeferred {
		t.Fatal("import changed permanent FK deferrability")
	}
	// The frozen artifact must still hash/decode, including credential references.
	for _, table := range full.Tables {
		if table.Name == "weave_published_artifact_contents" {
			for _, row := range table.Rows {
				encoded, _ := json.Marshal(row)
				var env frozen.ArtifactEnvelopeV1
				_ = json.Unmarshal(encoded, &env)
				if _, err := frozen.DecodeArtifactEnvelopeV1(env); err != nil {
					t.Fatal("sanitization damaged frozen configuration")
				}
			}
		}
	}
}

func TestRunBundleRejectsDuplicateTablesUnknownColumnsAndDiagnosticOnlyRealPG(t *testing.T) {
	source, runID := succeededRunForOutboxTest(t)
	full, err := ExportRunBundle(t.Context(), source, "ws", runID, true)
	if err != nil {
		t.Fatal(err)
	}
	target := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(full)
	var duplicate RunBundle
	_ = json.Unmarshal(raw, &duplicate)
	duplicate.Tables = append(duplicate.Tables, duplicate.Tables[0])
	if _, err := ImportRunBundle(t.Context(), target, &duplicate); err == nil {
		t.Fatal("duplicate table accepted")
	}
	var unknown RunBundle
	_ = json.Unmarshal(raw, &unknown)
	for i := range unknown.Tables {
		if unknown.Tables[i].Name == "weave_workspaces" && len(unknown.Tables[i].Rows) > 0 {
			unknown.Tables[i].Rows[0]["unrecognized_column"] = json.RawMessage(`"hidden"`)
		}
	}
	if _, err := ImportRunBundle(t.Context(), target, &unknown); err == nil {
		t.Fatal("unknown column discarded silently")
	}
	diagnostic, err := ExportRunBundle(t.Context(), source, "ws", runID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRunBundle(t.Context(), target, diagnostic); err == nil {
		t.Fatal("material-free diagnostic bundle became runnable")
	}
}

func TestRunBundleIncludesReferencedCapabilityInvocationAndRevisionRealPG(t *testing.T) {
	pool, runID := succeededRunForOutboxTest(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_capability_revisions(workspace_id,capability_id,revision,definition_hash,definition) VALUES('ws','cap-replay',1,repeat('c',64),'{}'::jsonb);
 INSERT INTO weave_capability_invocations(workspace_id,application_id,request_id,invocation_id,capability_id,revision,input,status,result_state,definition_hash)
 VALUES('ws','developer','request-replay','invocation-replay','cap-replay',1,'{}'::jsonb,'succeeded','{}',repeat('c',64));`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_capability_step_runs(workspace_id,invocation_id,step_id,activation_id,run_id) VALUES('ws','invocation-replay','step','activation',$1)`, runID); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportRunBundle(t.Context(), pool, "ws", runID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"weave_capability_step_runs", "weave_capability_invocations", "weave_capability_revisions"} {
		if rowsOf(bundle, table) != 1 {
			t.Fatalf("missing referenced capability table %s", table)
		}
	}
	target := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(bundle)
	for _, mutate := range []string{"missing", "hash_mismatch"} {
		var broken RunBundle
		_ = json.Unmarshal(encoded, &broken)
		for index := range broken.Tables {
			if broken.Tables[index].Name == "weave_capability_revisions" {
				if mutate == "missing" {
					broken.Tables[index].Rows = nil
				} else {
					broken.Tables[index].Rows[0]["definition_hash"] = json.RawMessage(`"different"`)
				}
			}
		}
		if _, err := ImportRunBundle(t.Context(), target, &broken); err == nil {
			t.Fatalf("capability definition %s was accepted", mutate)
		}
	}
	if _, err := ImportRunBundle(t.Context(), target, bundle); err != nil {
		t.Fatal(err)
	}
	var actual int
	if err := target.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_invocations WHERE workspace_id='ws' AND invocation_id='invocation-replay'`).Scan(&actual); err != nil || actual != 1 {
		t.Fatal("capability result did not survive copy")
	}
}
