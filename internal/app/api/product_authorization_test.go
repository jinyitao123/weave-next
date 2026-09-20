package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCerbosProductAuthorizerSendsBoundedPrincipalAndReadsDecisions(t *testing.T) {
	pdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/check/resources" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Principal struct {
				ID    string         `json:"id"`
				Roles []string       `json:"roles"`
				Attr  map[string]any `json:"attr"`
			} `json:"principal"`
			Resources []struct {
				Actions  []string `json:"actions"`
				Resource struct {
					Kind string         `json:"kind"`
					ID   string         `json:"id"`
					Attr map[string]any `json:"attr"`
				} `json:"resource"`
			} `json:"resources"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Principal.ID != "user-1" || body.Principal.Attr["organizationId"] != "org-1" || !hasString(body.Principal.Roles, "developer") {
			t.Fatalf("principal = %+v", body.Principal)
		}
		if len(body.Resources) != 1 || body.Resources[0].Resource.Kind != "weave:product" || body.Resources[0].Resource.Attr["environment"] != "sandbox" {
			t.Fatalf("resources = %+v", body.Resources)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"resource":{"id":"org-1","kind":"weave:product"},"actions":{"team.read":"EFFECT_ALLOW","release.publish":"EFFECT_DENY"}}]}`))
	}))
	defer pdp.Close()

	authorizer := NewCerbosProductAuthorizer(pdp.URL, pdp.Client())
	result, err := authorizer.Check(t.Context(), ProductAuthorizationRequest{
		Principal: ProductAuthorizationPrincipal{ID: "user-1", OrganizationID: "org-1", Roles: []string{"employee", "developer"}},
		Actions:   []string{"team.read", "release.publish"},
		Resource:  ProductAuthorizationResource{Kind: "weave:product", ID: "org-1", Attributes: map[string]any{"environment": "sandbox"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Allowed["team.read"] || result.Allowed["release.publish"] {
		t.Fatalf("decisions = %+v", result.Allowed)
	}
}
