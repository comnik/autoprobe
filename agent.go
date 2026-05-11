package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/comnik/autoprobe/internal/provider"
	"golang.org/x/sync/errgroup"
)

const (
	idleBackoffInitial    = 1 * time.Second
	idleBackoffMax        = 30 * time.Second
	maxProgramConcurrency = 8
)

func NewAgent(prov provider.Provider, root, goal string, debug bool) *Agent {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	programsDir := filepath.Join(root, "programs")
	reinforcementDir := filepath.Join(root, "reinforcement")
	return &Agent{
		provider:         prov,
		programsDir:      programsDir,
		reinforcementDir: reinforcementDir,
		goal:             goal,
		tools:            DefaultTools,
		debug:            debug,
	}
}

type Agent struct {
	provider         provider.Provider
	programsDir      string
	reinforcementDir string
	goal             string
	tools            []ToolDefinition
	debug            bool

	conversation []provider.Message
	iteration    int

	// Carried across Steps: lastStopReason controls whether the prior
	// assistant/tool history is preserved (tool-using cycle) or thrown away
	// (cycle ended on StopEnd). lastSent is the conversation we last handed
	// to the provider — when the freshly reconstructed conversation matches
	// it byte-for-byte, the next Step idles instead of re-querying the model.
	lastStopReason provider.StopReason
	lastSent       []provider.Message
	idleBackoff    time.Duration
}

func (a *Agent) Conversation() []provider.Message { return a.conversation }
func (a *Agent) Iteration() int                   { return a.iteration }
func (a *Agent) StepThrough() bool                { return a.debug }
func (a *Agent) Provider() provider.Provider      { return a.provider }

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

// Step runs a single inference + tool execution iteration. The conversation
// is reconstructed at the start of every Step by re-running the programs;
// assistant/tool history from prior Steps is preserved only while the model
// is mid tool-using cycle (StopToolUse). When the reconstructed conversation
// matches the previous one byte-for-byte (programs produced identical output
// and there's no new history), the Step idles with exponential backoff
// rather than re-querying the model. The agent never auto-terminates —
// done is always false on success.
func (a *Agent) Step(ctx context.Context) (provider.AssistantMessage, bool, error) {
	for {
		var history []provider.Message
		if a.lastStopReason == provider.StopToolUse && len(a.conversation) > 1 {
			history = append(history, a.conversation[1:]...)
		}
		fresh, err := a.buildConversation(ctx)
		if err != nil {
			return provider.AssistantMessage{}, false, err
		}
		a.conversation = append(fresh, history...)

		if !conversationsEqual(a.conversation, a.lastSent) {
			a.idleBackoff = 0
			break
		}

		d := a.nextIdleBackoff()
		select {
		case <-time.After(d):
			continue
		case <-ctx.Done():
			return provider.AssistantMessage{}, false, ctx.Err()
		}
	}

	c := provider.Context{
		Messages: a.conversation,
		Tools:    a.toolSchemas(),
	}
	msg, err := a.provider.Generate(ctx, "", c, provider.Options{MaxTokens: 8192})
	if err != nil {
		return provider.AssistantMessage{}, false, err
	}
	if msg.StopReason == provider.StopError {
		return msg, false, fmt.Errorf("provider error: %s", msg.Err)
	}
	if msg.StopReason == provider.StopMaxTokens {
		// The trailing block was likely cut off mid-stream. Tool-call
		// arguments are JSON and unsafe to execute when partial, so drop
		// a trailing ToolCall. Truncated text/thinking blocks are kept
		// as-is — text is still readable, and unsigned thinking is
		// filtered on replay by the provider layer.
		if n := len(msg.Content); n > 0 {
			if _, ok := msg.Content[n-1].(provider.ToolCall); ok {
				msg.Content = msg.Content[:n-1]
			}
		}
		// If complete tool calls remain, treat the turn as mid-cycle so
		// the next Step preserves the assistant + tool-result history
		// and lets the model continue from where it was cut off.
		if hasToolCall(msg.Content) {
			msg.StopReason = provider.StopToolUse
		}
	}
	a.iteration++
	a.lastSent = a.conversation
	a.lastStopReason = msg.StopReason
	a.conversation = append(a.conversation, msg)

	for _, c := range msg.Content {
		if call, ok := c.(provider.ToolCall); ok {
			a.conversation = append(a.conversation, a.executeTool(call))
		}
	}
	return msg, false, nil
}

