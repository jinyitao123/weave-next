package api

import (
	"encoding/json"
	"log/slog"
	"strings"
)

var allowedComponentTypes = map[string]struct{}{
	"bar_chart":      {},
	"line_chart":     {},
	"data_card":      {},
	"status_bar":     {},
	"alert_banner":   {},
	"table":          {},
	"action_buttons": {},
	"quick_options":  {},
}

var dispatchBlockTerms = []string{"dispatch", "派工进度", "织卡"}

// filterContentBlocks applies the shared component vocabulary and product
// redlines used by both streaming output and persisted message metadata.
func filterContentBlocks(blocks []ContentBlock, agentName string) []ContentBlock {
	if len(blocks) == 0 {
		return nil
	}

	filtered := make([]ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "component" {
			filtered = append(filtered, block)
			continue
		}

		componentType := strings.ToLower(block.ComponentType)
		props := lowerJSON(block.Props)
		if componentType == "dispatch_card" || containsAny(props, dispatchBlockTerms) {
			slog.Warn("assistant content block dropped by redline",
				"agent", agentName,
				"redline", "dispatch_progress",
				"component_type", block.ComponentType,
			)
			continue
		}
		if _, ok := allowedComponentTypes[block.ComponentType]; !ok {
			slog.Warn("assistant component block dropped outside vocabulary",
				"agent", agentName,
				"component_type", block.ComponentType,
			)
			continue
		}

		filtered = append(filtered, block)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func lowerJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(string(encoded))
}

func containsAny(value string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}
