#!/usr/bin/env bash
# Restore the `autoprobe-programbench` sprite to its eval-ready checkpoint,
# refresh autoprobe to HEAD, and run autoprobe as the unprivileged `runner`
# user against the pristine workspace.
#
# Assumes the sprite has been provisioned by evals/setup-sprite.sh, which
# captures the eval-ready state (runner user, /usr/local/bin/autoprobe, sealed
# workspace at /home/sprite/programbench) as a sprite checkpoint.
# Requires ANTHROPIC_API_KEY in the local environment.
set -euo pipefail

SPRITE_NAME="autoprobe-programbench"
# TASK_ID / TASK_IMAGE are consumed by setup-sprite.sh; here for traceability
# and to drive the grading step at the end of this script.
TASK_ID="abishekvashok__cmatrix.5c082c6"
TASK_IMAGE="programbench/abishekvashok_1776_cmatrix.5c082c6:task_cleanroom"
WORKSPACE="/home/sprite/programbench/workspace"
MODEL="claude-opus-4-7"
ITERATIONS=50

if ! command -v sprite >/dev/null 2>&1; then
  echo "error: sprite CLI not found in PATH" >&2
  exit 1
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "error: ANTHROPIC_API_KEY must be set in the local environment" >&2
  exit 1
fi

echo "==> Restoring sprite to eval-ready checkpoint"
# `sprite restore` replaces the writable overlay with the captured state,
# resetting everything we'd otherwise have to clean by hand: workspace, runner
# home, /tmp /var/tmp /dev/shm, leftover runner processes. setup-sprite.sh
# leaves exactly one checkpoint behind, so the newest vN line is unambiguous.
CHECKPOINT=$(sprite checkpoint list -s "${SPRITE_NAME}" | awk '/^v[0-9]+/ {print $1; exit}')
if [[ -z "${CHECKPOINT}" ]]; then
  echo "error: no checkpoint on ${SPRITE_NAME} — run evals/setup-sprite.sh first" >&2
  exit 1
fi
sprite restore -s "${SPRITE_NAME}" "${CHECKPOINT}"
echo "    waiting for sprite to come back online..."
for _ in $(seq 1 60); do
  sprite exec -s "${SPRITE_NAME}" -- true 2>/dev/null && break
  sleep 1
done
sprite exec -s "${SPRITE_NAME}" -- true

echo "==> Refreshing autoprobe to HEAD"
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  cd "$HOME/autoprobe"
  git fetch --depth=1 origin main
  git reset --hard origin/main
  go install .
  sudo install -m 0755 "$(go env GOPATH)/bin/autoprobe" /usr/local/bin/autoprobe
'

read -r -d "" GOAL <<EOF || true
There is a binary plus documentation and related materials in the ${WORKSPACE} directory.

Treat this binary as a black box and model its behaviour accurately and exhaustively.

Don't access the internet, don't try to look up the source code.
EOF

# NOTE leaving this out as an experiment
# Your goal is to exactly reimplement the functionality of this binary in a language of your choice, without access to the original source code.
# Create a compile.sh script in ${WORKSPACE} that, when run with cwd=${WORKSPACE}, produces a working binary at ${WORKSPACE}/executable
# (overwriting the sealed one). The grader will wipe the workspace, extract your submission, chmod +x compile.sh, and run ./compile.sh — so anything
# needed at compile time must live in the workspace.

echo "==> Running autoprobe as runner (${MODEL}, n=${ITERATIONS})"
# sprite user (has sudo) hands off to runner (no sudo, not in docker group).
# `sudo -n -u runner env ...` re-establishes the env vars after sudo's
# env_reset; the inner bash is single-quoted so $WORKSPACE etc. are expanded
# by runner's shell, not by sprite's outer shell.
sprite exec --tty -s "${SPRITE_NAME}" \
  --env ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}",MODEL="${MODEL}",GOAL="${GOAL}",ITERATIONS="${ITERATIONS}",WORKSPACE="${WORKSPACE}" \
  -- bash -lc '
  set -euo pipefail
  sudo -n -u runner env \
    ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY" \
    MODEL="$MODEL" \
    GOAL="$GOAL" \
    ITERATIONS="$ITERATIONS" \
    WORKSPACE="$WORKSPACE" \
    bash -lc '\''
      set -euo pipefail
      cd "$WORKSPACE"
      autoprobe init --provider anthropic --model "$MODEL"
      autoprobe run -n "$ITERATIONS" --goal "$GOAL"
    '\''
'

echo "==> Grading candidate against oracle"
# The checkpoint has dockerd stopped; programbench eval spins up per-branch
# containers, so we need it running for the grade step. Then we tar up the
# agent-edited workspace (minus its own scratch + the sealed original exe)
# into the layout `programbench eval` expects: <run_dir>/<instance_id>/submission.tar.gz.
sprite exec --tty -s "${SPRITE_NAME}" \
  --env TASK_ID="${TASK_ID}",WORKSPACE="${WORKSPACE}" \
  -- bash -lc '
  set -euo pipefail
  export PATH="$HOME/.local/bin:$PATH"

  if ! sudo docker info >/dev/null 2>&1; then
    sudo setsid sh -c "nohup dockerd >/tmp/dockerd.log 2>&1 &" </dev/null
    for _ in $(seq 1 30); do
      sudo docker info >/dev/null 2>&1 && break
      sleep 1
    done
    sudo docker info >/dev/null
  fi

  RUN_OUT="$HOME/eval-runs/$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$RUN_OUT/$TASK_ID"
  # Workspace is runner-owned; sudo needed to read the entire tree including
  # the agent-written files. Exclude agent scratch and the sealed original
  # executable (the grader runs the agent_s compile.sh to produce a fresh one).
  sudo tar -czf "$RUN_OUT/$TASK_ID/submission.tar.gz" \
    --exclude=./.autoprobe \
    --exclude=./.autoprobe-last-run \
    --exclude=./executable \
    -C "$WORKSPACE" .
  sudo chown -R "$(id -un):$(id -gn)" "$RUN_OUT"
  echo "submission archive: $RUN_OUT/$TASK_ID/submission.tar.gz"
  ls -la "$RUN_OUT/$TASK_ID/submission.tar.gz"

  uvx programbench eval "$RUN_OUT"
  uvx programbench info "$RUN_OUT"
'