func (a *Agent) toolSchemas() []provider.ToolDefinition {
	out := make([]provider.ToolDefinition, len(a.tools))
	for i, t := range a.tools {
		out[i] = provider.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}
	}
	return out
}

func (a *Agent) nextIdleBackoff() time.Duration {
	if a.idleBackoff == 0 {
		a.idleBackoff = idleBackoffInitial
	} else {
		a.idleBackoff *= 2
		if a.idleBackoff > idleBackoffMax {
			a.idleBackoff = idleBackoffMax
		}
	}
	return a.idleBackoff
}

func hasToolCall(content []provider.AssistantContent) bool {
	for _, c := range content {
		if _, ok := c.(provider.ToolCall); ok {
			return true
		}
	}
	return false
}

func conversationsEqual(a, b []provider.Message) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ja, jb)
}

func (a *Agent) executeTool(call provider.ToolCall) provider.ToolResultMessage {
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
		return provider.ToolResultMessage{
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Content:    []provider.TextContent{{Text: "tool not found"}},
			IsError:    true,
		}
	}

	response, err := toolDef.Function(call.Arguments)
	isError := err != nil
	if isError {
		response = err.Error()
	}
	if r := a.readReinforcement(call); r != "" {
		response = response + "\n\n" + r
	}
	return provider.ToolResultMessage{
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    []provider.TextContent{{Text: response}},
		IsError:    isError,
	}
}

// readReinforcement executes every program in reinforcement/<tool>/, piping
// the tool call's argument JSON to each on stdin and exporting
// $AUTOPROBE_PROGRAMS_DIR. Non-empty stdout from each program is joined with
// blank lines. Missing tool dirs, missing executables, and program errors
// silently contribute nothing — the reinforcement layer must never block a
// tool result.
func (a *Agent) readReinforcement(call provider.ToolCall) string {
	dir := filepath.Join(a.reinforcementDir, call.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	args := call.Arguments
	if len(args) == 0 {
		args = []byte("{}")
	}
	env := append(os.Environ(), "AUTOPROBE_PROGRAMS_DIR="+a.programsDir)

	var parts []string
	for _, name := range names {
		cmd := exec.Command(filepath.Join(dir, name))
		cmd.Stdin = bytes.NewReader(args)
		cmd.Env = env
		out, runErr := cmd.Output()
		if runErr != nil {
			continue
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (a *Agent) buildConversation(ctx context.Context) ([]provider.Message, error) {
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

	// Programs run concurrently into indexed slots so ordering stays
	// deterministic (sorted by filename) regardless of completion order.
	outputs := make([][]byte, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxProgramConcurrency)
	for i, name := range names {
		g.Go(func() error {
			path := filepath.Join(a.programsDir, name)
			out, runErr := exec.CommandContext(gctx, path).CombinedOutput()

			var exitErr *exec.ExitError
			if runErr != nil && !errors.As(runErr, &exitErr) {
				return fmt.Errorf("running %s: %w", name, runErr)
			}
			exitCode := 0
			if exitErr != nil {
				exitCode = exitErr.ExitCode()
			}
			header := fmt.Sprintf("[program=%s exit=%d]\n", name, exitCode)
			outputs[i] = append([]byte(header), out...)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	contents := make([]provider.TextContent, 0, len(outputs)+1)
	for _, out := range outputs {
		contents = append(contents, provider.TextContent{Text: string(out)})
	}
	if a.goal != "" {
		contents = append(contents, provider.TextContent{Text: "[YOUR GOAL]\n" + a.goal})
	}

	return []provider.Message{provider.UserMessage{Content: contents}}, nil
}
