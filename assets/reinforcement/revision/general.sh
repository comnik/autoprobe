#!/usr/bin/env bash
# Reinforcement fired when total program output exceeds the context budget.
# Stdout is appended at the tail of the iteration's context as the revision
# prompt; it should explain to the agent what just happened and how to fix
# it. Paths are derived from this script's own location so the prompt always
# carries fully-resolved paths regardless of where the probe directory lives.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROBE_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
PROGRAMS_DIR="$PROBE_DIR/programs"
INACTIVE="$PROBE_DIR/inactive"

cat <<EOF
[REVISION]
The total output of your installed programs is exceeding the context budget,
so some are being dropped from this iteration (see [program=... dropped: ...]
sentinels above). How the harness manages this:

- Every program in $PROGRAMS_DIR runs every iteration regardless of whether
  you see its output. Running them is essentially free; spending tokens on
  their output is not.
- The agent-controlled demotion list lives at $INACTIVE — a newline-delimited
  list of program names. Programs listed there are still run, but their output
  only enters the context when they exit non-zero (the alarm channel) or when
  they are randomly drawn into the 20% exploration slot at the tail of the
  context.
- Under overflow the harness gives 80% of the budget to programs NOT listed in
  that file, packed in lex order; later-sorting active programs are first to be
  dropped if the active set still doesn't fit.

Do two things now:

1. Improve information density of the programs in $PROGRAMS_DIR. Rewrite
   verbose programs to emit more compressed output, merge redundant programs,
   or delete dead ones. This is the durable fix.
2. Update $INACTIVE to demote less-valuable programs into the exploration slot.
   Edit it with the normal write/edit tools — create the file if it does not
   exist, append a program's filename to demote it, remove the line later to
   promote it back. Nothing is destroyed.
EOF
