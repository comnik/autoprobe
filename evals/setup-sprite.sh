#!/usr/bin/env bash
# (Re-)create the `autoprobe-programbench` sprite with autoprobe @ HEAD installed
# and the programbench task `abishekvashok__cmatrix.5c082c6` staged in ~/programbench.
set -euo pipefail

SPRITE_NAME="autoprobe-programbench"
AUTOPROBE_REPO="https://github.com/comnik/autoprobe.git"
TASK_ID="abishekvashok__cmatrix.5c082c6"
TASK_IMAGE="programbench/abishekvashok_1776_cmatrix.5c082c6:task_cleanroom"

if ! command -v sprite >/dev/null 2>&1; then
  echo "error: sprite CLI not found in PATH" >&2
  exit 1
fi

if sprite list 2>/dev/null | awk '{print $1}' | grep -qx "${SPRITE_NAME}"; then
  echo "==> Destroying existing sprite '${SPRITE_NAME}'"
  sprite destroy --force "${SPRITE_NAME}"
fi

echo "==> Creating sprite '${SPRITE_NAME}'"
sprite create --skip-console "${SPRITE_NAME}"

echo "==> Installing docker"
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  if ! command -v docker >/dev/null 2>&1; then
    curl -fsSL https://get.docker.com | sudo sh
  fi
  if ! sudo docker info >/dev/null 2>&1; then
    # Sprites do not run systemd; launch dockerd directly and detach it from
    # this exec session so it survives subsequent `sprite exec` invocations.
    sudo setsid sh -c "nohup dockerd >/tmp/dockerd.log 2>&1 &" </dev/null
    for _ in $(seq 1 30); do
      sudo docker info >/dev/null 2>&1 && break
      sleep 1
    done
    sudo docker info >/dev/null
  fi
  sudo usermod -aG docker "$(id -un)" || true
'

echo "==> Creating low-privilege 'runner' user"
# The agent process runs as `runner`: no sudo, not in the docker group, can't
# escalate. setup-sprite.sh and run-eval.sh still run as the sprite user (which
# has sudo) for staging — runner only inherits the prepared workspace.
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  if ! id runner >/dev/null 2>&1; then
    sudo useradd -m -s /bin/bash runner
  fi
'

echo "==> Installing autoprobe (HEAD of ${AUTOPROBE_REPO})"
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  rm -rf "$HOME/autoprobe"
  git clone --depth=1 '"${AUTOPROBE_REPO}"' "$HOME/autoprobe"
  cd "$HOME/autoprobe"
  go install .
  sudo install -m 0755 "$(go env GOPATH)/bin/autoprobe" /usr/local/bin/autoprobe
'

echo "==> Staging eval-ready workspace at ~/programbench"
# Pull just /workspace out of the task image (not the whole rootfs — that
# trips over symlinks like usr/lib64 -> usr/lib). The checkpoint we create
# at the end of this script captures this state; run-eval.sh restores to it
# so the workspace is pristine on every run with no docker calls at run time.
# Workspace contents are owned by `runner` so the unprivileged agent can
# write there; the sealed ./executable stays root-owned mode ---x--x--x.
sprite exec -s "${SPRITE_NAME}" --env TASK_IMAGE="${TASK_IMAGE}" -- bash -lc '
  set -euo pipefail
  sudo rm -rf "$HOME/programbench"
  sudo mkdir -p "$HOME/programbench"
  sudo docker pull "$TASK_IMAGE"
  cid=$(sudo docker create "$TASK_IMAGE")
  trap "sudo docker rm -f \"$cid\" >/dev/null 2>&1 || true" EXIT
  sudo docker cp "$cid":/workspace "$HOME/programbench/workspace"
  sudo chown -R runner:runner "$HOME/programbench"
  sudo chown root:root "$HOME/programbench/workspace/executable"
  sudo chmod 0111 "$HOME/programbench/workspace/executable"
'

echo "==> Syncing programbench task blobs"
sprite exec -s "${SPRITE_NAME}" --env TASK_ID="${TASK_ID}" -- bash -lc '
  set -euo pipefail
  if ! command -v uvx >/dev/null 2>&1; then
    curl -LsSf https://astral.sh/uv/install.sh | sh
    export PATH="$HOME/.local/bin:$PATH"
  fi
  uvx programbench blob sync "$TASK_ID"
'

echo "==> Verifying task container (./executable -h runs, README.md present)"
sprite exec -s "${SPRITE_NAME}" --env TASK_IMAGE="${TASK_IMAGE}" -- bash -lc '
  set -euo pipefail
  sudo docker run --rm --network none "$TASK_IMAGE" /bin/bash -c "test -f README.md && ./executable -h"
'

echo "==> Disabling docker init hooks before checkpoint"
# Sprites do not run systemd, and the SysV init script chokes on a `ulimit`
# call. If either is left in place, the sprite fails to boot from the
# checkpoint with "failed to start overlay service tree". We launch dockerd
# explicitly via setsid in run-eval.sh, so neither hook is needed.
sprite exec -s "${SPRITE_NAME}" -- bash -lc '
  set -euo pipefail
  sudo update-rc.d -f docker remove >/dev/null 2>&1 || true
  sudo rm -f /etc/init.d/docker \
             /lib/systemd/system/docker.service \
             /lib/systemd/system/docker.socket \
             /etc/systemd/system/multi-user.target.wants/docker.service \
             /etc/systemd/system/sockets.target.wants/docker.socket
  sudo pkill -x dockerd || true
'

echo "==> Clearing existing checkpoints"
sprite checkpoint list -s "${SPRITE_NAME}" \
  | awk '"'"'/^v[0-9]+/ {print $1}'"'"' \
  | while read -r cp; do
      echo "    deleting ${cp}"
      sprite checkpoint delete -s "${SPRITE_NAME}" "${cp}"
    done

echo "==> Creating checkpoint of ready-for-eval state"
sprite checkpoint create -s "${SPRITE_NAME}" --comment "ready for eval: ${TASK_ID}"

echo "==> Done. Open a shell with: sprite console -s ${SPRITE_NAME}"
