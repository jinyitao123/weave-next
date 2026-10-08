package engine

import "io"

func parseClaudeOutput(stdout io.Reader) claudeOutput {
	return parseClaudeOutputWithEvents(stdout, nil)
}

func parseCodexOutput(stdout interface{ Read([]byte) (int, error) }) codexOutput {
	return parseCodexOutputWithEvents(stdout, nil)
}
