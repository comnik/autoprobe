# Signal-driven reinforcement

## Implementation status

**Proposed, deferred.** Not yet implemented. To be revisited after
[dedicated distill turn](dedicated-distill-turn.md) lands and we have
measured its effect on library quality.

## Problem

Today's distillation reinforcement (`assets/reinforcement/distill/general.sh`)
fires on a single trigger: cumulative in-cycle input tokens crossing a
configured threshold. That is a coarse proxy for "you might have learned
something worth capturing." It catches the case where the agent is grinding
on a task and accumulating context, but it does not tell the agent *what*
specifically would be worth distilling. The reinforcement reads as a general
nag — "compress and yield" — and the agent has to do its own diagnosis of
what to compress.

A more useful reinforcement would be specific and grounded: "you just
re-read `foo/bar.go` for the third time this cycle — write a probe that
emits the salient summary." Concrete, actionable nudges are easier for the
agent to comply with than ambient guilt-trip prompts, and they target
behaviours the harness can actually detect.

## Design

### Detectable repetition signals

The harness already sees every tool call the agent makes during a cycle.
That history is enough to compute a handful of cheap repetition signals:

- **Repeated `read` of the same file.** Same path, same range (or
  overlapping ranges) read ≥2 times within a cycle. Strong signal that the
  file's salient summary belongs in a probe.
- **Repeated `bash` of the same command.** Same argv (modulo whitespace,
  modulo trivially-varying args like timestamps), invoked ≥2 times.
  Candidate for a probe that runs the command and emits the compact
  result.
- **Repeated substring extraction.** Same regex pulled from the same
  output, or the same JSON path queried from the same payload, ≥2 times.
  Candidate for a probe that does the extraction once.
- **Repeated probe re-run.** The agent manually re-running an existing
  probe via `bash` rather than waiting for the next iteration to refresh
  it. Hint that either the iteration cadence is too slow for this probe
  or the probe's output isn't trusted.

Each signal is cheap to compute from the tool-call log already captured
for tracing.

### Targeted reinforcement messages

When a signal triggers, the harness fires a reinforcement that *names the
specific repetition*. Examples:

```
[DISTILL: repeated read]
You read foo/bar.go three times this cycle. The salient information you
extracted each time was approximately:
  - line 42: the SessionContext struct definition
  - line 87: the AuthHandler interface
Consider writing a probe that emits this summary so the next cycle starts
with it in context.
```

```
[DISTILL: repeated bash]
You ran `pytest tests/auth/ -x` four times this cycle. This is a candidate
for a probe — running it from a probe means the next cycle starts knowing
whether auth tests pass, without you having to invoke pytest yourself.
```

The reinforcement is appended to the most recent tool result the same way
today's `distill/general.sh` is. The key difference is that the message
references concrete state from the agent's own history, not a generic
prompt about distillation.

### Interaction with the dedicated distill turn

These signals are also valuable inputs to the [dedicated distill
turn](dedicated-distill-turn.md). When that design lands, the distill
turn's user message can include a "things you repeated" section derived
from the same detectors — turning the distill turn from a generic
"review what happened" prompt into a checklist of concrete capture
opportunities.

In other words: the same signals serve two purposes — gentle in-cycle
nudges (this design) and structured input to the dedicated distill turn
(the other design). Implementing the detection once supports both.

### Suppression and cooldown

Targeted nudges that fire too often are still nags. Each signal type
should have its own cooldown — once the harness has fired a "repeated
read of foo/bar.go" reinforcement, it should not refire on the same file
within the same cycle. The signal has been delivered; either the agent
acted on it or chose not to, and re-firing adds noise.

Across cycles, signals reset — a file re-read in two consecutive cycles
is informative regardless of whether the agent saw a nudge about it
last cycle.

## Open questions

- **What counts as "the same" command or read?** Exact-match argv is
  conservative but misses `pytest tests/auth/ -x` vs.
  `pytest tests/auth/ -xvs`. A normalization step (strip flags, sort
  positional args) would catch more cases at the cost of occasional
  false positives.
- **Where does the harness compute the salient-line summary in the
  repeated-read example?** The cheap version: just count occurrences
  and name the file. The richer version: diff the read ranges, surface
  what was actually returned each time. Start cheap; richer is a follow-up.
- **Should low-confidence signals fire as advisory hints rather than
  full reinforcements?** E.g., a single repeated read (only twice) is a
  weaker signal than five repetitions; surfacing it differently may
  reduce noise.
