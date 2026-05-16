package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/comnik/autoprobe/internal/provider"
)

// programSpec describes one program to install in a test agent's programs
// dir: filename and shell-script body. Order doesn't matter — tests assert
// against lex-sorted-by-filename behavior, which mirrors the harness.
type programSpec struct {
	name string
	body string
}

// newAgentWithPrograms builds a test agent rooted in a fresh temp dir,
// installs the given programs, and returns the agent. No provider is wired
// up — callers that exercise runIteration / assembleUserMessage do not need
// one; callers that drive Step pass a provider via SetProvider before use.
func newAgentWithPrograms(t *testing.T, specs ...programSpec) *Agent {
	t.Helper()
	root := t.TempDir()
	progDir := filepath.Join(root, "programs")
	if err := os.MkdirAll(progDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "reinforcement"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if err := os.WriteFile(filepath.Join(progDir, s.name), []byte(s.body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return NewAgent(nil, root, "", 0)
}

// writeInactive writes a .autoprobe/inactive file with the given program
// names, one per line.
func writeInactive(t *testing.T, root string, names ...string) {
	t.Helper()
	body := strings.Join(names, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, inactiveFileName), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// renderUserMessage runs one iteration and returns the concatenated text of
// the resulting UserMessage. Tests grep this string for program output and
// sentinel lines.
func renderUserMessage(t *testing.T, a *Agent) string {
	t.Helper()
	data, err := a.runIteration(context.Background())
	if err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	return provider.JoinText(a.assembleUserMessage(data, false).Content)
}

func TestHashResultsFlipsWhenExitCodeChanges(t *testing.T) {
	t.Parallel()
	// Identical name and stdout, only the exit code differs. The pre-
	// selection hash MUST treat this as a change so a probe flipping from
	// exit 0 to non-zero promotes itself into the exploration slot instead
	// of being eaten by idle backoff.
	zero := []programResult{{name: "p", exitCode: 0, output: []byte("hello\n")}}
	nonzero := []programResult{{name: "p", exitCode: 1, output: []byte("hello\n")}}
	if hashResults(zero) == hashResults(nonzero) {
		t.Fatal("hash should differ when exit code flips with identical stdout")
	}
}

func TestRunProgramsChmodsNonExecutableFile(t *testing.T) {
	t.Parallel()
	// The write tool creates files as 0644, and the agent reliably forgets to
	// chmod +x before the next iteration. runPrograms sets the execute bit
	// itself rather than burning a turn on the fix.
	a := newAgentWithPrograms(t, programSpec{"good", "#!/bin/sh\necho ok\n"})
	bad := filepath.Join(a.programsDir, "bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho ran\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := a.runPrograms(context.Background())
	if err != nil {
		t.Fatalf("runPrograms must not error on a non-executable file: %v", err)
	}
	var got *programResult
	for i := range results {
		if results[i].name == "bad" {
			got = &results[i]
			break
		}
	}
	if got == nil {
		t.Fatal("expected a result row for the non-executable file")
	}
	if got.exitCode != 0 {
		t.Errorf("file should run successfully after auto-chmod; got exit %d, output %q", got.exitCode, got.output)
	}
	if !strings.Contains(string(got.output), "ran") {
		t.Errorf("output should be from the program; got %q", got.output)
	}
	info, err := os.Stat(bad)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("execute bit should be set on disk after run; mode=%v", info.Mode())
	}
}

func TestNoOverflowIncludesEveryProgram(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t,
		programSpec{"aaa", "#!/bin/sh\necho first\n"},
		programSpec{"bbb", "#!/bin/sh\necho second\n"},
	)
	// Default 128K budget; two tiny programs are nowhere near it.
	rendered := renderUserMessage(t, a)
	if !strings.Contains(rendered, "first") {
		t.Errorf("missing first program output: %q", rendered)
	}
	if !strings.Contains(rendered, "second") {
		t.Errorf("missing second program output: %q", rendered)
	}
	if strings.Contains(rendered, "dropped:") {
		t.Errorf("unexpected sentinel under no-overflow: %q", rendered)
	}
}

func TestInactiveZeroExitDoesNotEnterActiveSlot(t *testing.T) {
	t.Parallel()
	// Engineer overflow with a bulky active program so the budget split
	// actually fires. The inactive zero-exit program ("bbb") may or may
	// not be drawn into the random exploration slot, but it must never
	// appear in the active 80% lex-ordered slice — that's the property
	// inactive demotion is meant to guarantee.
	a := newAgentWithPrograms(t,
		programSpec{"aaa-bulk", "#!/bin/sh\nprintf 'A%.0s' $(seq 1 200)\n"},
		programSpec{"bbb-quiet", "#!/bin/sh\necho QUIET-MARKER\n"},
	)
	a.contextBudget = 40 // tokens — small enough to force overflow
	writeInactive(t, a.root, "bbb-quiet")

	data, err := a.runIteration(context.Background())
	if err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if !data.overflowed(a.contextBudget) {
		t.Fatalf("test setup did not produce overflow (total=%d budget=%d)", data.totalTokens, a.contextBudget)
	}

	active, inactive := splitByActive(data.results, data.inactive)
	for _, r := range active {
		if r.name == "bbb-quiet" {
			t.Fatalf("bbb-quiet was demoted in .autoprobe/inactive but ended up in the active slice")
		}
	}
	foundInactive := false
	for _, r := range inactive {
		if r.name == "bbb-quiet" {
			foundInactive = true
		}
	}
	if !foundInactive {
		t.Fatalf("bbb-quiet should be in the inactive partition; got active=%v inactive=%v", active, inactive)
	}
}

func TestInactiveNonzeroExitPromotedIntoExploration(t *testing.T) {
	t.Parallel()
	// An inactive program that exits non-zero must reach the agent's
	// context via the exploration slot's phase 1 — that's how the exit-code
	// contract carries alarms from demoted programs back to the agent.
	a := newAgentWithPrograms(t,
		programSpec{"aaa-bulk", "#!/bin/sh\nprintf 'A%.0s' $(seq 1 200)\n"},
		programSpec{"bbb-active", "#!/bin/sh\necho ACTIVE-MARKER\n"},
		programSpec{"ccc-alarm", "#!/bin/sh\necho ALARM-MARKER\nexit 7\n"},
	)
	a.contextBudget = 80 // large enough that alarm fits in 20% slot, small enough to overflow
	writeInactive(t, a.root, "ccc-alarm")

	rendered := renderUserMessage(t, a)
	if !strings.Contains(rendered, "ACTIVE-MARKER") {
		t.Errorf("active program should always appear: %q", rendered)
	}
	if !strings.Contains(rendered, "ALARM-MARKER") {
		t.Errorf("inactive program with non-zero exit should be promoted into exploration: %q", rendered)
	}
	if !strings.Contains(rendered, "exit=7") {
		t.Errorf("exit code 7 should be visible in the rendered header: %q", rendered)
	}
}

func TestPackLexWithSentinelsEmitsSentinelForOversizedProgram(t *testing.T) {
	t.Parallel()
	results := []programResult{
		{name: "small", exitCode: 0, output: []byte("ok\n")},
		{name: "huge", exitCode: 0, output: []byte(strings.Repeat("z", 4096))},
	}
	contents, used := packLexWithSentinels(results, 100) // 100 tokens budget
	if used == 0 {
		t.Fatal("expected the small program to use some of the budget")
	}
	joined := provider.JoinText(contents)
	if !strings.Contains(joined, "ok") {
		t.Errorf("small fitting program missing: %q", joined)
	}
	if !strings.Contains(joined, "[program=huge dropped:") {
		t.Errorf("oversized program should yield a dropped: sentinel, got: %q", joined)
	}
	if strings.Contains(joined, strings.Repeat("z", 50)) {
		t.Errorf("oversized program body should NOT be included (truncation is worse than absence)")
	}
}

func TestAdvanceOverflowStreakEdgeAndPeriodic(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	// First overflow → edge fires.
	if !a.advanceOverflowStreak(true) {
		t.Fatal("expected edge trigger on first overflowing iteration")
	}
	// Iterations 2..revisionPromptCadence: still overflowing but quiet.
	for i := 2; i <= revisionPromptCadence; i++ {
		if a.advanceOverflowStreak(true) {
			t.Fatalf("expected no prompt at sustained-overflow iteration %d", i)
		}
	}
	// Iteration revisionPromptCadence+1: periodic fires.
	if !a.advanceOverflowStreak(true) {
		t.Fatalf("expected periodic trigger at iteration %d", revisionPromptCadence+1)
	}
	// Non-overflow resets the streak — next overflow is edge again.
	if a.advanceOverflowStreak(false) {
		t.Fatal("non-overflow iteration must not fire the prompt")
	}
	if !a.advanceOverflowStreak(true) {
		t.Fatal("expected edge trigger after the streak reset")
	}
}

func TestStepIdlesWhenProgramHashUnchanged(t *testing.T) {
	t.Parallel()
	// A single program with deterministic output. After the first Step
	// queries the model, the second Step should observe identical (name,
	// exit, stdout) triples, match lastOutputHash, and idle indefinitely.
	// We prove "idled" by cancelling the context while it is sleeping
	// and asserting the provider was only called once.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "first"}}, StopReason: provider.StopEnd},
		},
	}
	a := newAgentWithPrograms(t, programSpec{"static", "#!/bin/sh\necho stable\n"})
	a.provider = prov

	if _, _, err := a.Step(context.Background()); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if got := len(prov.calls); got != 1 {
		t.Fatalf("after Step 1 expected 1 provider call, got %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Give Step 2 enough time to enter the backoff sleep, then cut it
		// short. Any cancel before idleBackoffInitial expires is plenty.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, err := a.Step(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Step 2: expected context.Canceled (Step should have been idling), got %v", err)
	}
	if got := len(prov.calls); got != 1 {
		t.Fatalf("Step 2 should have idled without calling the provider; total calls=%d", got)
	}
}

func TestStepDoesNotIdleWhenOutputChanges(t *testing.T) {
	t.Parallel()
	// counterProgram increments on every invocation, so Step 2 sees a
	// different stdout from Step 1 and must query the provider rather
	// than idle. Regression guard against the hash-comparison accidentally
	// matching too eagerly.
	prov := &scriptedProvider{
		responses: []provider.AssistantMessage{
			{Content: []provider.AssistantContent{provider.TextContent{Text: "one"}}, StopReason: provider.StopEnd},
			{Content: []provider.AssistantContent{provider.TextContent{Text: "two"}}, StopReason: provider.StopEnd},
		},
	}
	a := newTestAgent(t, prov)

	if _, _, err := a.Step(context.Background()); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	if _, _, err := a.Step(context.Background()); err != nil {
		t.Fatalf("Step 2: %v", err)
	}
	if got := len(prov.calls); got != 2 {
		t.Fatalf("expected 2 provider calls (no idle), got %d", got)
	}
}

// writeRevisionScript drops an executable script at reinforcement/revision/<name>
// inside the agent's probe dir, returning its absolute path. Tests use this
// to install minimal inline scripts rather than copying the shipped asset
// when they only need to verify the harness wiring.
func writeRevisionScript(t *testing.T, a *Agent, name, body string) string {
	t.Helper()
	dir := filepath.Join(a.reinforcementDir, revisionReinforcementName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunRevisionPromptReturnsEmptyWhenDirMissing(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t, programSpec{"only", "#!/bin/sh\necho hi\n"})
	// reinforcement/revision/ is intentionally not created.
	if got := a.runReinforcementPrompt(revisionReinforcementName); got != "" {
		t.Fatalf("expected empty string when reinforcement/revision/ is missing, got %q", got)
	}
}

func TestRunRevisionPromptConcatenatesScriptsInLexOrder(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t, programSpec{"only", "#!/bin/sh\necho hi\n"})
	writeRevisionScript(t, a, "b-second.sh", "#!/bin/sh\necho TWO\n")
	writeRevisionScript(t, a, "a-first.sh", "#!/bin/sh\necho ONE\n")

	out := a.runReinforcementPrompt(revisionReinforcementName)
	idxOne := strings.Index(out, "ONE")
	idxTwo := strings.Index(out, "TWO")
	if idxOne < 0 || idxTwo < 0 {
		t.Fatalf("missing one or both script outputs: %q", out)
	}
	if idxOne >= idxTwo {
		t.Errorf("scripts should run in lex order (a-first before b-second), got: %q", out)
	}
	if !strings.Contains(out, "ONE\n\nTWO") {
		t.Errorf("expected blank line between script outputs, got: %q", out)
	}
}

func TestRunRevisionPromptSilentlySkipsFailingScripts(t *testing.T) {
	t.Parallel()
	// A failing script must not abort the whole revision prompt — the
	// remaining scripts still contribute. Mirrors readReinforcement's
	// "must never block a tool result" rule for the same reason: a
	// broken revision script should degrade gracefully, not blank the
	// guidance the agent needs.
	a := newAgentWithPrograms(t, programSpec{"only", "#!/bin/sh\necho hi\n"})
	writeRevisionScript(t, a, "a-broken.sh", "#!/bin/sh\necho boom; exit 1\n")
	writeRevisionScript(t, a, "b-ok.sh", "#!/bin/sh\necho SURVIVED\n")

	out := a.runReinforcementPrompt(revisionReinforcementName)
	if !strings.Contains(out, "SURVIVED") {
		t.Errorf("a failing earlier script must not suppress later scripts: %q", out)
	}
	if strings.Contains(out, "boom") {
		t.Errorf("failing script's stdout should not be included: %q", out)
	}
}

func TestAssembleUserMessageAppendsRevisionPromptAtTail(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t,
		programSpec{"aaa-bulk", "#!/bin/sh\nprintf 'A%.0s' $(seq 1 200)\n"},
	)
	a.contextBudget = 20 // force overflow with one bulky program
	writeRevisionScript(t, a, "general.sh", "#!/bin/sh\necho REVISION-MARKER\n")

	data, err := a.runIteration(context.Background())
	if err != nil {
		t.Fatalf("runIteration: %v", err)
	}
	if !data.overflowed(a.contextBudget) {
		t.Fatalf("test setup did not produce overflow")
	}

	msg := a.assembleUserMessage(data, true)
	if len(msg.Content) == 0 {
		t.Fatal("assembled message is empty")
	}
	// The revision prompt must land at the tail of the context — that's
	// the high-attention end of the U-curve, which is where this guidance
	// is supposed to live.
	tail := msg.Content[len(msg.Content)-1].Text
	if !strings.Contains(tail, "REVISION-MARKER") {
		t.Errorf("expected revision prompt at tail, got tail %q (full message has %d contents)", tail, len(msg.Content))
	}
}

func TestAssembleUserMessageOmitsRevisionPromptWhenScriptMissing(t *testing.T) {
	t.Parallel()
	// showRevisionPrompt=true but reinforcement/revision/ has nothing to
	// run — the harness must silently omit the prompt rather than emit
	// an empty TextContent that wastes a slot in the user message.
	a := newAgentWithPrograms(t,
		programSpec{"aaa-bulk", "#!/bin/sh\nprintf 'A%.0s' $(seq 1 200)\n"},
	)
	a.contextBudget = 20
	data, err := a.runIteration(context.Background())
	if err != nil {
		t.Fatalf("runIteration: %v", err)
	}

	msg := a.assembleUserMessage(data, true)
	for _, c := range msg.Content {
		if strings.Contains(c.Text, "[REVISION]") {
			t.Fatalf("revision prompt was emitted despite missing script: %q", c.Text)
		}
		if c.Text == "" {
			t.Errorf("empty TextContent slipped into message: %#v", msg.Content)
		}
	}
}

func TestReadInactiveTreatsMissingFileAsEmpty(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t, programSpec{"only", "#!/bin/sh\necho hi\n"})
	set, err := a.readInactive()
	if err != nil {
		t.Fatalf("readInactive: %v", err)
	}
	if len(set) != 0 {
		t.Fatalf("expected empty inactive set when file is missing, got %v", set)
	}
}

