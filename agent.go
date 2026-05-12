package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
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

	// Token ceiling for the assembled program-output portion of the context.
	// 128K assumes a 256K base window — the size frontier models are still
	// trained on — leaving roughly half the window for in-flight tool use.
	defaultContextBudgetTokens = 128 * 1024

	// Fraction of the budget reserved for active programs; the remainder is
	// the exploration slot. Encoded as a percentage so the 80/20 split is
	// readable off the constant.
	activeBudgetPercent = 80

	// Iterations between revision-prompt firings while overflow is sustained.
	// The first overflowing iteration after any non-overflow gap always
	// fires; subsequent firings happen every revisionPromptCadence iterations.
	revisionPromptCadence = 10

	inactiveFileName = "inactive"

	// Pseudo-tool name under reinforcement/ whose scripts emit the revision
	// prompt. Treated like any other reinforcement directory by readDir+exec,
	// but invoked from the context-assembly path rather than a tool call so
	// stdin is empty.
	revisionReinforcementName = "revision"
)

func NewAgent(prov provider.Provider, root, goal string, debug bool) *Agent {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Agent{
		provider:         prov,
		root:             root,
		programsDir:      filepath.Join(root, "programs"),
		reinforcementDir: filepath.Join(root, "reinforcement"),
		goal:             goal,
		tools:            DefaultTools,
		debug:            debug,
		contextBudget:    defaultContextBudgetTokens,
	}
}

