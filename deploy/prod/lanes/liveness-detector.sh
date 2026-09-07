# deploy/prod/lanes/liveness-detector.sh -- the lane-liveness detector tail
# (issue #308, plan loop-closure-claude-codex task t11, spec c4/h13).
#
# Sourced by deploy.sh; reads its helpers at call time: say, unix_user_target.
# Sits in its own file because deploy.sh is at the 1000-line guard.
#
# Why this line exists: on 2026-09-07 both codex lanes answered /healthz 200
# and `codex login status` printed "Logged in using ChatGPT" (the codex
# preflight's check 3, now advisory), and minutes later every dispatch failed
# with "refresh token was revoked". The fact that tells a live lane from a
# dead one is the bridge's own `liveness` host fact (adapters/*/liveness.py,
# task t9), advertised on /v1/capabilities:
#
#     {"session_ok": true|false|null, "reason": <closed vocabulary>,
#      "checked_at": <ISO-8601 UTC>, "mode": "LOCK"|"CHECK"}
#
# A deploy is the moment an operator is already reading the terminal, so the
# deploy prints one line per lane with that fact right after `nodes doctor`.
#
# A DETECTOR, NEVER A GATE. Every path returns 0: a dead lane, a bridge that
# cannot be read, a bridge older than the fact, an account that was never
# bootstrapped. The stack is already up when this speaks, and a deploy that
# refused on a dead session would block the very re-deploy that restores it
# (the operator re-logs in as the engine account, re-copies the credential,
# restarts the bridge; the start-up probe clears the latch). `nodes doctor`'s
# lane_liveness check reads the same fact off the control plane for the
# pre-fan-out decision; this is the per-host, per-bridge view of it.
#
# Authentication: /v1/capabilities is a bearer-authenticated route (only
# /healthz is open), so the read runs AS THE ENGINE ACCOUNT and sources the
# account's own ~/.culture-nodes/codex-bridge.env on the far side -- the same
# shape deploy.sh's codex preflight step uses. The token is used inside the
# remote shell only; nothing on this side ever holds or prints it.

# LIVENESS_DETECTOR_START
# lane_liveness_detector <host> -- print `liveness[codex-<host>@<host>] ...`
# for the codex bridge on <host> (port 8086). Always returns 0.
lane_liveness_detector() { # host
  local host=$1 target role lane body line
  target=$(unix_user_target "$host" codex)
  # The actor's role name, by the same host gates deploy.sh's case uses.
  case "$host" in thor*) role=thor ;; orin*) role=orin ;; *) role=${host%%.*} ;; esac
  lane="codex-$role@$host"
  # The remote script rides stdin, not argv: the bearer is sourced and used
  # on the far side only, and ssh's own argv is just `bash -s`.
  body=$(ssh "$target" bash -s 2>/dev/null <<'REMOTE'
set -a; . ~/.culture-nodes/codex-bridge.env 2>/dev/null; set +a
curl -fsS --max-time 5 -H "Authorization: Bearer $CODEX_BRIDGE_AUTH_TOKEN" http://127.0.0.1:8086/v1/capabilities
REMOTE
) || body=""
  if [ -z "$body" ]; then
    say "liveness[$lane] session_ok=unmeasured reason=unmeasured (bridge /v1/capabilities unreadable as $target -- not a verdict; a lane nobody measured is not a dead lane)"
    return 0
  fi
  line=$(printf '%s' "$body" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except ValueError:
    print("session_ok=unmeasured reason=unmeasured (bridge answered non-JSON)"); sys.exit(0)
host = doc.get("host") if isinstance(doc, dict) else None
fact = host.get("liveness") if isinstance(host, dict) else None
if not isinstance(fact, dict):
    print("session_ok=unmeasured reason=unmeasured (bridge advertises no liveness fact -- predates t9; redeploy the bridge)"); sys.exit(0)
ok = fact.get("session_ok")
ok_s = "unmeasured" if ok is None else str(bool(ok)).lower()
out = "session_ok=%s reason=%s mode=%s checked_at=%s" % (ok_s, fact.get("reason", "unmeasured"), fact.get("mode", "-"), fact.get("checked_at", "-"))
if ok is False:
    out += " -- DEAD LANE: leave it out of the next split plan; restore with an interactive engine re-login as the engine account, re-copy the credential, restart the bridge"
print(out)
' 2>/dev/null) || line="session_ok=unmeasured reason=unmeasured (could not parse the bridge answer)"
  say "liveness[$lane] $line"
  return 0
}
# LIVENESS_DETECTOR_END
