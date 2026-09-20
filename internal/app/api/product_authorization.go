package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ProductAuthorizationPrincipal struct {
	ID             string
	OrganizationID string
	Roles          []string
}

type ProductAuthorizationResource struct {
	Kind       string
	ID         string
	Attributes map[string]any
}

type ProductAuthorizationRequest struct {
	Principal ProductAuthorizationPrincipal
	Actions   []string
	Resource  ProductAuthorizationResource
}

type ProductAuthorizationResult struct {
	Allowed       map[string]bool
	PolicyVersion string
}

type ProductAuthorizer interface {
	Check(context.Context, ProductAuthorizationRequest) (ProductAuthorizationResult, error)
}

type CerbosProductAuthorizer struct {
	endpoint *url.URL
	client   *http.Client
}

func NewCerbosProductAuthorizer(rawURL string, client *http.Client) *CerbosProductAuthorizer {
	endpoint, _ := url.Parse(strings.TrimRight(rawURL, "/") + "/api/check/resources")
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &CerbosProductAuthorizer{endpoint: endpoint, client: client}
}

func (a *CerbosProductAuthorizer) Check(ctx context.Context, request ProductAuthorizationRequest) (ProductAuthorizationResult, error) {
	if a == nil || a.endpoint == nil || request.Principal.ID == "" || request.Principal.OrganizationID == "" || len(request.Actions) == 0 {
		return ProductAuthorizationResult{}, errors.New("invalid product authorization request")
	}
	payload := map[string]any{
		"requestId": fmt.Sprintf("weave-%d", time.Now().UnixNano()),
		"principal": map[string]any{
			"id":    request.Principal.ID,
			"roles": request.Principal.Roles,
			"attr":  map[string]any{"organizationId": request.Principal.OrganizationID},
		},
		"resources": []any{map[string]any{
			"actions":  request.Actions,
			"resource": map[string]any{"kind": request.Resource.Kind, "id": request.Resource.ID, "attr": request.Resource.Attributes},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ProductAuthorizationResult{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return ProductAuthorizationResult{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return ProductAuthorizationResult{}, fmt.Errorf("check Cerbos: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return ProductAuthorizationResult{}, fmt.Errorf("check Cerbos: status %d", response.StatusCode)
	}
	var decoded struct {
		Results []struct {
			Actions map[string]string `json:"actions"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil || len(decoded.Results) != 1 {
		return ProductAuthorizationResult{}, errors.New("invalid Cerbos response")
	}
	result := ProductAuthorizationResult{Allowed: make(map[string]bool, len(request.Actions)), PolicyVersion: "default"}
	for _, action := range request.Actions {
		result.Allowed[action] = decoded.Results[0].Actions[action] == "EFFECT_ALLOW"
	}
	return result, nil
}
