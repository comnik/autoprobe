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
	"sync"
	"sync/atomic"
	"time"

	"github.com/comnik/autoprobe/internal/provider"
	"golang.org/x/sync/errgroup"
)

const (
	idleBackoffInitial    = 1 * time.Second
	idleBackoffMax        = 30 * time.Second
	maxProgramConcurrency = 8

	// Token ceiling for the assembled program-output portion of the context. 64K assumes a
	// 128K maximum effective context window, leaving roughly half the window for in-flight
	// tool use.
	defaultContextBudgetTokens = 64_000

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

	// Pseudo-tool name under reinforcement/ whose scripts emit the
	// modeling prompt — a yield nudge asking the work cycle to close so
	// the dedicated modeling turn can take over. Fires when a Step's
	// in-cycle drag crosses modelingThresholdTokens. Wrap-up framing on
	// the final turn lives in modelingFinalGuidance, not here.
	modelingReinforcementName = "modeling"

	// Inside a tool-use cycle, the non-program portion of an inference's
	// input (InputTokens − the program-output portion of the user message)
	// is what the model is paying to drag prior history forward. When that
	// crosses this threshold on a single Step, the modeling prompt is appended
	// to that Step's last tool result so the model sees the nudge attached
	// to what it is reacting to. The threshold doubles as the working-set
	// alarm: ≈16 pages (2K each) of in-cycle drag is enough that compressing
	// to a program saves more on every subsequent inference than the
	// compression itself costs.
	modelingThresholdTokens = 32 * 1024

	// Steps to wait after a periodic modeling firing before considering the
	// next one. Without this, once history exceeds the threshold every
	// subsequent Step would re-fire, since the per-Step drag stays above the
	// threshold until the model actually ends the cycle.
	modelingCooldownSteps = 5

	// Step count inside a modeling turn that, once reached on a step that
	// ended StopToolUse, triggers a one-shot YIELD nudge appended to the
	// last tool result. Mirrors the work cycle's YIELD mechanism: an
	// advisory ask to wrap the turn, not a hard truncation. Healthy
	// modeling turns close in 1–3 steps; firing at 5 catches turns that
	// are spinning without cutting off ones doing real work. A model that
	// ignores the nudge is bounded by the global maxIterations budget.
	modelingYieldStepThreshold = 5

	// Periodic safety net: force a modeling turn after this many consecutive
	// work cycles with no other trigger, so long quiet phases still get
	// curated.
	modelingPeriodicWorkCycles = 10
)

// TurnKind distinguishes a work turn (pursue the user goal) from a modeling
// turn (review the prior work cycle and update the library accordingly).
// Threaded through Step so the harness composes the right system prompt,
// builds the right user message, and tags the trace.
type TurnKind int

const (
	TurnWork TurnKind = iota
	TurnModeling
)

func (k TurnKind) String() string {
	if k == TurnModeling {
		return "modeling"
	}
	return "work"
}

// modelingGuidance is the user-message guidance block appended to every
// modeling turn after the program-output region and the prior work cycle's
// transcript. Inline rather than a script for the first cut so the
// implementation diff stays focused; promote to an asset if it grows.
const modelingGuidance = `[MODELING GUIDANCE]
Use this turn to update the programs in your library, so that your world model reflects
anything new the prior work cycle revealed about your environment:
- Compress repeated reads or bash commands into programs that emit the same
  information on the next iteration's first dashboard.
- Tighten verbose program outputs that took disproportionate context.
- Edit ` + "`.autoprobe/inactive`" + ` to demote programs that are not pulling their
  weight (demotion is reversible; deletion is permanent).
- Encode any new invariants the cycle revealed as programs that exit
  non-zero when they are violated.

When done, respond with a brief plain-text summary and NO further tool calls.`

const modelingBootstrapGuidance = `[MODELING GUIDANCE — BOOTSTRAP]
The program library is empty so you don't have a world model yet. This
is the bootstrap modeling turn before the first work iteration. Install
initial programs that model the parts of the environment relevant to the
goal.

Some commonly useful programs:
- test summary and other verifiable success conditions
- self-validating maps or conceptual models of the codebase
- todo list / logbook of things you have tried and what you learned from them

When you have installed enough to give the first work cycle traction,
respond with a brief plain-text summary and NO further tool calls.`

// modelingYieldGuidance is the YIELD nudge appended to the last tool
// result when a modeling turn has run for modelingYieldStepThreshold
// steps without closing. Mirrors the work-mode yield prompt: an advisory
// ask to wrap, not a hard truncation. Suppressed on bootstrap turns,
// which legitimately do more exploration. Inline for now; promote to a
// reinforcement asset if it grows.
const modelingYieldGuidance = `[YIELD]
You have spent several steps in this modeling turn. The library on disk
is the durable output here — anything that doesn't land as a program or
an inactive entry costs inference without persisting.

Wrap up: respond with a brief plain-text summary and NO further tool calls.
The next work cycle will start with your updated library in context.`

const modelingFinalGuidance = `[MODELING GUIDANCE — FINAL]
The iteration budget configured with -n has been reached. This is the
wrap-up modeling turn before the harness terminates the run. Anything not
written to disk now will be lost — your conversation history does not
survive. Use this turn to:

- Capture any new understanding of the environment as an executable program.
- Update or fix programs that produced misleading output during this run.
- Record the solution — if you converged on one — as a program so the next
  run can pick up from where this one left off.

When done, respond with a brief plain-text summary and NO further tool calls.`

func NewAgent(prov provider.Provider, root, goal string, maxIterations int) *Agent {
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
		contextBudget:    defaultContextBudgetTokens,
		maxIterations:    maxIterations,
		prevOutputs:      map[string][]byte{},
	}
}

// Phase values describe what Step is currently doing — the sub-stage of
// one inference. Read by the TUI to drive the phase-indicator strip;
// written by Step at each transition. Phase is the "what is the step doing
// right now" axis; turn kind (work vs. modeling) is a parallel axis
// surfaced separately via CurrentTurnKind so the TUI can render both
// without conflating them.
const (
	PhaseIdle uint32 = iota
	PhaseRunPrograms
	PhaseInference
	PhaseTools
)

