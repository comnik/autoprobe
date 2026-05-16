#!/bin/sh
# Work-mode add-on appended to identity.sh. Frames the upcoming inference as
# "pursue the user goal via the library." Stays byte-stable across work
# iterations so the system-slot cache breakpoint stays hot.
set -eu

cat <<'EOF'
[WORK MODE]
This is a work turn. Pursue the user's goal directly — read files, run
commands, edit code, install or refine programs — using the assembled
program outputs that follow as your dashboard onto the environment. You may
write to the library mid-flow when it is the natural place to encode what
you have learned, but you do not have to: a dedicated modeling turn will run
between work cycles to update the library based on what this cycle revealed.
EOF
