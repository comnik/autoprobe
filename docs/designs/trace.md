# Run tracing

## Implementation status

Not yet implemented. This document describes the design before code lands.

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

`autoprobe trace` starts a local HTTP server bound to 127.0.0.1 on an
ephemeral port, prints the URL, and best-effort opens a browser tab.
With no argument it serves `.autoprobe-last-run/`; a path argument
overrides that default, which is what the operator uses to inspect
a moved-aside trace. The server hosts the embedded HTML/CSS/JS
viewer bundle alongside the trace files, sidestepping the `file://`
fetch restrictions that would otherwise prevent a static-only viewer
from loading iteration JSON. The server is read-only and exits when
the operator stops it.

### On-disk layout

```
.autoprobe-last-run/
  log.jsonl             # one JSON object per line; line 1 is the run header
  iter-00001.json
  iter-00002.json
  …
```

Zero-padded iteration numbers sort naturally both alphabetically and
by iteration count. The padding width is fixed at run start (default
5); runs that exceed it still produce valid filenames, they just sort
less neatly past the cliff.

Per-iteration files give us three properties at once: cheap
incremental writes (one tmp + rename for the iter file plus one
append to the log, both O(1) in run length), crash resilience (a
process killed mid-iteration loses at most one in-flight file),
and simple lazy loading in the viewer (fetch only what the
operator is currently looking at, not the entire run).

### Log

