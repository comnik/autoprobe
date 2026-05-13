#!/usr/bin/env bash
# Refresh autoprobe to HEAD on the `autoprobe-programbench` sprite, stage a
# fresh copy of the task workspace, and run autoprobe against it.
#
# Assumes the sprite has already been provisioned by evals/setup-sprite.sh.
# Requires ANTHROPIC_API_KEY in the local environment.
set -euo pipefail

SPRITE_NAME="autoprobe-programbench"
TASK_ID="abishekvashok__cmatrix.5c082c6"
TASK_IMAGE="programbench/abishekvashok_1776_cmatrix.5c082c6:task_cleanroom"
WORKSPACE="/home/sprite/programbench/workspace"
MODEL="claude-opus-4-7"
ITERATIONS=100

if ! command -v sprite >/dev/null 2>&1; then
  echo "error: sprite CLI not found in PATH" >&2
  exit 1
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "error: ANTHROPIC_API_KEY must be set in the local environment" >&2
  exit 1
fi

echo "==> Refreshing autoprobe to HEAD"
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  cd "$HOME/autoprobe"
  git fetch --depth=1 origin main
  git reset --hard origin/main
  go install .
  sudo install -m 0755 "$(go env GOPATH)/bin/autoprobe" /usr/local/bin/autoprobe
'

echo "==> Ensuring dockerd is running"
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  if ! sudo docker info >/dev/null 2>&1; then
    sudo setsid sh -c "nohup dockerd >/tmp/dockerd.log 2>&1 &" </dev/null
    for _ in $(seq 1 30); do
      sudo docker info >/dev/null 2>&1 && break
      sleep 1
    done
    sudo docker info >/dev/null
  fi
'

echo "==> Refreshing task workspace at ${WORKSPACE}"
# Re-export the task image's rootfs into a fresh /home/sprite/programbench.
# Workspace contents are owned by `runner` so the unprivileged agent can edit
# them, but the sealed ./executable is left root-owned with mode ---x--x--x:
# runner can run it, but cannot read or chmod it.
sprite exec -s "${SPRITE_NAME}" --env TASK_IMAGE="${TASK_IMAGE}" -- bash -lc '
  set -euo pipefail
  sudo rm -rf "$HOME/programbench"
  mkdir -p "$HOME/programbench"
  cid=$(sudo docker create "$TASK_IMAGE")
  trap "sudo docker rm -f \"$cid\" >/dev/null 2>&1 || true" EXIT
  sudo docker export "$cid" | sudo tar -x -C "$HOME/programbench"
  sudo chown -R runner:runner "$HOME/programbench"
  sudo chown root:root "$HOME/programbench/workspace/executable"
  sudo chmod 0111 "$HOME/programbench/workspace/executable"
'

read -r -d "" GOAL <<EOF || true
You are running on a sprite host. The reverse-engineering target is staged
at:

    ${WORKSPACE}

Operate directly in that directory.

Your task:

- Explore: Run the ./executable binary in ${WORKSPACE} with various flags
  (e.g. ./executable --help, ./executable -v) to reverse-engineer its
  behavior.
- Read: Examine README.md and any other documentation files there.
- Implement: Write the source code in a language of your choice (C, Rust,
  Go, etc.) into ${WORKSPACE}.
- Build: Create a build.sh script in ${WORKSPACE} that compiles the source
  into an executable named "candidate" in the same directory.
EOF

echo "==> Running autoprobe as runner (${MODEL}, n=${ITERATIONS})"
# sprite user (has sudo) hands off to runner (no sudo, not in docker group).
# `sudo -n -u runner env ...` re-establishes the env vars after sudo's
# env_reset; the inner bash is single-quoted so $WORKSPACE etc. are expanded
# by runner's shell, not by sprite's outer shell.
sprite exec --tty -s "${SPRITE_NAME}" \
  --env ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}",MODEL="${MODEL}",GOAL="${GOAL}",ITERATIONS="${ITERATIONS}",WORKSPACE="${WORKSPACE}" \
  -- bash -lc '
  set -euo pipefail
  exec sudo -n -u runner env \
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
