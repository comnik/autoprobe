# TUI redesign — dashboard mode

## Implementation status

Implemented. `tui.go` is now the dashboard described below; the
viewport-based per-block model has been retired and step-through
(`--debug`) has been removed.

## Problem

The TUI has been doing two jobs that don't share a UX:

1. **Live operator dashboard** — at-a-glance answer to "is this run
   doing anything, is it healthy, is it expensive, and what is the
   model currently thinking." This wants constant screen real estate,
   no scrollback, and information density similar to `htop` or
   `iftop`.
2. **Inspection surface** — scroll through the conversation, see every
   tool call, every program-output user message, every thinking
   block. The current TUI tried to be this but is structurally
   ill-suited: every iteration replaces the leading user message, the
   outer viewport grows without bound, and what the model actually saw
   on a given iteration is hard to reconstruct after the fact.

Job 2 is now [served by the HTML trace viewer](./trace.md) — every run
writes `.autoprobe-last-run/` and the operator opens `index.html` to
inspect any iteration in detail. That removes the constraint that was
warping the TUI: it no longer needs to retain the conversation on
screen because nothing was being retained *only* by virtue of being on
screen. The trace is the durable inspection artifact; the TUI is the
live one.

With inspection out of scope, the TUI should be reshaped around the
operator question it is actually good at answering: "what is the agent
doing right now, and what does its working state look like." This
document describes that redesign.

## Design

### Constant real estate, no scrollback

The redesigned TUI occupies a fixed region of the terminal. Nothing
grows; nothing scrolls. Each refresh re-renders the same panels in
place from current agent state. There are no per-block viewports, no
tab navigation, no follow-vs-frozen distinction. The conversation is
not on screen — the HTML trace owns that view.

This is the `htop` shape: a dense status display that any single
glance answers "is the run alive, is it making progress, is it
expensive." Operators who need more open the trace.

### Layout (illustrative)

```
┌──────────────────────────────────────────────────────────────────────┐
│ autoprobe v0.5.3         model: claude-opus-4-7      cycles: 7      │
│ ● running programs ○ inference ○ tools                idle: —       │
├──────────────────────────────────────────────────────────────────────┤
│ tokens   142,310 in  /  4,820 out             est. cost   $0.6142   │
│ budget   ███████████░░░░░░░░░░░░░░  41%   53,891 / 131,072 tok      │
│ drag     ████████████████░░░░░░░░  68%   22,310 / 32,768 tok        │
│ library  ▓▓▒▓▓▒▒░▓▓░▓▒▓ (13 programs, 4 changed this iter)          │
├──────────────────────────────────────────────────────────────────────┤
│ ASSISTANT                                                            │
│   Looking at the failing tests, the common factor is the new        │
│   serializer path — every failure comes from a struct with an       │
│   embedded pointer. Going to instrument the marshaller.             │
├──────────────────────────────────────────────────────────────────────┤
│ q quit                                                               │
└──────────────────────────────────────────────────────────────────────┘
```

The terminal frame and the bordered panels are notional — actual
rendering uses `lipgloss` plain styles, not box-drawing chrome, to
keep the output legible on narrow terminals. The four conceptual
regions are:

1. **Header line** — version, model, tool-call cycles so far.
2. **Phase indicator strip** — the three-state visual showing which
   stage of the iteration loop is currently active.
3. **Vitals block** — cumulative tokens, cost estimate, the
   program-output budget bar, and the library bar.
4. **Last-message panel** — the most recent assistant text block,
   wrapped to width and capped at a fixed number of lines.
5. **Key hints** — single line of footer.

The whole thing fits comfortably in ~12 rows. On a terminal taller
than that, the empty rows below the footer are left blank — there is
no scrolling region to expand into. On a terminal narrower than ~80
columns, the bars compress (fewer cells) but the layout does not
reflow into multiple lines.

### Phase indicator

The agent's step loop walks three observable phases in sequence:

- **running programs** — `runIteration` is executing the library.
- **inference** — `provider.Generate` is in flight.
- **tools** — `executeTool` is running for one or more tool calls
  emitted by the assistant.

A fourth state — **idle** — applies when the harness is sitting in
its hash-match backoff (`IdleStatus` already returns this).

The indicator is a small row of three pips (or four including idle):
the currently active one is filled and colored; the others are
hollow. A subtle pulse animation on the active pip distinguishes "we
genuinely are in this phase" from a stuck/hung process.

Implementation: `Agent` gains a single atomic `phase` field that
`Step` updates at each transition (`phaseRunPrograms`,
`phaseInference`, `phaseTools`, `phaseIdle`). The TUI reads it on
every tick. This is a small, well-scoped addition — it doesn't change
behaviour, just exposes what `Step` is already doing.

