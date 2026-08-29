// Package cli implements the thin command-line transport for Weave's HTTP API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

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
	case "team", "status", "deliverable", "mcp":
		err := run(context.Background(), args, stdout, stderr)
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

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	client, err := clientFromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "team":
		return runTeam(ctx, client, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, client, args[1:], stdout, stderr)
	case "deliverable":
		return runDeliverable(ctx, client, args[1:], stdout, stderr)
	case "mcp":
		if len(args) != 2 || args[1] != "serve" {
			return usageError("mcp_serve_required")
		}
		return mcpstdio.Serve(ctx, os.Stdin, stdout, client)
	default:
		return usageError("unknown_command")
	}
}

func runTeam(ctx context.Context, client *weaveclient.Client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError("team_command_required")
	}
	switch args[0] {
	case "samples":
		if len(args) != 1 {
			return usageError("unexpected_arguments")
		}
		result, err := client.TeamTemplateList(ctx)
		return writeResult(stdout, result, err)
	case "up":
		flags := flag.NewFlagSet("team up", flag.ContinueOnError)
		flags.SetOutput(stderr)
		var filename string
		flags.StringVar(&filename, "f", "", "team template YAML file")
		flags.StringVar(&filename, "file", "", "team template YAML file")
		declarativeSpecFile := flags.String("declarative-spec", "", "declarative workflow JSON file")
		idempotencyKey := flags.String("idempotency-key", "", "caller-provided UUID")
		if err := flags.Parse(args[1:]); err != nil {
			return usageError("invalid_arguments")
		}
		if flags.NArg() != 0 || strings.TrimSpace(filename) == "" {
			return usageError("team_file_required")
		}
		if strings.TrimSpace(*idempotencyKey) == "" {
			return usageError("idempotency_key_required")
		}
		yaml, err := os.ReadFile(filename)
		if err != nil {
			return &commandError{code: "team_file_read_failed", exit: 1}
		}
		var declarativeSpec json.RawMessage
		if strings.TrimSpace(*declarativeSpecFile) != "" {
			declarativeSpec, err = os.ReadFile(*declarativeSpecFile)
			if err != nil {
				return &commandError{code: "declarative_spec_read_failed", exit: 1}
			}
			var object map[string]any
			if json.Unmarshal(declarativeSpec, &object) != nil || object == nil {
				return usageError("declarative_spec_invalid")
			}
		}
		result, err := client.TeamCreate(ctx, weaveclient.TeamCreateRequest{
			YAML: string(yaml), DeclarativeSpec: declarativeSpec, IdempotencyKey: *idempotencyKey,
		})
		return writeResult(stdout, result, err)
	case "dispatch":
		flags := flag.NewFlagSet("team dispatch", flag.ContinueOnError)
		flags.SetOutput(stderr)
		teamID := flags.String("team", "", "team ID")
		task := flags.String("task", "", "task")
		mode := flags.String("mode", "", "workflow or free_collab (default workflow)")
		workflowID := flags.String("workflow", "", "workflow ID override")
		workflowVersion := flags.Int("workflow-version", 0, "exact published workflow version")
		wait := flags.Bool("wait", false, "wait for a terminal dispatch state")
		clientRequestID := flags.String("client-request-id", "", "stable dispatch UUID")
		projectID := flags.String("project", "", "project ID")
		conversationID := flags.String("conversation", "", "conversation ID")
		if err := flags.Parse(args[1:]); err != nil {
			return usageError("invalid_arguments")
		}
		if flags.NArg() != 0 || strings.TrimSpace(*teamID) == "" || strings.TrimSpace(*task) == "" {
			return usageError("team_and_task_required")
		}
		request := weaveclient.DispatchRequest{
			TeamID: *teamID, Task: *task, Mode: *mode, WorkflowID: *workflowID,
			ClientRequestID: *clientRequestID, ProjectID: *projectID, ConversationID: *conversationID,
		}
		if *workflowVersion > 0 {
			request.WorkflowVersion = workflowVersion
		} else if *workflowVersion < 0 {
			return usageError("invalid_workflow_version")
		}
		var id string
		var result json.RawMessage
		var err error
		if *wait {
			id, result, err = client.TeamDispatchAndWait(ctx, request)
		} else {
			id, result, err = client.TeamDispatch(ctx, request)
		}
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"client_request_id": id, "result": result})
	default:
		return usageError("unknown_team_command")
	}
}

func runStatus(ctx context.Context, client *weaveclient.Client, args []string, stdout, _ io.Writer) error {
	if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
		return usageError("status_kind_and_id_required")
	}
	var result json.RawMessage
	var err error
	switch args[0] {
	case "build":
		result, err = client.BuildStatus(ctx, args[1])
	case "dispatch":
		result, err = client.DispatchStatus(ctx, args[1])
	case "team-run":
		result, err = client.TeamRunStatus(ctx, args[1])
	default:
		return usageError("unknown_status_kind")
	}
	return writeResult(stdout, result, err)
}

func runDeliverable(ctx context.Context, client *weaveclient.Client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError("deliverable_command_required")
	}
	switch args[0] {
	case "list":
		flags := flag.NewFlagSet("deliverable list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		limit := flags.Int("limit", 0, "maximum result count")
		offset := flags.Int("offset", 0, "result offset")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *limit < 0 || *offset < 0 {
			return usageError("invalid_arguments")
		}
		result, err := client.DeliverableList(ctx, *limit, *offset)
		return writeResult(stdout, result, err)
	case "get":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			return usageError("deliverable_id_required")
		}
		result, err := client.DeliverableGet(ctx, args[1])
		return writeResult(stdout, result, err)
	default:
		return usageError("unknown_deliverable_command")
	}
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

func writeResult(writer io.Writer, result json.RawMessage, err error) error {
	if err != nil {
		return err
	}
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	return writeJSON(writer, result)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return &commandError{code: "output_failed", exit: 1}
	}
	return nil
}

func usageError(code string) error {
	return &commandError{code: code, exit: 2}
}

func errorCode(err error) (string, int) {
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.code, commandErr.exit
	}
	var apiErr *weaveclient.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code, 1
	}
	return "command_failed", 1
}

func Usage() string {
	return strings.Join([]string{
		"weave team samples",
		"weave team up -f <team.yaml> [--declarative-spec <workflow.json>] --idempotency-key <uuid>",
		"weave team dispatch --team <id> --task <task> [--mode workflow|free_collab] [--workflow <id>] [--workflow-version <n>] [--client-request-id <uuid>] [--wait]",
		"weave status build|dispatch|team-run <id>",
		"weave deliverable list|get [id]",
		"weave mcp serve",
	}, "\n") + "\n"
}
