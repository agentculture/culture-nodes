#!/usr/bin/env bash
# Runs culture-nodes's own PRD §13 conformance kit (tests/conformance) —
# UNMODIFIED, the same kit adapters/colleague runs itself against — against
# a LIVE codex-bridge instance, dispatching into a throwaway scratch repo
# with a REAL, authenticated `codex exec`.
#
# Unlike adapters/colleague/scripts/run_conformance_kit.sh (which can point
# colleague at COLLEAGUE_ENGINE=mock, a free deterministic offline engine),
# codex has no equivalent mock engine: every run of this script dispatches
# real (billable) codex invocations against whatever account `codex login`
# is authenticated with. This is a LOCAL/MANUAL verification tool — it is
# never wired into CI (see .github/workflows/adapter-codex.yml, which runs
# the fake-driven unit suite only).
#
# Requires: `go` on PATH, a working, authenticated `codex` install on PATH
# (`codex login status` must report a logged-in session), and `uv`.
#
# Usage: adapters/codex/scripts/run_conformance_kit.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${BRIDGE_DIR}/../.." && pwd)"

command -v go >/dev/null 2>&1 || { echo "error: go is not on PATH" >&2; exit 2; }
command -v codex >/dev/null 2>&1 || { echo "error: codex is not on PATH" >&2; exit 2; }
command -v uv >/dev/null 2>&1 || { echo "error: uv is not on PATH" >&2; exit 2; }
codex login status >/dev/null 2>&1 || { echo "error: codex is not logged in — run 'codex login' first" >&2; exit 2; }

WORKDIR="$(mktemp -d)"
SCRATCH_REPO="${WORKDIR}/scratch-repo"
STATE_DIR="${WORKDIR}/state"
CONFIG_FILE="${WORKDIR}/bridge.json"
AUTH_TOKEN="conformance-kit-token-$$"

cleanup() {
  if [[ -n "${BRIDGE_PID:-}" ]] && kill -0 "${BRIDGE_PID}" 2>/dev/null; then
    kill "${BRIDGE_PID}" 2>/dev/null || true
    wait "${BRIDGE_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

echo "== setting up scratch repo at ${SCRATCH_REPO} =="
mkdir -p "${SCRATCH_REPO}"
git -C "${SCRATCH_REPO}" init -q
git -C "${SCRATCH_REPO}" config user.email "conformance@example.com"
git -C "${SCRATCH_REPO}" config user.name "conformance kit"
echo "# scratch" > "${SCRATCH_REPO}/README.md"
git -C "${SCRATCH_REPO}" add README.md
git -C "${SCRATCH_REPO}" commit -q -m init

echo "== writing bridge config =="
mkdir -p "${STATE_DIR}"
cat > "${CONFIG_FILE}" <<JSON
{
  "repo_allowlist": ["${SCRATCH_REPO}"],
  "default_sandbox": "read-only",
  "auth_token": "${AUTH_TOKEN}",
  "state_dir": "${STATE_DIR}",
  "host": "127.0.0.1",
  "port": 0,
  "sync_max_steps": 6,
  "default_max_steps": 6,
  "heartbeat_after_seconds": 2,
  "poll_interval_seconds": 0.1,
  "callback_retry_backoff_seconds": 0.1
}
JSON

echo "== syncing adapters/codex =="
uv sync --project "${BRIDGE_DIR}" >/dev/null

echo "== starting codex-bridge on an ephemeral port =="
PORT_FILE="${WORKDIR}/port"
uv run --project "${BRIDGE_DIR}" python -c "
import json, sys
sys.path.insert(0, '${BRIDGE_DIR}/src')
from codex_bridge.config import Config
from codex_bridge.server import start_background
import time
cfg = Config.load('${CONFIG_FILE}')
srv, _ = start_background(cfg)
with open('${PORT_FILE}', 'w') as f:
    f.write(str(srv.server_address[1]))
print(f'bridge listening on 127.0.0.1:{srv.server_address[1]}', file=sys.stderr)
while True:
    time.sleep(1)
" &
BRIDGE_PID=$!

for _ in $(seq 1 50); do
  [[ -s "${PORT_FILE}" ]] && break
  sleep 0.1
done
PORT="$(cat "${PORT_FILE}")"
echo "== bridge up on port ${PORT} (pid ${BRIDGE_PID}) =="

for _ in $(seq 1 50); do
  curl -s -o /dev/null -w '' "http://127.0.0.1:${PORT}/healthz" && break
  sleep 0.1
done

echo "== running the conformance kit (this dispatches REAL, billable codex invocations) =="
cd "${REPO_ROOT}"
set +e
go test -v ./tests/conformance -args \
  -endpoint="http://127.0.0.1:${PORT}" \
  -auth-token="${AUTH_TOKEN}" \
  -input="{\"instruction\": \"Reply with exactly the single word: OK\", \"repo\": \"${SCRATCH_REPO}\"}" \
  -async-input="{\"instruction\": \"Reply with exactly the single word: OK\", \"repo\": \"${SCRATCH_REPO}\", \"async\": true}" \
  -bad-input="{\"repo\": \"${SCRATCH_REPO}\"}" \
  -callback-wait=90s \
  -timeout=90s \
  -expect-callback-retry \
  -require-cancellation
STATUS=$?
set -e

echo "== conformance kit exit status: ${STATUS} =="
exit "${STATUS}"