func TestReadInactiveSkipsBlanksAndComments(t *testing.T) {
	t.Parallel()
	a := newAgentWithPrograms(t, programSpec{"only", "#!/bin/sh\necho hi\n"})
	body := "# leading comment\n\nfoo\n  bar  \n# trailing comment\n"
	if err := os.WriteFile(filepath.Join(a.root, inactiveFileName), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	set, err := a.readInactive()
	if err != nil {
		t.Fatalf("readInactive: %v", err)
	}
	if _, ok := set["foo"]; !ok {
		t.Errorf("missing entry foo: %v", set)
	}
	if _, ok := set["bar"]; !ok {
		t.Errorf("missing entry bar (whitespace should be trimmed): %v", set)
	}
	if len(set) != 2 {
		t.Errorf("expected exactly two entries, got %v", set)
	}
}

// renderData drives assembleUserMessage on a hand-constructed iterationData
// and returns the concatenated text. Useful for tests that exercise the
// selection algorithm in isolation from shell-script program execution.
func renderData(a *Agent, results []programResult, inactive map[string]struct{}, showPrompt bool) string {
	d := iterationData{results: results, inactive: inactive}
	for _, r := range results {
		d.totalTokens += r.renderedTokens()
	}
	return provider.JoinText(a.assembleUserMessage(d, showPrompt).Content)
}

func TestAssembleUserMessageFitsKeepsLexOrder(t *testing.T) {
	t.Parallel()
	// Below the budget, every program is included with no sentinels and
	// rendered in lex-sorted order. This is the fast path of the algorithm
	// and the byte-stable cacheable case.
	a := &Agent{contextBudget: 10_000}
	results := []programResult{
		{name: "aaa", exitCode: 0, output: []byte("AAA-BODY")},
		{name: "mmm", exitCode: 0, output: []byte("MMM-BODY")},
		{name: "zzz", exitCode: 0, output: []byte("ZZZ-BODY")},
	}
	text := renderData(a, results, nil, false)
	aaa := strings.Index(text, "AAA-BODY")
	mmm := strings.Index(text, "MMM-BODY")
	zzz := strings.Index(text, "ZZZ-BODY")
	if aaa < 0 || mmm < 0 || zzz < 0 {
		t.Fatalf("missing bodies (aaa=%d mmm=%d zzz=%d): %q", aaa, mmm, zzz, text)
	}
	if !(aaa < mmm && mmm < zzz) {
		t.Errorf("expected lex order aaa < mmm < zzz; positions %d, %d, %d", aaa, mmm, zzz)
	}
	if strings.Contains(text, "dropped:") {
		t.Errorf("no sentinels expected under no-overflow: %q", text)
	}
}

func TestActiveSlotPackedInLexOrderDropsLateNames(t *testing.T) {
	t.Parallel()
	// Three same-size active programs with a 100-token budget: active slot
	// is 80 tokens, each program is 35 tokens (header 21 + body 116 = 137
	// bytes → ⌈137/4⌉ = 35). The first two fit (70 used), the third can't
	// fit in the remaining 10 and emits a sentinel. This is the drop-order
	// half of why filename prefixes are load-bearing.
	a := &Agent{contextBudget: 100}
	body := []byte(strings.Repeat("X", 116))
	results := []programResult{
		{name: "aaa", exitCode: 0, output: body},
		{name: "mmm", exitCode: 0, output: body},
		{name: "zzz", exitCode: 0, output: body},
	}
	d := iterationData{results: results, totalTokens: 3 * results[0].renderedTokens()}
	if !d.overflowed(a.contextBudget) {
		t.Fatalf("setup did not overflow (total=%d budget=%d)", d.totalTokens, a.contextBudget)
	}
	text := provider.JoinText(a.assembleUserMessage(d, false).Content)
	aaaHdr := strings.Index(text, "[program=aaa exit=0]")
	mmmHdr := strings.Index(text, "[program=mmm exit=0]")
	zzzDrop := strings.Index(text, "[program=zzz dropped:")
	if aaaHdr < 0 || mmmHdr < 0 || zzzDrop < 0 {
		t.Fatalf("missing entries (aaa=%d mmm=%d zzz-dropped=%d): %q", aaaHdr, mmmHdr, zzzDrop, text)
	}
	if !(aaaHdr < mmmHdr && mmmHdr < zzzDrop) {
		t.Errorf("expected lex order with zzz last (dropped); got %d, %d, %d", aaaHdr, mmmHdr, zzzDrop)
	}
	if strings.Contains(text, "[program=zzz exit=0]") {
		t.Errorf("dropped program's body should not be rendered: %q", text)
	}
}

func TestActiveAlarmKeepsLexPositionUnderOverflow(t *testing.T) {
	t.Parallel()
	// Three small active programs that all fit in the 80% active slot, but
	// total overflows because of an oversized inactive program. The active
	// alarm in the middle (mmm exit=7) must stay in lex position — active
	// programs are already guaranteed visibility, so promoting alarms to
	// the tail would buy a duplicate guarantee at the cost of cache
	// stability.
	a := &Agent{contextBudget: 100}
	smallBody := []byte(strings.Repeat("X", 40))
	bulk := []byte(strings.Repeat("Y", 400))
	results := []programResult{
		{name: "aaa", exitCode: 0, output: smallBody},
		{name: "mmm", exitCode: 7, output: smallBody},
		{name: "zzz", exitCode: 0, output: smallBody},
		{name: "zzz-bulk-inactive", exitCode: 0, output: bulk},
	}
	inactive := map[string]struct{}{"zzz-bulk-inactive": {}}
	d := iterationData{results: results, inactive: inactive}
	for _, r := range results {
		d.totalTokens += r.renderedTokens()
	}
	if !d.overflowed(a.contextBudget) {
		t.Fatalf("setup did not overflow (total=%d budget=%d)", d.totalTokens, a.contextBudget)
	}
	text := provider.JoinText(a.assembleUserMessage(d, false).Content)
	aaa := strings.Index(text, "[program=aaa exit=0]")
	mmm := strings.Index(text, "[program=mmm exit=7]")
	zzz := strings.Index(text, "[program=zzz exit=0]")
	if aaa < 0 || mmm < 0 || zzz < 0 {
		t.Fatalf("missing active headers (aaa=%d mmm=%d zzz=%d): %q", aaa, mmm, zzz, text)
	}
	if !(aaa < mmm && mmm < zzz) {
		t.Errorf("active alarm must stay in lex position; got %d, %d, %d", aaa, mmm, zzz)
	}
}

func TestExplorationLexOrdersNonzeroInactives(t *testing.T) {
	t.Parallel()
	// Two inactive non-zero programs, both small enough to fit in the 20%
	// exploration slot, must be included in pure lex order (alarm-a before
	// alarm-b). The active program is oversized for the 80% slot and gets
	// a sentinel — its presence just forces overflow.
	a := &Agent{contextBudget: 100}
	results := []programResult{
		{name: "aaa-bulk-active", exitCode: 0, output: []byte(strings.Repeat("X", 400))},
		{name: "alarm-a", exitCode: 1, output: []byte("AAA-ALARM")},
		{name: "alarm-b", exitCode: 1, output: []byte("BBB-ALARM")},
	}
	inactive := map[string]struct{}{"alarm-a": {}, "alarm-b": {}}
	text := renderData(a, results, inactive, false)
	if !strings.Contains(text, "[program=aaa-bulk-active dropped:") {
		t.Errorf("expected sentinel for the oversized active program: %q", text)
	}
	a1 := strings.Index(text, "AAA-ALARM")
	b1 := strings.Index(text, "BBB-ALARM")
	if a1 < 0 || b1 < 0 {
		t.Fatalf("missing alarms (a=%d b=%d): %q", a1, b1, text)
	}
	if a1 >= b1 {
		t.Errorf("expected alarm-a before alarm-b in lex order; got %d >= %d", a1, b1)
	}
	if !strings.Contains(text, "exit=1") {
		t.Errorf("expected non-zero exit code to be visible on alarm headers: %q", text)
	}
}

func TestExplorationDrawsLoneZeroExitInactive(t *testing.T) {
	t.Parallel()
	// With exactly one zero-exit inactive program, rand.Perm(1) is
	// deterministically [0], so the exploration phase 2 draw always selects
	// it (assuming it fits). The active program is oversized and emits a
	// sentinel; the inactive program proves the exploration slot reaches
	// quiet demoted probes.
	a := &Agent{contextBudget: 100}
	results := []programResult{
		{name: "aaa-bulk-active", exitCode: 0, output: []byte(strings.Repeat("X", 400))},
		{name: "quiet-inactive", exitCode: 0, output: []byte("QUIET-DRAW")},
	}
	inactive := map[string]struct{}{"quiet-inactive": {}}
	text := renderData(a, results, inactive, false)
	if !strings.Contains(text, "QUIET-DRAW") {
		t.Errorf("expected the lone zero-exit inactive to be drawn into exploration: %q", text)
	}
}

func TestExplorationSkipsOversizedZeroExitInactiveSilently(t *testing.T) {
	t.Parallel()
	// A zero-exit inactive program that exceeds the exploration budget is
	// dropped silently — no sentinel. Sentinels are only for programs the
	// harness committed to including; random exploration draws were never
	// committed. This is the asymmetry between active (sentinel on miss)
	// and exploration zero-exit (silent on miss).
	a := &Agent{contextBudget: 100}
	results := []programResult{
		{name: "aaa-bulk-active", exitCode: 0, output: []byte(strings.Repeat("X", 400))},
		{name: "zzz-huge-inactive", exitCode: 0, output: []byte(strings.Repeat("Q", 4000))},
	}
	inactive := map[string]struct{}{"zzz-huge-inactive": {}}
	text := renderData(a, results, inactive, false)
	if strings.Contains(text, "[program=zzz-huge-inactive dropped:") {
		t.Errorf("zero-exit inactive should not emit a sentinel on a missed exploration draw: %q", text)
	}
	if strings.Contains(text, "QQQQ") {
		t.Errorf("oversized inactive body must not leak into context: %q", text)
	}
}

func TestExplorationNonzeroInactiveOversizedGetsSentinel(t *testing.T) {
	t.Parallel()
	// Non-zero exits are the alarm channel — phase 1 commits to including
	// every non-zero inactive program in lex order, so one that overflows
	// the remaining exploration budget DOES emit a sentinel (so the agent
	// notices the omission rather than silently losing the signal).
	a := &Agent{contextBudget: 100}
	results := []programResult{
		{name: "aaa-bulk-active", exitCode: 0, output: []byte(strings.Repeat("X", 400))},
		{name: "zzz-loud-inactive", exitCode: 1, output: []byte(strings.Repeat("L", 4000))},
	}
	inactive := map[string]struct{}{"zzz-loud-inactive": {}}
	text := renderData(a, results, inactive, false)
	if !strings.Contains(text, "[program=zzz-loud-inactive dropped:") {
		t.Errorf("oversized non-zero inactive should emit a sentinel into exploration: %q", text)
	}
}

func TestAssembleUserMessageGoalAtTail(t *testing.T) {
	t.Parallel()
	// The goal text lands after all program output. Tail placement puts it
	// in the high-attention region of the U-curve so the agent keeps the
	// goal in mind on every turn. The revision prompt (if any) is appended
	// after the goal and is exercised by its own tests.
	a := &Agent{contextBudget: 10_000, goal: "find-the-leak"}
	results := []programResult{{name: "p", exitCode: 0, output: []byte("BODY-MARK")}}
	text := renderData(a, results, nil, false)
	body := strings.Index(text, "BODY-MARK")
	goal := strings.Index(text, "[YOUR GOAL]")
	leak := strings.Index(text, "find-the-leak")
	if body < 0 || goal < 0 || leak < 0 {
		t.Fatalf("missing pieces (body=%d goal=%d leak=%d): %q", body, goal, leak, text)
	}
	if !(body < goal && goal < leak) {
		t.Errorf("expected program output, then [YOUR GOAL], then goal text; got %d, %d, %d",
			body, goal, leak)
	}
}

func TestAssembleUserMessageOmitsGoalWhenEmpty(t *testing.T) {
	t.Parallel()
	a := &Agent{contextBudget: 10_000} // empty goal
	results := []programResult{{name: "p", exitCode: 0, output: []byte("BODY")}}
	text := renderData(a, results, nil, false)
	if strings.Contains(text, "[YOUR GOAL]") {
		t.Errorf("goal section should be omitted when goal is empty: %q", text)
	}
}

func TestEstimateTokensRoundsUp(t *testing.T) {
	t.Parallel()
	// Bytes/4 with ceiling: a 5-byte program is at least 2 tokens, not 1.
	// Rounding down would let the budget bookkeeping under-count and quietly
	// overflow.
	cases := []struct{ in, want int }{
		{0, 0},
		{1, 1},
		{3, 1},
		{4, 1},
		{5, 2},
		{8, 2},
		{9, 3},
		{1024, 256},
	}
	for _, c := range cases {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("estimateTokens(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSentinelLineFormat(t *testing.T) {
	t.Parallel()
	// The design names this format verbatim; lock it so refactors don't
	// silently break the agent's expectation of seeing
	// "[program=... dropped: ...]" as the omission signal.
	got := sentinelLine("foo", 187*1024, 40*1024)
	want := "[program=foo dropped: 187K tokens exceeds remaining budget 40K]\n"
	if got != want {
		t.Errorf("sentinel format drift:\n got:  %q\n want: %q", got, want)
	}
}

func TestProgramResultRenderedHeader(t *testing.T) {
	t.Parallel()
	r := programResult{name: "foo", exitCode: 3, output: []byte("body")}
	got := r.rendered()
	if !strings.HasPrefix(got, "[program=foo exit=3]\n") {
		t.Errorf("header drift: rendered=%q", got)
	}
	if !strings.HasSuffix(got, "body") {
		t.Errorf("body must follow the header verbatim: %q", got)
	}
}

func TestSplitByActivePreservesLexOrder(t *testing.T) {
	t.Parallel()
	results := []programResult{
		{name: "aaa"},
		{name: "bbb"},
		{name: "ccc"},
		{name: "ddd"},
	}
	inactive := map[string]struct{}{"bbb": {}, "ddd": {}}
	active, demoted := splitByActive(results, inactive)
	if len(active) != 2 || active[0].name != "aaa" || active[1].name != "ccc" {
		t.Errorf("active partition wrong: %v", active)
	}
	if len(demoted) != 2 || demoted[0].name != "bbb" || demoted[1].name != "ddd" {
		t.Errorf("inactive partition wrong: %v", demoted)
	}
}

