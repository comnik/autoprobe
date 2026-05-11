package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptedProvider is a Provider that replays a fixed sequence of
// AssistantMessages and records the Context it was called with on each
// invocation. The recorded contexts let tests assert exactly what the
// agent sent to the model on each iteration.
type scriptedProvider struct {
	responses []AssistantMessage
	calls     []Context
}

func (p *scriptedProvider) Name() string         { return "scripted" }
func (p *scriptedProvider) DefaultModel() string { return "test-model" }

func (p *scriptedProvider) Generate(_ context.Context, _ string, c Context, _ Options) (AssistantMessage, error) {
	// Snapshot the messages slice so subsequent mutations of a.conversation
	// can't change what we captured.
	snap := make([]Message, len(c.Messages))
	copy(snap, c.Messages)
	p.calls = append(p.calls, Context{
		SystemPrompt: c.SystemPrompt,
		Messages:     snap,
		Tools:        c.Tools,
	})
	idx := len(p.calls) - 1
	if idx >= len(p.responses) {
		return AssistantMessage{}, fmt.Errorf("scriptedProvider: no scripted response for call %d", idx+1)
	}
	return p.responses[idx], nil
}

// counterProgram writes a monotonically increasing count to a sibling file
// and echoes "step=N" on each invocation. Using distinct output every Step
// keeps the idle-skip path out of the picture for history-management tests.
const counterProgram = `#!/bin/sh
DIR="$(cd "$(dirname "$0")" && pwd)"
COUNT_FILE="$DIR/../.count"
N=$(cat "$COUNT_FILE" 2>/dev/null || echo 0)
N=$((N + 1))
echo "$N" > "$COUNT_FILE"
echo "step=$N"
`

func newTestAgent(t *testing.T, prov Provider) *Agent {
	t.Helper()
	root := t.TempDir()
	progDir := filepath.Join(root, "programs")
	if err := os.MkdirAll(progDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "reinforcement"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(progDir, "counter"), []byte(counterProgram), 0755); err != nil {
		t.Fatal(err)
	}
	return NewAgent(prov, root, "", false)
}

func bashToolCall(id, cmd string) ToolCall {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return ToolCall{ID: id, Name: "bash", Arguments: args}
}

// runSteps drives Step n times, failing fast on errors or premature
// termination.
func runSteps(t *testing.T, a *Agent, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, done, err := a.Step(ctx)
		if err != nil {
			t.Fatalf("Step %d: %v", i+1, err)
		}
		if done {
			t.Fatalf("Step %d returned done=true; agent should never auto-terminate", i+1)
		}
	}
}

func TestStepKeepsHistoryAcrossToolUseCycle(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{Content: []AssistantContent{bashToolCall("c1", "true")}, StopReason: StopToolUse},
			{Content: []AssistantContent{bashToolCall("c2", "true")}, StopReason: StopToolUse},
			{Content: []AssistantContent{TextContent{Text: "done"}}, StopReason: StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 3)

	if got := len(prov.calls); got != 3 {
		t.Fatalf("provider call count: got %d, want 3", got)
	}

	// Each Step within the tool-using cycle should preserve all prior
	// assistant + tool-result pairs and refresh the leading user message.
	wantLens := []int{1, 3, 5}
	for i, want := range wantLens {
		if got := len(prov.calls[i].Messages); got != want {
			t.Fatalf("call %d messages: got %d, want %d", i+1, got, want)
		}
	}

	// The first message of each call must be a freshly built UserMessage
	// reflecting the latest program run (counter step=i+1), not a recycled
	// one from Prime or a prior Step.
	for i, c := range prov.calls {
		u, ok := c.Messages[0].(UserMessage)
		if !ok {
			t.Fatalf("call %d: first message is %T, want UserMessage", i+1, c.Messages[0])
		}
		want := fmt.Sprintf("step=%d", i+1)
		if got := joinText(u.Content); !strings.Contains(got, want) {
			t.Fatalf("call %d: user message missing %q (got %q)", i+1, want, got)
		}
	}

	// Within the tool cycle, the assistant + tool-result blocks from
	// earlier Steps must survive verbatim into later calls.
	if _, ok := prov.calls[1].Messages[1].(AssistantMessage); !ok {
		t.Fatalf("call 2 message 2: got %T, want AssistantMessage from Step 1", prov.calls[1].Messages[1])
	}
	if _, ok := prov.calls[1].Messages[2].(ToolResultMessage); !ok {
		t.Fatalf("call 2 message 3: got %T, want ToolResultMessage from Step 1", prov.calls[1].Messages[2])
	}
}

