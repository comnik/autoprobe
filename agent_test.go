package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comnik/autoprobe/internal/provider"
)

// scriptedProvider is a Provider that replays a fixed sequence of
// AssistantMessages and records the Context it was called with on each
// invocation. The recorded contexts let tests assert exactly what the
// agent sent to the model on each iteration.
type scriptedProvider struct {
	responses []provider.AssistantMessage
	calls     []provider.Context
}

func (p *scriptedProvider) Name() string         { return "scripted" }
func (p *scriptedProvider) DefaultModel() string { return "test-model" }

func (p *scriptedProvider) Generate(_ context.Context, _ string, c provider.Context, _ provider.Options) (provider.AssistantMessage, error) {
	// Snapshot the messages slice so subsequent mutations of a.conversation
	// can't change what we captured.
	snap := make([]provider.Message, len(c.Messages))
	copy(snap, c.Messages)
	p.calls = append(p.calls, provider.Context{
		SystemPrompt: c.SystemPrompt,
		Messages:     snap,
		Tools:        c.Tools,
	})
	idx := len(p.calls) - 1
	if idx >= len(p.responses) {
		return provider.AssistantMessage{}, fmt.Errorf("scriptedProvider: no scripted response for call %d", idx+1)
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

func newTestAgent(t *testing.T, prov provider.Provider) *Agent {
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
	return NewAgent(prov, root, "", 0)
}

func bashToolCall(id, cmd string) provider.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return provider.ToolCall{ID: id, Name: "bash", Arguments: args}
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
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{bashToolCall("c2", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "done"}}, StopReason: provider.StopEnd},
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
		u, ok := c.Messages[0].(provider.UserMessage)
		if !ok {
			t.Fatalf("call %d: first message is %T, want UserMessage", i+1, c.Messages[0])
		}
		want := fmt.Sprintf("step=%d", i+1)
		if got := provider.JoinText(u.Content); !strings.Contains(got, want) {
			t.Fatalf("call %d: user message missing %q (got %q)", i+1, want, got)
		}
	}

	// Within the tool cycle, the assistant + tool-result blocks from
	// earlier Steps must survive verbatim into later calls.
	if _, ok := prov.calls[1].Messages[1].(provider.AssistantMessage); !ok {
		t.Fatalf("call 2 message 2: got %T, want AssistantMessage from Step 1", prov.calls[1].Messages[1])
	}
	if _, ok := prov.calls[1].Messages[2].(provider.ToolResultMessage); !ok {
		t.Fatalf("call 2 message 3: got %T, want ToolResultMessage from Step 1", prov.calls[1].Messages[2])
	}
}

