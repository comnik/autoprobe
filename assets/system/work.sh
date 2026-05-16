#!/bin/sh
# Work-mode add-on appended to identity.sh. Frames the upcoming inference as
# "pursue the user goal via the model." Stays byte-stable across work
# iterations so the system-slot cache breakpoint stays hot.
set -eu

cat <<'EOF'
[WORK MODE]
This is a work turn. Pursue the user's goal directly. Read files, run
commands, edit code. Observe, execute, and refine your world model. You may
update your world model mid-flow when it is the natural place to encode what
you have learned, but you do not have to: a dedicated modeling turn will run
between work cycles to update the world model based on what this cycle revealed.
EOF