func TestStepResetsHistoryAfterStopEnd(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{Content: []AssistantContent{TextContent{Text: "first turn"}}, StopReason: StopEnd},
			{Content: []AssistantContent{TextContent{Text: "second turn"}}, StopReason: StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 2)

	if got := len(prov.calls); got != 2 {
		t.Fatalf("provider call count: got %d, want 2", got)
	}

	// After a StopEnd, the next Step must start fresh: only the rebuilt
	// user message, no carryover of the previous assistant turn.
	if got := len(prov.calls[1].Messages); got != 1 {
		t.Fatalf("call 2 messages: got %d, want 1 (history reset after StopEnd)", got)
	}
	if _, ok := prov.calls[1].Messages[0].(UserMessage); !ok {
		t.Fatalf("call 2 first message is %T, want UserMessage", prov.calls[1].Messages[0])
	}
}

// TestStepRebuildsUserMessageWithoutCarryover isolates the "context is
// rebuilt every Step and prior assistant turns don't leak through" property
// from the "never auto-terminate" property. It tolerates Step returning
// done=true on StopEnd so that the inner assertions actually run on pre-fix
// agents — making this the test that specifically pinpoints the carryover
// regression.
func TestStepRebuildsUserMessageWithoutCarryover(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{Content: []AssistantContent{TextContent{Text: "first"}}, StopReason: StopEnd},
			{Content: []AssistantContent{TextContent{Text: "second"}}, StopReason: StopEnd},
		},
	}
	a := newTestAgent(t, prov)
	ctx := context.Background()

	// Drive Step twice but ignore the done flag — pre-fix agents return
	// done=true on the first StopEnd, and we want the inner assertions to
	// run anyway.
	for i := 0; i < 2; i++ {
		if _, _, err := a.Step(ctx); err != nil {
			t.Fatalf("Step %d: %v", i+1, err)
		}
	}

	if got := len(prov.calls); got != 2 {
		t.Fatalf("provider call count: got %d, want 2", got)
	}

	// Pinpoint assertion: after a StopEnd, the next Step must rebuild the
	// context from programs alone. Pre-fix agents either keep no user
	// message at all (no Prime, no rebuild) or append asst1 to the running
	// conversation; both diverge from [freshUserMessage].
	got := prov.calls[1].Messages
	if len(got) != 1 {
		t.Fatalf("call 2 messages: got %d (%s), want 1 fresh UserMessage", len(got), describe(got))
	}
	u, ok := got[0].(UserMessage)
	if !ok {
		t.Fatalf("call 2 first message is %T (%s), want UserMessage", got[0], describe(got))
	}
	if text := joinText(u.Content); !strings.Contains(text, "step=2") {
		t.Fatalf("call 2 user message missing step=2 (programs not re-run between Steps?): %q", text)
	}
}

