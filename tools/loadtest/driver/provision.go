package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// provisioned is everything the scenarios need to call Weave.
type provisioned struct {
	TeamID       string            `json:"team_id"`
	Workflows    map[string]string `json:"workflows"`
	Capabilities map[string]string `json:"capabilities"` // flow -> capability id
	Revisions    map[string]int64  `json:"revisions"`
	AppID        string            `json:"app_id"`
	credential   string
}

func hashUUID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func newUUID() string { return hashUUID(randomHex(16), time.Now().String()) }

type provisionOptions struct {
	providerID   string
	seatModel    string
	rulerModel   string
	consultModel string
	maxActive    int
}

func provision(ctx context.Context, w *weaveInstance, o provisionOptions) (*provisioned, error) {
	c := newAPIClient(w.baseURL, 8)
	if err := c.signIn(ctx, w.loginKey); err != nil {
		return nil, fmt.Errorf("sign in: %w", err)
	}

	// Provider: the env-registered system provider points at the model stub;
	// mirror it into the workspace so members can bind its models.
	var providers []struct {
		ID       string   `json:"id"`
		Models   []string `json:"models"`
		Mirrored bool     `json:"mirrored"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/providers/system", nil, &providers); err != nil {
		return nil, err
	}
	idx := slices.IndexFunc(providers, func(p struct {
		ID       string   `json:"id"`
		Models   []string `json:"models"`
		Mirrored bool     `json:"mirrored"`
	}) bool {
		return p.ID == o.providerID
	})
	if idx < 0 {
		return nil, fmt.Errorf("system provider %q not registered", o.providerID)
	}
	if !providers[idx].Mirrored {
		if _, err := c.do(ctx, http.MethodPost, "/v1/providers/system/"+o.providerID+"/mirror", map[string]string{"reason": "loadtest"}, nil, http.StatusOK, http.StatusCreated); err != nil {
			return nil, err
		}
	}
	var catalog struct {
		Models []string `json:"models"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/development/model-catalog", nil, &catalog); err != nil {
		return nil, err
	}
	for _, m := range []string{o.seatModel, o.rulerModel, o.consultModel} {
		if !slices.Contains(catalog.Models, m) {
			return nil, fmt.Errorf("model catalog %v lacks %s", catalog.Models, m)
		}
	}

	pkg, err := buildTeamPackage(o.seatModel, o.rulerModel, o.consultModel)
	if err != nil {
		return nil, err
	}
	var created struct {
		TeamID      string   `json:"team_id"`
		Adjustments []string `json:"adjustments"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/development/teams/import",
		map[string]any{"request_id": newUUID(), "package": json.RawMessage(pkg)}, &created, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}
	if len(created.Adjustments) > 0 {
		return nil, fmt.Errorf("import adjusted the package: %s", strings.Join(created.Adjustments, "; "))
	}
	res := &provisioned{TeamID: created.TeamID, Workflows: map[string]string{}, Capabilities: map[string]string{}, Revisions: map[string]int64{}}

	devPath := "/v1/teams/" + res.TeamID + "/development"
	type trialView struct {
		RequestID  string `json:"request_id"`
		WorkflowID string `json:"workflow_id"`
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
	}
	var dev struct {
		Revision int64 `json:"revision"`
		Document struct {
			Workflows []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"workflows"`
		} `json:"document"`
		Trials []trialView `json:"trials"`
	}
	if _, err := c.do(ctx, http.MethodGet, devPath, nil, &dev); err != nil {
		return nil, err
	}
	for _, wf := range dev.Document.Workflows {
		res.Workflows[wf.Name] = wf.ID
	}
	flows := []string{flowDecide, flowConsult}
	pending := map[string]string{}
	for _, name := range flows {
		if res.Workflows[name] == "" {
			return nil, fmt.Errorf("draft lacks workflow %s", name)
		}
		input := mustJSONString(map[string]string{"request_id": "trial-" + name, "probe": "ltp-trial" + name, "lane": "trial", "topic": "试运行"})
		trialID := newUUID()
		if _, err := c.do(ctx, http.MethodPost, devPath+"/trials", map[string]any{"revision": dev.Revision, "workflow_id": res.Workflows[name], "request_id": trialID, "input": input}, nil, http.StatusCreated); err != nil {
			return nil, fmt.Errorf("trial %s: %w", name, err)
		}
		pending[name] = trialID
	}
	deadline := time.Now().Add(5 * time.Minute)
	for len(pending) > 0 {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("trials did not finish: %v", pending)
		}
		time.Sleep(500 * time.Millisecond)
		if _, err := c.do(ctx, http.MethodGet, devPath, nil, &dev); err != nil {
			return nil, err
		}
		for name, id := range pending {
			for _, t := range dev.Trials {
				if t.RequestID != id {
					continue
				}
				switch t.Status {
				case "succeeded":
					delete(pending, name)
				case "failed", "cancelled", "abandoned":
					return nil, fmt.Errorf("trial %s %s (run %s)", name, t.Status, t.RunID)
				}
			}
		}
	}
	if _, err := c.do(ctx, http.MethodPost, devPath+"/publish", map[string]any{"revision": dev.Revision}, nil); err != nil {
		return nil, err
	}

	for _, name := range flows {
		var detail struct {
			Workflow struct {
				PublishedVersion *int `json:"published_version"`
			} `json:"workflow"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/v1/workflows/"+res.Workflows[name], nil, &detail); err != nil {
			return nil, err
		}
		if detail.Workflow.PublishedVersion == nil {
			return nil, fmt.Errorf("workflow %s not published", name)
		}
		capID := "loadtest-" + name
		outSchema := rulerOutputSchema
		if name == flowConsult {
			outSchema = seatOutputSchema
		}
		def := map[string]any{
			"schema_version": 1, "capability_id": capID, "name": "压测 " + name, "description": "执行面压测 " + name,
			"input_schema": inputSchema, "output_schema": outSchema,
			"workflow": map[string]any{"workflow_id": res.Workflows[name], "workflow_version": *detail.Workflow.PublishedVersion},
		}
		if _, err := c.do(ctx, http.MethodPost, "/v1/capabilities/drafts", map[string]any{"definition": def}, nil, http.StatusAccepted); err != nil {
			return nil, err
		}
		if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/capabilities/%s/versions/1/publish", capID), nil, nil, http.StatusCreated); err != nil {
			return nil, err
		}
		res.Capabilities[name], res.Revisions[name] = capID, 1
	}

	var app struct {
		ID string `json:"id"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/capability-apps/actions", map[string]any{"action": "create", "name": "loadtest"}, &app, http.StatusCreated); err != nil {
		return nil, err
	}
	res.AppID = app.ID
	var issued struct {
		Key string `json:"key"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/capability-apps/actions", map[string]any{"action": "issue", "app_id": app.ID, "name": "loadtest", "scopes": []string{"invoke", "read", "cancel"}}, &issued, http.StatusCreated); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(issued.Key, "wv_cap_") {
		return nil, errors.New("issued credential lacks wv_cap_ prefix")
	}
	res.credential = issued.Key
	for _, name := range flows {
		if _, err := c.do(ctx, http.MethodPost, "/v1/capability-apps/actions", map[string]any{"action": "grant", "app_id": app.ID, "capability_id": res.Capabilities[name], "revision": res.Revisions[name], "enabled": true}, nil, http.StatusNoContent); err != nil {
			return nil, err
		}
	}
	var quota map[string]any
	if _, err := c.do(ctx, http.MethodGet, "/v1/capability-quotas", nil, &quota); err != nil {
		return nil, err
	}
	quota["max_active_invocations"] = o.maxActive
	if _, err := c.do(ctx, http.MethodPut, "/v1/capability-quotas", quota, nil); err != nil {
		return nil, err
	}
	log.Printf("provisioned team %s, capabilities %v, app %s", res.TeamID, res.Capabilities, res.AppID)
	return res, nil
}
