package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
)

var tgRefPattern = regexp.MustCompile(`tg_[A-Za-z0-9-]+`)

func extractTGRefs(content string) []string {
	matches := tgRefPattern.FindAllString(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	refs := make([]string, 0, len(matches))
	for _, ref := range matches {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

// groundAssistant verifies task-group references without changing assistant content.
func (s *Server) groundAssistant(ctx context.Context, workspaceID, content string) json.RawMessage {
	refs := extractTGRefs(content)
	if len(refs) == 0 || s.Fanout == nil {
		return nil
	}

	fabricated := make([]string, 0)
	for _, ref := range refs {
		exists, err := s.Fanout.GroupExists(ctx, workspaceID, ref)
		if err != nil {
			slog.Warn("grounding verification failed",
				"workspace_id", workspaceID,
				"group_id", ref,
				"error", err,
			)
			continue
		}
		if !exists {
			fabricated = append(fabricated, ref)
		}
	}
	if len(fabricated) == 0 {
		return nil
	}

	metadata, _ := json.Marshal(struct {
		Grounding struct {
			Fabricated []string `json:"fabricated"`
			Warning    string   `json:"warning"`
		} `json:"grounding"`
	}{
		Grounding: struct {
			Fabricated []string `json:"fabricated"`
			Warning    string   `json:"warning"`
		}{
			Fabricated: fabricated,
			Warning:    "declared dispatch not found",
		},
	})
	return metadata
}