type Agent struct {
	provider         provider.Provider
	root             string
	programsDir      string
	reinforcementDir string
	goal             string
	tools            []ToolDefinition
	debug            bool
	contextBudget    int // token ceiling for the program-output slot

	conversation []provider.Message
	iteration    int

	// Carried across Steps. lastStopReason controls whether the prior
	// assistant/tool history is preserved (tool-using cycle) or thrown away
	// (cycle ended on StopEnd). lastOutputHash is the pre-selection hash of
	// (name, exit, stdout) triples for the last substantive iteration — when
	// the freshly computed hash matches it, the harness idles instead of
	// re-querying the model. overflowStreak counts consecutive substantive
	// iterations whose total program output exceeded the budget and drives
	// the revision-prompt cadence.
	lastStopReason provider.StopReason
	lastOutputHash programHash
	idleBackoff    time.Duration
	overflowStreak int
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

// Step runs a single inference + tool execution iteration. Every Step
// re-runs all installed programs and rebuilds the leading user message
// from their output; assistant/tool history from prior Steps is preserved
// only while the model is mid tool-using cycle (StopToolUse). When the
// pre-selection hash of program outputs matches the previous substantive
// iteration's hash (and we are not mid-cycle), the Step idles with
// exponential backoff rather than re-querying the model. The agent never
// auto-terminates — done is always false on success.
func (a *Agent) Step(ctx context.Context) (provider.AssistantMessage, bool, error) {
	var data iterationData
	for {
		var history []provider.Message
		if a.lastStopReason == provider.StopToolUse && len(a.conversation) > 1 {
			history = append(history, a.conversation[1:]...)
		}

		fresh, err := a.runIteration(ctx)
		if err != nil {
			return provider.AssistantMessage{}, false, err
		}

		midCycle := a.lastStopReason == provider.StopToolUse
		if !midCycle && a.iteration > 0 && fresh.hash == a.lastOutputHash {
			d := a.nextIdleBackoff()
			select {
			case <-time.After(d):
				continue
			case <-ctx.Done():
				return provider.AssistantMessage{}, false, ctx.Err()
			}
		}

		showPrompt := a.advanceOverflowStreak(fresh.overflowed(a.contextBudget))
		userMsg := a.assembleUserMessage(fresh, showPrompt)
		a.conversation = append([]provider.Message{userMsg}, history...)
		a.idleBackoff = 0
		data = fresh
		break
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
	a.lastOutputHash = data.hash
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

// advanceOverflowStreak updates the consecutive-overflow counter and reports
// whether the revision prompt should fire on this iteration. The prompt
// fires on the first overflowing iteration after any non-overflow gap
// (edge-triggered) and then every revisionPromptCadence iterations while
// overflow persists (periodic). Called once per substantive iteration; idle
// polls do not advance the streak.
func (a *Agent) advanceOverflowStreak(overflow bool) bool {
	if !overflow {
		a.overflowStreak = 0
		return false
	}
	a.overflowStreak++
	return a.overflowStreak == 1 || (a.overflowStreak-1)%revisionPromptCadence == 0
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

// readInactive parses .autoprobe/inactive into a set of program names. A
// missing or empty file means "every program is active" — the file is the
// agent's explicit demotion list, and absence is not an error. Entries that
// no longer correspond to an installed program are harmlessly ignored
// downstream because they simply never match a result. Blank lines and
// lines starting with '#' are skipped so the agent can leave itself notes.
func (a *Agent) readInactive() (map[string]struct{}, error) {
	path := filepath.Join(a.root, inactiveFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = struct{}{}
	}
	return set, nil
}

// programHash is a sha256 over the sorted (name, exit_code, stdout) triples
// of one iteration's program runs. Idle detection compares hashes across
// iterations instead of byte-comparing rendered conversations, so random
// exploration draws don't defeat the backoff.
type programHash [sha256.Size]byte

// programResult captures everything one program contributed to this
// iteration: its name, its exit code (a status channel separate from stdout
// per the program contract), and the captured combined stdout+stderr bytes.
type programResult struct {
	name     string
	exitCode int
	output   []byte
}

func (r programResult) header() string {
	return fmt.Sprintf("[program=%s exit=%d]\n", r.name, r.exitCode)
}

func (r programResult) rendered() string {
	return r.header() + string(r.output)
}

func (r programResult) renderedTokens() int {
	return estimateTokens(len(r.header()) + len(r.output))
}

// estimateTokens converts a byte count to an approximate token count using
// the bytes/4 rule. Accurate counts need the provider's tokenizer; for
// budget bookkeeping this rough estimate is enough.
func estimateTokens(n int) int {
	return (n + 3) / 4
}

// hashResults computes the pre-selection program hash. Exit code is part of
// the hash so a probe flipping from 0 to non-zero with byte-identical stdout
// is treated as an environmental change and not eaten by idle backoff.
func hashResults(results []programResult) programHash {
	h := sha256.New()
	var buf [4]byte
	for _, r := range results {
		h.Write([]byte(r.name))
		h.Write([]byte{0})
		binary.LittleEndian.PutUint32(buf[:], uint32(r.exitCode))
		h.Write(buf[:])
		h.Write(r.output)
		h.Write([]byte{0})
	}
	var out programHash
	copy(out[:], h.Sum(nil))
	return out
}

func sentinelLine(name string, costTokens, remainingTokens int) string {
	return fmt.Sprintf("[program=%s dropped: %s tokens exceeds remaining budget %s]\n",
		name, humanTokens(costTokens), humanTokens(remainingTokens))
}

func humanTokens(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dM", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dK", n/1024)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// runRevisionPrompt executes every script in reinforcement/revision/ in lex
// order and joins their non-empty stdout with blank lines. Scripts derive
// their own paths from $0 (see the shipped general.sh) so the rendered
// prompt always carries fully-resolved paths regardless of where the probe
// directory lives — the harness deliberately passes no env vars and no
// stdin. A missing or empty reinforcement/revision/ dir yields "" and the
// revision prompt is simply omitted from the iteration's context.
func (a *Agent) runRevisionPrompt() string {
	dir := filepath.Join(a.reinforcementDir, revisionReinforcementName)
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

	var parts []string
	for _, name := range names {
		out, runErr := exec.Command(filepath.Join(dir, name)).Output()
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
	data, err := a.runIteration(ctx)
	if err != nil {
		return nil, err
	}
	return []provider.Message{a.assembleUserMessage(data, false)}, nil
}

// iterationData bundles one iteration's worth of program execution: the
// per-program results sorted by name, the pre-selection hash over (name,
// exit, stdout) triples, the inactive-set membership, and the summed token
// estimate. Carried into assembleUserMessage and into the idle/overflow
// bookkeeping in Step.
type iterationData struct {
	results     []programResult
	hash        programHash
	inactive    map[string]struct{}
	totalTokens int
}

func (d iterationData) overflowed(budget int) bool {
	return d.totalTokens > budget
}

// runIteration runs all programs and gathers the side state needed to make
// inclusion decisions (inactive list, hash, token total). Kept distinct
// from runPrograms so callers that only want the raw results — tests, or
// callers built around different selection policies — don't pay for reads
// or hashing they don't need.
func (a *Agent) runIteration(ctx context.Context) (iterationData, error) {
	results, err := a.runPrograms(ctx)
	if err != nil {
		return iterationData{}, err
	}
	inactive, err := a.readInactive()
	if err != nil {
		return iterationData{}, err
	}
	total := 0
	for _, r := range results {
		total += r.renderedTokens()
	}
	return iterationData{
		results:     results,
		hash:        hashResults(results),
		inactive:    inactive,
		totalTokens: total,
	}, nil
}

// assembleUserMessage builds the leading UserMessage from one iteration's
// data. When the total program output fits inside the budget we include
// every program unconditionally; otherwise we split the budget 80/20
// between the active set (lex order, sentinels for individually-oversized
// programs) and the exploration slot (non-zero-exit inactives lex-ordered
// first, then a uniform random draw from zero-exit inactives). The goal
// and revision prompt land at the tail of the context.
func (a *Agent) assembleUserMessage(d iterationData, showRevisionPrompt bool) provider.UserMessage {
	var contents []provider.TextContent

	if !d.overflowed(a.contextBudget) {
		for _, r := range d.results {
			contents = append(contents, provider.TextContent{Text: r.rendered()})
		}
	} else {
		active, inactive := splitByActive(d.results, d.inactive)
		activeBudget := a.contextBudget * activeBudgetPercent / 100
		explorationBudget := a.contextBudget - activeBudget

		activeContents, _ := packLexWithSentinels(active, activeBudget)
		contents = append(contents, activeContents...)
		contents = append(contents, packExploration(inactive, explorationBudget)...)
	}

	if a.goal != "" {
		contents = append(contents, provider.TextContent{Text: "[YOUR GOAL]\n" + a.goal})
	}
	if showRevisionPrompt {
		if text := a.runRevisionPrompt(); text != "" {
			contents = append(contents, provider.TextContent{Text: text})
		}
	}
	return provider.UserMessage{Content: contents}
}

// splitByActive partitions lex-sorted results into the active and inactive
// slices, preserving lex order in both.
func splitByActive(results []programResult, inactive map[string]struct{}) (active, demoted []programResult) {
	for _, r := range results {
		if _, off := inactive[r.name]; off {
			demoted = append(demoted, r)
		} else {
			active = append(active, r)
		}
	}
	return
}

// packLexWithSentinels walks results in lex order, including each program's
// rendered output if it fits in the remaining budget and emitting a
// one-line sentinel otherwise. Sentinels stand in for the dropped output
// rather than truncating it — a half-truncated probe is worse than an
// absent one because the agent can't tell whether the suppressed bytes
// contained the signal.
func packLexWithSentinels(results []programResult, budget int) ([]provider.TextContent, int) {
	var contents []provider.TextContent
	used := 0
	for _, r := range results {
		cost := r.renderedTokens()
		remaining := budget - used
		if cost <= remaining {
			contents = append(contents, provider.TextContent{Text: r.rendered()})
			used += cost
			continue
		}
		contents = append(contents, provider.TextContent{Text: sentinelLine(r.name, cost, remaining)})
	}
	return contents, used
}

// packExploration fills the exploration budget in two phases. Phase 1
// pulls in every inactive program that exited non-zero this iteration in
// lex order — this is how the exit-code contract reaches previously-
// demoted programs — with sentinels for any that individually exceed the
// remaining budget. Phase 2 fills any leftover budget with a uniform
// random draw from the inactive programs that exited zero so low-scoring
// programs stay measurable. Random skips are silent: we never committed
// to including any specific zero-exit inactive, so there's nothing to
// sentinel.
func packExploration(inactive []programResult, budget int) []provider.TextContent {
	var nonzero, zero []programResult
	for _, r := range inactive {
		if r.exitCode != 0 {
			nonzero = append(nonzero, r)
		} else {
			zero = append(zero, r)
		}
	}

	contents, used := packLexWithSentinels(nonzero, budget)
	if used >= budget || len(zero) == 0 {
		return contents
	}
	remaining := budget - used
	for _, i := range rand.Perm(len(zero)) {
		r := zero[i]
		cost := r.renderedTokens()
		if cost > remaining {
			continue
		}
		contents = append(contents, provider.TextContent{Text: r.rendered()})
		remaining -= cost
	}
	return contents
}

// runPrograms executes everything in programsDir concurrently and returns
// results sorted by name. Programs run on every iteration regardless of
// their active/inactive status — running them is essentially free and the
// harness needs every exit code in order to decide what reaches the context.
// A missing programs dir or a real I/O failure surfaces as an error;
// ordinary non-zero exits are captured into the result, not treated as run
// failures.
func (a *Agent) runPrograms(ctx context.Context) ([]programResult, error) {
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
	results := make([]programResult, len(names))
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
			results[i] = programResult{name: name, exitCode: exitCode, output: out}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
