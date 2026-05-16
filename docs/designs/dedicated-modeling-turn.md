# Dedicated modeling turn

## Implementation status

**Landed.** The work/modeling split is live in `agent.go` (see `TurnKind`,
`stepModeling`, `assembleModelingUserMessage`, and the
`identity.sh`/`work.sh`/`modeling.sh` system assets). A few details
diverged from the design as written; they are flagged inline below with
**[diverged]** notes. The other two designs in this thread
([signal-driven reinforcement](signal-driven-reinforcement.md) and
[probe-debt meta-probe](probe-debt-meta-probe.md)) remain deferred.

## Problem

Today the agent is asked to hold two goals simultaneously within a single
tool-use cycle:

1. **The user goal.** Make tests pass, debug a regression, build a feature,
   etc.
2. **The meta-goal.** Evolve the program library so that it remains an
   accurate executable model of the environment — one tailored to the
   current user goal, but persistent across iterations.

These two goals compete for attention during the same inference, and in
practice the user goal dominates. The agent is mid-thought when a `[MODELING]`
reinforcement fires (see `assets/reinforcement/modeling/general.sh`), and the
nudge interrupts a working train of reasoning to ask it to context-switch into
"librarian mode." The agent resists the switch — sometimes by ignoring the
nudge, sometimes by writing a perfunctory probe and immediately resuming work,
sometimes by complying but losing the thread of the original task.

The result is that the model of the environment drifts: it captures less of
what the agent has actually learned than it could, and stays out of date
relative to the current state of the codebase and the in-flight task.

An earlier exploration considered a more invasive split — spawning a separate
coding agent each turn via `bash` with a verifiable sub-goal, then deriving
library updates from its exported session. That approach pays heavy token and
engineering costs (sub-agent sessions are 50K–500K tokens; the entity touching
the environment can't directly write to the library; mid-flight steering and
feedback loops weaken; coupling to a specific frontier harness). The
dedicated-modeling-turn design described here aims at the same underlying
tension with a much smaller delta.

## Design

### Two turn types in the agent loop

The work cycle stays as it is today: the agent receives the assembled
program-output context, runs tool-use turns until it produces a response with
no tool calls, and the cycle closes. Then, *before* starting the next work
cycle, the harness runs a **modeling turn** — a separate inference, with the
same model, the same library, the same probe outputs, but a different system
prompt and a different user message.

The modeling turn's framing:

- **System prompt.** Composed from `assets/system/identity.sh` plus
  `assets/system/modeling.sh` — see [Splitting the cornerstone into
  system-prompt assets](#splitting-the-cornerstone-into-system-prompt-assets)
  below. The mode-specific portion says, in effect: "You are not solving
  the user's task right now. The previous work cycle's assistant messages
  and tool results are below for context. Your only job is to update the
  library so it remains an accurate executable model of the environment —
  incorporating anything the last cycle revealed, removing anything that
  has drifted out of date. When you are done, respond with no tool calls."
- **User message.** Three parts, in order:
  1. The same assembled program-output context the just-closed work cycle
     saw (probes packed in lex order, the byte-stable region from
     [context-budget.md](context-budget.md)).
  2. The full transcript of the just-closed work cycle's assistant turns
     and tool results — the same messages the work cycle accumulated, in
     order. First version passes the transcript verbatim; filtering or
     summarizing is deferred until we measure whether it's worth it.
  3. A short guidance block (repeated reads, repeated bash commands,
     verbose outputs that should be compressed, dead programs to remove)
     appended after the transcript.

The modeling turn uses the same tool surface (`read`, `bash`, `edit`, `write`)
and runs the same way as a work cycle from the harness's perspective —
multiple tool-use turns until the model emits a response with no tool calls.
The only differences are the system prompt, the user message, and that the
turn does not count toward the configured iteration budget (`-n`).

### Splitting the cornerstone into system-prompt assets

Today autoprobe has no formal system prompt: the `aaa-cornerstone` program
emits identity/framing text into the user-message slot every iteration.
That conflates two things — *who the agent is* (stable across iterations
and across modes) and *what's currently in the environment* (the probe
outputs that follow). It also wastes prompt-cache potential, because the
identity text sits below the user-message position where the byte-stable
region ends.

