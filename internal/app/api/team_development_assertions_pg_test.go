package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/labstack/echo/v4"
)

// The successful trial fixture must still be refused by the real publish
// handler when its draft promises a check loop the graph does not contain.
func assertUnprovenGuaranteeBlocksPublication(t *testing.T, ctx context.Context, pool *pgxpool.Pool, envelope frozen.ArtifactEnvelopeV1) {
	t.Helper()
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	document := developmentDocument{Name: "Readiness fixture", Workflows: []developmentWorkflow{{ID: envelope.WorkflowID, Name: "Checked workflow", Graph: payload.GraphDefinition,
		Guarantees: []developmentGuarantee{{Type: guaranteeChecked, Step: "compute"}}}}}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$1 WHERE workspace_id='ws' AND team_id='team'`, encodeDevelopment(document)); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/teams/team/development/publish", strings.NewReader(`{"revision":1}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	c := echo.New().NewContext(request, recorder)
	c.SetParamNames("id")
	c.SetParamValues("team")
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	err = (&Server{Pool: pool}).handlePublishTeamDevelopment(c)
	if err == nil || !strings.Contains(err.Error(), "流程保证不成立") {
		t.Fatalf("successful trial bypassed an unproven guarantee: %v", err)
	}
	var publishing int64
	if err := pool.QueryRow(ctx, `SELECT publishing_revision FROM weave_team_development_drafts WHERE workspace_id='ws' AND team_id='team'`).Scan(&publishing); err != nil || publishing != 0 {
		t.Fatalf("refused draft entered publication: revision=%d err=%v", publishing, err)
	}
}
