package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultBashTimeoutMillis bounds every bash tool call. Without it, a single
// agent-issued command that doesn't return (ncurses programs, `tail -f`, a
// stuck network call) would freeze the whole eval loop.
const DefaultBashTimeoutMillis = 60_000

type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema; each provider translates to its native shape
	Function    func(input json.RawMessage) (string, error)
}

var DefaultTools = []ToolDefinition{ReadTool, BashTool, EditTool, WriteTool}

var ReadTool = ToolDefinition{
	Name:        "read",
	Description: "Read the contents of a file at the given path. Returns the file's text contents.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to read.",
			},
		},
		"required": []string{"path"},
	},
	Function: readFile,
}

func readFile(input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

var BashTool = ToolDefinition{
	Name:        "bash",
	Description: "Execute a bash command and return the combined stdout and stderr.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Bash command to execute.",
			},
			"timeout": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf(
					"Optional timeout in milliseconds (must be > 0). Defaults to %d. "+
						"On expiry the whole process group is killed.",
					DefaultBashTimeoutMillis,
				),
				"minimum": 1,
			},
		},
		"required": []string{"command"},
	},
	Function: runBash,
}

func runBash(input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	timeoutMs := args.Timeout
	if timeoutMs <= 0 {
		timeoutMs = DefaultBashTimeoutMillis
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", args.Command)
	// Put bash and its descendants in a fresh process group so we can kill the
	// whole tree on timeout. Otherwise SIGKILL hits bash but leaves ncurses
	// children (e.g. cmatrix) running, draining the eval host.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative pid = signal the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Backstop: if Cancel returns but some descendant is still holding stdio
	// pipes open, Wait would otherwise hang indefinitely. Give it a grace
	// window then force-return whatever output we collected.
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("timed out after %dms (process group killed)", timeoutMs)
	}
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

var EditTool = ToolDefinition{
	Name:        "edit",
	Description: "Replace exactly one occurrence of old_string with new_string in the file at path. Fails if old_string is missing or appears more than once.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to edit.",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "Exact text to be replaced.",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "Replacement text.",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	},
	Function: editFile,
}

func editFile(input json.RawMessage) (string, error) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	content := string(data)
	count := strings.Count(content, args.OldString)
	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", args.Path)
	}
	if count > 1 {
		return "", fmt.Errorf("old_string appears %d times in %s; must be unique", count, args.Path)
	}
	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(args.Path, []byte(updated), 0644); err != nil {
		return "", err
	}
	return "ok", nil
}

var WriteTool = ToolDefinition{
	Name:        "write",
	Description: "Create a new file or overwrite an existing file with the given content.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Content to write to the file.",
			},
		},
		"required": []string{"path", "content"},
	},
	Function: writeFile,
}

func writeFile(input json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if err := os.WriteFile(args.Path, []byte(args.Content), 0644); err != nil {
		return "", err
	}
	return "ok", nil
}