// TestStepMaxTokensDropsTrailingToolCall covers the case where the model
// hit max_tokens with a partial trailing tool_use block. Its arguments may
// be malformed JSON, so we must not execute it. With no other tool calls
// in the message, the turn behaves like StopEnd: the next Step rebuilds
// from programs alone.
func TestStepMaxTokensDropsTrailingToolCall(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{
				Content: []AssistantContent{
					TextContent{Text: "thinking out loud"},
					bashToolCall("partial", "true"),
				},
				StopReason: StopMaxTokens,
			},
			{Content: []AssistantContent{TextContent{Text: "fresh start"}}, StopReason: StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 2)

	// The truncated tool call must not have been executed: no
	// ToolResultMessage for it should appear in the second call's context.
	for _, m := range prov.calls[1].Messages {
		if tr, ok := m.(ToolResultMessage); ok && tr.ToolCallID == "partial" {
			t.Fatalf("partial tool_use was executed; ToolResultMessage with ID %q leaked into next turn", tr.ToolCallID)
		}
	}
	// With no surviving tool calls, history resets — second call sees
	// only the freshly built user message.
	if got := len(prov.calls[1].Messages); got != 1 {
		t.Fatalf("call 2 messages: got %d (%s), want 1 (history reset)", got, describe(prov.calls[1].Messages))
	}
}

// TestStepMaxTokensExecutesCompleteCallsAndPreservesHistory covers the case
// where the model emitted one or more complete tool_use blocks before
// max_tokens cut off a trailing partial one. The complete calls must
// execute, the trailing partial must not, and the turn must continue as
// a tool-using cycle so the model can see the tool results next Step.
func TestStepMaxTokensExecutesCompleteCallsAndPreservesHistory(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{
				Content: []AssistantContent{
					bashToolCall("done", "true"),
					bashToolCall("partial", "true"),
				},
				StopReason: StopMaxTokens,
			},
			{Content: []AssistantContent{TextContent{Text: "ok"}}, StopReason: StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 2)

	// Call 2 must preserve the assistant message (with the partial call
	// stripped, leaving one ToolCall) and the tool result for "done".
	msgs := prov.calls[1].Messages
	if got := len(msgs); got != 3 {
		t.Fatalf("call 2 messages: got %d (%s), want 3 (user + asst + tool_result)", got, describe(msgs))
	}
	asst, ok := msgs[1].(AssistantMessage)
	if !ok {
		t.Fatalf("call 2 message 2: got %T, want AssistantMessage", msgs[1])
	}
	calls := 0
	for _, c := range asst.Content {
		if tc, ok := c.(ToolCall); ok {
			calls++
			if tc.ID == "partial" {
				t.Fatalf("partial tool_use survived into next turn's assistant message")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("call 2 assistant message tool calls: got %d, want 1 (the complete one)", calls)
	}
	tr, ok := msgs[2].(ToolResultMessage)
	if !ok {
		t.Fatalf("call 2 message 3: got %T, want ToolResultMessage", msgs[2])
	}
	if tr.ToolCallID != "done" {
		t.Fatalf("call 2 tool result ID: got %q, want %q", tr.ToolCallID, "done")
	}
}

// describe renders a Messages slice as a compact "Type, Type, ..." list,
// purely for failure diagnostics.
func describe(msgs []Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = fmt.Sprintf("%T", m)
	}
	return strings.Join(parts, ", ")
}

func TestStepMixedCycleResetsOnlyAfterStopEnd(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []AssistantMessage{
			{Content: []AssistantContent{bashToolCall("c1", "true")}, StopReason: StopToolUse},
			{Content: []AssistantContent{TextContent{Text: "wrapping up"}}, StopReason: StopEnd},
			{Content: []AssistantContent{bashToolCall("c2", "true")}, StopReason: StopToolUse},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 3)

	if got := len(prov.calls); got != 3 {
		t.Fatalf("provider call count: got %d, want 3", got)
	}
	// Call 1: just the user message.
	if got := len(prov.calls[0].Messages); got != 1 {
		t.Fatalf("call 1 messages: got %d, want 1", got)
	}
	// Call 2: user + asst1 + result1 (still mid-cycle from Step 1's StopToolUse).
	if got := len(prov.calls[1].Messages); got != 3 {
		t.Fatalf("call 2 messages: got %d, want 3", got)
	}
	// Call 3: history wiped after Step 2's StopEnd; just the fresh user message.
	if got := len(prov.calls[2].Messages); got != 1 {
		t.Fatalf("call 3 messages: got %d, want 1 (history reset after StopEnd)", got)
	}
}
