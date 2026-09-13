// Package admissionfence defines resource authorization generations. It does
// not own product membership, credentials, execution state or retry policy.
package admissionfence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const ContractVersion = "weave.admission-fence/v1"

type Action string

const (
	Block   Action = "block"
	Regrant Action = "regrant"
)

var (
	ErrInvalid  = errors.New("invalid resource admission fence")
	ErrConflict = errors.New("resource fence operation conflict")
	ErrBlocked  = errors.New("resource admission is blocked")
	ErrChanged  = errors.New("resource authorization generation changed")
)

type Resource struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version"`
}
type State struct {
	Resource Resource `json:"resource"`
	Epoch    int64    `json:"epoch"`
	Blocked  bool     `json:"blocked"`
}
type Command struct {
	Version     string     `json:"version"`
	WorkspaceID string     `json:"workspace_id"`
	OperationID string     `json:"operation_id"`
	Action      Action     `json:"action"`
	Resources   []Resource `json:"resources"`
}
type Receipt struct {
	Version     string            `json:"version"`
	WorkspaceID string            `json:"workspace_id"`
	OperationID string            `json:"operation_id"`
	Digest      string            `json:"digest"`
	Subject     execution.Subject `json:"subject"`
	Action      Action            `json:"action"`
	States      []State           `json:"states"`
}

// Transition must be authorized from persisted product intent. Regrant is a
// distinct operation after the new product authorization has been committed.
type Service interface {
	TransitionFence(context.Context, Command) (Receipt, error)
}

// Authority must verify this exact persisted intent is still current for every
// affected resource. In particular, a previous Regrant must stay superseded by
// a newer Block even when the newer product mutation has not committed yet.
// Checking only the current product resource's active flag is insufficient.
type Authority interface {
	AuthorizeFence(context.Context, Command) error
}

func valid(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= 2048 && !strings.ContainsRune(s, 0)
}
func Normalize(resources []Resource, parents bool) ([]Resource, error) {
	if len(resources) == 0 || len(resources) > 4096 {
		return nil, ErrInvalid
	}
	unique := map[Resource]bool{}
	for _, r := range resources {
		switch r.Kind {
		case "actor_user", "actor_service", "workspace_member", "team", "team_worker", "workflow", "credential", "credential_resource":
		default:
			return nil, ErrInvalid
		}
		if !valid(r.ID) || !valid(r.Version) {
			return nil, ErrInvalid
		}
		unique[r] = true
		if parents && r.Version != "*" {
			r.Version = "*"
			unique[r] = true
		}
	}
	result := make([]Resource, 0, len(unique))
	for r := range unique {
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version < b.Version
	})
	return result, nil
}
func (c Command) Fingerprint(ctx context.Context) (string, error) {
	subject, err := execution.RequireSubject(ctx, c.WorkspaceID)
	if err != nil {
		return "", err
	}
	if c.Version != ContractVersion || !valid(c.OperationID) || (c.Action != Block && c.Action != Regrant) {
		return "", ErrInvalid
	}
	c.Resources, err = Normalize(c.Resources, false)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Subject execution.Subject `json:"subject"`
		Command Command           `json:"command"`
	}{subject, c})
	if err != nil {
		return "", err
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(ContractVersion+"\x00"), canonical...))
	return hex.EncodeToString(sum[:]), nil
}
func (r Receipt) Verify(ctx context.Context, c Command) error {
	digest, err := c.Fingerprint(ctx)
	if err != nil {
		return err
	}
	subject, _ := execution.RequireSubject(ctx, c.WorkspaceID)
	resources, _ := Normalize(c.Resources, false)
	if r.Version != ContractVersion || r.WorkspaceID != c.WorkspaceID || r.OperationID != c.OperationID || r.Subject != subject || r.Action != c.Action || r.Digest != digest || len(r.States) != len(resources) {
		return ErrConflict
	}
	for i, state := range r.States {
		if state.Resource != resources[i] || state.Epoch < 1 || state.Blocked != (c.Action == Block) {
			return ErrConflict
		}
	}
	return nil
}
func Actor(subject execution.Subject) Resource {
	if subject.UserID != "" {
		return Resource{Kind: "actor_user", ID: subject.UserID, Version: "*"}
	}
	return Resource{Kind: "actor_service", ID: subject.ServiceID, Version: "*"}
}
func Member(userID string) Resource {
	return Resource{Kind: "workspace_member", ID: userID, Version: "*"}
}
func Team(id string) Resource { return Resource{Kind: "team", ID: id, Version: "*"} }
func Worker(team, worker string) Resource {
	raw, _ := json.Marshal([]string{team, worker})
	return Resource{Kind: "team_worker", ID: string(raw), Version: "*"}
}
func Workflow(id string, version int) Resource {
	v := "*"
	if version > 0 {
		v = strconv.Itoa(version)
	}
	return Resource{Kind: "workflow", ID: id, Version: v}
}
func CredentialResource(ref frozen.CredentialReference) Resource {
	raw, _ := json.Marshal([]string{string(ref.Kind), ref.ResourceID, string(ref.Scope), ref.UserID, ref.ServiceID})
	return Resource{Kind: "credential_resource", ID: string(raw), Version: "*"}
}
func Credential(ref frozen.CredentialReference, functionalRevision int64) Resource {
	raw, _ := json.Marshal([]string{CredentialResource(ref).ID, ref.Slot})
	version := "*"
	if functionalRevision > 0 {
		version = strconv.FormatInt(functionalRevision, 10)
	}
	return Resource{Kind: "credential", ID: string(raw), Version: version}
}