type Agent struct {
	provider         provider.Provider
	root             string
	programsDir      string
	reinforcementDir string
	goal             string
	tools            []ToolDefinition
	contextBudget    int // token ceiling for the program-output slot
	maxIterations    int // exit after this many runIteration calls; 0 = unlimited
	tracer           *Tracer

	conversation    []provider.Message
	workIteration   int // counts substantive work inferences; advances at the end of stepWork only. Surfaced to the TUI ("iter N") and stamped on trace records for timeline grouping. Distinct from totalIterations, which is the cost-accounting counter checked against maxIterations.
	totalIterations int // counts every runIteration call, including idle polls

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

	// finalPhase flips true on the first substantive Step that hits the -n cap
	// and stays true for the remainder of the run. While set, Step skips idle
	// backoff and forces a modeling firing on the next user message (with
	// $AUTOPROBE_FINAL=1 so the prompt can carry last-chance framing). The
	// run terminates once the model returns a non-tool-use stop reason on
	// (or after) entering this phase.
	finalPhase bool

	// Steps remaining in the post-firing cooldown. Decremented at the end of
	// each Step inside a tool-use cycle; while > 0 the per-Step threshold
	// check is suppressed. Zeroed on cycle end. The wrap-up firing bypasses
	// cooldown because it goes through the user-message path, not the
	// tool-result path.
	modelingCooldown int

	// prevOutputs is a session-local buffer of each program's last-
	// iteration output, used to compute change frequency, change amount,
	// and staleness on the next iteration. Per-program statistics
	// themselves live on disk at .autoprobe/statistics/<name>.json — they
	// aren't cached on the Agent because parallel updaters read/write
	// their own files independently and a shared cache would only force
	// a serial merge.
	prevMu      sync.Mutex
	prevOutputs map[string][]byte

	// Live idle-polling visibility for the TUI. Step sits in its own
	// goroutine relative to the TUI, so the header reads these atomically
	// to render a "idle: N polls, Ms" indicator while the hash-match
	// backoff loop is silently re-running programs and sleeping.
	// idleStartedAtNs == 0 means "not currently idling".
	idlePollsInFlight atomic.Int32
	idleStartedAtNs   atomic.Int64

	// Dashboard vitals. Atomics so the TUI can read them at any tick
	// without locking against Step. phaseChangedCh, when set by the TUI
	// via SubscribePhaseChanges, gets a non-blocking nudge whenever phase
	// transitions so the indicator updates without waiting for the next
	// 1s tick.
	phase             atomic.Uint32
	phaseChangedCh    chan struct{}
	totalInputTokens  atomic.Int64
	totalOutputTokens atomic.Int64
	toolCycles        atomic.Int64
	lastProgramTokens atomic.Int64
	lastDrag          atomic.Int64
	inToolCycle       atomic.Bool
	modelingFiredAtNs atomic.Int64

	snapshotMu   sync.Mutex
	lastSnapshot []ProgramSnapshot

	// Per-mode system prompts composed at Prime time from assets/system/
	// identity.sh plus the mode-specific add-on (work.sh or modeling.sh).
	// Held byte-stable thereafter so the provider's system-slot cache
	// breakpoint stays hot across iterations.
	workSystemPrompt     string
	modelingSystemPrompt string

	// currentTurnKind drives which system prompt / user-message shape the
	// next Step uses. Flipped by the cadence predicate at cycle close
	// (work → modeling when triggered, modeling → work always after the
	// modeling turn closes). Stored as an atomic so the TUI can read it
	// from a separate goroutine without locking against Step.
	currentTurnKind atomic.Uint32

	// Modeling-turn state.
	//
	// modelingStepsThisTurn counts inference sub-steps inside one modeling
	// turn. Used to (a) distinguish the first step from subsequent steps
	// (kickoff user message vs. fresh program-output region) and (b) gate
	// the modeling-side YIELD nudge.
	//
	// workCycleTranscript is the prior work cycle's conversation tail
	// (everything after the leading user message), captured at the moment
	// that cycle closed and embedded into the first modeling step's user
	// message. Discarded when the modeling turn closes.
	//
	// preModelingLibraryHash captures programs/ + inactive at the start of
	// a modeling turn so the harness can detect a no-op modeling turn
	// (nothing changed → suppress the next firing on the same stale signal).
	//
	// modelingNoOpSuppressed is set when the previous modeling turn made no
	// library changes; cleared by any fresh trigger condition firing again.
	//
	// workCyclesSinceModeling counts consecutive closed work cycles since
	// the last modeling turn; once it hits modelingPeriodicWorkCycles, the
	// periodic safety-net trigger fires.
	//
	// yieldFiredThisCycle mirrors modelingFiredAtNs scoped to the
	// just-closed work cycle — the cadence predicate reads it once at
	// cycle close to decide whether the default trigger fires.
	//
	// modelingYieldFiredThisTurn ensures the modeling-side YIELD nudge
	// fires at most once per modeling turn; cleared when the turn closes.
	//
	// needsBootstrap marks the very first modeling turn so the user-message
	// guidance switches to bootstrap framing (no prior cycle to review).
	// Also suppresses the modeling-side YIELD nudge: bootstrap turns
	// legitimately do more exploration to install the initial library and
	// shouldn't be nudged to wrap up early.
	modelingStepsThisTurn      int
	workCycleTranscript        []provider.Message
	preModelingLibraryHash     [sha256.Size]byte
	modelingNoOpSuppressed     bool
	workCyclesSinceModeling    int
	yieldFiredThisCycle        bool
	modelingYieldFiredThisTurn bool
	needsBootstrap             bool

	// traceSeq is the monotonic record counter for trace writes. Advances
	// on every Step (work or modeling) so each iter-NNNNN.html filename is
	// unique even when modeling turns interleave with work iterations.
	traceSeq int
}

// ProgramSnapshot is one program's state in the most recent iteration —
// exposed to the TUI so the library bar can render width, color, and
// active/included flags without poking at internal agent slices.
type ProgramSnapshot struct {
	Name              string
	RenderedTokens    int
	Active            bool
	IncludedInContext bool
	Dropped           bool // sentinel emitted in place of the rendered output
	ChangedThisIter   bool
}

