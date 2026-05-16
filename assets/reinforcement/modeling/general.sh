#!/usr/bin/env bash
# Yield nudge: when in-cycle drag crosses modelingThresholdTokens, this
# reinforcement is appended to the last tool result so the model sees it
# right before its next response. The dedicated modeling turn that runs
# between work cycles is responsible for actually updating the program
# library — this script's only job now is to ask the model to close the
# cycle. The $AUTOPROBE_FINAL=1 firing on the wrap-up turn switches in
# last-chance framing because no modeling turn will follow.
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
[YIELD]
You have accumulated significant in-cycle drag this tool-use cycle. Continuing
to call tools here makes every subsequent inference re-pay for the history
you have accumulated. Respond with a brief plain-text summary and NO further
tool calls.

A dedicated modeling turn will run next and update the program library based
on what just happened, then the next work cycle will start fresh with your
updated programs contributing their output to context — and you will be able
to attack the next sub-problem from a clean slate instead of dragging this
cycle's history forward.
EOF
