#!/usr/bin/env bash
# Reinforcement fired when the write target is OUTSIDE $AUTOPROBE_PROGRAMS_DIR.
# Reads the write tool's JSON arguments from stdin.
set -eu

path=$(python3 -c 'import json, sys; print(json.load(sys.stdin).get("path", ""))')
[ -n "$path" ] || exit 0

if [ -n "${AUTOPROBE_PROGRAMS_DIR:-}" ]; then
    case "$path" in
        /*) abs="$path" ;;
        *)  abs="$(pwd)/$path" ;;
    esac
    case "$abs" in
        "$AUTOPROBE_PROGRAMS_DIR"/*) exit 0 ;;
    esac
fi

cat <<'EOF'
REMEMBER: Check your existing library of programs in the `$AUTOPROBE_PROGRAMS_DIR` for established
abstractions and existing capabilities. You may have moved some of them to the inactive list due to context budget pressure,
but they are still there and can be reactivated if they are relevant to the current problem.

REMEMBER: The `$AUTOPROBE_PROGRAMS_DIR` directory is the only persistent memory have, so you should write
programs that model your environment and and compress what you learn about how to achieve
the user's goal. Prefer executable programs over static files, so that you can encode your
assumptions.

The solution program should also be written to the `$AUTOPROBE_PROGRAMS_DIR` directory.
EOF
