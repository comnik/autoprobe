# Dedicated distill turn

## Implementation status

**Proposed.** Not yet implemented. This is the first of three designs aimed at
relieving the tension between the agent's user-provided goal and its meta-goal
of distilling learnings into the program library; the other two
([signal-driven reinforcement](signal-driven-reinforcement.md) and
[probe-debt meta-probe](probe-debt-meta-probe.md)) are deferred and to be
revisited after this one lands.

## Problem

Today the agent is asked to hold two goals simultaneously within a single
tool-use cycle:

1. **The user goal.** Make tests pass, debug a regression, build a feature,
   etc.
2. **The meta-goal.** Evolve the program library so that what it learns this
   cycle compresses into a probe and persists across iterations.

These two goals compete for attention during the same inference, and in
practice the user goal dominates. The agent is mid-thought when a `[DISTILL]`
reinforcement fires (see `assets/reinforcement/distill/general.sh`), and the
nudge interrupts a working train of reasoning to ask it to context-switch into
"librarian mode." The agent resists the switch — sometimes by ignoring the
nudge, sometimes by writing a perfunctory probe and immediately resuming work,
sometimes by complying but losing the thread of the original task.

The result is that distillation happens unevenly and shallowly, and the
library captures less of what the agent has actually learned than it could.

An earlier exploration considered a more invasive split — spawning a separate
coding agent each turn via `bash` with a verifiable sub-goal, then distilling
from its exported session. That approach pays heavy token and engineering
costs (sub-agent sessions are 50K–500K tokens; the entity touching the
environment can't directly write to the library; mid-flight steering and
feedback loops weaken; coupling to a specific frontier harness). The
dedicated-distill-turn design described here aims at the same underlying
tension with a much smaller delta.

## Design

### Two turn types in the agent loop

The work cycle stays as it is today: the agent receives the assembled
program-output context, runs tool-use turns until it produces a response with
no tool calls, and the cycle closes. Then, *before* starting the next work
cycle, the harness runs a **distill turn** — a separate inference, with the
same model, the same library, the same probe outputs, but a different system
prompt and a different user message.

The distill turn's framing:

- **System prompt.** "You are not solving the user's task right now. The
  previous work cycle's assistant messages and tool results are below for
  context. Your only job is to update the library so that what was learned
  this cycle persists across iterations. When you are done, respond with no
  tool calls."
- **User message.** Includes
  - the same probe-output context the work cycle saw, so the agent sees the
    current dashboard;
  - a transcript of the work cycle's assistant turns and tool results, so
    the agent can see what it just did;
  - guidance on what to look for (repeated reads, repeated bash commands,
    verbose outputs that should be compressed, dead programs to remove —
    much of the substance that today's `distill/general.sh` reinforcement
    inlines into the work cycle).

The distill turn uses the same tool surface (`read`, `bash`, `edit`, `write`)
and runs the same way as a work cycle from the harness's perspective —
multiple tool-use turns until the model emits a response with no tool calls.
The only differences are the system prompt, the user message, and that the
turn does not count toward the configured iteration budget (`-n`).

### Cadence

A distill turn runs between work cycles, not on every work cycle. Always-on
distillation would double the inference cost without much marginal benefit
for cycles where the agent did little or nothing novel. The harness decides
when to fire one based on a coarse trigger:

- **Default trigger.** A work cycle that consumed more than a configurable
  in-cycle token budget (a similar threshold to the one
  `distill/general.sh` already uses).
- **Forced trigger.** The wrap-up turn after `-n` is exhausted runs as a
  distill turn rather than appending the final-iteration reinforcement to
  the next work cycle.
- **Skip trigger.** A work cycle that did no tool calls (e.g., the agent
  idled because nothing changed) does not get a distill turn — nothing
  happened to distill.

A simple counter — "iterations since last distill turn" — can also force a
distill turn periodically if the in-cycle threshold has not been hit in a
while, to ensure long-running quiet cycles still get curated.

### What the distill turn does *not* do

- It does not change the work cycle's behavior. The work cycle still has
  access to the full tool set and can still write to the library mid-flow
  if the agent chooses to — this is not a moratorium on in-cycle library
  edits, just an explicit place to do them with full attention.
- It does not replace `distill/general.sh`. That reinforcement still fires
  inside the work cycle on the existing threshold to encourage yielding;
  the new distill turn is what happens *after* the yield. Over time, if the
  dedicated turn proves effective, the in-cycle reinforcement can be
  softened or removed.
- It does not introduce a separate process or sub-agent. Same harness,
  same model, same library, same provider connection. Just an additional
  inference with a different prompt.

### Interaction with idle backoff

A distill turn that produces no library mutations is informative: it means
the work cycle didn't accumulate anything worth capturing. The harness
should still count this as a substantive iteration for statistics purposes,
because an inference happened, but it should not fight idle backoff — the
backoff logic compares pre-selection program-output hashes, which a distill
turn does not change.

### Interaction with the trace

The distill turn is captured in the run trace the same way as any other
iteration, but tagged distinctly so operators reviewing
`.autoprobe-last-run/index.html` can see at a glance which turns were work
and which were distill. This also gives us a measurable signal — "what did
the distill turn change" — which is the natural success metric for this
design.

## Why this is the minimal viable intervention

The dedicated distill turn keeps every property of the current architecture:

- One process, one model, one library, one harness.
- The same agent that touched the environment writes the probes — no
  information loss across a sub-agent boundary.
- Mid-flight human steering via the TUI is preserved.
- Provider-agnostic — the design adds no provider-specific machinery.

It changes exactly one thing: it stops asking the agent to hold both goals
within a single turn. The two goals are alternated *between* turns instead
of held in tension *within* turns. Everything else follows from that.

## Open questions

- **How much context from the work cycle should the distill turn see?**
  Full transcript is most informative but expensive. A summary or a
  filtered view (e.g., only tool calls + brief excerpts) may be enough.
  Worth measuring empirically once the basic flow is in place.
- **Distill turn iteration budget.** The distill turn itself can run
  multiple tool-use turns (read a file, edit a probe, run it to verify,
  etc.). Some upper bound is needed to prevent runaway. A simple
  in-cycle token cap mirrors the work cycle's existing protections.
- **Does the in-cycle `[DISTILL]` reinforcement still earn its keep?**
  If the dedicated distill turn handles capture well, the in-cycle nudge
  may become unnecessary or even counterproductive. Plan to measure
  before/after and revisit.
- **Should the distill turn see the *next* work cycle's anticipated
  framing?** I.e., should it know what the user goal still is so it can
  prioritize what to capture? Probably yes — the goal probe is part of the
  program-output context the distill turn already sees, so this comes for
  free.
