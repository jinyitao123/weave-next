package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// cuaClient drives the local cua-driver through its stdio MCP server
// (`cua-driver mcp`): one JSON-RPC message per line in each direction. Tool
// names and parameters are cua-driver's own; tools/list says which exist.

const cuaCallTimeout = 2 * time.Minute

type cuaContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type cuaResult struct {
	Content           []cuaContent    `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError"`
}

// fields returns the result's JSON object: the structured content when the
// driver sends it, otherwise the first text item that parses as an object.
func (r *cuaResult) fields() map[string]any {
	var fields map[string]any
	if len(r.StructuredContent) > 0 && json.Unmarshal(r.StructuredContent, &fields) == nil {
		return fields
	}
	for _, item := range r.Content {
		if item.Type == "text" && json.Unmarshal([]byte(item.Text), &fields) == nil {
			return fields
		}
	}
	return map[string]any{}
}

func (r *cuaResult) text() string {
	parts := []string{}
	for _, item := range r.Content {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (r *cuaResult) image() *cuaContent {
	for i := range r.Content {
		if r.Content[i].Type == "image" && r.Content[i].Data != "" {
			return &r.Content[i]
		}
	}
	return nil
}

type cuaClient struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	nextID  int64
	schemas map[string]json.RawMessage
}

func startCuaClient(ctx context.Context, driver string) (*cuaClient, error) {
	if driver == "" {
		return nil, errors.New("cua-driver path is not configured")
	}
	cmd := exec.Command(driver, "mcp")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cua-driver: %w", err)
	}
	client := &cuaClient{cmd: cmd, stdin: stdin, lines: make(chan []byte, 16), schemas: map[string]json.RawMessage{}}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for scanner.Scan() {
			client.lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(client.lines)
	}()
	if _, err := client.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "weave-device", "version": "1"},
	}); err != nil {
		client.close()
		return nil, fmt.Errorf("initialize cua-driver: %w", err)
	}
	if err := client.notify("notifications/initialized"); err != nil {
		client.close()
		return nil, err
	}
	listed, err := client.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		client.close()
		return nil, fmt.Errorf("list cua-driver tools: %w", err)
	}
	var tools struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed, &tools); err != nil {
		client.close()
		return nil, fmt.Errorf("read cua-driver tools: %w", err)
	}
	for _, tool := range tools.Tools {
		client.schemas[tool.Name] = tool.InputSchema
	}
	return client, nil
}

func (c *cuaClient) has(tool string) bool {
	_, ok := c.schemas[tool]
	return ok
}

// param names the first of candidates the tool's input schema declares.
func (c *cuaClient) param(tool string, candidates ...string) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(c.schemas[tool], &schema)
	for _, name := range candidates {
		if _, ok := schema.Properties[name]; ok {
			return name
		}
	}
	return candidates[0]
}

func (c *cuaClient) call(ctx context.Context, tool string, args map[string]any) (*cuaResult, error) {
	if !c.has(tool) {
		return nil, fmt.Errorf("cua-driver has no %s tool", tool)
	}
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, err
	}
	var result cuaResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("read %s result: %w", tool, err)
	}
	return &result, nil
}

func (c *cuaClient) notify(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method})
}

func (c *cuaClient) write(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(encoded, '\n'))
	return err
}

func (c *cuaClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("cua-driver %s: %w", method, err)
	}
	timer := time.NewTimer(cuaCallTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("cua-driver %s timed out", method)
		case line, open := <-c.lines:
			if !open {
				return nil, errors.New("cua-driver exited")
			}
			var reply struct {
				ID     *int64          `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			// Notifications and log lines are not the answer; keep reading.
			if json.Unmarshal(line, &reply) != nil || reply.ID == nil || *reply.ID != id {
				continue
			}
			if reply.Error != nil {
				return nil, fmt.Errorf("cua-driver %s: %s", method, reply.Error.Message)
			}
			return reply.Result, nil
		}
	}
}

func (c *cuaClient) close() {
	if c == nil {
		return
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}
