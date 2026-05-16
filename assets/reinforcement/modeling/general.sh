#!/usr/bin/env bash
# Reinforcement that nudges the agent to update its executable model of the
# environment — the programs in $PROGRAMS_DIR — with what it has learned
# during the current tool-use cycle. Fires periodically once cumulative
# in-cycle input tokens cross a threshold (see modelingThresholdTokens in
# agent.go) by appending to the last tool result, and once unconditionally
# on the wrap-up turn after -n is exhausted via the leading user message.
# The wrap-up firing sets $AUTOPROBE_FINAL=1 so we can switch to last-chance
# framing.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROBE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
PROGRAMS_DIR="$PROBE_DIR/programs"

if [ "${AUTOPROBE_FINAL:-0}" = "1" ]; then
    cat <<EOF
[FINAL ITERATION]
The iteration budget configured with -n has been reached. This is your last
turn before the harness terminates the run. Anything not written to disk now
will be lost — your conversation history does not survive.

$PROGRAMS_DIR is the only persistent memory you have. Use this turn to:

- Capture any new understanding of the environment as an executable program
  in $PROGRAMS_DIR (encode assumptions, surface violations via non-zero exit).
- Update or fix programs that produced misleading output during this run.
- Record the solution — if you converged on one — as a program in
  $PROGRAMS_DIR so the next run can pick up from where this one left off.

Write what is worth keeping; the next run will see exactly the program library
you leave behind.
EOF
    exit 0
fi

cat <<EOF
[MODELING]
You have accumulated significant in-cycle context this tool-use cycle. The
conversation history is ephemeral — it will be wiped when this cycle ends,
so anything not committed to a program in $PROGRAMS_DIR is about to be lost.
The programs in $PROGRAMS_DIR are your executable model of the environment;
this is the moment to update that model with what you have just learned.

Now is the right moment to:
  1. Write or update a program in $PROGRAMS_DIR that captures what you have
     learned so far this cycle — a reading you keep re-fetching becomes a
     program that prints the compact summary; a check you keep running
     becomes a program that exits non-zero when the assumption is violated;
     a computation you keep doing in tokens becomes a program that does it
     in bytes.
  2. END this tool-use cycle: after writing, respond with a brief plain-text
     summary and NO further tool calls. The next iteration will start fresh
     with your updated programs contributing their output to context — and
     you will be able to attack the next sub-problem from a clean slate
     instead of dragging this cycle's history forward.

Continuing to call tools here makes every subsequent inference re-pay for
the history you have accumulated. Compress and yield.
EOF
