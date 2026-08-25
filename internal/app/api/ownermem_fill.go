package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/ownermem"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const ownerMemoryFillTimeout = 30 * time.Second

// OwnerMemoryStore is the workspace-, agent-, and user-scoped profile store
// used by chat and the profile administration API.
type OwnerMemoryStore interface {
	Get(ctx context.Context, workspaceID, agentID, userID string) (ownermem.Profile, error)
	Upsert(ctx context.Context, workspaceID, agentID, userID string, slots map[string]string, expectedVersion int) error
}

// profileMemoryBoundary keeps recalled slots honest: injected together with the
// profile so the model treats the listed slots as its complete cross-session
// memory instead of confidently confabulating unlisted facts. Chinese, matching
// the platform's other injected scaffolding (grounding.PlatformRule), so a
// single assembled context never mixes languages.
const profileMemoryBoundary = "以上是你对当前对话对象的全部跨会话记忆。" +
	"凡未在此列出、且本次对话中也未提及的事项，你都不知道——被问到时如实说明没有记录，不要臆测编造。"

// emptyProfileNotice covers the symmetric failure: slots are defined but
// nothing is remembered yet, so pretending to remember would be fabrication.
const emptyProfileNotice = "你尚未保存任何关于当前对话对象的跨会话记忆。" +
	"若被问及本次对话之外你还记得关于对方的什么，如实说明没有记录，切勿编造。"

func formatProfile(definitions []registry.MemorySlot, values map[string]string) string {
	lines := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		value := strings.TrimSpace(values[definition.Key])
		if value == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", definition.Label, value))
	}
	if len(lines) == 0 {
		return emptyProfileNotice
	}
	return "关于当前对话对象，你已经知道：\n" +
		strings.Join(lines, "\n") + "\n" + profileMemoryBoundary
}

func buildFillPrompt(
	definitions []registry.MemorySlot,
	current map[string]string,
	userMessage, assistantReply string,
) string {
	definitionsJSON, _ := json.Marshal(definitions)
	currentJSON, _ := json.Marshal(current)
	return fmt.Sprintf(`Update a bounded profile about the current conversation participant from this completed exchange.

Allowed slot definitions: %s
Current profile: %s
Participant message: %s
Assistant reply: %s

Return only one JSON object whose keys are allowed slot keys and whose values are strings. Include only values supported by the exchange. Do not invent values, and do not include unknown keys.`,
		definitionsJSON, currentJSON, userMessage, assistantReply)
}

func (s *Server) contextWithOwnerProfile(
	ctx context.Context,
	workspaceID, userID string,
	rec *registry.AgentRecord,
	base map[string]any,
) map[string]any {
	if s.OwnerMem == nil || rec == nil || userID == "" || len(rec.MemorySlots) == 0 {
		return base
	}
	profile, err := s.OwnerMem.Get(ctx, workspaceID, rec.ID, userID)
	if err != nil {
		return base
	}
	// formatProfile always returns a non-empty string (the empty-slots honesty
	// notice when nothing is stored), so owner_profile is injected whenever the
	// agent defines memory slots — there is no skip-on-empty path.
	merged := make(map[string]any, len(base)+1)
	for key, value := range base {
		merged[key] = value
	}
	merged["owner_profile"] = formatProfile(rec.MemorySlots, profile.Slots)
	return merged
}

// startOwnerMemoryFill starts a detached best-effort update. It intentionally
// does not wait during process shutdown, matching other post-chat memory hooks.
func (s *Server) startOwnerMemoryFill(
	workspaceID string,
	rec *registry.AgentRecord,
	userID, conversationID, userMessage, assistantReply string,
	innerDispatch bool,
) {
	if s.OwnerMem == nil || s.Models == nil || rec == nil || userID == "" || conversationID == "" ||
		len(rec.MemorySlots) == 0 || innerDispatch || assistantReply == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ownerMemoryFillTimeout)
		defer cancel()
		if err := s.fillOwnerMemory(ctx, workspaceID, rec, userID, userMessage, assistantReply); err != nil {
			slog.Warn("owner memory fill failed",
				"workspace_id", workspaceID,
				"agent", rec.Name,
				"user_id", userID,
				"error", err,
			)
		}
	}()
}

func (s *Server) fillOwnerMemory(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	userID, userMessage, assistantReply string,
) error {
	profile, err := s.OwnerMem.Get(ctx, workspaceID, rec.ID, userID)
	if err != nil {
		return err
	}
	llm, err := s.llmFor(ctx, workspaceID)
	if err != nil {
		return err
	}
	response, err := llm.Chat(ctx, contract.ChatRequest{
		Model: rec.Model,
		Messages: []contract.Message{{
			Role:    "user",
			Content: buildFillPrompt(rec.MemorySlots, profile.Slots, userMessage, assistantReply),
		}},
	})
	if err != nil {
		return fmt.Errorf("extract owner memory profile: %w", err)
	}
	if response == nil {
		return errors.New("extract owner memory profile: empty LLM response")
	}
	updates := parseFilledSlots(response.Content, rec.MemorySlots)
	if len(updates) == 0 {
		return nil
	}

	merged := mergeDefinedSlots(rec.MemorySlots, profile.Slots, updates)
	err = s.OwnerMem.Upsert(ctx, workspaceID, rec.ID, userID, merged, profile.Version)
	if !errors.Is(err, ownermem.ErrVersionConflict) {
		return err
	}

	profile, err = s.OwnerMem.Get(ctx, workspaceID, rec.ID, userID)
	if err != nil {
		return err
	}
	merged = mergeDefinedSlots(rec.MemorySlots, profile.Slots, updates)
	return s.OwnerMem.Upsert(ctx, workspaceID, rec.ID, userID, merged, profile.Version)
}

func parseFilledSlots(content string, definitions []registry.MemorySlot) map[string]string {
	allowed := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Key] = struct{}{}
	}

	for offset := 0; offset < len(content); {
		relative := strings.IndexByte(content[offset:], '{')
		if relative < 0 {
			return nil
		}
		start := offset + relative
		var object map[string]json.RawMessage
		decoder := json.NewDecoder(strings.NewReader(content[start:]))
		if err := decoder.Decode(&object); err != nil {
			offset = start + 1
			continue
		}
		result := make(map[string]string)
		for key, raw := range object {
			if _, ok := allowed[key]; !ok {
				continue
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				continue
			}
			value = strings.TrimSpace(value)
			if value != "" {
				result[key] = value
			}
		}
		return result
	}
	return nil
}

func mergeDefinedSlots(
	definitions []registry.MemorySlot,
	current, updates map[string]string,
) map[string]string {
	merged := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		if value := strings.TrimSpace(current[definition.Key]); value != "" {
			merged[definition.Key] = value
		}
		if value := strings.TrimSpace(updates[definition.Key]); value != "" {
			merged[definition.Key] = value
		}
	}
	return merged
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
