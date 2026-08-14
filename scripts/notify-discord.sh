#!/usr/bin/env bash
# Post a short operator notification to the Discord channel.
#
# This is the OPERATOR lane, deliberately separate from the two product
# lanes that also reach Discord:
#
#   * cmd/nodes-notifier   — the daemon that announces run lifecycle from the
#                            SSE feed (task t14). Product, unattended.
#   * the notify actor     — a workflow node that sends a message as a step
#                            in a run, with a ledger record (issue #68).
#
# This script is neither. It is how a human or an assistant working the repo
# pings the channel about work in progress, and it exists because that should
# not require standing up a run.
#
# Same posture the notifier daemon takes, for the same reasons (devex's proven
# design, ported in internal/notifier):
#   * the URL comes from the environment only — it embeds a token, so it must
#     never reach a config file, an argv, or this repository
#   * one bounded POST, no retries, no redirects
#   * fail-open: a webhook outage must never fail the caller's work
#   * never journal the URL or the payload
#
# Usage:
#   scripts/notify-discord.sh "title" "body"
#   scripts/notify-discord.sh --level warn "title" "body"
set -uo pipefail

LEVEL=info
if [ "${1:-}" = "--level" ]; then
  LEVEL=$2
  shift 2
fi

TITLE=${1:-}
BODY=${2:-}
if [ -z "$TITLE" ]; then
  echo "usage: $0 [--level info|warn|error] <title> [body]" >&2
  echo "hint: set CULTURE_NODES_WEBHOOK_URL in the environment (never in a file in this repo)" >&2
  exit 1
fi

# Resolve from the environment, falling back to a gitignored .env at the repo
# root so an interactive operator does not have to export it every time.
if [ -z "${CULTURE_NODES_WEBHOOK_URL:-}" ]; then
  repo_root=$(git rev-parse --show-toplevel 2>/dev/null || echo .)
  if [ -f "$repo_root/.env" ]; then
    # shellcheck disable=SC1090
    CULTURE_NODES_WEBHOOK_URL=$(grep -m1 '^CULTURE_NODES_WEBHOOK_URL=' "$repo_root/.env" | cut -d= -f2-)
  fi
fi

if [ -z "${CULTURE_NODES_WEBHOOK_URL:-}" ]; then
  # Fail OPEN. A missing webhook is an absent transport, not an error in
  # whatever the caller was actually doing.
  echo "notify-discord: no CULTURE_NODES_WEBHOOK_URL — skipping (this is not a failure)" >&2
  exit 0
fi

case "$LEVEL" in
  warn)  COLOR=16763904 ;;   # amber
  error) COLOR=15158332 ;;   # red
  *)     COLOR=3066993  ;;   # green
esac

# Discord's embed limits: 256 for a title, 4096 for a description. Trim
# defensively rather than letting the API reject the whole post.
TITLE=$(printf '%s' "$TITLE" | cut -c1-250)
BODY=$(printf '%s' "$BODY" | cut -c1-3900)

PAYLOAD=$(TITLE="$TITLE" BODY="$BODY" COLOR="$COLOR" python3 -c '
import json, os
embed = {"title": os.environ["TITLE"], "color": int(os.environ["COLOR"])}
body = os.environ.get("BODY", "")
if body:
    embed["description"] = body
print(json.dumps({"embeds": [embed]}))
')

# One bounded POST. No retries (a retry storm against a rate-limited webhook
# is worse than a dropped message), no redirects, and the URL never appears
# in output — only the status code does.
CODE=$(curl -sS -o /dev/null -w '%{http_code}' \
  --max-time 5 --no-location \
  -H 'Content-Type: application/json' \
  -X POST -d "$PAYLOAD" \
  "$CULTURE_NODES_WEBHOOK_URL" 2>/dev/null)

case "$CODE" in
  2*) echo "notify-discord: delivered ($CODE)" ;;
  *)  echo "notify-discord: not delivered (HTTP ${CODE:-none}) — continuing anyway" >&2 ;;
esac
exit 0