### Tokens and cost

Cumulative `InputTokens` and `OutputTokens` are summed across every
`Step` from `provider.AssistantMessage.Usage`. The agent doesn't
currently aggregate these; the redesign adds two counters
(`totalInputTokens`, `totalOutputTokens`) on `Agent`, updated after
each `Generate` returns.

Cost is the sum `inputTokens · inPrice + outputTokens · outPrice`,
where the per-million-token prices come from a small table keyed on
provider name and model id. The table lives in `tui.go` (or a sibling
`pricing.go` if it grows) and is best-effort — an unknown model falls
back to displaying "—" for cost rather than a misleading number. The
table is hand-maintained; we don't fetch live prices.

Prompt-caching discounts are not modelled in the first cut. The
displayed cost is therefore an upper bound for providers that bill
cache hits at a discount (Anthropic). A future iteration can extend
`provider.Usage` with cache-hit/miss token counts and refine the
estimate; until then, "est. cost" is labelled exactly that.

### Program-output budget bar

A horizontal bar showing the fraction of the configured
`defaultContextBudgetTokens` consumed by this iteration's assembled
program output. Source data: `iterationData.totalTokens` (already
computed by `runIteration`) divided by `agent.ContextBudget()`.

The bar fills proportionally and turns red when the fraction exceeds
100% (overflow — the harness has dropped programs into sentinels). A
shoulder tick at the 80% mark visualises the active/exploration split
described in [context-budget.md](./context-budget.md), so the
operator can see at a glance whether overflow is eating the
exploration slot, the active set, or both.