// IdleStatus reports whether Step is currently sitting in its idle-poll
// backoff loop and, if so, how many polls have fired and how long the wait
// has lasted. Used by the TUI header.
func (a *Agent) IdleStatus() (polls int, since time.Duration, active bool) {
	n := a.idleStartedAtNs.Load()
	if n == 0 {
		return 0, 0, false
	}
	return int(a.idlePollsInFlight.Load()), time.Since(time.Unix(0, n)), true
}

func (a *Agent) Conversation() []provider.Message { return a.conversation }
func (a *Agent) WorkIteration() int               { return a.workIteration }
func (a *Agent) Provider() provider.Provider      { return a.provider }
func (a *Agent) ContextBudget() int               { return a.contextBudget }

// Phase reports the current step phase. Atomically updated by Step so the
// TUI can render the phase-indicator strip from any goroutine.
func (a *Agent) Phase() uint32 { return a.phase.Load() }

// CurrentTurnKind reports which kind of turn the next (or in-flight) Step
// is running as. Atomically updated when the cadence predicate flips
// between work and modeling, so the TUI can read it concurrently with
// Step.
func (a *Agent) CurrentTurnKind() TurnKind { return TurnKind(a.currentTurnKind.Load()) }

// SubscribePhaseChanges returns a channel that receives a non-blocking
// nudge on every phase transition. Replaces any previous subscription;
// the TUI is the sole subscriber.
func (a *Agent) SubscribePhaseChanges() <-chan struct{} {
	ch := make(chan struct{}, 1)
	a.phaseChangedCh = ch
	return ch
}

