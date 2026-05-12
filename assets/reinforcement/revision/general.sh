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
STATS_DIR="$PROBE_DIR/statistics"

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

# Statistics block. Each program has its own JSON file under
# $STATS_DIR/<name>.json — the harness writes them in parallel during the
# substantive-iteration finalize step. We glob the directory and let
# python merge them in-memory for ranking; a missing/empty directory just
# suppresses this section. Programs are sorted ascending by a composite
# of change-information-content + overlap-with-response so the least
# valuable rows surface at the top of the table and at the bottom-k
# callout — those are the agent's first candidates for $INACTIVE.
if [ -d "$STATS_DIR" ]; then
    if rendered=$(python3 - "$STATS_DIR" 2>/dev/null <<'PY'
import json, os, sys

stats_dir = sys.argv[1]
data = {}
try:
    entries = sorted(os.listdir(stats_dir))
except OSError:
    sys.exit(1)
for fname in entries:
    if not fname.endswith(".json"):
        continue
    name = fname[:-len(".json")]
    try:
        with open(os.path.join(stats_dir, fname)) as f:
            data[name] = json.load(f)
    except Exception:
        continue
if not data:
    sys.exit(0)

def composite(s):
    return (s.get("avg_change_amount", 0.0) + s.get("overlap_with_response", 0.0)) / 2

rows = sorted(((composite(s), name, s) for name, s in data.items()))

print()
print("[STATISTICS]")
print("Per-program metrics (EWMA over recent iterations; lowest composite first):")
print()
hdr = f"{'name':<32} {'tokens':>8} {'chg-frq':>8} {'chg-amt':>8} {'ovlp':>6} {'stale':>6} {'lat-ms':>8} {'n':>5}"
print(hdr)
print("-" * len(hdr))
for _, name, s in rows:
    print(
        f"{name:<32.32} "
        f"{s.get('avg_output_tokens', 0):>8.0f} "
        f"{s.get('change_frequency', 0):>8.2f} "
        f"{s.get('avg_change_amount', 0):>8.2f} "
        f"{s.get('overlap_with_response', 0):>6.2f} "
        f"{s.get('staleness', 0):>6d} "
        f"{s.get('avg_latency_ms', 0):>8.0f} "
        f"{s.get('samples', 0):>5d}"
    )

bottom = min(3, len(rows))
if bottom:
    print()
    print(f"Deactivation candidates (lowest composite = chg-amt + ovlp):")
    for c, name, _ in rows[:bottom]:
        print(f"  - {name}  composite={c:.2f}")
PY
)
    then
        if [ -n "$rendered" ]; then
            printf '%s\n' "$rendered"
        fi
    fi
fi
