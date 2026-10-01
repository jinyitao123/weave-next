package businessaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

const TaskDelegationPath = "/api/v1/apps/forge/task-delegations"

const TaskDelegationMaxLifetime = 24 * time.Hour

var stableTaskIdentityIssuerPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:\S{1,240}$`)

func validStableTaskIdentityIssuer(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || !stableTaskIdentityIssuerPattern.MatchString(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return false
	}
	return (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == ""
}

type TaskBusinessRecord struct {
	ObjectName string `json:"object_name"`
	RecordID   string `json:"record_id"`
}
type TaskDelegationScope struct {
	InputRevisionID string                   `json:"input_revision_id"`
	RegistrationID  string                   `json:"registration_id"`
	TaskSHA256      string                   `json:"task_sha256"`
	WorkflowID      string                   `json:"workflow_id"`
	WorkflowVersion int                      `json:"workflow_version"`
	AllowedActions  []string                 `json:"allowed_actions"`
	Resources       []TaskDelegationResource `json:"resources"`
	BusinessRecord  *TaskBusinessRecord      `json:"business_record,omitempty"`
}

type TaskDelegationResource struct {
	Type       string `json:"type"`
	SourceKind string `json:"sourceKind,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	MaterialID string `json:"materialId,omitempty"`
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256"`
	ObjectName string `json:"object_name,omitempty"`
}

type TaskDelegationGrant struct {
	Version        string `json:"version"`
	Active         bool   `json:"active"`
	TokenType      string `json:"token_type"`
	Issuer         string `json:"issuer"`
	IdentityIssuer string `json:"identity_issuer"`
	Subject        struct {
		ID             string `json:"id"`
		OrganizationID string `json:"organization_id"`
	} `json:"subject"`
	GrantID     string              `json:"grant_id"`
	Generation  int64               `json:"generation"`
	IssuedAt    time.Time           `json:"issued_at"`
	ExpiresAt   time.Time           `json:"expires_at"`
	ScopeSHA256 string              `json:"scope_sha256"`
	Scope       TaskDelegationScope `json:"scope"`
}

// TaskScopeMatches compares the frozen scope, not JWT contents or model text.
// Actions are sets; material ordering remains the exact frozen input ordering.
func TaskScopeMatches(actual, expected TaskDelegationScope) bool {
	normalize := func(scope TaskDelegationScope) TaskDelegationScope {
		scope.AllowedActions = append([]string{}, scope.AllowedActions...)
		sort.Strings(scope.AllowedActions)
		scope.Resources = append([]TaskDelegationResource{}, scope.Resources...)
		return scope
	}
	return reflect.DeepEqual(normalize(actual), normalize(expected))
}

