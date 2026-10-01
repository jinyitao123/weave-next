package businessaction

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
)

// Leave room in the 32 KiB activity detail for immutable action provenance.
const ActionOutcomeResultMaxBytes = 24 * 1024

func actionParameterName(param actionParam) string {
	if param.Name != "" {
		return param.Name
	}
	return param.Field
}

func isSystemIdempotencyParameter(name string) bool {
	return name == "idempotency_key" || name == "idempotencyKey"
}

func actionReplayResult(call contract.ToolCall, replay ActionOutcomeReplay) *contract.ToolResult {
	if replay.Blocked && replay.SameOperation &&
		(replay.Status == ActionOutcomeStatusSucceeded || replay.Status == ActionOutcomeStatusFailed) {
		if cached := SanitizeActionOutcomeResult(replay.Result); cached != nil && ValidateActionOutcomeResultStatus(cached, replay.Status) == nil {
			cached.CallID, cached.ToolName = call.ID, call.Name
			if replay.Status == ActionOutcomeStatusFailed {
				cached.IsError = true
			}
			return cached
		}
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, IsError: true, StopLoop: true,
			Content: "该业务操作已有结果，但原业务回执无法恢复；请先核对业务记录，本次没有再次调用 Forge。"}
	}
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, IsError: true, StopLoop: true,
		Content: "该业务动作上一次结果仍未知，请先核对业务记录；本次没有再次调用 Forge。"}
}

var receiptCredentialText = regexp.MustCompile(`(?i)(bearer\s+[^\s"',;]+|(?:password|passwd|secret|access[_-]?token|api[_-]?key|authorization)\s*[:=]\s*[^\s"',;]+)`)
var receiptURLCredentials = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)

// SanitizeActionOutcomeResult keeps only the native business receipt. It drops
// Loom control deltas and request/model-private/credential fields recursively.
// Unsupported prose, malformed or oversized receipts are not cached: a known
// outcome without a recoverable receipt must stop instead of redispatching.
func SanitizeActionOutcomeResult(result *contract.ToolResult) *contract.ToolResult {
	if result == nil || len(result.Content) == 0 || len(result.Content) > ActionOutcomeResultMaxBytes ||
		!utf8.ValidString(result.Content) || utf8.RuneCountInString(result.CallID) > 256 || utf8.RuneCountInString(result.ToolName) > 256 {
		return nil
	}
	var content any
	decoder := json.NewDecoder(strings.NewReader(result.Content))
	decoder.UseNumber()
	if decoder.Decode(&content) != nil {
		return nil
	}
	// Native receipts are JSON objects, not arbitrary model text.
	if _, ok := content.(map[string]any); !ok || !json.Valid([]byte(result.Content)) {
		return nil
	}
	cleaned, ok := sanitizeReceiptValue(content, 0)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil
	}
	cached := &contract.ToolResult{CallID: result.CallID, ToolName: result.ToolName, Content: string(encoded), IsError: result.IsError}
	raw, err := json.Marshal(cached)
	if err != nil || len(raw) > ActionOutcomeResultMaxBytes {
		return nil
	}
	return cached
}

func sanitizeReceiptValue(value any, depth int) (any, bool) {
	if depth > 16 {
		return nil, false
	}
	switch item := value.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(item))
		for key, value := range item {
			if privateReceiptField(key) {
				continue
			}
			clean, ok := sanitizeReceiptValue(value, depth+1)
			if !ok {
				return nil, false
			}
			cleaned[key] = clean
		}
		return cleaned, true
	case []any:
		cleaned := make([]any, len(item))
		for index, value := range item {
			clean, ok := sanitizeReceiptValue(value, depth+1)
			if !ok {
				return nil, false
			}
			cleaned[index] = clean
		}
		return cleaned, true
	case string:
		return receiptURLCredentials.ReplaceAllString(receiptCredentialText.ReplaceAllString(item, "[redacted]"), "${1}[redacted]@"), true
	default:
		return value, true
	}
}

func privateReceiptField(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(key))
	switch normalized {
	case "params", "parameters", "args", "arguments", "request", "input", "prompt", "systemprompt", "messages", "reasoning", "thinking", "chainofthought", "modeltext", "modelresponse", "privatecontent", "headers", "cookie", "cookies", "credentials", "credential", "authorization", "password", "passwd", "passphrase", "apikey", "secret", "clientsecret":
		return true
	}
	return strings.HasSuffix(normalized, "token") || strings.HasSuffix(normalized, "password") || strings.HasSuffix(normalized, "apikey") || strings.HasSuffix(normalized, "secret")
}
