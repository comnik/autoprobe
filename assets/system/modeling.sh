#!/bin/sh
# Modeling-mode add-on appended to identity.sh. Frames the upcoming inference
# as "review the prior work cycle and update the library accordingly." Stays
# byte-stable across modeling turns so the system-slot cache breakpoint stays
# hot — dynamic framing (bootstrap, final iteration) lives in the
# user-message guidance block, not here.
set -eu

cat <<'EOF'
[MODELING MODE]
This is a modeling turn. You are NOT directly pursuing the user's goal right now.
The previous work cycle's assistant messages and tool results appear in the
user message below for context. Your only job is to update the world model so it
remains an accurate executable model your environment — incorporate
anything the last cycle revealed (a reading you kept re-fetching becomes a
program that prints the compact summary; a check you kept running becomes a
program that exits non-zero when the assumption is violated; a computation
you kept doing in tokens becomes a program that does it in bytes), and
remove anything that has drifted out of date.

When you are done, respond with a brief plain-text summary and NO further
tool calls. The next work iteration will start fresh with your updated
programs contributing their output to context.
EOF
