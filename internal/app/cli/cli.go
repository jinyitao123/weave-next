// Package cli starts the MCP transport used by Workbench.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/jinyitao123/weave/internal/app/mcpstdio"
	"github.com/jinyitao123/weave/internal/app/weaveclient"
)

type commandError struct {
	code string
	exit int
}

func (e *commandError) Error() string { return e.code }

func Dispatch(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] == "serve" || args[0] == "runtime" || args[0] == "daemon" {
		return false, 0
	}
	switch args[0] {
	case "mcp":
		err := runMCP(context.Background(), args, os.Stdin, stdout)
		if err == nil {
			return true, 0
		}
		code, exit := errorCode(err)
		_ = writeJSON(stderr, map[string]string{"error": code})
		return true, exit
	default:
		_ = writeJSON(stderr, map[string]string{"error": "unknown_command"})
		return true, 2
	}
}

func runMCP(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) != 2 || args[0] != "mcp" || args[1] != "serve" {
		return &commandError{code: "mcp_serve_required", exit: 2}
	}
	client, err := clientFromEnv()
	if err != nil {
		return err
	}
	return mcpstdio.Serve(ctx, input, output, client)
}

func clientFromEnv() (*weaveclient.Client, error) {
	config, err := weaveclient.ConfigFromEnv()
	if err != nil {
		return nil, &commandError{code: "configuration_invalid", exit: 2}
	}
	client, err := weaveclient.New(config, nil)
	if err != nil {
		return nil, &commandError{code: "configuration_invalid", exit: 2}
	}
	return client, nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return &commandError{code: "output_failed", exit: 1}
	}
	return nil
}

func errorCode(err error) (string, int) {
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.code, commandErr.exit
	}
	return "command_failed", 1
}
