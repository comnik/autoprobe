# Probe-debt meta-probe

## Implementation status

**Proposed, deferred.** Not yet implemented. To be revisited after
[dedicated modeling turn](dedicated-modeling-turn.md) lands and after
[signal-driven reinforcement](signal-driven-reinforcement.md) is in place
(this design reuses the same detectors).

## Problem

The current modeling reinforcement tells the agent "you should update
the model" in the abstract. Even the targeted variant from
[signal-driven reinforcement](signal-driven-reinforcement.md) only fires
when the agent crosses a threshold during the current cycle — at which
point the agent is already mid-flow and has to context-switch.

A complementary approach: make probe-debt *visible in the dashboard* the
agent sees at the start of every cycle, as a probe like any other. The
agent doesn't get told "you should update the model." It sees, in its
own context, a clear-eyed accounting of where the previous cycle wasted
tokens that a probe could have absorbed. The information is just
*there*, alongside everything else, with no nag attached.

This leans into autoprobe's central thesis: knowledge belongs in
executable programs whose output flows into the context, not in
out-of-band reinforcement messages. Probe-debt is itself probe-shaped.

## Design

### A meta-probe that consumes the prior cycle's trace

A new program (likely `aaa-probe-debt` or similar, named to sit at the
high-attention head of the context) reads the prior work cycle's
tool-call log from the trace and emits a compact report like:

```
PROBE_DEBT (previous cycle):
  - read tests/conftest.py 3 times — no probe summarizes fixtures
  - bash `pytest tests/auth/ -x` 4 times — no probe checks auth tests
  - bash `grep -n TODO src/` 2 times — no probe surfaces open TODOs
  - read foo/bar.go 5 times — no probe summarizes this file
Total: ~12K tokens spent on repeated work that probes could absorb.
```

Each line names a concrete repetition the agent did *last cycle*. The
"no probe summarizes/checks/surfaces…" framing makes the gap visible
without prescribing the fix.

The token estimate at the bottom is the cost of *not* having those
probes — derived from the size of the responses to those repeated tool
calls. Concretizing the cost in tokens is what makes this different from
generic reinforcement: the agent is making a measurable tradeoff, not
responding to a vague guilt-trip.

### Reuses the signal-driven detectors

The repetition detection here is exactly the same as in
[signal-driven reinforcement](signal-driven-reinforcement.md) — same
"same `read`," same "same `bash`," same cooldown logic. The difference is
*where the output lands*:

- Signal-driven reinforcement: appended to a tool result mid-cycle,
  interrupts flow.
- Probe-debt meta-probe: appears in the assembled context at the start
  of the next cycle, alongside every other probe output.

Both have a place. The in-cycle signal catches the agent while it can
still act on the current cycle's accumulating debt. The meta-probe is
the persistent dashboard view — even if the agent ignored the in-cycle
nudge, the debt shows up again next cycle, in the dashboard, with the
total still climbing.

### Self-clearing when probes are written

When the agent writes a probe that captures one of the repetitions, the
meta-probe should detect this and stop reporting that line. Two ways to
do this:

- **Implicit.** The next cycle doesn't repeat the work (because the new
  probe's output is already in context), so the corresponding line
  drops out of the meta-probe's report naturally.
- **Explicit.** The agent annotates a probe with a comment naming what
  repetition it absorbs, and the meta-probe filters its report against
  those annotations.

The implicit version is preferable — it's self-validating. If the agent
*thought* it captured the repetition but the work cycle still re-reads
the file, the line stays in the report and the gap is visible.

### Interaction with the dedicated modeling turn

The probe-debt meta-probe is one of the inputs the dedicated modeling
turn sees. The modeling turn's framing changes meaningfully when this
probe is in the dashboard: "here are the gaps the agent itself can see;
your job is to close some of them." The modeling turn becomes
goal-directed in a way that's hard to achieve when the goal is "review
the cycle and decide what's worth capturing" in the abstract.

### Cost

The meta-probe runs every iteration like any other program. It only
needs to read the prior cycle's trace (already on disk), aggregate
repetition counts, and emit a small report. It is `O(tool calls in
prior cycle)` in compute, and the output is a handful of lines — well
within budget even under the [context budget](context-budget.md) rules.

## Open questions

- **Where does the meta-probe live in the lex order?** Probably `aaa-`
  prefix to put it at the high-attention head, since the agent acting
  on probe debt is more valuable than most ambient probes. Worth
  testing whether the tail (`zzz-`) attention slot works as well.
- **Should the meta-probe persist debt across cycles?** A repetition
  that the agent did last cycle but not this cycle: still a gap, or
  resolved? Probably resolved — the cheap heuristic is "report debt
  from the most recent cycle only," and let the agent decide if it
  wants to write a probe for something that has not recurred recently.
- **What if the prior cycle was a [modeling
  turn](dedicated-modeling-turn.md)?** The meta-probe should look back
  past modeling turns to the most recent work cycle — modeling turns
  don't accumulate user-task debt, so reporting on them is
  meaningless. This is one of the reasons modeling turns should be
  tagged distinctly in the trace.
- **False positives from intentional re-reads.** Sometimes re-reading
  the same file *is* the right move (e.g., the file changed between
  reads). The meta-probe could check timestamps or content hashes to
  distinguish "re-read the same content" from "re-read because content
  changed" — start without this filter and add it if false positives
  become noisy.
