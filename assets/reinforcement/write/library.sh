#!/usr/bin/env bash
# Reinforcement fired when the write target is inside $AUTOPROBE_PROGRAMS_DIR.
# Reads the write tool's JSON arguments from stdin.
set -eu

path=$(python3 -c 'import json, sys; print(json.load(sys.stdin).get("path", ""))')
[ -n "$path" ] || exit 0
[ -n "${AUTOPROBE_PROGRAMS_DIR:-}" ] || exit 0

case "$path" in
    /*) abs="$path" ;;
    *)  abs="$(pwd)/$path" ;;
esac

case "$abs" in
    "$AUTOPROBE_PROGRAMS_DIR"/*) ;;
    *) exit 0 ;;
esac

cat <<'EOF'
REMEMBER: the programs in your world model should encode and validate the assumptions they make about
the environment that you are operating in. Exit code is a status channel, separate from stdout.
Exit 0 means the program ran successfully (stdout may be normal output or empty). 
Non-zero exit means something is wrong that requires your attention — a violated assumption, an environment change,
an error condition. Do NOT use non-zero exits for "no data" or routine empty output; reserve them for signals
worth attending to now. The harness force-includes non-zero-exit programs in your context.

REMEMBER: program filenames sort lexicographically and the harness packs outputs in that
order, so use prefixes deliberately to place programs where they belong in attention space.
Under budget pressure, lex order also determines drop order (later-sorting first), but rely on `.autoprobe/inactive` — not filename gymnastics — to make sure important probes fit.
Pick a name that places this program in attention space where it belongs.

REMEMBER: The world model in `$AUTOPROBE_PROGRAMS_DIR` is the only persistent memory have, so make sure it
captures, compresses, and continuously validates what you learn about your environment, in a way that helps you take action towards the user's goal.
Prefer executable programs over static files, so that you can encode your assumptions.
EOF
