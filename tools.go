package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

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
		},
		"required": []string{"command"},
	},
	Function: runBash,
}

func runBash(input json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	out, err := exec.Command("bash", "-c", args.Command).CombinedOutput()
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
