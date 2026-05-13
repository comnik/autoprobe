#!/usr/bin/env bash
# Download an autoprobe trace directory from a sprite to a local directory.
#
# `sprite exec` streams unreliably for files past ~600KB and `--http-post`
# truncates even small ones, so we always pipe through gzip on the remote
# and verify byte counts against `wc -c` afterwards, retrying mismatches.
set -euo pipefail

SPRITE_NAME="${SPRITE_NAME:-autoprobe-programbench}"
REMOTE_DIR="${1:-/home/sprite/programbench/workspace/.autoprobe-last-run}"
LOCAL_DIR="${2:-traces/$(basename "${REMOTE_DIR}")}"
MAX_RETRIES=3

if ! command -v sprite >/dev/null 2>&1; then
  echo "error: sprite CLI not found in PATH" >&2
  exit 1
fi

echo "==> Listing ${REMOTE_DIR} on ${SPRITE_NAME}"
# `wc -c *` gives us "<size> <name>" per file plus a "total" line we strip.
manifest=$(sprite exec -s "${SPRITE_NAME}" -- bash -c "cd '${REMOTE_DIR}' && wc -c *" 2>/dev/null | awk '$2 != "total" && $2 != ""')

if [[ -z "${manifest}" ]]; then
  echo "error: remote directory is empty or unreachable" >&2
  exit 1
fi

mkdir -p "${LOCAL_DIR}"

download_one() {
  local name="$1" expected="$2" dest="${LOCAL_DIR}/$1"
  local attempt actual
  for attempt in $(seq 1 "${MAX_RETRIES}"); do
    sprite exec -s "${SPRITE_NAME}" -- gzip -c "${REMOTE_DIR}/${name}" </dev/null 2>/dev/null \
      | gunzip -c > "${dest}" || true
    actual=$(wc -c < "${dest}" | tr -d ' ')
    if [[ "${actual}" == "${expected}" ]]; then
      printf '  %s (%s bytes)\n' "${name}" "${actual}"
      return 0
    fi
    printf '  %s attempt %d: got %s, expected %s\n' "${name}" "${attempt}" "${actual}" "${expected}" >&2
  done
  echo "error: failed to download ${name} after ${MAX_RETRIES} attempts" >&2
  return 1
}

echo "==> Downloading to ${LOCAL_DIR}"
failed=0
while read -r expected name; do
  download_one "${name}" "${expected}" || failed=1
done <<< "${manifest}"

if [[ "${failed}" -ne 0 ]]; then
  exit 1
fi

echo "==> Done"
