package capabilities

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/capability"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"strings"
)

type DebugRequest struct {
	WorkspaceID   string
	ApplicationID string
	RequestID     string
	Definition    capability.Definition
	Input         json.RawMessage
	ActorUserID   string
	MaxSteps      int
}

func (s *Service) Debug(ctx context.Context, request DebugRequest) (Invocation, bool, error) {
	if s == nil || s.invocations == nil {
		return Invocation{}, false, fmt.Errorf("debug store unavailable")
	}
	if strings.TrimSpace(request.WorkspaceID) == "" || strings.TrimSpace(request.ApplicationID) == "" || strings.TrimSpace(request.RequestID) == "" {
		return Invocation{}, false, fmt.Errorf("%w: debug identity required", capability.ErrInvalidDefinition)
	}
	snapshot, err := capability.FreezeDraft(request.Definition)
	if err != nil {
		return Invocation{}, false, err
	}
	if _, err := capability.CompileDebug(snapshot); err != nil {
		return Invocation{}, false, err
	}
	input, err := frozen.CanonicalizeJSON(request.Input)
	if err != nil {
		return Invocation{}, false, fmt.Errorf("%w: invalid debug input", capability.ErrInvalidDefinition)
	}
	if err := capability.ValidateValue(snapshot.Definition.InputSchema, input); err != nil {
		return Invocation{}, false, fmt.Errorf("%w: debug input schema: %v", capability.ErrInvalidDefinition, err)
	}
	if request.MaxSteps <= 0 {
		request.MaxSteps = 100
	}
	return s.invocations.ClaimDebugInvocation(ctx, Invocation{
		CallerKind:  "developer",
		WorkspaceID: request.WorkspaceID, ApplicationID: request.ApplicationID, InvocationID: uuid.NewString(),
		RequestID: request.RequestID, CapabilityID: snapshot.Definition.CapabilityID, RunKind: "debug",
		DefinitionHash: snapshot.DefinitionHash, Input: input, Status: "queued", ResultState: "unavailable", ActorUserID: request.ActorUserID, MaxSteps: request.MaxSteps,
	}, snapshot)
}

func (m *MemoryStore) ClaimDebugInvocation(_ context.Context, invocation Invocation, snapshot capability.DefinitionSnapshot) (Invocation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := invocation.WorkspaceID + "\x00" + invocation.ApplicationID + "\x00" + invocation.RequestID
	if existing, ok := m.invokes[key]; ok {
		if existing.RunKind != "debug" || existing.DefinitionHash != snapshot.DefinitionHash || existing.CapabilityID != invocation.CapabilityID || string(existing.Input) != string(invocation.Input) {
			return Invocation{}, false, ErrIdempotencyConflict
		}
		return cloneValue(existing), true, nil
	}
	invocation.TaskID = "cap-debug-" + uuid.NewString()
	m.debug[invocation.WorkspaceID+"\x00"+invocation.InvocationID] = cloneValue(snapshot)
	m.invokes[key] = cloneValue(invocation)
	return cloneValue(invocation), false, nil
}
