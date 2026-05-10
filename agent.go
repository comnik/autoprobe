package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func NewAgent(provider Provider, root, goal string, debug bool) *Agent {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	programsDir := filepath.Join(root, "programs")
	reinforcementDir := filepath.Join(root, "reinforcement")
	return &Agent{
		provider:         provider,
		programsDir:      programsDir,
		reinforcementDir: reinforcementDir,
		goal:             goal,
		tools:            DefaultTools,
		debug:            debug,
	}
}

func (a *Agent) expandVar(name string) string {
	switch name {
	case "AUTOPROBE_PROGRAMS_DIR":
		return a.programsDir
	}
	return ""
}

type Agent struct {
	provider         Provider
	programsDir      string
	reinforcementDir string
	goal             string
	tools            []ToolDefinition
	debug            bool

	conversation []Message
	iteration    int
}

func (a *Agent) Conversation() []Message { return a.conversation }
func (a *Agent) Iteration() int          { return a.iteration }
func (a *Agent) StepThrough() bool       { return a.debug }
func (a *Agent) Provider() Provider      { return a.provider }

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
func (a *Agent) Step(ctx context.Context) (AssistantMessage, bool, error) {
	c := Context{
		Messages: a.conversation,
		Tools:    a.tools,
	}
	msg, err := a.provider.Generate(ctx, "", c, Options{MaxTokens: 8192})
	if err != nil {
		return AssistantMessage{}, false, err
	}
	if msg.StopReason == StopMaxTokens {
		return msg, false, fmt.Errorf("response hit the max_tokens limit; bump the budget in Step")
	}
	if msg.StopReason == StopError {
		return msg, false, fmt.Errorf("provider error: %s", msg.Err)
	}
	a.iteration++
	a.conversation = append(a.conversation, msg)

	var results []Message
	for _, c := range msg.Content {
		if call, ok := c.(ToolCall); ok {
			results = append(results, a.executeTool(call))
		}
	}
	if len(results) == 0 {
		return msg, true, nil
	}
	a.conversation = append(a.conversation, results...)
	return msg, false, nil
}

func (a *Agent) executeTool(call ToolCall) ToolResultMessage {
	var toolDef ToolDefinition
	var found bool
	for _, tool := range a.tools {
		if tool.Name == call.Name {
			toolDef = tool
			found = true
			break
		}
	}
	if !found {
		return ToolResultMessage{
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Content:    []TextContent{{Text: "tool not found"}},
			IsError:    true,
		}
	}

	response, err := toolDef.Function(call.Arguments)
	isError := err != nil
	if isError {
		response = err.Error()
	}
	if r := a.readReinforcement(call.Name); r != "" {
		response = response + "\n\n" + r
	}
	return ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    []TextContent{{Text: response}},
		IsError:    isError,
	}
}

func (a *Agent) readReinforcement(name string) string {
	data, err := os.ReadFile(filepath.Join(a.reinforcementDir, name+".md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(os.Expand(string(data), a.expandVar))
}

func (a *Agent) buildConversation(ctx context.Context) ([]Message, error) {
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

	contents := make([]TextContent, 0, len(outputs)+1)
	for _, out := range outputs {
		contents = append(contents, TextContent{Text: string(out)})
	}
	if a.goal != "" {
		contents = append(contents, TextContent{Text: "[YOUR GOAL]\n" + a.goal})
	}

	return []Message{UserMessage{Content: contents}}, nil
}