// ReadTaskDelegationGrant accepts only Forge's online task-token connection.
// It never sends a token to general auth, native MCP, CRUD, or a redirect.
func ReadTaskDelegationGrant(ctx context.Context, issuer string, token []byte) (TaskDelegationGrant, error) {
	var grant TaskDelegationGrant
	endpoint, err := url.Parse(issuer)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || len(token) == 0 || len(token) > 64<<10 {
		return grant, errors.New("Forge task authority is unavailable")
	}
	endpoint.Path, endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = TaskDelegationPath+"/current", "", "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return grant, err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return grant, errors.New("Forge task authority could not be checked")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		return grant, errors.New("Forge task authority response is invalid")
	}
	if response.StatusCode != http.StatusOK {
		if refusal := taskAuthorizationRefusal(response.StatusCode, body); refusal != nil {
			return grant, refusal
		}
		return grant, errors.New("Forge task token is invalid or no longer authorized")
	}
	var raw struct {
		TaskDelegationGrant
		Scope json.RawMessage `json:"scope"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&raw) != nil {
		return grant, errors.New("Forge task authority response is invalid")
	}
	grant = raw.TaskDelegationGrant
	scopeDecoder := json.NewDecoder(bytes.NewReader(raw.Scope))
	scopeDecoder.DisallowUnknownFields()
	if scopeDecoder.Decode(&grant.Scope) != nil {
		return grant, errors.New("Forge task scope is invalid")
	}
	digest, err := frozen.HashCanonicalJSON(raw.Scope)
	if err != nil || digest != grant.ScopeSHA256 || grant.Version != "1" || !grant.Active || grant.TokenType != "forge_task" || grant.GrantID == "" || grant.Generation < 1 || grant.Subject.ID == "" || grant.Subject.OrganizationID == "" || grant.Issuer != issuer || !validStableTaskIdentityIssuer(grant.IdentityIssuer) || !grant.ExpiresAt.After(grant.IssuedAt) || grant.ExpiresAt.Sub(grant.IssuedAt) > TaskDelegationMaxLifetime || grant.IssuedAt.After(time.Now().UTC().Add(30*time.Second)) {
		return TaskDelegationGrant{}, errors.New("Forge task authority identity or scope is invalid")
	}
	if !grant.ExpiresAt.After(time.Now().UTC()) {
		return TaskDelegationGrant{}, ErrDelegationExpired
	}
	return grant, nil
}

// ReadVerifiedTaskFile reads only the task connection and verifies the exact
// frozen bytes. General employee storage/original endpoints are never used.
func ReadVerifiedTaskFile(ctx context.Context, issuer string, token []byte, resource TaskDelegationResource) error {
	endpoint, err := url.Parse(issuer)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || resource.ID == "" || resource.Bytes < 1 || resource.Bytes > 2<<20 {
		return errors.New("task material reference is invalid")
	}
	endpoint.Path = TaskDelegationPath + "/files/" + resource.ID + "/original"
	endpoint.RawPath = TaskDelegationPath + "/files/" + url.PathEscape(resource.ID) + "/original"
	endpoint.RawQuery, endpoint.Fragment = "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("If-Match", `"`+resource.SHA256+`"`)
	request.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("task material is unavailable")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	defer clear(content)
	if response.StatusCode != 200 || err != nil || int64(len(content)) != resource.Bytes {
		return errors.New("task material does not match frozen bytes")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != resource.SHA256 {
		return errors.New("task material digest differs")
	}
	return nil
}

// Only the dedicated connection's authenticated authorization phase can
// provide a no-effect proof. A message, unknown code or generic 401 cannot.
func taskAuthorizationRefusal(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Code     string `json:"code"`
			NoEffect bool   `json:"no_effect"`
			Phase    string `json:"phase"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || !envelope.Error.NoEffect || envelope.Error.Phase != "authorization" {
		return nil
	}
	switch status {
	case http.StatusUnauthorized:
		switch envelope.Error.Code {
		case "FORGE_TASK_DELEGATION_EXPIRED", "FORGE_TASK_PARENT_REVOKED", "FORGE_TASK_DELEGATION_REPLACED", "FORGE_TASK_DELEGATION_REVOKED":
			return &taskAuthorizationRefusalError{code: envelope.Error.Code, cause: ErrDelegationExpired}
		case "FORGE_TASK_SUBJECT_INACTIVE":
			return &taskAuthorizationRefusalError{code: envelope.Error.Code}
		}
	case http.StatusForbidden:
		if envelope.Error.Code == "FORGE_TASK_ORGANIZATION_FORBIDDEN" || envelope.Error.Code == "FORGE_TASK_CANCELLED" {
			return &taskAuthorizationRefusalError{code: envelope.Error.Code}
		}
	}
	return nil
}

type taskAuthorizationRefusalError struct {
	code  string
	cause error
}

func (err *taskAuthorizationRefusalError) Error() string { return err.code }
func (err *taskAuthorizationRefusalError) Unwrap() error { return err.cause }

func (err *taskAuthorizationRefusalError) nonRenewableReason() string {
	if err == nil {
		return ""
	}
	switch err.code {
	case "FORGE_TASK_ORGANIZATION_FORBIDDEN", "FORGE_TASK_SUBJECT_INACTIVE", "FORGE_TASK_CANCELLED":
		return err.code
	default:
		return ""
	}
}
