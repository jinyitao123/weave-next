package loomruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

const (
	protocolProbePhysicalLimit = 4
)

// ModelProtocolProbePolicy selects only already-authorized execution. MaxModelCalls
// counts reserved journal model-perform attempts across nodes and restarts; each
// reservation stores at most four provider invocations, never their payloads.
type ModelProtocolProbePolicy struct {
	WorkspaceID, WorkflowID, RunID string
	ExpiresAt                      time.Time
	MaxModelCalls                  int
	Now                            func() time.Time
}

func (p *ModelProtocolProbePolicy) now() time.Time {
	if p != nil && p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *ModelProtocolProbePolicy) matches(request MemberRequest) bool {
	return p != nil && p.WorkspaceID != "" && p.WorkflowID != "" && p.MaxModelCalls >= 1 && p.MaxModelCalls <= 64 &&
		request.WorkspaceID == p.WorkspaceID && request.Attribution.workflowID.present && request.Attribution.workflowID.value == p.WorkflowID &&
		(p.RunID == "" || request.ParentRunID == p.RunID) && p.now().Before(p.ExpiresAt)
}

func (p *ModelProtocolProbePolicy) digest() string {
	raw, _ := json.Marshal([]any{"tool-protocol-probe/1", p.WorkspaceID, p.WorkflowID, p.RunID, p.ExpiresAt.UTC(), p.MaxModelCalls})
	return protocolDigest(raw)
}

// This metadata is attached to the original model journal slot. Its member run
// and slot bind parent/snapshot/node/invocation; digests connect the normalized
// ToolCalls to the original tool journal and Forge receipt without new IDs.
type ModelProtocolProbeSample struct {
	Version             int                                  `json:"version"`
	PolicySHA256        string                               `json:"policy_sha256"`
	State               string                               `json:"state"`
	AttemptGeneration   int64                                `json:"attempt_generation"`
	Attempt             int64                                `json:"attempt"`
	Observed            []llmrouter.ModelProtocolObservation `json:"observed"`
	DroppedObservations int                                  `json:"dropped_observations,omitempty"`
	NormalizedToolCount *int                                 `json:"normalized_tool_count,omitempty"`
	NormalizedError     bool                                 `json:"normalized_error,omitempty"`
	NormalizedTools     []llmrouter.NormalizedToolProtocol   `json:"normalized_tools,omitempty"`
	NormalizedTruncated bool                                 `json:"normalized_truncated,omitempty"`
}

type modelProtocolProbeCollector struct {
	mu     sync.Mutex
	policy *ModelProtocolProbePolicy
	sample ModelProtocolProbeSample
}

func (member *memberExecution) reserveProtocolProbe(ctx context.Context, tx pgx.Tx, op *memberOperation) *modelProtocolProbeCollector {
	p := member.runner.ProtocolProbe
	if !p.matches(member.request) {
		return nil
	}
	// A diagnostic query failure must not poison the original intent transaction.
	if _, err := tx.Exec(ctx, "SAVEPOINT weave_protocol_probe"); err != nil {
		return nil
	}
	policy := p.digest()
	failed := func() *modelProtocolProbeCollector {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT weave_protocol_probe")
		_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT weave_protocol_probe")
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, policy); err != nil {
		return failed()
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM loom_store j
		CROSS JOIN LATERAL jsonb_array_elements(COALESCE(convert_from(j.value,'UTF8')::jsonb->'protocol_probe','[]'::jsonb)) p
		WHERE j.namespace=$1 AND convert_from(j.value,'UTF8')::jsonb->>'kind'='model' AND p->>'policy_sha256'=$2`,
		"member-operation:"+member.request.WorkspaceID, policy).Scan(&count); err != nil {
		return failed()
	}
	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT weave_protocol_probe"); err != nil {
		return failed()
	}
	if count >= p.MaxModelCalls || !p.matches(member.request) {
		return nil
	}
	collector := &modelProtocolProbeCollector{policy: p, sample: ModelProtocolProbeSample{
		Version: 1, PolicySHA256: policy, State: "incomplete", AttemptGeneration: op.AttemptGeneration, Attempt: op.Attempts,
		Observed: []llmrouter.ModelProtocolObservation{},
	}}
	op.ProtocolProbe = append(op.ProtocolProbe, collector.sample)
	return collector
}

func (c *modelProtocolProbeCollector) observe(observation llmrouter.ModelProtocolObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sample.Observed) >= protocolProbePhysicalLimit || !c.policy.now().Before(c.policy.ExpiresAt) {
		c.sample.DroppedObservations++
		return
	}
	c.sample.Observed = append(c.sample.Observed, observation)
}

func (c *modelProtocolProbeCollector) normalized(value llmrouter.NormalizedModelProtocol) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.policy.now().Before(c.policy.ExpiresAt) {
		return
	}
	c.sample.NormalizedError, c.sample.NormalizedToolCount = value.Error, value.Count
	c.sample.NormalizedTruncated, c.sample.NormalizedTools = value.Truncated, value.Tools
}

func (c *modelProtocolProbeCollector) result() ModelProtocolProbeSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sample.State = "observed"
	if len(c.sample.Observed) == 0 {
		c.sample.State = "unavailable"
	}
	if c.sample.DroppedObservations > 0 || c.sample.NormalizedTruncated || !c.policy.now().Before(c.policy.ExpiresAt) || c.sample.NormalizedError || len(c.sample.Observed) > 0 && c.sample.NormalizedToolCount == nil {
		c.sample.State = "incomplete"
	}
	for _, observation := range c.sample.Observed {
		if !observation.Complete || observation.Truncated {
			c.sample.State = "incomplete"
		}
	}
	return c.sample
}

func protocolDigest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

type modelProtocolProbeLLM struct{ inner contract.LLM }

func (m modelProtocolProbeLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	member, _ := ctx.Value(memberExecutionKey{}).(*memberExecution)
	if member == nil || member.protocolProbe == nil {
		return m.inner.Chat(ctx, request)
	}
	collector := member.protocolProbe
	if !collector.policy.now().Before(collector.policy.ExpiresAt) {
		return m.inner.Chat(ctx, request)
	}
	return llmrouter.ObserveModelProtocolCall(ctx, m.inner, request, collector.observe, collector.normalized)
}

func (m modelProtocolProbeLLM) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return m.inner.Stream(ctx, request)
}
