# Context budget and active program selection

## Problem

The library of installed programs grows monotonically. Programs are cheap to write,
cheap to keep around, and cheap to run — but every program that runs spends tokens in
the context window, which is neither cheap nor unbounded. Without a mechanism to keep
total program output below some ceiling, the harness eventually breaks against the
provider's context limit, and well before that it degrades quality by drowning useful
probes in noise.

This document describes how the harness manages a fixed context budget while still
keeping every installed program executing on every iteration, and how the agent itself
participates in deciding which programs' output gets into the context.

## Design

### Context limit

The harness maintains a configurable maximum context size for program outputs (default:
**128K tokens**). This budget covers the assembled program output portion of the context
only; the remaining room in the model's window is left for the current tool-calling
cycle (assistant turns and tool results). 128K assumes a 256K base window — the size
frontier models are still trained on — leaving roughly half the window for in-flight
tool use.

### Always run, conditionally include

Every installed program is executed on every iteration regardless of whether it is
considered "active." Running a program is essentially free (out-of-context compute),
and the harness needs each program's exit code and output in order to make inclusion
decisions and to update its statistics. The active/inactive distinction governs
**inclusion in the context**, not execution.

### Exit code contract

Probes MUST treat their exit code as a status channel, separate from stdout:

- **Exit 0** — the program ran successfully. Its stdout may be normal output, or
  empty if there is nothing to report this iteration. This is the common case.
- **Non-zero exit** — something is wrong that the agent should look at: a violated
  assumption, an environment change the program wasn't expecting, an error
  condition. Non-zero exits are not for "no data" or routine empty output;
  they are reserved for "the agent should pay attention to this now."

