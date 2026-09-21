package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type subjectlessContext struct{ context.Context }

func (c subjectlessContext) Value(key any) any {
	if _, ok := key.(subjectKey); ok {
		return nil
	}
	return c.Context.Value(key)
}

// WithoutAuthenticatedSubject preserves cancellation and all unrelated values
// while forcing a durable parent (task or run snapshot) to supply the execution
// subject. Use this only for continuations where the current user is an audited
// reviewer and must not replace the original run identity.
func WithoutAuthenticatedSubject(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return subjectlessContext{Context: ctx}
}

// Subject is the platform-authenticated end user or an explicitly delegated
// workspace service. Runtime-host authentication does not replace this identity.
type Subject struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id,omitempty"`
	ServiceID   string `json:"service_id,omitempty"`
}

var ErrSubjectMismatch = errors.New("execution subject does not match")
var ErrSubjectRequired = errors.New("authenticated execution subject is required")

func (s Subject) Validate() error {
	if s.WorkspaceID == "" || strings.TrimSpace(s.WorkspaceID) != s.WorkspaceID ||
		(s.UserID == "") == (s.ServiceID == "") || strings.TrimSpace(s.UserID) != s.UserID || strings.TrimSpace(s.ServiceID) != s.ServiceID {
		return ErrSubjectRequired
	}
	return nil
}

// Digest provides a stable, non-identifying partition for caches and directories.
func (s Subject) Digest() string {
	if s.Validate() != nil {
		return ""
	}
	data, _ := json.Marshal(s)
	hash := sha256.Sum256(append([]byte("weave-execution-subject-v1\x00"), data...))
	return hex.EncodeToString(hash[:])
}

type subjectKey struct{}

func WithSubject(ctx context.Context, subject Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, subject)
}

func SubjectFromContext(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(subjectKey{}).(Subject)
	return s, ok
}

func RequireSubject(ctx context.Context, workspaceID string) (Subject, error) {
	s, ok := SubjectFromContext(ctx)
	if !ok || s.Validate() != nil {
		return Subject{}, ErrSubjectRequired
	}
	if s.WorkspaceID != workspaceID {
		return Subject{}, ErrSubjectMismatch
	}
	return s, nil
}

// BindSubject restores durable identity while rejecting a conflicting caller.
func BindSubject(ctx context.Context, subject Subject) (context.Context, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	if existing, ok := SubjectFromContext(ctx); ok && (existing.Validate() != nil || existing != subject) {
		return nil, ErrSubjectMismatch
	}
	return WithSubject(ctx, subject), nil
}

// Environment carries only the validated actor, never values supplied by a prompt.
func (s Subject) Environment() map[string]string {
	return map[string]string{"WEAVE_ACTOR_USER_ID": s.UserID, "WEAVE_ACTOR_SERVICE_ID": s.ServiceID, "WEAVE_WORKSPACE_ID": s.WorkspaceID, "WEAVE_ACTOR_DIGEST": s.Digest()}
}