func TestStepResetsHistoryAfterStopEnd(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "first turn"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "second turn"}}, StopReason: provider.StopEnd},
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
	if _, ok := prov.calls[1].Messages[0].(provider.UserMessage); !ok {
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
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "first"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "second"}}, StopReason: provider.StopEnd},
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
	u, ok := got[0].(provider.UserMessage)
	if !ok {
		t.Fatalf("call 2 first message is %T (%s), want UserMessage", got[0], describe(got))
	}
	if text := provider.JoinText(u.Content); !strings.Contains(text, "step=2") {
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
		responses: []provider.AssistantMessage{
			{
				Content: []provider.AssistantContent{
					provider.TextContent{Text: "thinking out loud"},
					bashToolCall("partial", "true"),
				},
				StopReason: provider.StopMaxTokens,
			},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "fresh start"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 2)

	// The truncated tool call must not have been executed: no
	// ToolResultMessage for it should appear in the second call's context.
	for _, m := range prov.calls[1].Messages {
		if tr, ok := m.(provider.ToolResultMessage); ok && tr.ToolCallID == "partial" {
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
		responses: []provider.AssistantMessage{
			{
				Content: []provider.AssistantContent{
					bashToolCall("done", "true"),
					bashToolCall("partial", "true"),
				},
				StopReason: provider.StopMaxTokens,
			},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "ok"}}, StopReason: provider.StopEnd},
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
	asst, ok := msgs[1].(provider.AssistantMessage)
	if !ok {
		t.Fatalf("call 2 message 2: got %T, want AssistantMessage", msgs[1])
	}
	calls := 0
	for _, c := range asst.Content {
		if tc, ok := c.(provider.ToolCall); ok {
			calls++
			if tc.ID == "partial" {
				t.Fatalf("partial tool_use survived into next turn's assistant message")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("call 2 assistant message tool calls: got %d, want 1 (the complete one)", calls)
	}
	tr, ok := msgs[2].(provider.ToolResultMessage)
	if !ok {
		t.Fatalf("call 2 message 3: got %T, want ToolResultMessage", msgs[2])
	}
	if tr.ToolCallID != "done" {
		t.Fatalf("call 2 tool result ID: got %q, want %q", tr.ToolCallID, "done")
	}
}

// describe renders a Messages slice as a compact "Type, Type, ..." list,
// purely for failure diagnostics.
func describe(msgs []provider.Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = fmt.Sprintf("%T", m)
	}
	return strings.Join(parts, ", ")
}

func TestStepMixedCycleResetsOnlyAfterStopEnd(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "wrapping up"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{bashToolCall("c2", "true")}, StopReason: provider.StopToolUse},
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

// writeModelingScript drops a modeling reinforcement script into the agent's
// reinforcement/modeling/ directory. Mirror of writeRevisionScript from
// budget_test.go (kept local rather than exporting that helper).
func writeModelingScript(t *testing.T, a *Agent, name, body string) {
	t.Helper()
	dir := filepath.Join(a.reinforcementDir, modelingReinforcementName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestStepGivesAgentWrapupChanceAfterMaxIterations(t *testing.T) {
	t.Parallel()
	// -n is 1, so iteration 1 hits the cap. We expect a final wrap-up
	// modeling turn (forced trigger) where the user message carries the
	// FINAL guidance block, then termination.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "iter1"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "saved"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)
	a.maxIterations = 1

	ctx := context.Background()
	_, done, err := a.Step(ctx)
	if err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if done {
		t.Fatalf("Step 1 returned done=true; expected the wrap-up modeling turn to still run")
	}
	if a.currentTurnKind != TurnModeling {
		t.Fatalf("after Step 1: currentTurnKind = %v, want TurnModeling (forced trigger should have scheduled the wrap-up)", a.currentTurnKind)
	}

	_, done, err = a.Step(ctx)
	if err != nil {
		t.Fatalf("Step 2: %v", err)
	}
	if !done {
		t.Fatalf("Step 2 returned done=false; expected termination after wrap-up modeling turn")
	}

	if got := len(prov.calls); got != 2 {
		t.Fatalf("provider call count: got %d, want 2", got)
	}
	// The wrap-up modeling turn's user message must carry the FINAL
	// guidance and the prior cycle's transcript.
	u, ok := prov.calls[1].Messages[0].(provider.UserMessage)
	if !ok {
		t.Fatalf("call 2 message 1: got %T, want UserMessage", prov.calls[1].Messages[0])
	}
	got := provider.JoinText(u.Content)
	if !strings.Contains(got, "MODELING GUIDANCE — FINAL") {
		t.Fatalf("call 2 user message missing FINAL guidance: %q", got)
	}
	if !strings.Contains(got, "PRIOR WORK CYCLE TRANSCRIPT") {
		t.Fatalf("call 2 user message missing prior transcript section: %q", got)
	}
	if !strings.Contains(got, "iter1") {
		t.Fatalf("call 2 user message missing prior cycle's assistant text: %q", got)
	}
	// The first call must NOT carry the FINAL framing — only the wrap-up
	// modeling turn does.
	u0, _ := prov.calls[0].Messages[0].(provider.UserMessage)
	if got := provider.JoinText(u0.Content); strings.Contains(got, "MODELING GUIDANCE — FINAL") {
		t.Fatalf("call 1 user message unexpectedly carries FINAL guidance: %q", got)
	}
}

func TestStepWrapupAllowsToolCycleToComplete(t *testing.T) {
	t.Parallel()
	// The wrap-up turn can spend tool calls — termination must wait until the
	// model returns a non-tool-use stop reason so writes to the program
	// library actually land.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "iter1"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{bashToolCall("save", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "saved"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)
	a.maxIterations = 1

	ctx := context.Background()
	// Step 1: hits -n cap, no done yet (wrap-up turn pending).
	if _, done, err := a.Step(ctx); err != nil || done {
		t.Fatalf("Step 1: err=%v done=%v; want err=nil done=false", err, done)
	}
	// Step 2: wrap-up turn, model uses a tool — must not terminate mid-cycle.
	if _, done, err := a.Step(ctx); err != nil || done {
		t.Fatalf("Step 2: err=%v done=%v; want err=nil done=false (mid tool cycle)", err, done)
	}
	// Step 3: model finishes — now we terminate.
	if _, done, err := a.Step(ctx); err != nil || !done {
		t.Fatalf("Step 3: err=%v done=%v; want err=nil done=true", err, done)
	}
}

// usageProvider is a scriptedProvider that also lets the test stamp
// InputTokens on the replayed responses, which is what the per-Step modeling
// threshold check reads.
type usageProvider struct {
	scriptedProvider
	inputTokens []int // parallel to responses
}

func (p *usageProvider) Generate(ctx context.Context, sysPrompt string, c provider.Context, opts provider.Options) (provider.AssistantMessage, error) {
	idx := len(p.calls)
	msg, err := p.scriptedProvider.Generate(ctx, sysPrompt, c, opts)
	if err != nil {
		return msg, err
	}
	if idx < len(p.inputTokens) {
		msg.Usage.InputTokens = p.inputTokens[idx]
	}
	return msg, nil
}

// lastToolResultText returns the trailing TextContent of the most recent
// ToolResultMessage in a provider call's input messages — that's where a
// periodic modeling firing lands and what the model will see right before
// generating its next response.
func lastToolResultText(t *testing.T, msgs []provider.Message) string {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if tr, ok := msgs[i].(provider.ToolResultMessage); ok {
			return provider.JoinText(tr.Content)
		}
	}
	return ""
}

func TestStepFiresPeriodicModelingIntoLastToolResult(t *testing.T) {
	t.Parallel()
	// Step 1's InputTokens crosses the threshold on its own, so the modeling
	// prompt must be appended to Step 1's tool result — Step 2's input then
	// carries it at the tail. Step 2 also crosses the threshold but cooldown
	// suppresses the second firing.
	bigInput := modelingThresholdTokens + 1024
	prov := &usageProvider{
		scriptedProvider: scriptedProvider{
			responses: []provider.AssistantMessage{
				{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
				{Content: []provider.AssistantContent{bashToolCall("c2", "true")}, StopReason: provider.StopToolUse},
				{Content: []provider.AssistantContent{provider.TextContent{Text: "ok"}}, StopReason: provider.StopEnd},
			},
		},
		inputTokens: []int{bigInput, bigInput, 1024},
	}
	a := newTestAgent(t, prov)
	writeModelingScript(t, a, "general.sh",
		"#!/bin/sh\nif [ \"$AUTOPROBE_FINAL\" = \"1\" ]; then echo FINAL-MARKER; else echo MODELING-MARKER; fi\n")

	runSteps(t, a, 3)

	// Call 1 input: just the user message — no tool result yet.
	if got := lastToolResultText(t, prov.calls[0].Messages); got != "" {
		t.Fatalf("call 1 unexpectedly already has a tool result: %q", got)
	}
	// Call 2 input: should contain the tool result from Step 1 with the
	// modeling prompt appended at its tail.
	got2 := lastToolResultText(t, prov.calls[1].Messages)
	if !strings.Contains(got2, "MODELING-MARKER") {
		t.Fatalf("call 2 last tool result missing modeling prompt: %q", got2)
	}
	if strings.Contains(got2, "FINAL-MARKER") {
		t.Fatalf("call 2 carries FINAL framing but this is a periodic firing: %q", got2)
	}
	// Call 3 input: the tool result from Step 2 must NOT carry the prompt —
	// we are inside the cooldown window even though Step 2 also crossed.
	got3 := lastToolResultText(t, prov.calls[2].Messages)
	if strings.Contains(got3, "MODELING-MARKER") {
		t.Fatalf("call 3 last tool result fired during cooldown: %q", got3)
	}
}

func TestStepRunsFreshInferenceAfterCycleEndsEvenIfProgramsStable(t *testing.T) {
	t.Parallel()
	// The first cycle runs StopToolUse → StopEnd. Programs are deterministic
	// (the counter writes via runIteration, but the model itself does no
	// tool calls in the StopEnd step). Without clearing lastOutputHash on
	// the cycle-end transition, Step 3 would hit the idle branch because
	// the program hash hasn't changed since Step 2. With the clear, Step 3
	// runs a fresh inference — that's the "clean next cycle" the model's
	// narrative typically asks for.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "modelinging and yielding"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "still done"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 3)

	if got := len(prov.calls); got != 3 {
		t.Fatalf("provider call count: got %d, want 3 (the third must run despite stable program hash)", got)
	}
}

func TestStepIdlesAfterRepeatedYieldsWithStableHash(t *testing.T) {
	t.Parallel()
	// After the first StopEnd (post-cycle) re-queries, a SECOND StopEnd with
	// no tool calls in between should NOT clear the hash again — otherwise
	// we'd loop forever. The third Step is allowed to idle. We drive the
	// agent until the idle branch fires by giving it a context that expires
	// quickly: a stale program output hash + a StopEnd-after-StopEnd should
	// route into the idle backoff, not into another provider call.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "yield 1"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "yield 2"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 3)

	// After Step 3 returned StopEnd-after-StopEnd, lastOutputHash must be
	// the real hash (not the zero/cleared one), so a hypothetical Step 4
	// would hit the idle branch instead of re-querying. Asserting on the
	// stored hash is the cleanest check: zero means "we would re-query",
	// non-zero means "we'd idle on a stable cycle."
	var zero programHash
	if a.lastOutputHash == zero {
		t.Fatalf("lastOutputHash was cleared after StopEnd-after-StopEnd; idle would never engage and we'd loop")
	}
}

func TestBootstrapModelingTurnFiresOnEmptyLibrary(t *testing.T) {
	t.Parallel()
	// Empty programs/ at Prime time arms the bootstrap modeling turn: the
	// first Step runs as a modeling turn (not a work step) and the user
	// message carries the BOOTSTRAP guidance rather than the regular one.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "installed initial programs"}}, StopReason: provider.StopEnd},
		},
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "programs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "reinforcement"), 0755); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(prov, root, "", 0)

	ctx := context.Background()
	if err := a.Prime(ctx); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if a.currentTurnKind != TurnModeling || !a.needsBootstrap {
		t.Fatalf("after Prime with empty programs/: kind=%v bootstrap=%v, want TurnModeling/true", a.currentTurnKind, a.needsBootstrap)
	}
	if _, _, err := a.Step(ctx); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := len(prov.calls); got != 1 {
		t.Fatalf("provider call count: got %d, want 1", got)
	}
	u, ok := prov.calls[0].Messages[0].(provider.UserMessage)
	if !ok {
		t.Fatalf("call 1 message 1: got %T, want UserMessage", prov.calls[0].Messages[0])
	}
	text := provider.JoinText(u.Content)
	if !strings.Contains(text, "MODELING GUIDANCE — BOOTSTRAP") {
		t.Fatalf("bootstrap modeling turn missing BOOTSTRAP guidance: %q", text)
	}
	if strings.Contains(text, "PRIOR WORK CYCLE TRANSCRIPT") {
		t.Fatalf("bootstrap modeling turn unexpectedly carries a prior transcript section: %q", text)
	}
	// After the modeling turn closes the agent flips back to work.
	if a.currentTurnKind != TurnWork {
		t.Fatalf("after Step: kind=%v, want TurnWork (modeling turn should close)", a.currentTurnKind)
	}
}

func TestModelingTurnFiresAfterYield(t *testing.T) {
	t.Parallel()
	// A work cycle that fires the in-cycle yield reinforcement should
	// schedule a modeling turn between work cycles. The default trigger
	// fires only when the cycle actually used tools.
	bigInput := modelingThresholdTokens + 1024
	prov := &usageProvider{
		scriptedProvider: scriptedProvider{
			responses: []provider.AssistantMessage{
				{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
				{Content: []provider.AssistantContent{provider.TextContent{Text: "yielding"}}, StopReason: provider.StopEnd},
				{Content: []provider.AssistantContent{provider.TextContent{Text: "library curation done"}}, StopReason: provider.StopEnd},
			},
		},
		inputTokens: []int{bigInput, bigInput, 1024},
	}
	a := newTestAgent(t, prov)
	writeModelingScript(t, a, "general.sh",
		"#!/bin/sh\necho YIELD-MARKER\n")

	ctx := context.Background()
	// Step 1: tool call, yield fires (drag > threshold).
	if _, _, err := a.Step(ctx); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if !a.yieldFiredThisCycle {
		t.Fatalf("yieldFiredThisCycle should be set after Step 1's threshold crossing")
	}
	// Step 2: cycle closes (StopEnd). At cycle close, the cadence
	// predicate should schedule a modeling turn for the next Step.
	if _, _, err := a.Step(ctx); err != nil {
		t.Fatalf("Step 2: %v", err)
	}
	if a.currentTurnKind != TurnModeling {
		t.Fatalf("after Step 2 (cycle close after yield): kind=%v, want TurnModeling", a.currentTurnKind)
	}
	// Step 3: the modeling turn runs.
	if _, _, err := a.Step(ctx); err != nil {
		t.Fatalf("Step 3: %v", err)
	}
	if got := len(prov.calls); got != 3 {
		t.Fatalf("provider call count: got %d, want 3", got)
	}
	// Call 3 was the modeling turn: its user message carries the modeling
	// guidance block.
	u, _ := prov.calls[2].Messages[0].(provider.UserMessage)
	if got := provider.JoinText(u.Content); !strings.Contains(got, "MODELING GUIDANCE") {
		t.Fatalf("call 3 (modeling turn) missing guidance: %q", got)
	}
	// Calls 1 and 2 (work steps) must not carry the modeling guidance.
	for i, c := range prov.calls[:2] {
		u, _ := c.Messages[0].(provider.UserMessage)
		if got := provider.JoinText(u.Content); strings.Contains(got, "MODELING GUIDANCE") {
			t.Fatalf("call %d (work step) unexpectedly carries modeling guidance: %q", i+1, got)
		}
	}
}

func TestModelingTurnNotFiredAfterIdleCycle(t *testing.T) {
	t.Parallel()
	// A work cycle that closes without ever invoking a tool (one StopEnd
	// response with no tool calls) is an "idle" cycle — nothing happened
	// that could have moved the model, so the cadence skips it even
	// though the cycle closed.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "no work to do"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	ctx := context.Background()
	if _, _, err := a.Step(ctx); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if a.currentTurnKind != TurnWork {
		t.Fatalf("after no-tool cycle: kind=%v, want TurnWork (modeling should not fire on idle cycles)", a.currentTurnKind)
	}
}

func TestStepClearsModelingCooldownWhenCycleEnds(t *testing.T) {
	t.Parallel()
	// A cycle that fires the modeling prompt and then ends naturally must
	// clear the cooldown — the next cycle starts with a fresh history slate
	// and should be allowed to fire on its own merits.
	bigInput := modelingThresholdTokens + 1024
	prov := &usageProvider{
		scriptedProvider: scriptedProvider{
			responses: []provider.AssistantMessage{
				{Content: []provider.AssistantContent{bashToolCall("c1", "true")}, StopReason: provider.StopToolUse},
				{Content: []provider.AssistantContent{provider.TextContent{Text: "done"}}, StopReason: provider.StopEnd},
			},
		},
		inputTokens: []int{bigInput, 100},
	}
	a := newTestAgent(t, prov)

	runSteps(t, a, 2)

	if a.modelingCooldown != 0 {
		t.Fatalf("modelingCooldown should reset to 0 on cycle end, got %d", a.modelingCooldown)
	}
}
