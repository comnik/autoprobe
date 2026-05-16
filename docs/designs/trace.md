# Run tracing

## Implementation status

Phase 1 landed in v0.3.0: capture at the tail of `Step` in `agent.go`,
on-disk file `Tracer` in `trace.go`, `log.jsonl` index with `run` header
and `iteration` summary lines, clear-and-recreate of
`.autoprobe-last-run/` at the start of every `autoprobe run`.

Phases 2–4 then landed as **static HTML rendered at trace-write time**
rather than as a server + SPA. The original design's argument for a
server was that the viewer would need to `fetch()` JSON files, which
`file://` blocks — but that only applies if the JSON is the durable
artifact. With one self-contained HTML page per iteration we
sidestep both the fetch restriction and the operator-facing process
lifecycle, so `autoprobe trace` does not exist; operators view a trace
by opening `.autoprobe-last-run/index.html` directly.

Per-iteration `iter-NNNNN.json` files are no longer written; the
machine-readable index (`log.jsonl`) remains, and the durable per-
iteration artifact is the rendered HTML. The rendering helpers live in
`trace_render.go`; the embedded templates / CSS / JS bundle is under
`viewer/`.

The `autoprobe init` flow does not add `.autoprobe-last-run/` to a
project's `.gitignore` — operators in environments where program
output may contain secrets must add the entry themselves.

## Problem

Autoprobe runs are non-deterministic: each iteration depends on what the
agent's programs are emitting *right now* and on whatever the model
decides to do with that context. When something goes wrong — an
unexpected demotion, a stuck program, a budget overflow that wasn't
supposed to happen, a tool call the operator can't account for — there
is currently no way to inspect what the model actually saw on the
iteration in question. The TUI shows the conversation as it grows but
discards each iteration's user-message content when the next iteration's
program output replaces it, and the model's response only stays visible
until it scrolls past the viewport.

A reliable post-hoc inspection mode is needed so the operator can step
through a recorded run iteration by iteration, see the exact context
window the model received and the exact response it produced, and
correlate that with the program outputs, budget state, and statistics
of the moment. Because the operator never knows in advance which run
will need scrutiny, the capture must be unconditional: every run
records itself, automatically, with no flag to remember.

## Design

### CLI surface

Tracing is always on. Every `autoprobe run` writes its trace to a
fixed directory, `.autoprobe-last-run/`, relative to the operator's
current working directory. There is no flag to enable or disable it
and no choice of destination — one trace, one well-known place, no
configuration.

At the start of each run the harness clears `.autoprobe-last-run/`
and recreates it empty. Only the most recent run is retained;
operators who want to keep a particular trace move it aside (e.g.
`mv .autoprobe-last-run .autoprobe-trace-slow-startup`) before
starting the next run. Treating the directory as a single-slot
scratch space rather than an archive keeps the lifecycle trivial and
matches how `.autoprobe/` itself is used.

There is no `autoprobe trace` subcommand: the trace dir is
self-contained, so the operator just opens
`.autoprobe-last-run/index.html` in a browser. After `autoprobe run`
exits, the harness prints a one-line hint pointing at that path.

### On-disk layout

```
.autoprobe-last-run/
  log.jsonl             # one JSON object per line; line 1 is the run header
  style.css             # shared viewer stylesheet (verbatim copy of viewer/)
  viewer.js             # shared viewer behaviors (keyboard nav, sortable tables)
  index.html            # run overview + iteration table; re-rendered each step
  iter-00001.html
  iter-00002.html
  …
```

Zero-padded iteration numbers sort naturally both alphabetically and
by iteration count. The padding width is fixed at run start (default
5); runs that exceed it still produce valid filenames, they just sort
less neatly past the cliff.

The HTML files are the durable artifact. Rendering happens at trace-
write time (so the trace dir is always a complete, openable view
without needing the autoprobe binary), and each page links only to
relative paths within the directory — moving or copying the dir
preserves the entire viewer. `style.css` and `viewer.js` are written
once when the run starts; `index.html` plus the previous iteration's
HTML are re-rendered every step so navigation links and the
iteration table stay current.

### Log

`log.jsonl` is an append-only index of the run, one JSON object per
line. The first line is a `run` header carrying the run-level
metadata; every subsequent line is an `iteration` summary written
after that iteration finishes. The HTML viewer doesn't read it —
the rendered pages already embed everything they need — but it's the
machine-readable index for `jq`, `tail -f`, and any future post-
processing. An append-only log is the right shape for this: we never
need to mutate an earlier line, the writes are O(1) regardless of
run length, and an `O_APPEND` write of a single ~200-byte line is
atomic on POSIX so concurrent readers (`tail -f`) never see a
partial record.

Shape — first line:

```json
{"kind": "run", "format_version": 1, "autoprobe_version": "0.5.3",
 "started_at": "2026-05-13T10:30:00Z", "probe_dir": ".autoprobe",
 "provider": "anthropic", "model": "claude-opus-4-7",
 "goal": "investigate slow startup", "context_budget_tokens": 64000}
```

Shape — iteration lines:

```json
{"kind": "iteration", "n": 1, "file": "iter-00001.html",
 "started_at": "2026-05-13T10:30:00Z", "duration_ms": 2310,
 "stop_reason": "tool_use", "overflowed": false,
 "revision_prompt_fired": false, "idle_polls_before": 0,
 "input_tokens": 12345, "output_tokens": 678}
```

Autoprobe sends no system prompt and the tool list is fixed in code,
so neither needs to live in the trace. If either becomes dynamic
later — a configurable system prompt, or tools registered at runtime
— the run header is the natural place to record them.

Idle polls don't produce log entries — by definition they have no
new context and no model response to inspect. They're summarized via
`idle_polls_before` on the next substantive iteration so the
operator can still see "the harness waited X seconds before this
iteration" without each idle poll inflating the log.

There is no run-end marker. A run that terminated cleanly is one
where the last iteration's `stop_reason` is `end`; an interrupted
run is one where it isn't, or where the process is no longer alive
when the viewer inspects the directory. Adding an explicit
`{"kind": "end"}` line later is a one-line change if the distinction
turns out to need to be sharper.

### Per-iteration record

The in-memory record handed to the tracer (`IterationTrace` in
`trace.go`) is one iteration's slice: the exact context the harness
sent to the provider, the response that came back, the tool results
the harness then synthesized, and the per-program data that fed both.
The renderer in `trace_render.go` turns this into `iter-NNNNN.html`
directly — there is no on-disk JSON sibling. The conceptual shape:

- `iteration`, `started_at`, `completed_at`, `idle_polls_before`,
  `idle_wait_ms` — iteration metadata.
- `context.messages` — flat sequence of user / assistant /
  tool_result messages, in submission order, exactly as sent to the
  provider.
- `response` — the assistant message returned for this iteration:
  model id, stop reason, usage, content blocks (text / thinking /
  tool_call).
- `tool_results` — synthesized tool results, each linked back to its
  originating `tool_call` id.
- `programs[]` — per-program slice: name, exit, latency, active flag,
  inclusion flag, exploration phase tag, raw output, rendered token
  cost.
- `budget` — limit, pre-selection used tokens, overflow flag,
  revision-prompt-fired flag, active/exploration budget split.
- `stats_snapshot` — post-iteration `programStats` map, capturing
  the statistics the *next* iteration's revision prompt would render.

Two choices worth calling out:

- **`programs[]` duplicates the leading user message** as structured
  data. The user message renders as opaque text; `programs[]` lets the
  viewer present a sortable table with exit codes, latencies,
  inclusion status, and per-program budget bookkeeping. The
  duplication is bounded by the configured budget and worth it.

- **`stats_snapshot` is taken post-iteration**, after this iteration's
  EWMA updates have folded in. It captures the statistics the *next*
  iteration's revision prompt would render. This makes a trace
  self-contained — viewing through the rendered HTML never needs to
  reach into `.autoprobe/statistics/`, which the live agent may have
  since overwritten.

Signature fields on assistant content (`ThinkingSignature`,
`TextSignature`, `ToolCall.ThoughtSignature`) are preserved verbatim
in the in-memory record. The current viewer surfaces only a short
preview as a hover hint; the full bytes can be recovered by extending
the renderer if a future debugging need requires them.

### Capture point

The trace write lives at the tail of `Step` in `agent.go`, after tool
results are appended to the conversation. By then the harness has the
iteration data (`iterationData`), the conversation that was sent to
the provider, the assistant response, and the synthesized tool
results. None of those four exist together at any earlier point.

The write is synchronous — it takes microseconds compared to the
seconds a model round-trip takes, and writing async would require
either a queue with backpressure (more code) or a fire-and-forget that
drops records on shutdown (worse behavior than synchronous).

A failed trace write logs a warning and continues the run. Tracing is
diagnostic, not load-bearing.

### Crash safety

Every HTML write — `iter-NNNNN.html`, the re-rendered prior
iteration, and `index.html` — goes through tmp-write + rename so a
process killed mid-write never leaves a partially-written page; the
prior page on disk remains intact and openable. `log.jsonl` is
opened once with `O_APPEND` and each iteration's line is written
with a single newline-terminated `write(2)`; the kernel serializes
appends so a `tail -f` reader never sees a partial line, and a
process killed between writes leaves the existing lines intact.

Per-iteration write order: (1) re-render the prior iteration's HTML
so its "next" link points at the new file, (2) render the current
iteration's HTML, (3) re-render `index.html` with the updated list,
(4) append the `iteration` line to `log.jsonl`. The log line is
last so it never references an iteration file that isn't on disk
yet, and the prior-iter re-render is first so the moment the new
HTML appears the navigation links pointing into it already exist.

