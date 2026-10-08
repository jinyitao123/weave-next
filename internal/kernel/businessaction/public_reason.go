package businessaction

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
)

const PublicActionReasonMaxRunes = 240

var privateActionReason = regexp.MustCompile(`(?i)(bearer|password|passwd|passphrase|secret|token|api[_ -]?key|authorization|cookie|密码|口令|密钥|令牌|https?[:/]|www\.|://|[a-z]:[\\/]|/(users|home|var|tmp|etc|srv)/|\b(?:[a-z0-9-]+\.)+[a-z]{2,}\b|\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|::|(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|\b[a-z][a-z0-9-]*:[0-9]{1,5}\b|\b(?:id|uuid|requestid|traceid)[ :：]|[a-z0-9]+_[a-z0-9_]+|[a-z0-9_-]{20,}|[0-9]{8,}|[0-9a-f]{8,}|sqlstate|typeerror|referenceerror|syntaxerror|rangeerror|stacktrace|econn|etimedout|\b(error|exception|relation|column|constraint|postgres)\b|\b(select|insert|update|delete)\s+.+\b(from|into|set)\b)`)

// SafePublicActionReason is a display-only boundary, never a recoverable
// receipt. The product's native business messages are Chinese. Unclassified
// diagnostics keep the generic failure instead of exposing arbitrary text.
func SafePublicActionReason(value string) string {
	if !utf8.ValidString(value) || len(value) > 4096 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for _, line := range lines[1:] {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "at ") {
			return ""
		}
	}
	value = strings.TrimSpace(lines[0])
	if value == "" || utf8.RuneCountInString(value) > PublicActionReasonMaxRunes || privateActionReason.MatchString(value) {
		return ""
	}
	humanText := false
	for _, char := range value {
		if unicode.Is(unicode.Han, char) {
			humanText = true
		}
		if !unicode.IsLetter(char) && !unicode.IsNumber(char) && !strings.ContainsRune(" ，。；：！？、（）()【】[]“”‘’'\".,;:!?%-+", char) {
			return ""
		}
	}
	if !humanText {
		return ""
	}
	return value
}

// PublicActionFailureReason accepts only the controlled native failure shape
// for the selected action. Callers must additionally bind the Forge source and
// operation identity; model output and transport errors never call this path.
func PublicActionFailureReason(result *contract.ToolResult, actionName string) string {
	if result == nil || classifyNativeActionResult(result) != ActionOutcomeStatusFailed {
		return ""
	}
	var envelope struct {
		OK      *bool           `json:"ok"`
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal([]byte(result.Content), &envelope) == nil {
		var message string
		if json.Unmarshal(envelope.Error, &message) != nil {
			var detail struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(envelope.Error, &detail)
			message = detail.Message
		}
		if message == "" && envelope.OK != nil && !*envelope.OK {
			message = envelope.Message
		}
		return SafePublicActionReason(message)
	}
	prefix := "action '" + actionName + "' threw: Error: "
	if actionName != "" && result.IsError && strings.HasPrefix(result.Content, prefix) {
		return SafePublicActionReason(strings.TrimPrefix(result.Content, prefix))
	}
	return ""
}