With a dedicated modeling turn, autoprobe now needs two system prompts
(one per mode), so this is the moment to formalize the system slot
properly. The cornerstone is split into mode-aware assets under a new
`assets/system/` directory:

```
assets/system/
├── identity.sh   # shared — who the agent is, tool usage prose,
│                 #   exit codes as status channel, lex-order attention,
│                 #   $AUTOPROBE_PROGRAMS_DIR is your persistent memory.
├── work.sh       # work-mode add-on — pursue the user goal *via*
│                 #   keeping the library an accurate model.
└── modeling.sh   # modeling-mode add-on — review the prior cycle,
                  #   update the library accordingly.
```

The harness composes the per-mode system prompt at startup: work mode is
`identity + work`, modeling mode is `identity + modeling`. Both render to
strings that go into the provider's system slot, which is exactly where
breakpoint 2 lives in the [Prompt caching](#prompt-caching) layout —
identity material gets the deepest cache hits, mode add-ons cache
per-mode without interfering.

Tools usage prose (the "Available tools" list and the "Guidelines"
block from the current cornerstone) lives in `identity.sh`, not a
separate file. Keeping it bundled there avoids a third asset for one
small section and matches how a reader actually wants to consume the
prompt — identity, then how-to-use-tools, then mode-specific framing.
Note that this is *prose about tool usage*, distinct from the JSON tool
schemas which already live in `tools.go` and are passed via the
provider's `tools` parameter (breakpoint 1).

The trailing transition sentence in today's cornerstone — *"Here are the
current outputs from active programs in the $AUTOPROBE_PROGRAMS_DIR
directory…"* — has no home in the new structure. It was a hand-off
between the cornerstone's identity text and the program outputs in the
same user message. With identity moved into the system slot, the program
outputs are simply the user message; they don't need an introductory
sentence. Drop it.

After this split, `assets/programs/aaa-cornerstone` is deleted. Nothing
remains for it to emit, and the `aaa-` attention slot in the program-
output region opens up for the agent's own high-priority probes.

### Cadence

A modeling turn runs between work cycles, not on every work cycle. Always-on
modeling would double the inference cost without much marginal benefit for
cycles where the agent did little or nothing novel. The harness decides
when to fire one based on a small predicate:

- **Bootstrap trigger.** At the start of a run, if `programs/` is empty
  (or contains only the inline `--goal` probe), the harness runs a
  modeling turn *before* the first work iteration. The cornerstone used
  to seed the library implicitly; now that the cornerstone is gone, an
  empty library means the agent has no model to start from, and a
  bootstrap modeling turn is the right way to install one. See
  [Bootstrap modeling turn](#bootstrap-modeling-turn) below for how its
  framing differs from the regular case.
- **Default trigger.** The in-cycle yield reinforcement (see [The in-cycle
  reinforcement becomes yield-only](#the-in-cycle-reinforcement-becomes-yield-only)
  below) fired at least once during the just-closed work cycle. That
  condition already means "the cycle accumulated enough in-cycle drag that
  it was worth yielding," which is the same signal we want for "there is
  likely something worth modeling." Reuses the existing
  `modelingThresholdTokens` (32K) and the `modelingFired` flag the harness
  already tracks.
- **Forced trigger.** `-n` exhausted. The wrap-up runs as a modeling turn
  rather than appending a final-iteration reinforcement to a next work
  cycle that will never happen.
- **Periodic safety net.** If neither of the above has fired for N work
  cycles in a row (default: 10), force a modeling turn anyway, so
  long-running quiet phases still get curated.
- **Skip trigger.** A work cycle that did no tool calls (the agent idled,
  or returned text without invoking any tool) does not get a modeling
  turn — nothing happened that could have moved the model.

#### Bootstrap modeling turn

The bootstrap firing is structurally identical to a regular modeling
turn — same system prompt, same harness machinery, same tool surface —
but the user message differs in two ways because there is no prior work
cycle to look at:

1. No work-cycle transcript section. The agent has nothing to review.
2. The guidance block names the situation: *"The library is empty (or
   contains only a goal probe). Install initial programs that model the
   parts of the environment relevant to the goal — what's in the
   repository, what builds, what passes, what's broken — so the first
   work iteration starts from a real dashboard rather than a blank
   one."*

**[diverged]** The implementation does not use `AUTOPROBE_BOOTSTRAP`.
Bootstrap framing is selected in-process: `Prime` sets
`a.needsBootstrap=true` when `programs/` is empty, and
`assembleModelingUserMessage` switches the guidance constant from
`modelingGuidance` to `modelingBootstrapGuidance`. The guidance text
lives inline in `agent.go` rather than in a script asset — promote it
to `assets/system/modeling-guidance/*.sh` if it grows.

A bootstrap modeling turn that produces no library mutations is a
warning sign — the agent declined to install anything despite an empty
library — but the harness still proceeds to the first work cycle. The
work cycle will run with a sparse dashboard; the agent has the option
to install probes mid-cycle if it wants to. Subsequent modeling turns
will fire under the regular triggers.

#### No-op suppression

If a modeling turn closes without writing to `programs/` or `inactive`, it
produced nothing of value. The harness suppresses the next modeling turn
until something in the work loop justifies one again — specifically, until
the in-cycle yield reinforcement fires again, or `-n` is exhausted, or the
periodic safety-net counter elapses. Without this, every threshold-
crossing work cycle would trigger an empty modeling turn, training the
agent to phone in the work (and burning inference on nothing).

The suppression is per-trigger, not global: a no-op modeling turn doesn't
disable future modeling, it just gates the next one on a *fresh* signal
rather than the stale "this cycle dragged" signal that already led to the
no-op.

### The in-cycle reinforcement becomes yield-only

The existing `assets/reinforcement/modeling/general.sh` does two jobs
today: it asks the agent to (a) write a probe capturing what was just
learned, and (b) end the tool-use cycle. With a dedicated modeling turn
taking over (a) properly, the in-cycle reinforcement collapses to (b)
alone: a yield nudge.

Rewriting the script's body removes the "compress" language and keeps
only the close-the-cycle framing — something like: *"You have accumulated
significant in-cycle drag. Respond with a brief plain-text summary and
NO further tool calls. A modeling turn will run next and update the
library based on what just happened."*

The trigger plumbing stays as it is — same threshold, same cooldown,
same `modelingFired` flag — because the trigger is also what tells the
modeling-turn cadence (above) that a modeling turn should fire. The
content change is one shell script and nothing else.

**[diverged]** An early version of the script kept an
`AUTOPROBE_FINAL=1` branch that emitted last-iteration framing when
the wrap-up turn fired. That branch is gone: the wrap-up runs as a
modeling turn (per the forced trigger above), and its framing lives
in the `modelingFinalGuidance` constant selected by
`assembleModelingUserMessage(..., final=true)`. The reinforcement
script now does the yield-only job and nothing else.

Once we have measured the effect of the modeling turn, a follow-up rename
of the in-cycle thing (directory `assets/reinforcement/modeling/` →
`yield/`, constant `modelingReinforcementName` → `yieldReinforcementName`,
field `modelingFired` → `yieldFired`, etc.) makes the split between the
two concerns clean at the code level too. Deferred so the diff for this
design stays focused.

### What the modeling turn does *not* do

- It does not change the work cycle's behavior. The work cycle still has
  access to the full tool set and can still write to the library mid-flow
  if the agent chooses to — this is not a moratorium on in-cycle library
  edits, just an explicit place to do them with full attention.
- It does not introduce a separate process or sub-agent. Same harness,
  same model, same library, same provider connection. Just an additional
  inference with a different prompt.
- It does not run unconditionally. The cadence predicate above gates
  firing; idle cycles and no-op modeling turns both back off rather than
  spinning.

### Implementation mechanics

The modeling turn slots into the existing `Step`/`Run` structure in
`agent.go` as a parallel inference path, not a parallel goroutine. After
each work cycle closes (`lastStopReason != StopToolUse`), the cadence
predicate is evaluated; if it fires, the harness runs a modeling turn
before the next `runIteration` call.

A modeling turn is itself a loop of `Step`-like inferences with a
distinct `kind` flag threaded through, controlling:

- which system prompt is used (work-mode cornerstone vs. modeling-mode
  asset);
- whether the iteration counter (`a.iteration`) advances — it does for
  work iterations, it does not for modeling turns. `maxIterations` (`-n`)
  is a budget over work iterations only;
- which cache breakpoint set is applied (see [Prompt
  caching](#prompt-caching) below);
- which value is written into the trace's `turn_kind` field.

The modeling turn has its own in-turn safety cap — a maximum number of
inference steps inside one modeling turn (default: 8). Beyond that, the
harness closes the modeling turn even if the model is still calling
tools, logs the truncation, and proceeds to the next work cycle. This
mirrors the work cycle's existing protections and prevents a runaway
modeling turn from blocking forward progress.

#### Failure handling

If a modeling turn hits a provider error, exceeds its in-turn cap, or
otherwise terminates abnormally:

- Whatever library mutations the modeling turn *did* commit to disk stay
  committed — `programs/` and `inactive` are persistent state and not
  rolled back.
- The trace records the failure on the modeling-turn iteration.
- The next work cycle starts normally. Library state is whatever the
  partial modeling turn left it as; the next work cycle's program
  outputs reflect that.

Failure modes are explicitly recoverable: the modeling turn can be
incomplete without corrupting forward progress, because the library is
the ground truth and a half-applied modeling turn just means the next
modeling turn has more to do.

### Interaction with idle backoff

A modeling turn that produces no library mutations is informative: it means
the work cycle didn't reveal anything worth capturing. The harness should
still count this as a substantive iteration for statistics purposes, because
an inference happened, but it should not fight idle backoff — the backoff
logic compares pre-selection program-output hashes, which a modeling turn
does not change.

### Interaction with the trace

The modeling turn is captured in the run trace the same way as any other
iteration, but tagged distinctly so operators reviewing
`.autoprobe-last-run/index.html` can see at a glance which turns were work
and which were modeling. This also gives us a measurable signal — "what did
the modeling turn change" — which is the natural success metric for this
design.

Concretely: `IterationTrace` and the `LogIteration` summary line gain a
`TurnKind string` field with values `"work"` (default) or `"modeling"`.
Existing traces without the field render as `"work"` for backward
compatibility. The HTML renderer adds a small badge in the per-iteration
header so operators can scan the iteration list and tell at a glance
which turn was which.

### Interaction with the TUI

The dashboard's phase strip (see [tui.md](./tui.md)) gets a new phase
value, `PhaseModeling`, that the agent sets while a modeling turn's
inferences are running and clears when the turn closes. The strip
shows it as a distinct state alongside `PhaseRunPrograms` /
`PhaseInference` / `PhaseTools`.

The existing `MODELING` flash on the drag bar is separate: it indicates
that the in-cycle yield reinforcement fired inside the current work
cycle, not that the dedicated turn is running. Both signals coexist —
the flash is "the yield prompt just landed in a tool result," the
phase indicator is "a modeling turn is currently executing." Keeping
them visually distinct (a brief flash vs. a sustained phase) reads
correctly to an operator watching the dashboard.

### Prompt caching

The natural worry about alternating modes is cache thrashing: if work and
modeling turns invalidate each other's prefixes, we pay full input cost on
every iteration. They don't, because the two modes are not flipping one
conversation back and forth — each autoprobe iteration is already a fresh
API call (context is reconstructed from scratch every iteration; see the
top-level architecture notes in the README). Work and modeling are simply
two different requests with two different prefixes, both held in the
provider's prefix cache simultaneously, each with its own TTL-refresh-on-hit
lifecycle.

Anthropic's explicit-breakpoint model (up to 4 `cache_control` blocks per
request, 5-minute ephemeral TTL by default, 1-hour available at a higher
write cost) makes this controllable. Other providers (OpenAI, DeepSeek)
use implicit prefix matching but the same reasoning applies — two
distinct prefixes both stay warm independently. The structuring discipline
described here is provider-agnostic; the specific breakpoint API is
Anthropic-specific.

#### Breakpoint layout

Four breakpoints, ordered by stability (most stable first):

1. **End of tools.** Tools are identical between work and modeling modes,
   so this breakpoint caches once and is reused across every iteration in
   either mode. Keeping the tool list literally identical between modes
   is what makes this work — see the failure modes section below.
2. **End of system prompt.** Mode-specific. The work-mode system prompt
   and the modeling-mode system prompt sit in two separate cache lanes;
   both stay warm as long as their respective TTLs are refreshed by
   hits.
3. **End of the byte-stable program-output region in the user message.**
   This corresponds to the 80% active slice from
   [context-budget.md](context-budget.md), packed in lex order. That
   design already commits to byte-stability of this region precisely for
   prefix-caching reasons; the dedicated modeling turn inherits the
   benefit. The 20% exploration tail and any reinforcement messages
   must sit *after* this breakpoint.
4. **Rolling breakpoint at the end of the message history.** Within a
   work cycle, the agent's tool-use turns accumulate (tool call → tool
   result → tool call → …). Placing breakpoint 4 at the latest message
   lets each subsequent tool call inside a cycle hit cache at the full
   prior depth. Modeling turns are expected to be short (1–3 tool-use
   turns); the rolling breakpoint is less load-bearing for them but
   costs nothing to include.

#### TTL choice differs by mode

- **Work-mode system prompt: default 5-minute ephemeral.** Work cycles
  fire frequently — typically well under 5 minutes apart — so
  TTL-refresh-on-hit keeps the prefix permanently warm at the cheap
  1.25x write cost.
- **Modeling-mode system prompt: 1-hour ephemeral.** Modeling turns fire
  less often by design (only after a work cycle that revealed something
  worth capturing). If modeling turns can be > 5 minutes apart, the
  5-minute TTL would go cold between fires and we'd pay the write cost
  every time. The 1-hour TTL costs 2x on write but is amortized over
  many subsequent modeling turns within the hour. This applies only to
  the system prompt block; the user-message portion changes every
  modeling turn and should not be cached at extended TTL.

#### Failure modes to engineer against

- **Tool drift between modes.** Tools sit *before* the system prompt in
  the cached prefix. Any divergence in the tool list (different
  schemas, different ordering, an extra tool only available in one
  mode) breaks the tools-layer cache for the diverging mode. The two
  modes must share one tool definition, defined once and passed
  identically to both. If we ever decide a modeling-only tool is worth
  it, we accept paying the tools-layer write cost on every modeling
  turn.
- **Dynamic content above breakpoints.** No timestamps, run UUIDs, or
  iteration counters in the system prompt. If we want those values
  visible to the agent, they belong inside a probe — i.e., in the
  user-message portion where churn is expected.
- **Reinforcement placement.** The in-cycle yield reinforcement and the
  `[REVISION]` reinforcement are appended to user-message content. They
  must land *after* breakpoint 3 (the byte-stable program-output prefix),
  not interspersed within it, so adding or removing a reinforcement
  doesn't invalidate the program-output cache.

#### Expected post-modeling cache behaviour

When a modeling turn modifies the library, the *next* work cycle's
program-output region will change wherever the modified probes
contribute output. This is unavoidable — it is the point of the
modeling turn. Cache behaviour after a modeling turn:

- Breakpoints 1 and 2 (tools, work-mode system) still hit.
- Breakpoint 3 (program outputs) partially invalidates from the
  position of the first changed probe onward in lex order. Probes
  before that position still hit; probes from that position to the
  end of the 80% slice re-hash.
- Breakpoint 4 doesn't apply yet — fresh work cycle starts with an
  empty message history.

Lex order makes this predictable: a modeling turn that adds or
modifies a `zzz-` probe invalidates less of the program-output prefix
than one that touches `aaa-` probes. Not worth optimizing for
explicitly, but worth understanding when reading cache-hit telemetry.

## Why this is the minimal viable intervention

The dedicated modeling turn keeps every property of the current
architecture:

- One process, one model, one library, one harness.
- The same agent that touched the environment writes the probes — no
  information loss across a sub-agent boundary.
- Mid-flight human steering via the TUI is preserved.
- Provider-agnostic — the design adds no provider-specific machinery.

It changes exactly one thing: it stops asking the agent to hold both goals
within a single turn. The two goals are alternated *between* turns instead
of held in tension *within* turns. Everything else follows from that.

## Files touched by this design

- `agent.go` — thread a `kind` parameter through `Step` (work vs.
  modeling). Add the cadence predicate evaluated at cycle close
  (in-cycle yield fired ∨ `-n` exhausted ∨ periodic counter elapsed,
  minus no-op suppression). Add the bootstrap firing at run start when
  the library is empty. Add `PhaseModeling` to the phase enum and
  set/clear it around modeling-turn inferences. Don't advance
  `a.iteration` on modeling-turn steps. Track the modeling-turn in-turn
  step counter and cap (default 8). Track the no-op suppression flag
  (cleared by any of the trigger conditions firing fresh). Wire two
  system-prompt compositions (`identity + work`, `identity + modeling`)
  into the provider call.
- `trace.go` — add `TurnKind string` to `IterationTrace` and the
  `LogIteration` summary line. Default `"work"` for backward compatibility.
- `trace_render.go` / viewer assets — render the modeling badge in the
  per-iteration header of the HTML trace.
- `tui.go` — surface `PhaseModeling` in the phase strip alongside the
  existing phase values. The drag-bar `MODELING` flash stays as-is — it
  signals the in-cycle yield reinforcement, not the dedicated turn.
- `assets/system/identity.sh` (new) — shared system-prompt asset: who
  the agent is, tool usage prose, exit-code contract, lex-order attention
  placement, `$AUTOPROBE_PROGRAMS_DIR` as persistent memory.
- `assets/system/work.sh` (new) — work-mode add-on: pursue the user goal
  via maintaining an accurate library model.
- `assets/system/modeling.sh` (new) — modeling-mode add-on: review the
  prior work cycle (or the empty starting state when `AUTOPROBE_BOOTSTRAP=1`)
  and update the library accordingly.
- `assets/programs/aaa-cornerstone` — **deleted.** Identity content moves
  to `assets/system/identity.sh`; mode-specific framing moves into
  `work.sh`/`modeling.sh`; trailing user-message transition sentence is
  dropped.
- `assets/reinforcement/modeling/general.sh` — rewrite body to yield-only
  framing (drop the "compress" / "write a program" language; keep the
  close-the-cycle framing).
- `init_tui.go` / `config.go` — `autoprobe init` no longer ships the
  cornerstone into `programs/`; `programs/` starts empty (or with the
  inline `--goal` probe). The first run picks up the bootstrap trigger
  naturally.
- `docs/designs/dedicated-modeling-turn.md` — this document.

## Open questions

- **How much context from the work cycle should the modeling turn see?**
  First version passes the full transcript verbatim. Worth measuring
  whether a summarized or filtered transcript performs as well at lower
  token cost once the basic flow is in place.
- **Periodic safety-net cadence.** Default of "force after 10 work cycles
  with no other trigger" is a guess. Worth tuning once we have data on how
  often the default trigger fires in practice.
- **In-turn step cap for modeling turns.** Default 8 is a guess too —
  enough for "read a file, write a probe, run it, fix it, write another"
  but capped well short of runaway. Tune empirically.
- **Should the modeling turn see the *next* work cycle's anticipated
  framing?** I.e., should it know what the user goal still is so it can
  prioritize what to capture? **Resolved (with a divergence).** The
  modeling turn sees the goal. The design assumed it would arrive "for
  free" as a goal probe in the library; the implementation instead
  appends `--goal` as a dedicated `[YOUR GOAL]` text block at the tail
  of `assembleUserMessage`, which `assembleModelingUserMessage`
  inherits. Same outcome (goal visible in both turn kinds), different
  mechanism. Worth revisiting if/when we want a single source of truth
  in the library.