func (a *Agent) setPhase(p uint32) {
	if a.phase.Swap(p) == p {
		return
	}
	if ch := a.phaseChangedCh; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// TotalTokens returns cumulative provider-reported input/output tokens
// across every Step in this run.
func (a *Agent) TotalTokens() (in, out int) {
	return int(a.totalInputTokens.Load()), int(a.totalOutputTokens.Load())
}

// ToolCycles returns the number of completed tool-calling cycles
// (continuous runs of StopToolUse steps that ended on a non-tool stop).
func (a *Agent) ToolCycles() int { return int(a.toolCycles.Load()) }

// LastProgramTokens returns the totalTokens of the most recent
// substantive iteration's assembled program output.
func (a *Agent) LastProgramTokens() int { return int(a.lastProgramTokens.Load()) }

// LastDrag returns the most recent Step's in-cycle drag (input tokens
// minus program-output tokens). valid is true only when the most recent
// Step ended mid tool-use cycle — outside a cycle the value is undefined.
func (a *Agent) LastDrag() (drag int, valid bool) {
	return int(a.lastDrag.Load()), a.inToolCycle.Load()
}

// LastModelingFiredAt returns the wall-clock time of the most recent
// modeling firing, or the zero time if none has fired yet. The TUI uses
// the recency to drive the bar flash.
func (a *Agent) LastModelingFiredAt() time.Time {
	n := a.modelingFiredAtNs.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// LastProgramSnapshot returns a copy of the per-program state from the
// most recent substantive iteration. Safe to call from any goroutine;
// the returned slice is independent of agent state.
func (a *Agent) LastProgramSnapshot() []ProgramSnapshot {
	a.snapshotMu.Lock()
	defer a.snapshotMu.Unlock()
	if len(a.lastSnapshot) == 0 {
		return nil
	}
	out := make([]ProgramSnapshot, len(a.lastSnapshot))
	copy(out, a.lastSnapshot)
	return out
}

// SetTracer attaches a run tracer. Trace writes are best-effort; a nil
// tracer disables tracing entirely. Setter rather than constructor arg so
// the existing call sites (tests, current main.go) don't have to be
// rewired for callers that never trace.
func (a *Agent) SetTracer(t *Tracer) { a.tracer = t }

func (a *Agent) Run(ctx context.Context) error {
	return runTUI(ctx, a)
}

// Prime loads the per-mode system prompts, arms the bootstrap modeling
// turn if the program library is empty, and builds the initial
// conversation. Must be called once before Step.
func (a *Agent) Prime(ctx context.Context) error {
	if err := a.loadSystemPrompts(); err != nil {
		return err
	}
	if empty, err := a.programsDirEmpty(); err == nil && empty {
		a.currentTurnKind.Store(uint32(TurnModeling))
		a.needsBootstrap = true
		a.preModelingLibraryHash = a.libraryHash()
	}
	c, err := a.buildConversation(ctx)
	if err != nil {
		return err
	}
	a.conversation = c
	return nil
}

// loadSystemPrompts composes the two mode-specific system prompts from
// assets/system/identity.sh plus the mode-specific add-on (work.sh /
// modeling.sh). Each asset is a script the harness executes once at
// startup; the resulting strings stay byte-stable thereafter so the
// system-slot cache breakpoint hits repeatedly. Missing assets are
// tolerated (yields an empty prompt) so test harnesses that don't extract
// the asset tree can still drive Step without setup.
func (a *Agent) loadSystemPrompts() error {
	sysDir := filepath.Join(a.root, "system")
	identity, err := runSystemScript(filepath.Join(sysDir, "identity.sh"))
	if err != nil {
		return fmt.Errorf("loading system/identity.sh: %w", err)
	}
	work, err := runSystemScript(filepath.Join(sysDir, "work.sh"))
	if err != nil {
		return fmt.Errorf("loading system/work.sh: %w", err)
	}
	modeling, err := runSystemScript(filepath.Join(sysDir, "modeling.sh"))
	if err != nil {
		return fmt.Errorf("loading system/modeling.sh: %w", err)
	}
	a.workSystemPrompt = joinPromptParts(identity, work)
	a.modelingSystemPrompt = joinPromptParts(identity, modeling)
	return nil
}

func runSystemScript(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	cmd := exec.Command(path)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func joinPromptParts(a, b string) string {
	switch {
	case a == "" && b == "":
		return ""
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

// programsDirEmpty reports whether the programs/ directory contains no
// regular files. A missing directory counts as empty so a fresh init
// without the cornerstone naturally triggers the bootstrap modeling turn.
func (a *Agent) programsDirEmpty() (bool, error) {
	entries, err := os.ReadDir(a.programsDir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		return false, nil
	}
	return true, nil
}

// libraryHash digests the program library (file names + contents) and the
// .autoprobe/inactive file. Used to detect no-op modeling turns: if a
// modeling turn closes with the same hash it started with, the harness
// suppresses the next firing on the same stale signal. Best-effort —
// individual read errors silently contribute nothing.
func (a *Agent) libraryHash() [sha256.Size]byte {
	h := sha256.New()
	entries, err := os.ReadDir(a.programsDir)
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			h.Write([]byte(name))
			h.Write([]byte{0})
			if data, err := os.ReadFile(filepath.Join(a.programsDir, name)); err == nil {
				h.Write(data)
			}
			h.Write([]byte{0})
		}
	}
	if data, err := os.ReadFile(filepath.Join(a.root, inactiveFileName)); err == nil {
		h.Write([]byte("inactive\x00"))
		h.Write(data)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Step runs a single inference + tool execution iteration. Branches by
// currentTurnKind: work turns pursue the user's goal and may idle when the
// environment is unchanged; modeling turns sit between work cycles and
// update the program library based on what the last cycle revealed. The
// agent never auto-terminates on a work turn unless the -n cap has been
// reached; modeling turns close themselves when the model stops calling
// tools or the in-turn step cap is exceeded.
func (a *Agent) Step(ctx context.Context) (provider.AssistantMessage, bool, error) {
	if a.CurrentTurnKind() == TurnModeling {
		return a.stepModeling(ctx)
	}
	return a.stepWork(ctx)
}

// stepWork runs a single work-mode inference. Re-runs all installed
// programs, rebuilds the leading user message, preserves assistant/tool
// history while mid-cycle (StopToolUse), and idles with exponential backoff
// when the environment is unchanged. At cycle close, evaluates the modeling
// cadence predicate and may flip currentTurnKind to TurnModeling for the
// next Step.
func (a *Agent) stepWork(ctx context.Context) (provider.AssistantMessage, bool, error) {
	var (
		data            iterationData
		userMsg         provider.UserMessage
		showPrompt      bool
		idlePollsBefore int
		idleWait        time.Duration
	)
	for {
		var history []provider.Message
		if a.lastStopReason == provider.StopToolUse && len(a.conversation) > 1 {
			history = append(history, a.conversation[1:]...)
		}

		a.setPhase(PhaseRunPrograms)
		fresh, err := a.runIteration(ctx)
		if err != nil {
			return provider.AssistantMessage{}, false, err
		}
		a.totalIterations++

		midCycle := a.lastStopReason == provider.StopToolUse
		if !midCycle && a.workIteration > 0 && fresh.hash == a.lastOutputHash {
			if a.reachedMaxIterations() {
				// Hitting the cap while idle: the (skipped) idle turn stands
				// in for the final normal turn — shortcut into the wrap-up
				// phase rather than terminate without giving the agent a
				// chance to persist what it learned.
				a.finalPhase = true
			} else {
				d := a.nextIdleBackoff()
				if a.idleStartedAtNs.Load() == 0 {
					a.idleStartedAtNs.Store(time.Now().UnixNano())
				}
				a.idlePollsInFlight.Add(1)
				a.setPhase(PhaseIdle)
				select {
				case <-time.After(d):
					idlePollsBefore++
					idleWait += d
					continue
				case <-ctx.Done():
					a.idlePollsInFlight.Store(0)
					a.idleStartedAtNs.Store(0)
					return provider.AssistantMessage{}, false, ctx.Err()
				}
			}
		}

		showPrompt = a.advanceOverflowStreak(fresh.overflowed(a.contextBudget))
		userMsg = a.assembleUserMessage(fresh, showPrompt)
		a.conversation = append([]provider.Message{userMsg}, history...)
		a.idleBackoff = 0
		a.idlePollsInFlight.Store(0)
		a.idleStartedAtNs.Store(0)
		data = fresh
		a.lastProgramTokens.Store(int64(fresh.totalTokens))
		a.refreshSnapshot(fresh, userMsg)
		break
	}

	iterStartedAt := time.Now()
	// Snapshot the slice header that gets sent to the provider so a later
	// append (assistant message, tool results) doesn't bleed into the
	// trace's captured context — len is fixed here even if the underlying
	// array grows. The same Context value is later handed to the tracer so
	// the trace's input record is byte-for-byte what the model saw.
	sent := provider.Context{
		SystemPrompt: a.workSystemPrompt,
		Messages:     a.conversation,
		Tools:        a.toolSchemas(),
	}
	a.setPhase(PhaseInference)
	msg, err := a.provider.Generate(ctx, "", sent, provider.Options{MaxTokens: 8192})
	if err != nil {
		return provider.AssistantMessage{}, false, err
	}
	a.totalInputTokens.Add(int64(msg.Usage.InputTokens))
	a.totalOutputTokens.Add(int64(msg.Usage.OutputTokens))
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
	a.updateStats(data.results, joinAssistantText(msg.Content))

	a.workIteration++
	// On the StopToolUse → StopEnd transition the agent has yielded after
	// doing real work, and its narrative typically expects "the next cycle
	// to start clean." Clear the cached hash so the next Step's idle check
	// fails and the agent gets one fresh inference with its updated
	// programs in context. A subsequent StopEnd-after-StopEnd stores the
	// real hash and idles as normal, so we don't loop on repeated yields.
	if msg.StopReason == provider.StopEnd && a.lastStopReason == provider.StopToolUse {
		a.lastOutputHash = programHash{}
	} else {
		a.lastOutputHash = data.hash
	}
	prevStopReason := a.lastStopReason
	a.lastStopReason = msg.StopReason
	a.conversation = append(a.conversation, msg)

	if msg.StopReason == provider.StopToolUse {
		a.lastDrag.Store(int64(msg.Usage.InputTokens - data.totalTokens))
		a.inToolCycle.Store(true)
	} else {
		a.lastDrag.Store(0)
		a.inToolCycle.Store(false)
		if prevStopReason == provider.StopToolUse {
			a.toolCycles.Add(1)
		}
	}

	var toolResults []provider.ToolResultMessage
	if hasToolCall(msg.Content) {
		a.setPhase(PhaseTools)
	}
	for _, c := range msg.Content {
		if call, ok := c.(provider.ToolCall); ok {
			toolResults = append(toolResults, a.executeTool(call))
		}
	}

	// In-cycle yield reinforcement. Inside a tool-use cycle, when drag
	// crosses modelingThresholdTokens, append the yield prompt to the last
	// tool result so the model sees it right before its next response. The
	// cooldown then suppresses further firings — once history is large,
	// every subsequent inference would re-cross the threshold and nag
	// every turn. The cycle ending (msg.StopReason != StopToolUse) clears
	// the cooldown because the history is about to be wiped. The
	// reinforcement now only nudges the model to close the cycle — the
	// library updates happen in the dedicated modeling turn that follows.
	yieldFired := false
	if msg.StopReason == provider.StopToolUse {
		if a.modelingCooldown > 0 {
			a.modelingCooldown--
		} else {
			drag := msg.Usage.InputTokens - data.totalTokens
			if drag >= modelingThresholdTokens && len(toolResults) > 0 {
				if text := a.runReinforcementPrompt(modelingReinforcementName); text != "" {
					last := &toolResults[len(toolResults)-1]
					last.Content = append(last.Content, provider.TextContent{Text: text})
					yieldFired = true
					a.yieldFiredThisCycle = true
					a.modelingFiredAtNs.Store(time.Now().UnixNano())
				}
				a.modelingCooldown = modelingCooldownSteps
			}
		}
	} else {
		a.modelingCooldown = 0
	}

	for _, tr := range toolResults {
		a.conversation = append(a.conversation, tr)
	}

	a.writeTrace(iterStartedAt, time.Now(), idlePollsBefore, idleWait, sent, msg, toolResults, data, userMsg, showPrompt, yieldFired, TurnWork)

	// Work-cycle close: evaluate cadence and possibly flip to modeling.
	// Termination semantics: the agent terminates after the wrap-up
	// modeling turn closes; the work cycle that hits -n schedules that
	// modeling turn rather than running a wrap-up work step.
	done := false
	cycleClosed := msg.StopReason != provider.StopToolUse
	if cycleClosed {
		if !a.finalPhase && a.reachedMaxIterations() {
			a.finalPhase = true
		}
		a.maybeScheduleModelingTurn(prevStopReason)
		// If no modeling turn was scheduled but we just closed the wrap-up
		// cycle, the run terminates here.
		if a.CurrentTurnKind() == TurnWork && a.finalPhase {
			done = true
		}
	}
	return msg, done, nil
}

// stepModeling runs a single inference inside the current modeling turn.
// Modeling steps do not advance a.workIteration, do not engage idle backoff,
// and use the modeling system prompt. The first step of a modeling turn
// builds the kickoff user message (program outputs + prior cycle transcript
// + guidance); subsequent steps preserve assistant/tool history exactly
// like a work cycle would. When the model stops calling tools (or the
// in-turn step cap is hit), the modeling turn closes: currentTurnKind flips
// back to TurnWork, no-op suppression is evaluated, and on the final-phase
// path done=true is returned.
func (a *Agent) stepModeling(ctx context.Context) (provider.AssistantMessage, bool, error) {
	// The turn-kind axis (work vs. modeling) is surfaced via
	// CurrentTurnKind; the phase axis still tracks the step's sub-stage
	// (RunPrograms → Inference → Tools), set below.
	var history []provider.Message
	// History preservation inside the modeling turn mirrors the work cycle:
	// while the model is mid tool-using (StopToolUse), keep its prior
	// assistant + tool-result messages so it can continue from where it left
	// off. On the first step of the modeling turn (modelingStepsThisTurn==0)
	// we ignore lastStopReason entirely — it carries state from the prior
	// work cycle that does not apply here.
	if a.modelingStepsThisTurn > 0 && a.lastStopReason == provider.StopToolUse && len(a.conversation) > 1 {
		history = append(history, a.conversation[1:]...)
	}

	a.setPhase(PhaseRunPrograms)
	data, err := a.runIteration(ctx)
	if err != nil {
		return provider.AssistantMessage{}, false, err
	}
	a.totalIterations++

	var userMsg provider.UserMessage
	if a.modelingStepsThisTurn == 0 {
		// First step of the modeling turn: build the kickoff user message
		// with program outputs, prior cycle transcript, and guidance.
		final := a.finalPhase
		userMsg = a.assembleModelingUserMessage(data, a.workCycleTranscript, a.needsBootstrap, final)
	} else {
		// Subsequent steps: refresh the program outputs at the head, drop
		// the transcript/guidance (the model has seen them). Just emit the
		// fresh program-output region — modeling guidance is implicit at
		// this point.
		userMsg = a.assembleUserMessage(data, false)
	}
	a.conversation = append([]provider.Message{userMsg}, history...)
	a.lastProgramTokens.Store(int64(data.totalTokens))
	a.refreshSnapshot(data, userMsg)

	iterStartedAt := time.Now()
	// Same Context value goes to provider.Generate and to the tracer below,
	// so the trace's input record matches what the model actually saw.
	sent := provider.Context{
		SystemPrompt: a.modelingSystemPrompt,
		Messages:     a.conversation,
		Tools:        a.toolSchemas(),
	}
	a.setPhase(PhaseInference)
	msg, err := a.provider.Generate(ctx, "", sent, provider.Options{MaxTokens: 8192})
	if err != nil {
		return provider.AssistantMessage{}, false, err
	}
	a.totalInputTokens.Add(int64(msg.Usage.InputTokens))
	a.totalOutputTokens.Add(int64(msg.Usage.OutputTokens))
	if msg.StopReason == provider.StopError {
		return msg, false, fmt.Errorf("provider error: %s", msg.Err)
	}
	if msg.StopReason == provider.StopMaxTokens {
		if n := len(msg.Content); n > 0 {
			if _, ok := msg.Content[n-1].(provider.ToolCall); ok {
				msg.Content = msg.Content[:n-1]
			}
		}
		if hasToolCall(msg.Content) {
			msg.StopReason = provider.StopToolUse
		}
	}
	// Skip stats updates on modeling turns: their assistant text is about
	// library curation, not about responding to program outputs, so
	// overlap-with-response would be noise.

	a.lastStopReason = msg.StopReason
	a.conversation = append(a.conversation, msg)
	a.modelingStepsThisTurn++

	var toolResults []provider.ToolResultMessage
	if hasToolCall(msg.Content) {
		a.setPhase(PhaseTools)
	}
	for _, c := range msg.Content {
		if call, ok := c.(provider.ToolCall); ok {
			toolResults = append(toolResults, a.executeTool(call))
		}
	}

	// Modeling-side YIELD nudge. Once the turn has run for
	// modelingYieldStepThreshold steps and is still mid tool-use,
	// append a one-shot wrap-up prompt to the last tool result so the
	// model sees it right before its next response. Suppressed on the
	// bootstrap turn (initial library construction legitimately takes
	// more exploration) and on the wrap-up turn (final guidance already
	// frames it as the last chance to persist). The global maxIterations
	// budget remains the ultimate runaway guard for a model that ignores
	// the nudge.
	if msg.StopReason == provider.StopToolUse &&
		!a.modelingYieldFiredThisTurn &&
		!a.needsBootstrap &&
		!a.finalPhase &&
		a.modelingStepsThisTurn >= modelingYieldStepThreshold &&
		len(toolResults) > 0 {
		last := &toolResults[len(toolResults)-1]
		last.Content = append(last.Content, provider.TextContent{Text: modelingYieldGuidance})
		a.modelingYieldFiredThisTurn = true
	}

	for _, tr := range toolResults {
		a.conversation = append(a.conversation, tr)
	}

	a.writeTrace(iterStartedAt, time.Now(), 0, 0, sent, msg, toolResults, data, userMsg, false, false, TurnModeling)

	// Modeling-turn close: when the model stops calling tools, evaluate
	// no-op suppression, clear modeling state, and flip back to work
	// mode. If finalPhase is set (this was the wrap-up modeling turn),
	// terminate. There is no per-turn step cap: runaway protection
	// inside a turn is the YIELD nudge above, and run-scope protection
	// is the global maxIterations budget enforced at work-cycle
	// boundaries.
	turnClosed := msg.StopReason != provider.StopToolUse
	if turnClosed {
		postHash := a.libraryHash()
		a.modelingNoOpSuppressed = postHash == a.preModelingLibraryHash
		a.currentTurnKind.Store(uint32(TurnWork))
		a.modelingStepsThisTurn = 0
		a.modelingYieldFiredThisTurn = false
		a.workCycleTranscript = nil
		a.needsBootstrap = false
		a.workCyclesSinceModeling = 0
		// Force the next work step to run a fresh inference rather than
		// idle on a stale hash — the modeling turn likely changed the
		// program library, so the next dashboard is materially different.
		a.lastOutputHash = programHash{}
		a.lastStopReason = provider.StopEnd
		if a.finalPhase {
			return msg, true, nil
		}
	}
	return msg, false, nil
}

// maybeScheduleModelingTurn evaluates the cadence predicate at a work cycle
// close and flips currentTurnKind to TurnModeling when one of the triggers
// fires. Triggers (any):
//   - default: the in-cycle yield reinforcement fired during this cycle.
//   - forced: the wrap-up turn after -n was exhausted.
//   - periodic safety net: this many consecutive work cycles closed with
//     no other trigger.
//
// Skipped when the closed work cycle did no tool calls (idle cycle — nothing
// to model) and when the previous modeling turn was a no-op without a fresh
// trigger condition firing.
func (a *Agent) maybeScheduleModelingTurn(prevStopReason provider.StopReason) {
	defaultTrigger := a.yieldFiredThisCycle
	forcedTrigger := a.finalPhase
	a.workCyclesSinceModeling++
	periodicTrigger := a.workCyclesSinceModeling >= modelingPeriodicWorkCycles

	// A cycle that did no tool calls (the agent idled or returned text
	// without invoking any tool) does not produce a modeling turn: nothing
	// happened that could have moved the model. We detect this by checking
	// whether the just-closed cycle was a one-step StopEnd (prev was not
	// StopToolUse), which means no tools were ever invoked in that cycle.
	noToolCycle := prevStopReason != provider.StopToolUse
	if noToolCycle && !forcedTrigger {
		a.yieldFiredThisCycle = false
		return
	}

	freshTrigger := defaultTrigger || forcedTrigger || periodicTrigger
	if !freshTrigger {
		a.yieldFiredThisCycle = false
		return
	}
	// No-op suppression: if the previous modeling turn produced no library
	// changes, we suppress the next firing UNTIL a fresh trigger condition
	// distinct from the one that led to the no-op. Forced and periodic
	// triggers always fire (they are not the "stale" signal); only the
	// default (yield) trigger can be suppressed by the prior no-op.
	if a.modelingNoOpSuppressed && defaultTrigger && !forcedTrigger && !periodicTrigger {
		a.yieldFiredThisCycle = false
		return
	}

	a.currentTurnKind.Store(uint32(TurnModeling))
	transcript := make([]provider.Message, 0, len(a.conversation))
	if len(a.conversation) > 1 {
		transcript = append(transcript, a.conversation[1:]...)
	}
	a.workCycleTranscript = transcript
	a.preModelingLibraryHash = a.libraryHash()
	a.workCyclesSinceModeling = 0
	a.yieldFiredThisCycle = false
	a.modelingFiredAtNs.Store(time.Now().UnixNano())
}

// writeTrace assembles this iteration's trace record and hands it to the
// tracer. Best-effort: tracing is diagnostic, not load-bearing, so any
// error is logged to stderr (visible after the TUI exits) and the run
// continues. A nil tracer short-circuits.
func (a *Agent) writeTrace(
	started, completed time.Time,
	idlePolls int,
	idleWait time.Duration,
	sent provider.Context,
	resp provider.AssistantMessage,
	toolResults []provider.ToolResultMessage,
	data iterationData,
	userMsg provider.UserMessage,
	revisionFired bool,
	yieldFired bool,
	kind TurnKind,
) {
	if a.tracer == nil {
		return
	}
	a.traceSeq++
	activeBudget := a.contextBudget * activeBudgetPercent / 100
	rec := IterationTrace{
		Iteration:       a.traceSeq,
		WorkIteration:   a.workIteration,
		TurnKind:        kind.String(),
		StartedAt:       started,
		CompletedAt:     completed,
		IdlePollsBefore: idlePolls,
		IdleWaitMs:      idleWait.Milliseconds(),
		Context:         serializeProviderContext(sent),
		Response: TraceResponse{
			Model:      resp.Model,
			StopReason: stopReasonString(resp.StopReason),
			Usage:      TraceUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
			Content:    serializeAssistantContent(resp.Content),
		},
		ToolResults: serializeToolResults(toolResults),
		Programs:    buildTracePrograms(data.results, data.inactive, userMsg),
		Budget: TraceBudget{
			LimitTokens:             a.contextBudget,
			UsedTokens:              data.totalTokens,
			Overflowed:              data.overflowed(a.contextBudget),
			RevisionPromptFired:     revisionFired,
			ModelingPromptFired:     yieldFired,
			ActiveBudgetTokens:      activeBudget,
			ExplorationBudgetTokens: a.contextBudget - activeBudget,
		},
		StatsSnapshot: snapshotStats(a.root),
	}
	if err := a.tracer.WriteIteration(rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: trace write failed for iteration %d: %v\n", rec.Iteration, err)
	}
}

// reachedMaxIterations reports whether the -n cap has been hit. Returns false
// when no cap was configured (maxIterations == 0).
func (a *Agent) reachedMaxIterations() bool {
	return a.maxIterations > 0 && a.totalIterations >= a.maxIterations
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

// updateStats folds this iteration's per-program observations into the
// EWMA-based statistics and persists each program's record to
// .autoprobe/statistics/<name>.json. Called once per substantive
// iteration, right after the assistant message comes back so the overlap
// metric can compare program output against what the model actually
// said. Persistence failures are silent — stats are best-effort
// telemetry, not load-bearing for correctness.
//
// Workers run in parallel under a separate errgroup (capped at
// maxProgramConcurrency) because each program's record is its own file
// and there is no shared map to coordinate. The assistant's trigram set
// is computed once and shared read-only across workers; recomputing it
// per program would be pure waste. prevOutputs accesses go through
// prevMu since they're the only shared mutable state.
//
// Metrics that need a prior observation (ChangeFrequency, AvgChangeAmount,
// Staleness) are only updated when prevOutputs has the program's last
// output. On the first iteration after a restart those metrics retain
// their on-disk values and only resume ticking on the second iteration
// when a prev output is available.
func (a *Agent) updateStats(results []programResult, assistantText string) {
	respTri := wordTrigramSet(assistantText)

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxProgramConcurrency)
	for _, r := range results {
		wg.Add(1)
		sem <- struct{}{}
		go func(r programResult) {
			defer wg.Done()
			defer func() { <-sem }()
			a.updateOneStat(r, respTri)
		}(r)
	}
	wg.Wait()
	_ = pruneStats(a.root, liveNamesFromResults(results))
}

// updateOneStat loads, updates, and saves one program's stats record.
// Safe to call concurrently from multiple goroutines as long as each
// goroutine owns a distinct program name: the per-program files are
// independent on disk, and prevOutputs accesses are serialized by prevMu.
func (a *Agent) updateOneStat(r programResult, respTri map[[3]string]struct{}) {
	s := loadStatsFor(a.root, r.name)
	if s == nil {
		s = &programStats{}
	}
	n := s.Samples
	s.AvgOutputTokens = ewma(s.AvgOutputTokens, float64(r.renderedTokens()), n, statsEWMAAlpha)
	s.AvgLatencyMs = ewma(s.AvgLatencyMs, float64(r.latency.Milliseconds()), n, statsEWMAAlpha)
	s.OverlapWithResp = ewma(s.OverlapWithResp, overlapRecall(string(r.output), respTri), n, statsEWMAAlpha)

	a.prevMu.Lock()
	prev, hasPrev := a.prevOutputs[r.name]
	a.prevOutputs[r.name] = r.output
	a.prevMu.Unlock()

	if hasPrev {
		changed := !bytes.Equal(prev, r.output)
		obs := 0.0
		if changed {
			obs = 1.0
		}
		s.ChangeFrequency = ewma(s.ChangeFrequency, obs, n, statsEWMAAlpha)
		s.AvgChangeAmount = ewma(s.AvgChangeAmount, changeAmount(prev, r.output), n, statsEWMAAlpha)
		if changed {
			s.Staleness = 0
		} else {
			s.Staleness++
		}
	}
	s.Samples++
	_ = saveStatsFor(a.root, r.name, s)
}

// refreshSnapshot rebuilds the per-program snapshot the TUI reads via
// LastProgramSnapshot. Called once per substantive iteration, right after
// assembleUserMessage so the inclusion check reflects what was actually
// packed into the user message (rendered vs sentinel). prevOutputs is read
// under prevMu but not modified — updateStats overwrites it later in the
// same Step, after Generate returns.
func (a *Agent) refreshSnapshot(d iterationData, userMsg provider.UserMessage) {
	const renderedPrefix = "[program="
	included := map[string]int{} // 1 = rendered, 2 = sentinel
	for _, c := range userMsg.Content {
		s := c.Text
		if !strings.HasPrefix(s, renderedPrefix) {
			continue
		}
		rest := s[len(renderedPrefix):]
		end := strings.IndexAny(rest, " ]")
		if end < 0 {
			continue
		}
		name := rest[:end]
		// Sentinels write "[program=NAME dropped: ...]"; rendered output
		// writes "[program=NAME exit=N]\n...". Distinguish on the bytes
		// immediately after the name token.
		after := rest[end:]
		if strings.HasPrefix(after, " dropped:") {
			included[name] = 2
		} else {
			included[name] = 1
		}
	}

	a.prevMu.Lock()
	defer a.prevMu.Unlock()
	snap := make([]ProgramSnapshot, 0, len(d.results))
	for _, r := range d.results {
		_, inactive := d.inactive[r.name]
		prev, hasPrev := a.prevOutputs[r.name]
		changed := hasPrev && !bytes.Equal(prev, r.output)
		kind := included[r.name]
		snap = append(snap, ProgramSnapshot{
			Name:              r.name,
			RenderedTokens:    r.renderedTokens(),
			Active:            !inactive,
			IncludedInContext: kind == 1,
			Dropped:           kind == 2,
			ChangedThisIter:   changed,
		})
	}

	a.snapshotMu.Lock()
	a.lastSnapshot = snap
	a.snapshotMu.Unlock()
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
// per the program contract), the captured combined stdout+stderr bytes,
// and the wall-clock time the run took. Latency feeds the per-program
// latency EWMA in stats.
type programResult struct {
	name     string
	exitCode int
	output   []byte
	latency  time.Duration
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

// runReinforcementPrompt executes every script in reinforcement/<name>/ in
// lex order and joins their non-empty stdout with blank lines. Scripts
// derive their own paths from $0 (see the shipped general.sh) so the
// rendered prompt always carries fully-resolved paths regardless of where
// the probe directory lives. extraEnv is appended to the script's
// environment ("KEY=VALUE" entries) and lets the harness signal context to
// the script — e.g. $AUTOPROBE_FINAL=1 on the wrap-up firing. A missing or
// empty directory yields "" and the prompt is simply omitted from the
// iteration's context.
func (a *Agent) runReinforcementPrompt(name string, extraEnv ...string) string {
	dir := filepath.Join(a.reinforcementDir, name)
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
		cmd := exec.Command(filepath.Join(dir, name))
		if len(extraEnv) > 0 {
			cmd.Env = append(os.Environ(), extraEnv...)
		}
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
	data, err := a.runIteration(ctx)
	if err != nil {
		return nil, err
	}
	if a.CurrentTurnKind() == TurnModeling {
		return []provider.Message{a.assembleModelingUserMessage(data, nil, a.needsBootstrap, false)}, nil
	}
	return []provider.Message{a.assembleUserMessage(data, false)}, nil
}

// assembleModelingUserMessage builds the user message that kicks off a
// modeling turn. Three parts in order:
//  1. The same assembled program-output context the just-closed work cycle
//     saw (packed with the same lex-order / 80-20 logic so the byte-stable
//     region cache hit carries over from the work cycle's last request).
//  2. The prior work cycle's transcript serialized to text — flattened from
//     provider.Message records so we don't have to round-trip provider-
//     native signatures (Anthropic thinking, OpenAI reasoning, Google
//     thought) under a different system prompt where they may not validate.
//  3. A short guidance block (bootstrap / final / default framing).
//
// transcript may be nil (bootstrap firing — there is no prior cycle to
// review). When bootstrap is set, the program-output region is also empty
// in practice because the library is empty.
func (a *Agent) assembleModelingUserMessage(d iterationData, transcript []provider.Message, bootstrap, final bool) provider.UserMessage {
	work := a.assembleUserMessage(d, false)
	contents := work.Content
	if len(transcript) > 0 {
		var b strings.Builder
		b.WriteString("[PRIOR WORK CYCLE TRANSCRIPT]\n")
		serializeTranscriptText(&b, transcript)
		contents = append(contents, provider.TextContent{Text: b.String()})
	}
	guidance := modelingGuidance
	switch {
	case bootstrap:
		guidance = modelingBootstrapGuidance
	case final:
		guidance = modelingFinalGuidance
	}
	contents = append(contents, provider.TextContent{Text: guidance})
	return provider.UserMessage{Content: contents}
}

// serializeTranscriptText writes each prior-cycle message to b in a compact
// human-readable form. Assistant text/thinking content is rendered with role
// markers, tool calls are summarized as "[tool=NAME args=...]", and tool
// results carry their ToolName / IsError flag inline. Lossy compared to the
// structured form but adequate for the modeling turn's review pass.
func serializeTranscriptText(b *strings.Builder, msgs []provider.Message) {
	for _, m := range msgs {
		switch m := m.(type) {
		case provider.AssistantMessage:
			b.WriteString("\n--- assistant ---\n")
			for _, c := range m.Content {
				switch c := c.(type) {
				case provider.TextContent:
					b.WriteString(c.Text)
					b.WriteByte('\n')
				case provider.ThinkingContent:
					if c.Thinking != "" {
						b.WriteString("(thinking) ")
						b.WriteString(c.Thinking)
						b.WriteByte('\n')
					}
				case provider.ToolCall:
					b.WriteString("[tool=")
					b.WriteString(c.Name)
					if len(c.Arguments) > 0 {
						b.WriteString(" args=")
						b.Write(c.Arguments)
					}
					b.WriteString("]\n")
				}
			}
		case provider.ToolResultMessage:
			b.WriteString("\n--- tool result: ")
			b.WriteString(m.ToolName)
			if m.IsError {
				b.WriteString(" (error)")
			}
			b.WriteString(" ---\n")
			b.WriteString(provider.JoinText(m.Content))
			b.WriteByte('\n')
		}
	}
}

// iterationData bundles one iteration's worth of program execution: the
// per-program results sorted by name, the pre-selection hash over (name,
// exit, stdout) triples, the inactive-set membership, and the summed
// token estimate. Carried into assembleUserMessage and into the idle/
// overflow bookkeeping in Step.
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
		if text := a.runReinforcementPrompt(revisionReinforcementName); text != "" {
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
			info, statErr := os.Stat(path)
			if statErr != nil {
				return fmt.Errorf("stat %s: %w", name, statErr)
			}
			// The write tool creates files as 0644, and the agent reliably forgets
			// to chmod +x before the next iteration. Rather than burning a turn on
			// the fix, just set the execute bit ourselves.
			if info.Mode()&0o111 == 0 {
				if err := os.Chmod(path, info.Mode()|0o111); err != nil {
					return fmt.Errorf("chmod %s: %w", name, err)
				}
			}
			start := time.Now()
			out, runErr := exec.CommandContext(gctx, path).CombinedOutput()
			elapsed := time.Since(start)

			var exitErr *exec.ExitError
			if runErr != nil && !errors.As(runErr, &exitErr) {
				return fmt.Errorf("running %s: %w", name, runErr)
			}
			exitCode := 0
			if exitErr != nil {
				exitCode = exitErr.ExitCode()
			}
			results[i] = programResult{name: name, exitCode: exitCode, output: out, latency: elapsed}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}