`log.jsonl` is an append-only index of the run, one JSON object per
line. The first line is a `run` header carrying the run-level
metadata; every subsequent line is an `iteration` summary written
after that iteration finishes. An append-only log is the right shape
for this data: we never need to mutate an earlier line, the writes
are O(1) regardless of run length, and an `O_APPEND` write of a
single ~200-byte line is atomic on POSIX so concurrent readers (the
viewer's server, `tail -f`) never see a partial record.

Shape — first line:

```json
{"kind": "run", "format_version": 1, "autoprobe_version": "0.5.3",
 "started_at": "2026-05-13T10:30:00Z", "probe_dir": ".autoprobe",
 "provider": "anthropic", "model": "claude-opus-4-7",
 "goal": "investigate slow startup", "context_budget_tokens": 131072}
```

Shape — iteration lines:

```json
{"kind": "iteration", "n": 1, "file": "iter-00001.json",
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

`iter-NNNNN.json` is one iteration's slice: the exact context the
harness sent to the provider, the response that came back, the tool
results the harness then synthesized, and the per-program data that
fed both. Shape:

```json
{
  "format_version": 1,
  "iteration": 42,
  "started_at": "2026-05-12T10:34:11Z",
  "completed_at": "2026-05-12T10:34:14Z",
  "idle_polls_before": 3,
  "idle_wait_ms": 7000,

  "context": {
    "messages": [
      {"role": "user", "content": [
        {"text": "[program=aaa-cornerstone exit=0]\n…"},
        {"text": "[program=net-listeners exit=0]\n…"},
        {"text": "[YOUR GOAL]\ninvestigate slow startup"}
      ]},
      {"role": "assistant", "content": [
        {"kind": "thinking", "text": "…", "signature": "…"},
        {"kind": "text", "text": "I'll check the lockfile…"},
        {"kind": "tool_call", "id": "toolu_1", "name": "read", "arguments": {…}}
      ]},
      {"role": "tool_result", "tool_call_id": "toolu_1", "tool_name": "read",
       "content": [{"text": "…"}], "is_error": false}
    ]
  },

  "response": {
    "model": "claude-opus-4-7",
    "stop_reason": "tool_use",
    "usage": {"input_tokens": 12345, "output_tokens": 678},
    "content": [
      {"kind": "text", "text": "Adding a probe for slow imports…"},
      {"kind": "tool_call", "id": "toolu_2", "name": "write", "arguments": {…}}
    ]
  },

  "tool_results": [
    {"tool_call_id": "toolu_2", "tool_name": "write",
     "content": "wrote programs/import-timing", "is_error": false}
  ],

  "programs": [
    {"name": "aaa-cornerstone", "exit": 0, "latency_ms": 23,
     "active": true, "included": true, "output": "…", "output_tokens": 87},
    {"name": "demoted-foo", "exit": 1, "latency_ms": 12,
     "active": false, "included": true, "exploration_phase": "nonzero",
     "output": "…", "output_tokens": 41}
  ],

  "budget": {
    "limit_tokens": 131072,
    "used_tokens": 105210,
    "overflowed": false,
    "revision_prompt_fired": false,
    "active_budget_tokens": 104857,
    "exploration_budget_tokens": 26214
  },

  "stats_snapshot": {
    "aaa-cornerstone": {…programStats…}
  }
}
```

Two choices worth calling out:

- **`programs[]` duplicates the leading user message** as structured
  data. The user message renders as opaque text; `programs[]` lets the
  viewer present a sortable table with exit codes, latencies,
  inclusion status, and per-program budget bookkeeping. The
  duplication is bounded by the configured budget and worth it.

- **`stats_snapshot` is taken post-iteration**, after this iteration's
  EWMA updates have folded in. It captures the statistics the *next*
  iteration's revision prompt would render. This makes a trace
  self-contained — replaying through the viewer never needs to reach
  into `.autoprobe/statistics/`, which the live agent may have since
  overwritten.

Signature fields on assistant content (`ThinkingSignature`,
`TextSignature`, `ToolCall.ThoughtSignature`) are preserved verbatim.
The viewer hides them by default but the bytes are there for future
debugging that needs to inspect provider-native continuity tokens.

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

Per-iteration files are written via tmp-write + rename so a process
killed mid-write never leaves a partially-written `iter-NNNNN.json`.
`log.jsonl` is opened once with `O_APPEND` and each iteration's line
is written with a single newline-terminated `write(2)`; the kernel
serializes appends so a concurrent reader (the viewer or `tail -f`)
never sees a partial line, and a process killed between writes
leaves the existing lines intact.

The write order per iteration is `iter-NNNNN.json` first, then the
matching `log.jsonl` line. That way the log never references an
iteration file that isn't on disk yet, which keeps the viewer's
load path simple: any line in the log can be followed without a
"file may not exist yet" race. The opposite order would also work
but would require the viewer to retry-or-skip when it sees a
freshly-logged iteration whose file is still being written.

The viewer also backfills any `iter-*.json` files it finds on disk
that aren't yet referenced from `log.jsonl`, so an interrupted run
loses at most its most recent iteration record but never an earlier
one.

Clearing `.autoprobe-last-run/` happens at the start of the next
run, not in a shutdown hook, so an aborted run's trace remains
inspectable up until the moment the operator launches the next one.
The clear-then-recreate is best-effort: if removal fails (e.g., a
file in the directory is held open by another process), the run
aborts before any new trace is written rather than mixing two runs'
records together.

### Viewer

The viewer is a single-page HTML/CSS/JS bundle embedded into the
binary alongside the existing assets. It's served by
`autoprobe trace <dir>` from a localhost HTTP server. No framework,
no build step — source files ship as embedded assets and the JS
fetches manifest / iteration files on demand.

Layout:

- **Left rail**: iteration list, one row per entry, showing iteration
  number, elapsed-since-start, duration, stop reason, and overflow /
  revision-fired badges. Clicking a row selects that iteration.
  Keyboard `↑/↓` and `j/k` step through the list.
- **Main pane**: a single scrollable view with the iteration's
  sections:
  - Header strip — iteration number, timestamps, duration, usage,
    stop reason, a budget bar.
  - Conversation — `context.messages` rendered as a chat-style
    sequence. User messages break out their program-output children
    into per-program cards (the leading `[program=…]` line becomes
    a card header). Assistant messages render thinking, text, and
    tool calls; tool calls link to their corresponding tool result.
  - Programs table — sortable by name / exit / latency / output
    size, with a row per program that ran and a collapsible region
    for the full output.
  - Stats — `stats_snapshot` as a sortable table.
- **Keyboard**: `←/→` step between iterations within the main pane;
  `g` opens a goto-N input; URL fragments (`#iter-42`) deep-link
  into a specific iteration.

The viewer is read-only. It never writes to the trace directory.

### Trace artifacts are sensitive

A trace contains the full conversation the model saw, including
anything the agent's programs surfaced from the host environment. The
plan does no redaction — that's by design, since the value of the
trace is exactly its faithfulness to what happened. Because tracing
is unconditional, `.autoprobe-last-run/` always exists after a run
and always contains this material; operators in environments where
program output may contain secrets must treat the directory with the
same care they'd treat the source environment, and projects should
add `.autoprobe-last-run/` to `.gitignore`. The `autoprobe init`
flow writes this entry into the probe directory's `.gitignore` (or
the repo's, if present) so the default state is safe.

## Implementation phases

Suggested attack order so a partial landing is still useful:

1. **File tracer + per-iteration JSON.** Clear and recreate
   `.autoprobe-last-run/` at the start of `autoprobe run`. Write the
   `run` header to `log.jsonl`, then for each iteration write
   `iter-NNNNN.json` and append an `iteration` line to the log. No
   viewer yet — JSONL is already inspectable with `jq` and `tail -f`
   and is enough to validate the capture is complete and the format
   round-trips every field that matters.
2. **`autoprobe trace` server + minimal viewer.** Static HTML that
   lists iterations and renders the conversation for one at a time.
   No programs table, no stats panel yet — just enough UI to step
   through a run.
3. **Programs table + stats panel.** Sort, filter, expand outputs.
4. **Polish.** Keyboard nav, deep-link URLs, collapsible thinking
   blocks, budget visualisation, interrupted-trace handling.

Phase 1 is the load-bearing one — once iteration files exist on disk,
the rest is presentation that can be evolved without touching the
agent. Phases 2–4 can land independently and out of order.

## Files touched by this design

- `main.go` — clear `.autoprobe-last-run/` in `cmdRun`; new `trace`
  subcommand.
- `agent.go` — wire a tracer through `NewAgent` and call it from
  `Step` after tool results are synthesized.
- `trace.go` (new) — file tracer, atomic per-iteration writes,
  append-only `log.jsonl`, JSON shapes for the run header and
  iteration records.
- `assets/viewer/` (new, embedded) — HTML/CSS/JS bundle and the
  `autoprobe trace` server.
- `docs/designs/trace.md` — this document.