The harness relies on this convention to decide what's surfaced into the context
(see [Active set](#active-set-by-exclusion) and [Inclusion
algorithm](#inclusion-algorithm) below). A program that exits non-zero for
routine conditions will keep promoting itself into the exploration slot every
iteration, drowning out genuinely interesting signals from other demoted
programs; a program that swallows real errors with exit 0 will be selectable
out of the context exactly when the agent needs it most.

The cornerstone and the `write` reinforcement state this contract so that
newly-authored programs conform.

### Active set (by exclusion)

The set of programs whose output is preferentially included in the context is
defined by exclusion: programs are active **by default**, and the agent maintains
`.autoprobe/inactive` as the list of programs it has explicitly demoted.

A program is active iff:

- It is the **cornerstone program** (always active, even if listed in
  `.autoprobe/inactive`), or
- It is not listed in `.autoprobe/inactive`.

Non-zero exit codes do not change active-set membership. Instead, they bias
the 20% exploration slot's selection so that inactive programs with non-zero
exits this iteration are included ahead of random draws (see [Inclusion
algorithm](#inclusion-algorithm) below). That keeps the active set as a
deliberate, agent-controlled list, while still honouring the exit code
contract for alarms from demoted programs.

This means newly-installed programs are active on the very first iteration without
the agent having to remember to register them, which is the correct default: a
program the agent just wrote is almost certainly one the agent thought was worth
running. The `.autoprobe/inactive` file represents an explicit "I considered this
and chose to demote it" decision, not the absence of an "I considered this and
chose to keep it" decision.

The agent updates `.autoprobe/inactive` directly (it is just a file, edited via
the normal `write`/`edit` tools) when it revises which programs to demote. The
harness never modifies this file on its own. Entries for programs that no longer
exist in the library are harmlessly ignored.

### Inclusion algorithm

After all programs have run, the harness builds the context for this iteration:

1. **Sum the token count** of every program's output. Counting tokens accurately
   requires the provider's tokenizer; for the budget check, a rough estimate of
   `bytes / 4` is good enough.
2. **If the total fits within the limit**, include every program's output
   unconditionally. No selection, no exploration, no signal lost.
3. **Otherwise**, allocate the budget in two parts:
   - **80% to the active set**, packed in **pure lexicographic order by program
     name** — exit code does not reorder. The 80% slice is therefore byte-stable
     across iterations as long as the active programs' outputs don't change,
     which keeps the provider's prompt cache warm on the bulk of the context.
     The cornerstone naturally sorts first via the existing `aaa-` naming
     convention. Active programs that exit non-zero are already guaranteed
     visibility by being in the active set; promoting them ahead of other
     active programs would buy a duplicate guarantee at the cost of cache
     stability whenever alarm sets change.
   - **20% exploration budget**, filled in two phases:
     1. **Inactive programs that exited non-zero on this iteration**, in
        lexicographic order by program name. This is how the exit code contract
        reaches programs the agent had previously demoted — a previously-quiet
        probe that has just detected something gets promoted into the context
        without being subject to the random draw.
     2. **Random exploration** from the remaining (zero-exit) inactive programs,
        chosen uniformly, fills any budget left after the non-zero exits. This
        keeps low-scoring programs measurable so their scores stay fresh, and
        gives the agent a chance to rediscover programs whose usefulness has
        changed.

     The exploration slot is at the tail of the context, which lands in the
     high-attention region of the U-shaped attention curve — appropriate for
     both promoted alarms and exploration draws.

Scores are not used by the harness to order or select within the active set —
they are surfaced to the agent in the revision prompt and inform the agent's
decisions about what to add to `.autoprobe/inactive`. That keeps the harness's
behavior stable and the agent's involvement explicit.

#### Naming is load-bearing — but for attention, not survival

Lex order serves two distinct purposes that the agent needs to understand
separately:

- **Attention placement.** Models attend to tokens following a roughly
  U-shaped curve: tokens near the start and the end of the context get more
  attention than tokens in the middle ("lost in the middle"). Lex order places
  `aaa-`-prefixed programs at the high-attention head of the context and
  `zzz-`-prefixed programs at the high-attention tail; plain-named programs in
  the middle get less attention. So filename prefixes are how the agent
  positions a probe in attention space.
- **Drop order under pressure.** When the active set doesn't fit in 80% of the
  budget, later-sorting programs get dropped first. This is **not** the
  primary mechanism for ensuring important probes survive — `.autoprobe/inactive`
  is. The agent demotes less-valuable programs into `inactive` so that the
  important ones fit comfortably. Lex order is just the tiebreaker that picks
  *which* programs get dropped when the active set still overflows.

The two concerns line up neatly: `aaa-` programs sit at the high-attention
head and are last to be dropped, `zzz-` programs sit at the high-attention
tail and are first to be dropped, and the agent's main lever for "make sure
this fits" is `.autoprobe/inactive`, not filename gymnastics.

Active programs that exit non-zero stay in their lex position — they're
already guaranteed visibility by being in the active set, so they don't need
to be reordered. Inactive programs that exit non-zero are promoted into the
20% exploration slot, which sits at the tail of the context (the high-
attention end of the U-curve). Either way, the alarm reaches the agent at a
high-attention position without disturbing the byte-stable 80% prefix.

The cornerstone and the `write` reinforcement state this convention so newly-
authored programs are named with it in mind.

#### Alphabetic clustering

A burst of similarly-prefixed programs (e.g. several `analytics-*` probes
added at once) can starve later-sorting probes out of the budget without any
of the analytics programs being individually wasteful. This isn't a separate
mechanism to design around — it's the same overflow case the rest of the
design already handles: the dropped probes emit sentinel lines, the revision
prompt fires (edge-triggered on the first overflow, periodic during sustained
overflow), and the agent decides which programs to demote into
`.autoprobe/inactive` so the rest fit comfortably.

Programs whose individual output exceeds the remaining budget for their slot are
skipped, not truncated — a half-truncated probe is worse than an absent one because
the agent can't tell whether the suppressed bytes contained the signal. In place of
the dropped output, the harness emits a one-line sentinel like
`[program=foo dropped: output 187K exceeds remaining budget 40K]` so the agent
notices the omission and can rewrite the program to be more compact rather than
silently losing the signal forever.

### Interaction with idle backoff

The harness today decides whether to skip the model call and idle (with exponential
backoff capped at 30s) by byte-comparing the freshly-built conversation against the
last one it sent (`agent.go:108`, `conversationsEqual`). A random exploration slot
breaks this: even when nothing in the environment has changed, a different random
sample of inactive programs produces a different rendered conversation every
iteration, so the harness re-queries the model forever and idle backoff never
engages.

The fix is to **decouple the idle check from the selection policy**. Instead of
comparing the rendered conversation, the harness compares a hash of the
per-program outputs taken *before* selection — specifically, a hash of
`(name, exit_code, stdout)` triples for every program that ran this iteration,
sorted by name. Exit code is part of the hash because a program flipping from
exit 0 to exit 1 with byte-identical stdout is an environmental change (an
inactive program just promoted itself into the exploration slot, or an active
program just raised an alarm) and shouldn't be eaten by idle
backoff. If those hashes match the previous iteration's, the environment is
unchanged and the harness idles regardless of which random subset the selection
policy would have chosen this time.

This separation is also conceptually cleaner: "did the environment change" is a
property of the programs, not of the rendered context, and shouldn't be coupled to
budgeting or exploration decisions.

(An alternative considered was deterministically seeding the exploration RNG from
a hash of the inactive programs' outputs, so identical environments produce
identical selections. That works but entangles the selection policy with idle
detection — if we later change the policy to e.g. round-robin, the idle check has
to be revisited. Hashing the pre-selection outputs is the policy-independent
version.)

### Revision prompt

The harness surfaces a revision prompt as part of the context on two cadences,
both active simultaneously:

- **Edge-triggered.** Whenever step 1 transitions from "fit" to "overflow" — i.e.,
  the previous iteration fit within the budget and this one does not — the
  revision prompt is surfaced immediately. This catches the moment the agent's
  programs start costing more than they're worth, while the cause is still fresh.
- **Periodic during sustained overflow.** While iterations continue to overflow,
  the revision prompt is re-surfaced every N iterations (default: 10). This
  ensures the agent doesn't ignore the first nudge and forget; sustained overflow
  is a sustained problem and deserves a sustained signal.

When neither trigger fires (overflow is the same as last iteration and the
periodic counter has not elapsed), the prompt is omitted. The point is to be
noisy at transitions and patient in between, not to nag every iteration.

The prompt itself asks the agent to do two things:

1. **Improve information density** of the installed programs — rewrite verbose
   programs to emit more compressed output, merge redundant programs, or delete dead
   ones.
2. **Choose which programs to demote**, updating `.autoprobe/inactive`
   accordingly.

To support this decision, the harness includes the per-program statistics described
below.

## Per-program statistics

Statistics are persisted in `.autoprobe/statistics` (one record per program, format
TBD — likely line-delimited JSON keyed by program name). They are updated incrementally
as each iteration runs and exposed to the agent on demand (and unconditionally as part
of the revision prompt).

### Cheap always-on metrics

These are updated every iteration for every program and cost nothing beyond what the
harness already computes:

- **Average output tokens.** EWMA over iterations, with α ≈ 0.1. Tells the agent how
  much context each program is consuming.
- **Change frequency.** Fraction of recent iterations on which the program's output
  differed from its previous output. A clock that ticks every second has a change
  frequency near 1; a program that almost never changes has one near 0.
- **Information content of changes.** When the output does change, a measure of
  how much actually differs between the new output and the prior one.
  Distinguishes a program that re-emits its entire output with a single timestamp
  ticking (high change frequency, low information per change) from a program that
  rarely changes but emits genuinely new information when it does.

  The harness uses a **line-level sequence-matcher ratio**, matching Python's
  `difflib.SequenceMatcher.ratio()`:

  ```
  similarity = 2 * matched_lines / (len(prev_lines) + len(curr_lines))
  change     = 1 - similarity
  ```

  where `matched_lines` is the number of lines in the longest common subsequence
  between the prior and current outputs. The `2·M / (|A| + |B|)` denominator
  (sum-of-lengths, not max- or average-) makes the metric symmetric and bounded
  in `[0, 1]`: identical outputs score 0, fully disjoint outputs score 1, and
  the result doesn't depend on which output you pass first. Naming the formula
  explicitly avoids subtly different denominators producing subtly different
  statistics across implementations or refactors.

  It is O(n), cheap to compute, and natural for the line-oriented outputs most
  probes emit. Character-level edit distance (Levenshtein) is the textbook
  choice but is O(n·m) and works in the wrong unit (characters, not tokens) for
  this design. If the line-level ratio turns out to be too coarse in practice,
  token-level n-gram Jaccard is a natural next step — but start simple.
- **Latency.** EWMA of wall-time per execution. Programs that are slow contribute to
  the overall iteration cadence even when their output is cheap in tokens.
- **Staleness.** Iterations since the output last changed meaningfully. Distinguishes
  "quiet because nothing's happening" from "quiet because the program is stuck or
  dead."
- **Token overlap with assistant response.** N-gram overlap between this program's
  output and the next assistant turn. A cheap proxy for "did the model use this." Not
  load-bearing on its own, but useful as a free always-on signal.

### Causal influence (sampled)

The strongest signal of whether a program is pulling its weight is the
**counterfactual logprob shift**: how much less likely the agent's actual response
becomes when this program's output is removed from the context.

This is expensive — it requires re-scoring the response under an ablated context —
and it only works on providers that expose logprobs for given token sequences
(OpenAI, xAI, locally-hosted open-weight models). It is therefore measured **on a
sampled fraction of substantive iterations** (e.g., 1 in 50), and only when the
configured provider supports it. "Substantive" here means iterations on which the
harness actually queried the model — idle iterations, where program outputs were
unchanged and the harness backed off without inference (see [Interaction with
idle backoff](#interaction-with-idle-backoff) above), do not count toward the
sampling cadence. There is no model response to ablate against on those
iterations, and counting them would push the measurement frequency down
arbitrarily during quiet phases. On Anthropic, this metric is left unpopulated;
the cheap always-on metrics carry the load.

When measurable, the protocol is:

1. Take the assistant's response R and the full program-output context C used to
   produce it.
2. For each program p, compute `logP(R | C)` and `logP(R | C \ p)` by asking the
   provider to score R as a prefilled sequence.
3. The shift `Δ_p = logP(R | C) - logP(R | C \ p)`, divided by R's token count,
   is the per-token influence of program p on this turn.
4. Update p's EWMA influence score with the per-token shift.

Three things to be aware of with this metric:

- **Marginal value is contextual.** Ablating a single program measures its marginal
  contribution given everything else is present. Two programs providing redundant
  information will both score low individually because each compensates for the
  other's absence. Periodically ablating pairs (more expensive) catches this; without
  that, the agent should treat low-but-similar scores across related programs as a
  redundancy hint, not a "cut both" signal.
- **Length normalization matters.** Compare per-token shifts, not raw shifts —
  otherwise long responses mechanically dominate.
- **Cost normalization makes scores comparable.** A program that contributes Δ per
  response token but consumes 5K tokens of context is worse than one that contributes
  the same Δ for 200 tokens. The harness derives a **value-per-cost** ratio
  (influence per response token, divided by average program output tokens) and ranks
  programs on that.

### Aggregation and ranking

All time-varying metrics use an EWMA rather than a flat running mean — programs'
value drifts as the task changes phase, and a flat mean over the project's lifetime
will lag too far behind recent reality. The harness also retains the unweighted
sample count so the agent can tell whether a score is well-evidenced or based on a
handful of measurements.

For the revision prompt, programs are presented sorted by value-per-cost (when
influence data exists) or by a composite of change-information-content and
overlap-with-response (otherwise), with the bottom-k flagged as candidates for
deactivation. Ranks are more robust than absolute scores for cut/keep decisions,
since they are invariant to drift in the overall scale of measurements.

### Exploration is non-optional

The 20% exploration budget is not just nice-to-have. If the score itself drove which
programs run, low-scoring programs would never get fresh measurements and could never
recover from a temporary slump or a single bad streak. Running everything every
iteration (and randomly sampling the inactive set into the context) ensures every
program's statistics stay current and every program has a chance to demonstrate
renewed value.

## Files touched by this design

- `.autoprobe/inactive` — newline-delimited list of program names the agent has
  explicitly demoted. Edited by the agent. The file is allowed to be missing or
  empty; in that case, every program is active. The cornerstone is always active
  even if listed here. Entries naming programs that no longer exist are ignored.
- `.autoprobe/statistics` — per-program metrics, updated by the harness every
  iteration. Read-only from the agent's perspective; surfaced into the context on
  demand and as part of the revision prompt.

## Open questions

- **Pair-ablation budget.** Worth doing occasionally to detect redundant programs,
  but expensive. Likely a follow-up once the single-program flow is in place.
- **Statistics file format.** JSON-lines is easy to append and easy for programs to
  read, but a small SQLite database would make per-program updates atomic and queries
  cheap. Defer until the access patterns are clearer.
