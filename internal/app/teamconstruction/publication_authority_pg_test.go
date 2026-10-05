package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/jinyitao123/weave/internal/kernel/skills"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestPublicationAuthorityRefreshesCurrentUserAndExactDependencyProofsRealPG(t *testing.T) {
	for _, scope := range []frozen.CredentialScope{frozen.CredentialScopeUser, frozen.CredentialScopeWorkspaceService} {
		t.Run(string(scope), func(t *testing.T) {
			pool := testutil.PostgresPool(t)
			ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "authority", UserID: "alice"})
			if err := db.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role)VALUES('alice','authority','alice','unused','admin'); INSERT INTO weave_workspaces(id,slug,name)VALUES('authority','authority','Authority')`); err != nil {
				t.Fatal(err)
			}
			key := []byte(strings.Repeat("k", 32))
			providers := credentials.New(pool, key)
			config := llmrouter.ProviderConfig{ID: "provider", Name: "Provider", BaseURL: "https://fixture.invalid/v1", APIKey: "fixture-secret", Models: []string{"fixture"}, CredentialScope: scope, CredentialUserID: "alice"}
			if scope == frozen.CredentialScopeWorkspaceService {
				config.CredentialUserID = ""
				config.CredentialServiceID = "provider:provider"
			}
			providerContext := ctx
			if scope == frozen.CredentialScopeWorkspaceService {
				providerContext = execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "authority", ServiceID: config.CredentialServiceID})
			}
			if err := providers.Upsert(providerContext, "authority", config); err != nil {
				t.Fatal(err)
			}
			agents := agentcatalog.New(pool)
			lead := &registry.AgentRecord{Name: "lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
			if err := agents.Put(ctx, "authority", lead); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)VALUES('team','authority','Team',$1,'building')`, lead.ID); err != nil {
				t.Fatal(err)
			}
			flows := workflowcatalog.New(pool, nil, workflow.NewArtifactStore(pool, nil))
			draft, err := flows.Create(ctx, &workflow.TeamWorkflow{ID: "flow", WorkspaceID: "authority", TeamID: "team", Name: "Flow"}, workflow.DraftInput{CreatedBy: "alice", TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`), GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"lead","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"lead","type":"lead","config":{"instruction":"Answer"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"lead","path":""}}}],"edges":[{"id":"done","from_node_id":"lead","to_node_id":"deliver","route":"success"}]}`)})
			if err != nil {
				t.Fatal(err)
			}
			descriptors := compiler.NewDescriptorRegistry()
			if err := descriptors.Register(compiler.NewStandardFrozenDescriptor()); err != nil {
				t.Fatal(err)
			}
			builder := workflowcatalog.NewCandidateBuilder(flows, agents, delivery.New(pool, key), skills.New(pool), providers, schedule.New(pool, nil), descriptors)
			authority := NewPublicationAuthority(pool, builder)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			candidate, report, err := authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: "authority", WorkflowID: "flow", WorkflowVersion: draft.Version})
			_ = tx.Rollback(ctx)
			if err != nil || candidate == nil || report == nil || len(report.Issues) > 0 {
				t.Fatalf("candidate err=%v report=%+v", err, report)
			}
			envelope, err := workflow.CandidateEnvelope(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = authority.Authorize(ctx, "publish", envelope); err != nil {
				t.Fatal(err)
			}
			var ref frozen.CredentialReference
			for _, bundle := range candidate.Payload.Bundles {
				for _, candidateRef := range bundle.Credentials {
					if candidateRef.Kind == frozen.CredentialProviderAPIKey {
						ref = candidateRef
					}
				}
			}
			if ref.ResourceID == "" {
				t.Fatal("fixture has no provider credential")
			}
			proof := publicationCredentialProof{actor: execution.Subject{WorkspaceID: "authority", UserID: "alice"}, scope: workflow.CandidateCredentialScope{WorkspaceID: "authority", TeamID: "team", Lead: machine.AgentVersionKey{AgentID: lead.ID, AgentVersion: int64(lead.Version)}, Agents: []machine.AgentVersionKey{{AgentID: lead.ID, AgentVersion: int64(lead.Version)}}}, references: []frozen.CredentialReference{ref}, expiresAt: time.Now().Add(time.Minute)}
			if err = authority.verifyCredentialProof(ctx, proof, ref); err != nil {
				t.Fatal("fresh scoped proof rejected", err)
			}
			expired := proof
			expired.expiresAt = time.Now().Add(-time.Second)
			if err = authority.verifyCredentialProof(ctx, expired, ref); err == nil {
				t.Fatal("expired process proof reused")
			}
			other := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "authority", UserID: "bob"})
			if err = authority.verifyCredentialProof(other, proof, ref); err == nil {
				t.Fatal("proof reused by another actor")
			}
			foreign := &registry.AgentRecord{Name: "foreign", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
			if err = agents.Put(ctx, "authority", foreign); err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)VALUES('foreign','authority','Other',$1,'active')`, foreign.ID); err != nil {
				t.Fatal(err)
			}
			crossTeam := proof
			crossTeam.scope.TeamID = "foreign"
			if err = authority.verifyCredentialProof(ctx, crossTeam, ref); err == nil {
				t.Fatal("team proof expanded to whole workspace")
			}
			if _, err = pool.Exec(ctx, `UPDATE weave_users SET disabled=true WHERE id='alice'`); err != nil {
				t.Fatal(err)
			}
			if _, err = authority.Authorize(ctx, "publish", envelope); err == nil {
				t.Fatal("revoked actor retained publication access")
			}
			if _, err = pool.Exec(ctx, `UPDATE weave_users SET disabled=false WHERE id='alice'`); err != nil {
				t.Fatal(err)
			}
			if _, err = authority.Authorize(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "authority", UserID: "bob"}), "candidate_run", envelope); err == nil {
				t.Fatal("unregistered actor acquired candidate")
			}
			// Exercise the complete adapter boundary using a real kernel-owned pool.
			connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			params := connection.Query()
			params.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
			connection.RawQuery = params.Encode()
			kernel, err := publicationservice.Open(ctx, connection.String(), authority)
			if err != nil {
				t.Fatal(err)
			}
			defer kernel.Close()
			product := NewProductPublication(pool, kernel, authority.AuthorizeProduct)
			command, err := PublicationCommandForCandidate("authority-publication", candidate, PublicationTarget{})
			if err != nil {
				t.Fatal(err)
			}
			tx, err = pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = product.ReserveTx(ctx, tx, command); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			_ = tx.Rollback(ctx)
			if _, found, err := product.Lookup(ctx, "authority", command.Request.RequestID); err != nil || found {
				t.Fatal("rolled back preparation left publication intent", err)
			}
			failActivation := true
			product.SetActivationEffect(func(context.Context, pgx.Tx, PublicationRequestRecord) error {
				if failActivation {
					return errors.New("activation interrupted")
				}
				return nil
			})
			if _, err = product.Publish(ctx, command); err == nil {
				t.Fatal("expected product activation interruption")
			}
			retained, found, err := product.Lookup(ctx, "authority", command.Request.RequestID)
			if err != nil || !found || retained.State != PublicationRevisionObtained {
				t.Fatal("kernel receipt lost after product failure", err)
			}
			version, err := flows.GetVersion(ctx, "authority", "flow", draft.Version)
			if err != nil || version.Status != "draft" {
				t.Fatal("product activation partially committed", err)
			}
			candidateTarget := CandidateTarget{BuildRunID: "authority-build", RoundNo: 1, SourceRole: "evaluation"}
			product.SetCandidateAssociation(func(context.Context, pgx.Tx, CandidateRequestRecord) error { return nil })
			candidateRequest := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "authority-trial", Candidate: envelope, Input: json.RawMessage(`{"input":"test"}`), InputVersion: "v1", SourceRef: "authority-build", Purpose: "evaluation"}
			trial, err := product.AdmitCandidate(ctx, candidateTarget, candidateRequest)
			if err != nil {
				t.Fatal(err)
			}
			// A kernel receipt cannot carry access across product interruption or
			// candidate recovery. Current authorization must be checked again.
			if _, err = pool.Exec(ctx, `UPDATE weave_provider_credentials SET enabled=false WHERE workspace_id='authority' AND id='provider'`); err != nil {
				t.Fatal(err)
			}
			if _, err = product.Publish(ctx, retained.Command); err == nil {
				t.Fatal("revoked provider activated a retained kernel revision")
			}
			if _, err = product.AdmitCandidate(ctx, candidateTarget, candidateRequest); err == nil {
				t.Fatal("revoked provider reused a retained candidate admission")
			}
			version, err = flows.GetVersion(ctx, "authority", "flow", draft.Version)
			if err != nil || version.Status != "draft" {
				t.Fatal("denied recovery changed product activation", err)
			}
			if _, err = pool.Exec(ctx, `UPDATE weave_provider_credentials SET enabled=true WHERE workspace_id='authority' AND id='provider'`); err != nil {
				t.Fatal(err)
			}
			replayedTrial, err := product.AdmitCandidate(ctx, candidateTarget, candidateRequest)
			if err != nil || replayedTrial.Receipt == nil || *replayedTrial.Receipt != *trial.Receipt {
				t.Fatal("reauthorized candidate recovery replaced its original task", err)
			}
			failActivation = false
			if _, err = product.Publish(ctx, retained.Command); err != nil {
				t.Fatal(err)
			}
			if _, err = product.Publish(ctx, retained.Command); err != nil {
				t.Fatal("completed operation did not replay", err)
			}
			version, err = flows.GetVersion(ctx, "authority", "flow", draft.Version)
			if err != nil || version.Status != "published" {
				t.Fatal("product catalog did not activate", err)
			}
			readConfig := pool.Config()
			readConfig.MaxConns = 1
			readPool, err := pgxpool.NewWithConfig(ctx, readConfig)
			if err != nil {
				t.Fatal(err)
			}
			readCtx, stopRead := context.WithTimeout(ctx, 3*time.Second)
			readCatalog := workflowcatalog.New(readPool, nil, workflow.NewArtifactStore(readPool, nil))
			view, err := readCatalog.GetVersionAdmissionView(readCtx, "authority", "flow", draft.Version)
			stopRead()
			readPool.Close()
			if err != nil || view.Blocked || view.Tightened {
				t.Fatal("product view held its transaction while reading frozen facts", err)
			}
			if _, err = pool.Exec(ctx, `UPDATE weave_provider_credentials SET enabled=false WHERE workspace_id='authority' AND id='provider'`); err != nil {
				t.Fatal(err)
			}
			if err = authority.verifyCredentialProof(ctx, proof, ref); err == nil {
				t.Fatal("old proof survived provider revocation")
			}
			if _, err = authority.Authorize(ctx, "candidate_run", envelope); err == nil {
				t.Fatal("revoked provider admitted another candidate")
			}
			if _, err = pool.Exec(ctx, `UPDATE weave_provider_credentials SET enabled=true WHERE workspace_id='authority' AND id='provider'`); err != nil {
				t.Fatal(err)
			}
			config.BaseURL = "https://changed.invalid/v1"
			if err = providers.Upsert(providerContext, "authority", config); err != nil {
				t.Fatal(err)
			}
			if _, err = authority.Authorize(ctx, "candidate_run", envelope); err == nil {
				t.Fatal("changed provider revision silently replaced frozen dependency")
			}
		})
	}
}