Surfacing this requires `Agent` to expose the most recent
`iterationData.totalTokens` (currently it's local to `Step`). A small
accessor — `LastProgramTokens()` — is sufficient.

### In-cycle drag bar

A second horizontal bar shows the current tool-use cycle's working-
set drag against `distillThresholdTokens` (32K). The drag value is
`provider.AssistantMessage.Usage.InputTokens − iterationData.totalTokens`
— the same quantity `Step` computes to decide whether to fire the
DISTILL prompt — and reflects how much prior assistant/tool history
the current cycle is dragging forward.

The bar fills proportionally to `drag / distillThresholdTokens`. When
drag exceeds the threshold the bar turns red and stays red; this is
the same condition that triggers the distill firing (modulo
cooldown), so the visual matches the harness's actual decision.

When a DISTILL prompt has been attached to the most recent step —
either the periodic in-cycle firing or the forced wrap-up firing on
`finalPhase` — the bar flashes a "DISTILL" badge alongside the
percentage for a small number of refresh ticks. The flash is a
notice, not a sustained state: the operator should see *that* it
fired without the dashboard looking permanently alarmed afterwards.
The cooldown counter (`distillCooldown`) is not surfaced — it's
internal bookkeeping and would clutter the bar without changing
anything the operator can act on.

Outside a tool-use cycle the drag value is undefined (the next Step
will discard prior assistant/tool history), so the bar is rendered
dimmed at 0%. This makes the cycle boundary visible: drag climbs as
a cycle progresses, the bar goes empty when the cycle ends, then
climbs again on the next one.

Surfacing this needs `Agent` to expose the most recent step's drag
value and a flag indicating whether the most recent step attached a
DISTILL prompt. Two small accessors (`LastDrag()` and
`LastDistillFired()`), or a single bundled "last step vitals" struct
if more values accumulate, are enough.

### Library bar

A horizontal bar segmented by program, drawn in the same lex order
the active set is packed in. Each segment's **width** is proportional
to that program's `renderedTokens()` for the most recent iteration,
so the bar simultaneously shows "what's in the library" and "where
the context budget is actually going."

Each segment has a **state** rendered as its fill character / color:

- **active, unchanged** — solid mid-tone.
- **active, changed this iteration** — bright fill, pulsing for one
  refresh tick after the change is observed.
- **inactive, included via exploration** — striped / dim fill.
- **inactive, not included** — outline only.
- **dropped (sentinel)** — red outline.

"Changed this iteration" is read from the per-program `prevOutputs`
diff that the stats updater already computes — specifically, a
non-zero line-level change ratio. The pulse is a single-tick visual:
the segment goes bright, then settles back to "active, unchanged" on
the next refresh. That keeps the bar quiet during stable phases and
visibly active when the environment is moving.

A small annotation to the right of the bar shows `N programs, K
changed this iter` so the operator gets a numeric backstop for what
their eyes are reading off the colors.

Naming the segments inline is hard on a narrow bar — segment width
shrinks to a cell or two for most libraries. Instead, a tooltip-like
behaviour is deferred (TUIs don't have hover); operators who need to
identify a specific segment open the trace, where the programs table
is sortable. The bar is a vitals indicator, not a directory.

Sourcing this needs `Agent` to expose per-program output state for
the most recent iteration: name, rendered tokens, active/inactive,
included-in-context, changed-this-iter. A read-only snapshot method
(`LastProgramSnapshot()` returning a small slice of structs) keeps
the TUI from poking at internal `programResult` slices.

### Tool-calling cycles

A tool-calling cycle is one continuous run of `StopReason ==
StopToolUse` steps that ends when a step returns any non-tool stop
reason. The counter increments on the transition out of `StopToolUse`
(i.e., the step that ends a cycle). `Agent` adds a `toolCycles int`
field updated at the end of each `Step`; the TUI reads it for the
header.

This is meaningfully different from the raw iteration count: a single
"agent did something interesting" episode often spans a handful of
tool-use steps, so cycles correlate with operator-visible progress
better than iterations do.

### Most recent agent message

The bottom panel shows the most recent assistant `TextContent` from
the conversation — no tool calls, no thinking blocks, no tool
results. Found by walking `Conversation()` backwards until the first
`TextContent` on an `AssistantMessage`.

Word-wrapped to the panel width and capped at a configurable line
count (default 8). If the message is taller than the cap, the cap
holds and the trailing lines are truncated with an ellipsis — the
trace is the place to read the full text. The cap is a property of
the panel, not the message, which keeps the overall TUI height
constant regardless of how chatty the model gets.

When no assistant text exists yet (priming, first inference still in
flight), the panel shows a dimmed placeholder ("(waiting for first
response)") at the same height the eventual text would occupy, so the
layout doesn't shift when the first message lands.

### What goes away

- **Per-block viewports.** No more `m.msgViewports[]`, no
  `activeIdx`, no tab/shift-tab focus rotation.
- **Outer scrollable viewport.** No more `outerVp`.
- **Pinned-latest-assistant block.** Subsumed by the last-message
  panel, which is the only assistant text the TUI shows.
- **Step-through mode.** The current `s` toggle and the `stateReady`
  "press enter to step" flow go away entirely. The `-n` argument
  covers the bounded-run use cases that step-through was useful for
  (evaluation, debugging a specific number of iterations), and the
  HTML trace covers the inspect-what-happened use case. Removing the
  manual/auto state machine also collapses `tuiState` to "running"
  vs "done/error", which simplifies the rewrite.

### Refresh cadence

The existing `tickMsg` heartbeat (1s) is retained; on each tick the
TUI re-renders from agent state. Phase transitions trigger an
immediate refresh on top of the heartbeat by publishing a
`phaseMsg{}` from `Step` (a non-blocking send on a buffered channel
the TUI drains via a `tea.Cmd`), so the indicator updates the moment
a phase changes rather than waiting up to a second for the next
tick. If the channel is full (the TUI is slow to drain) the send is
dropped — phase state is read each tick anyway, so a dropped
notification at most delays the visual by one tick.

Anything driven purely by `Step` completing (cycles counter, tokens,
program snapshot) updates via the existing `stepMsg` handler.

## Files touched by this design

- `agent.go` — add `phase` atomic, `totalInputTokens` /
  `totalOutputTokens` counters, `toolCycles` counter, last-step drag
  + distill-fired flag, and accessor methods. Wire phase transitions
  into `Step`. Remove the `debug` / step-through plumbing
  (`StepThrough`, the `s`-key wiring path) since it no longer has a
  consumer.
- `tui.go` — rewrite. Replace the viewport-based model with a
  dashboard `View()` that composes the header, phase strip, vitals
  block, last-message panel, and footer from agent state. Drop
  `blockEntry`, `collectBlocks`, the viewport plumbing,
  `stateReady`, the `s` toggle, and most of the existing styles.
- `pricing.go` (new, optional) — hand-maintained per-model price
  table used by the cost estimator. If the table stays small (a
  handful of entries) it can live in `tui.go` instead.
- `docs/designs/tui.md` — this document.

## Deferred to a later phase

- **Cache-hit pricing.** Anthropic and (soon) other providers charge
  less for cache-hit input tokens. Until `provider.Usage` carries
  cache-hit/miss breakdown, the cost number is an upper bound and
  labelled "est. cost". Refining it is a follow-up once at least one
  provider's `Usage` surfaces the breakdown.

## Open questions

- **Pulse duration.** A single-tick pulse may be too brief to notice
  on a 1s heartbeat. Holding the bright fill for two or three ticks
  is an easy follow-up if the single-tick version reads as a flicker
  rather than a signal in practice. The same question applies to the
  DISTILL-fired flash on the drag bar; both should probably use the
  same duration so the dashboard's "something just happened" signals
  read consistently.