An interrupted run leaves an `index.html` that's one iteration
behind the latest fully-rendered iter file (the next index re-render
hadn't happened yet), and the latest iter's "next" link is the
disabled placeholder. The viewer is therefore degradation-tolerant
without any backfill logic — what's on disk is always a coherent
view, just possibly stale by one iteration.

Clearing `.autoprobe-last-run/` happens at the start of the next
run, not in a shutdown hook, so an aborted run's trace remains
inspectable up until the moment the operator launches the next one.
The clear-then-recreate is best-effort: if removal fails (e.g., a
file in the directory is held open by another process), the run
aborts before any new trace is written rather than mixing two runs'
records together.

### Viewer

The viewer is a set of plain HTML pages with a shared `style.css`
and a small `viewer.js` for client-side conveniences. There is no
SPA, no server, no build step — the renderer in `trace_render.go`
fills `html/template` templates from the embedded `viewer/` bundle
at trace-write time. Opening `index.html` works under `file://`
because every page is self-contained: nothing is fetched at view
time.

Two page types:

- **`index.html`** — run metadata strip (started_at, provider/model,
  budget, autoprobe version, goal) plus a table of iterations. Each
  row shows iteration number, elapsed-since-start, duration, token
  usage, stop reason, and overflow / revision-fired badges. The
  iteration number links to its `iter-NNNNN.html`.
- **`iter-NNNNN.html`** — one full iteration:
  - Navigation bar — prev / index / next links plus a keyboard hint
    line. The prev/next slot is a disabled placeholder when no such
    iteration exists, which keeps a freshly-written latest iter
    coherent until the next iteration backfills the link.
  - Summary grid — timestamps, duration, stop reason, model, token
    usage, idle-poll count, and a budget bar (red when overflowed).
  - Conversation — context messages plus this iteration's assistant
    response and synthesized tool results rendered as a chat-style
    sequence. User messages break out their program-output children
    into per-program cards (the leading `[program=…]` line becomes
    a card header); `[YOUR GOAL]` and other bracketed annotations
    render as a "note" block. Assistant messages render thinking
    (collapsed by default), text, and tool calls; tool calls anchor-
    link to their corresponding tool result.
  - Programs table — sortable by name / exit / latency / inclusion
    flag / tokens. Each row has a collapsible details block for
    the full program output.
  - Stats — `stats_snapshot` as a sortable table.

`viewer.js` adds three client-side conveniences:

- **Keyboard navigation**: `←` / `k` / `h` → previous iteration,
  `→` / `j` / `l` → next iteration, `Esc` / `u` → index, `g` →
  prompt for an iteration number to jump to. Targets are read from
  `data-prev-href` / `data-next-href` / `data-index-href` on
  `<body>`, which the templates emit only when the corresponding
  page exists, so the bindings never produce dead-link navigations.
- **Sortable tables**: any `<table class="sortable">` with `<th
  data-sort-key data-sort-type>` headers becomes click-to-sort,
  ascending then descending. Numeric and boolean sort types use
  `data-sort-value` on cells so the sort key is independent of the
  rendered label (e.g. badges).
- **Anchor scrolling**: tool-call links use plain `#tool-<id>`
  fragments to scroll the corresponding tool result into view, with
  a CSS `:target` outline to mark it.

The rendered pages are read-only. Re-opening an old trace is purely
a function of what's on disk — the autoprobe binary need not be
installed on the inspecting machine, since no rendering happens at
view time.

### Trace artifacts are sensitive

A trace contains the full conversation the model saw, including
anything the agent's programs surfaced from the host environment.
The plan does no redaction — that's by design, since the value of
the trace is exactly its faithfulness to what happened. Because
tracing is unconditional, `.autoprobe-last-run/` always exists
after a run and always contains this material; operators in
environments where program output may contain secrets must treat
the directory with the same care they'd treat the source
environment, and projects should add `.autoprobe-last-run/` to
`.gitignore`.

`autoprobe init` deliberately does *not* touch the project's
`.gitignore`. Modifying repo-level files behind the operator's back
is a poor default — there's no clean way to know whether the
project's ignore policy is hand-curated or generated, and a stray
edit is hard to spot in review. The README and this design note the
recommendation; operators add the entry explicitly.

## Implementation phases (historical)

This list is preserved as it stood when the design landed.

1. **File tracer + per-iteration JSON.** *(Landed in v0.3.0.)*
2. **Minimal viewer.** *(Landed.)* Originally specified as
   `autoprobe trace` server + SPA; reshaped during implementation
   into static HTML rendered at trace-write time, which removes
   the need for both a subcommand and `fetch()`-based loading.
3. **Programs table + stats panel.** *(Landed alongside phase 2.)*
   Sortable client-side via `viewer.js`.
4. **Polish.** *(Landed alongside phase 2.)* Keyboard navigation,
   collapsible thinking blocks, budget visualisation. The
   `autoprobe init` `.gitignore` write was dropped — see "Trace
   artifacts are sensitive".

## Files touched by this design

- `main.go` — clear `.autoprobe-last-run/` in `cmdRun`; print the
  view-trace hint after the run.
- `agent.go` — wire a tracer through `NewAgent` and call it from
  `Step` after tool results are synthesized.
- `trace.go` — file tracer state, atomic per-iteration HTML writes,
  append-only `log.jsonl`, in-memory types for the run header and
  iteration record.
- `trace_render.go` — `html/template` rendering, view types, the
  user-message block parser, sortable-table helpers.
- `viewer/` (embedded) — `style.css`, `viewer.js`,
  `index.html.tmpl`, `iter.html.tmpl`.
- `docs/designs/trace.md` — this document.
