package frozen

import "fmt"

const (
	ProtocolOpenAICompletions = "openai-completions"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// ValidateProviderProfile checks the complete request-affecting frozen profile.
// Protocol is explicit; neither addresses nor model names select a protocol.
func ValidateProviderProfile(protocol string, objectMode bool, thinking string, disableWithTools bool, timeout int) error {
	switch protocol {
	case ProtocolOpenAICompletions:
		if thinking != "" && thinking != "enabled" && thinking != "disabled" {
			return fmt.Errorf("invalid thinking mode")
		}
		if disableWithTools && thinking == "" {
			return fmt.Errorf("thinking mode is required for tool-specific control")
		}
	case ProtocolOpenAIResponses, ProtocolAnthropicMessages:
		if objectMode || thinking != "" || disableWithTools {
			return fmt.Errorf("chat-completions options are incompatible with native protocol")
		}
	default:
		return fmt.Errorf("model provider protocol is required and must be supported")
	}
	if timeout < 0 || timeout > 86400 {
		return fmt.Errorf("attempt timeout is outside 0..86400 seconds")
	}
	return nil
}
