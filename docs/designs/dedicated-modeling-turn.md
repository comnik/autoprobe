# Dedicated modeling turn

## Implementation status

**Proposed.** Not yet implemented. This is the first of three designs aimed at
relieving the tension between the agent's user-provided goal and its meta-goal
of modeling the environment in the program library; the other two
([signal-driven reinforcement](signal-driven-reinforcement.md) and
[probe-debt meta-probe](probe-debt-meta-probe.md)) are deferred and to be
revisited after this one lands.

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

- **System prompt.** "You are not solving the user's task right now. The
  previous work cycle's assistant messages and tool results are below for
  context. Your only job is to update the library so it remains an accurate
  executable model of the environment — incorporating anything the last cycle
  revealed, removing anything that has drifted out of date. When you are
  done, respond with no tool calls."
- **User message.** Includes
  - the same probe-output context the work cycle saw, so the agent sees the
    current dashboard;
  - a transcript of the work cycle's assistant turns and tool results, so
    the agent can see what it just did;
  - guidance on what to look for (repeated reads, repeated bash commands,
    verbose outputs that should be compressed, dead programs to remove —
    much of the substance that today's `modeling/general.sh` reinforcement
    inlines into the work cycle).

The modeling turn uses the same tool surface (`read`, `bash`, `edit`, `write`)
and runs the same way as a work cycle from the harness's perspective —
multiple tool-use turns until the model emits a response with no tool calls.
The only differences are the system prompt, the user message, and that the
turn does not count toward the configured iteration budget (`-n`).

### Cadence

A modeling turn runs between work cycles, not on every work cycle. Always-on
modeling would double the inference cost without much marginal benefit for
cycles where the agent did little or nothing novel. The harness decides when
to fire one based on a coarse trigger:

- **Default trigger.** A work cycle that consumed more than a configurable
  in-cycle token budget (a similar threshold to the one
  `modeling/general.sh` already uses).
- **Forced trigger.** The wrap-up turn after `-n` is exhausted runs as a
  modeling turn rather than appending the final-iteration reinforcement to
  the next work cycle.
- **Skip trigger.** A work cycle that did no tool calls (e.g., the agent
  idled because nothing changed) does not get a modeling turn — nothing
  happened that could have moved the model.

A simple counter — "iterations since last modeling turn" — can also force a
modeling turn periodically if the in-cycle threshold has not been hit in a
while, to ensure long-running quiet cycles still get curated.

### What the modeling turn does *not* do

- It does not change the work cycle's behavior. The work cycle still has
  access to the full tool set and can still write to the library mid-flow
  if the agent chooses to — this is not a moratorium on in-cycle library
  edits, just an explicit place to do them with full attention.
- It does not replace `modeling/general.sh`. That reinforcement still fires
  inside the work cycle on the existing threshold to encourage yielding;
  the new modeling turn is what happens *after* the yield. Over time, if
  the dedicated turn proves effective, the in-cycle reinforcement can be
  softened or removed.
- It does not introduce a separate process or sub-agent. Same harness,
  same model, same library, same provider connection. Just an additional
  inference with a different prompt.

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
- **Reinforcement placement.** The `[MODELING]` / `[REVISION]`
  reinforcements today are appended to user-message content. They must
  land *after* breakpoint 3 (the byte-stable program-output prefix),
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

## Open questions

- **How much context from the work cycle should the modeling turn see?**
  Full transcript is most informative but expensive. A summary or a
  filtered view (e.g., only tool calls + brief excerpts) may be enough.
  Worth measuring empirically once the basic flow is in place.
- **Modeling turn iteration budget.** The modeling turn itself can run
  multiple tool-use turns (read a file, edit a probe, run it to verify,
  etc.). Some upper bound is needed to prevent runaway. A simple
  in-cycle token cap mirrors the work cycle's existing protections.
- **Does the in-cycle `[MODELING]` reinforcement still earn its keep?**
  If the dedicated modeling turn handles capture well, the in-cycle nudge
  may become unnecessary or even counterproductive. Plan to measure
  before/after and revisit.
- **Should the modeling turn see the *next* work cycle's anticipated
  framing?** I.e., should it know what the user goal still is so it can
  prioritize what to capture? Probably yes — the goal probe is part of the
  program-output context the modeling turn already sees, so this comes for
  free.
