package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

func NewAgent(client *anthropic.Client, root, goal string, debug bool) *Agent {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	programsDir := filepath.Join(root, "programs")
	reinforcementDir := filepath.Join(root, "reinforcement")
	return &Agent{
		client:           client,
		programsDir:      programsDir,
		reinforcementDir: reinforcementDir,
		goal:             goal,
		tools:            DefaultTools,
		debug:            debug,
	}
}

func (a *Agent) expandVar(name string) string {
	switch name {
	case "HOPPER_PROGRAMS_DIR":
		return a.programsDir
	}
	return ""
}

type Agent struct {
	client           *anthropic.Client
	programsDir      string
	reinforcementDir string
	goal             string
	tools            []ToolDefinition
	debug            bool

	conversation []anthropic.MessageParam
	iteration    int
}

func (a *Agent) Conversation() []anthropic.MessageParam { return a.conversation }
func (a *Agent) Iteration() int                         { return a.iteration }
func (a *Agent) StepThrough() bool                      { return a.debug }

func (a *Agent) Run(ctx context.Context) error {
	return runTUI(ctx, a)
}

// Prime builds the initial conversation. Must be called once before Step.
func (a *Agent) Prime(ctx context.Context) error {
	c, err := a.buildConversation(ctx)
	if err != nil {
		return err
	}
	a.conversation = c
	return nil
}

// Step runs a single inference + tool execution iteration. Returns the
// assistant message produced and whether the agent has finished (no further
// tool calls).
func (a *Agent) Step(ctx context.Context) (*anthropic.Message, bool, error) {
	message, err := a.runInference(ctx, a.conversation)
	if err != nil {
		return nil, false, err
	}
	if message.StopReason == anthropic.StopReasonMaxTokens {
		return message, false, fmt.Errorf("response hit the max_tokens limit; bump the budget in runInference")
	}
	a.iteration++
	a.conversation = append(a.conversation, message.ToParam())

	toolResults := []anthropic.ContentBlockParamUnion{}
	for _, content := range message.Content {
		if content.Type == "tool_use" {
			result := a.executeTool(content.ID, content.Name, content.Input)
			toolResults = append(toolResults, result)
		}
	}
	if len(toolResults) == 0 {
		return message, true, nil
	}
	a.conversation = append(a.conversation, anthropic.NewUserMessage(toolResults...))
	return message, false, nil
}

func (a *Agent) executeTool(id, name string, input json.RawMessage) anthropic.ContentBlockParamUnion {
	var toolDef ToolDefinition
	var found bool
	for _, tool := range a.tools {
		if tool.Name == name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return anthropic.NewToolResultBlock(id, "tool not found", true)
	}

	response, err := toolDef.Function(input)
	isError := err != nil
	if isError {
		response = err.Error()
	}
	if r := a.readReinforcement(name); r != "" {
		response = response + "\n\n" + r
	}
	return anthropic.NewToolResultBlock(id, response, isError)
}

func (a *Agent) readReinforcement(name string) string {
	data, err := os.ReadFile(filepath.Join(a.reinforcementDir, name+".md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(os.Expand(string(data), a.expandVar))
}

func (a *Agent) buildConversation(ctx context.Context) ([]anthropic.MessageParam, error) {
	entries, err := os.ReadDir(a.programsDir)
	if err != nil {
		return nil, fmt.Errorf("reading programs dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Indexed slots, not append, so this can be parallelized later without changing
	// ordering.
	outputs := make([][]byte, len(names))
	for i, name := range names {
		path := filepath.Join(a.programsDir, name)
		out, runErr := exec.CommandContext(ctx, path).CombinedOutput()

		var exitErr *exec.ExitError
		if runErr != nil && !errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("running %s: %w", name, runErr)
		}
		exitCode := 0
		if exitErr != nil {
			exitCode = exitErr.ExitCode()
		}
		header := fmt.Sprintf("[program=%s exit=%d]\n", name, exitCode)
		outputs[i] = append([]byte(header), out...)
	}

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(outputs)+1)
	for _, out := range outputs {
		blocks = append(blocks, anthropic.NewTextBlock(string(out)))
	}
	if a.goal != "" {
		blocks = append(blocks, anthropic.NewTextBlock("[YOUR GOAL]\n"+a.goal))
	}

	return []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)}, nil
}

func (a *Agent) runInference(ctx context.Context, conversation []anthropic.MessageParam) (*anthropic.Message, error) {
	tools := make([]anthropic.ToolUnionParam, len(a.tools))
	for i, t := range a.tools {
		tools[i] = t.AsParam()
	}

	message, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeOpus4_7,
		MaxTokens: 8192,
		Messages:  conversation,
		Tools:     tools,
	})
	return message, err
}
