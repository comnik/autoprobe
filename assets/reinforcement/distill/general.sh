#!/usr/bin/env bash
# Reinforcement that nudges the agent to distill what it has learned during
# the current tool-use cycle into an executable program in $PROGRAMS_DIR.
# Fires periodically once cumulative in-cycle input tokens cross a threshold
# (see distillThresholdTokens in agent.go) and once unconditionally on the
# wrap-up turn after -n is exhausted. The wrap-up firing sets
# $AUTOPROBE_FINAL=1 so we can switch to last-chance framing.
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
[DISTILL]
You have spent a meaningful amount of context this tool-use cycle re-reading
prior results into your input. The conversation history is ephemeral — it
will be wiped when this cycle ends — so anything not committed to a program
is about to be lost.

$PROGRAMS_DIR is your only persistent memory. Before continuing, compress
what you have learned this cycle into a program there:

- A reading you keep re-fetching → a program that reads it once and prints
  the compact summary you actually use.
- A check you keep running ad hoc → a program that runs it deterministically
  and exits non-zero when the assumption is violated.
- A computation the model is doing in tokens → a program that does it in
  bytes, so the next iteration can read the answer instead of re-deriving it.

The buffer-manager rule: a page (≈2K tokens) carried for many iterations
costs more cumulatively than the program that subsumes it. If you are
dragging the same content forward, write the program now.
EOF
