package frozen

import "fmt"

// Output-limit fields a provider may declare for its Chat Completions client.
// Empty keeps the field the client already sends, max_tokens; model names
// never select it.
const (
	OutputLimitMaxTokens           = "max_tokens"
	OutputLimitMaxCompletionTokens = "max_completion_tokens"
)

// ValidateOutputLimitField rejects a declaration the provider's protocol
// cannot honor. Protocol is empty or "openai" for chat completions.
func ValidateOutputLimitField(protocol, field string) error {
	if field == "" {
		return nil
	}
	if protocol == ModelProtocolSystemOne || (field != OutputLimitMaxTokens && field != OutputLimitMaxCompletionTokens) {
		return fmt.Errorf("output_limit_field must be max_tokens or max_completion_tokens on an OpenAI-compatible provider")
	}
	return nil
}
