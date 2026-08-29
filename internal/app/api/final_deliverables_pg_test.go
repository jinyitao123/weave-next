package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/labstack/echo/v4"
)

func TestDeliverableGetSelectsJSONContentByPathRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID, id := "deliverable-path-"+uuid.NewString(), "deliverable-1"
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1);
		INSERT INTO weave_final_deliverables(
			id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,
			run_snapshot_id,title,content,content_type,metadata
		) VALUES($2,$1,'user','lead','session','event','run','snapshot','chapters',
			'{"chapters":{"one":"first","two":"second"}}','application/json','{}')
	`, workspaceID, id); err != nil {
		t.Fatal(err)
	}
	server := &Server{Deliverables: deliverable.New(pool)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/v1/deliverables/"+id+"?path="+url.QueryEscape("/chapters/two"), nil)
	echoContext := echo.New().NewContext(request, recorder)
	echoContext.Set("tenant", workspaceID)
	echoContext.SetPath("/v1/deliverables/:id")
	echoContext.SetParamNames("id")
	echoContext.SetParamValues(id)
	if err := server.handleGetFinalDeliverable(echoContext); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `"second"` {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
