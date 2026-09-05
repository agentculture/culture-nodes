#!/usr/bin/env bash
# Runs culture-nodes's own PRD §13 conformance kit (tests/conformance) against
# a LIVE qwen-bridge instance, dispatching into a throwaway scratch repo with
# `qwen_bin` pointed at the FAKE ACP agent
# (tests/fixtures/acp/fake_acp_agent.py) — the deterministic harness-swap
# proof (task t3, issue #297) that the kit exercises the qwen bridge over
# real HTTP, not just the kit's own in-process reference actor.
#
# tests/fixtures/acp/fake_acp_agent.py replays one static, committed set of
# ACP wire fixtures (FAKE_ACP_BEHAVIOR=ok, the default: firehose_72_updates
# + a terminal stopReason end_turn) rather than answering per-input the way
# tests/fake_claude.py does — see the identical script under
# adapters/pi/scripts/ for why that is enough here: neither the sync/async
# split (server.py's own `decide_async`, driven by the -input JSON's
# "async" field, not by anything the fake decides) nor cancellation
# (qwen_bridge/async_runner.py's `cancel()` always answers success, whether
# or not the invocation id is one it still knows about — PRD §13.6 is
# best-effort at the actor) needs the fake to distinguish sync from async
# from bad input. Only the contract-failure check needs a distinct fake
# behavior, and it gets one for free: `-bad-input` omits `instruction`,
# which server.py rejects before ever spawning the ACP driver. No
# per-input fake logic was added — if the kit ever needed one, that would
# be a deviation (see this task's acceptance criteria), not a silent fake
# change.
#
# One field the pi sibling script does not need: qwen-bridge refuses every
# invocation whose input omits `mode` (server.py, "the bridge sets the ACP
# session mode from policy and never defaults it" — spec c18/h15), so both
# scripts' -input/-async-input carry "mode": "default" — a mode the fake's
# measured session/new fixture actually offers. pi_bridge does not read
# `mode` at all; it is a harmless extra field there (folded into the
# instruction's "Bound inputs" block, which the fake ignores).
#
# Requires: `go` on PATH and `uv`. No real `qwen` install.
#
# Usage: adapters/qwen/scripts/run_conformance_kit.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${BRIDGE_DIR}/../.." && pwd)"
FAKE_ACP_AGENT="${BRIDGE_DIR}/tests/fixtures/acp/fake_acp_agent.py"

command -v go >/dev/null 2>&1 || {
  echo "error: go is not on PATH" >&2
  exit 2
}
command -v uv >/dev/null 2>&1 || {
  echo "error: uv is not on PATH" >&2
  exit 2
}
[[ -x "${FAKE_ACP_AGENT}" ]] || {
  echo "error: ${FAKE_ACP_AGENT} is not executable (chmod +x it)" >&2
  exit 2
}

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
echo "# scratch" >"${SCRATCH_REPO}/README.md"
git -C "${SCRATCH_REPO}" add README.md
git -C "${SCRATCH_REPO}" commit -q -m init

echo "== writing bridge config =="
mkdir -p "${STATE_DIR}"
cat >"${CONFIG_FILE}" <<JSON
{
  "repo_allowlist": ["${SCRATCH_REPO}"],
  "qwen_bin": "${FAKE_ACP_AGENT}",
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

echo "== syncing adapters/qwen =="
uv sync --project "${BRIDGE_DIR}" >/dev/null

echo "== starting qwen-bridge on an ephemeral port (fake ACP agent subprocess) =="
PORT_FILE="${WORKDIR}/port"
# FAKE_ACP_BEHAVIOR=ok is the fake's own default; set explicitly so the
# harness's chosen behavior is not left to an implicit default.
FAKE_ACP_BEHAVIOR="ok" \
  uv run --project "${BRIDGE_DIR}" python -c "
import json, sys
sys.path.insert(0, '${BRIDGE_DIR}/src')
from qwen_bridge.config import Config
from qwen_bridge.server import start_background
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

# Self-check (task t3's own acceptance criterion): the -input/-async-input/
# -bad-input JSON strings below must be identical to the pi sibling
# script's, modulo the scratch repo path — see
# tests/test_conformance_kit_inputs_match.py, which asserts this from BOTH
# scripts' source rather than trusting a comment here to stay true.
INPUT="{\"instruction\": \"write a short note (conformance sync)\", \"repo\": \"${SCRATCH_REPO}\", \"mode\": \"default\"}"
ASYNC_INPUT="{\"instruction\": \"write a short note (conformance async)\", \"repo\": \"${SCRATCH_REPO}\", \"async\": true, \"mode\": \"default\"}"
BAD_INPUT="{\"repo\": \"${SCRATCH_REPO}\"}"

echo "== running the conformance kit =="
cd "${REPO_ROOT}"
set +e
go test -v ./tests/conformance -args \
  -endpoint="http://127.0.0.1:${PORT}" \
  -auth-token="${AUTH_TOKEN}" \
  -input="${INPUT}" \
  -async-input="${ASYNC_INPUT}" \
  -bad-input="${BAD_INPUT}" \
  -callback-wait=60s \
  -timeout=60s \
  -expect-callback-retry \
  -require-cancellation
STATUS=$?
set -e

echo "== conformance kit exit status: ${STATUS} =="
exit "${STATUS}"
