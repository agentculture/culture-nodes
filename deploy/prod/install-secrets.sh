#!/usr/bin/env bash
# Generate the production secret set once and install it on thor and orin
# over ssh stdin — no secret ever appears in an argv, a shell history
# line, or this repo (plan t19; credential pattern per spec assumption c8,
# cited from reachy-mini-cli's argv-only ssh discipline).
#
# Usage: install-secrets.sh [thor-host] [orin-host]
# Idempotent per machine: refuses to overwrite an existing prod.env unless
# FORCE=1, so a re-deploy never silently rotates a live database password.
# The codex-bridge lane (~/.culture-nodes/codex-bridge.env) carries the
# same FORCE=1 guard so a re-run never silently rotates a live bridge
# token either; the NODES_ACTOR_CODEX_*_TOKEN lines it adds to prod.env
# are updated in place (or appended) instead, since they mirror rather
# than gate access.
set -euo pipefail

THOR=${1:-thor}
ORIN=${2:-orin}

# --- destructive-action confirmation protocol ------------------------------
# Rotating a live secret is irreversible: the old value is gone, and every
# component still holding it keeps working until it restarts and then fails
# auth. This lane therefore refuses a rotation the FIRST time it is asked,
# writes a confirmation file naming exactly what would be destroyed and what
# breaks, and only proceeds once a human (or agent) has READ that file and
# edited its verdict line — and only within a short window, so a stale
# confirmation from last week cannot authorize today's rotation.
#
# Written after a real incident: `FORCE=1` was passed intending to add one
# key to one file, and — because FORCE was a single global switch across
# every lane — it rotated prod.env, codex-bridge.env and runner.secret on a
# live host. Nothing broke immediately (running processes hold their creds in
# memory); the damage was latent until the next restart. Per-lane FORCE_*
# variables fixed the scoping; this protocol makes the remaining destructive
# path unrepeatable-by-accident.
CONFIRM_DIR=${CONFIRM_DIR:-$HOME/.culture-nodes}
CONFIRM_WINDOW_SECONDS=${CONFIRM_WINDOW_SECONDS:-900}   # 15 minutes

# require_destructive_confirmation <lane> <host> <what-breaks>
require_destructive_confirmation() {
  local lane=$1 host=$2 breaks=$3
  local file="$CONFIRM_DIR/CONFIRM-rotate-${lane}-${host}.md"
  mkdir -p "$CONFIRM_DIR"

  if [ -f "$file" ] && grep -qiE '^verdict:[[:space:]]*rotate[[:space:]]*$' "$file"; then
    local age now mtime
    now=$(date +%s); mtime=$(stat -c %Y "$file" 2>/dev/null || echo 0)
    age=$(( now - mtime ))
    if [ "$age" -le "$CONFIRM_WINDOW_SECONDS" ]; then
      rm -f "$file"   # single-use: the next rotation needs its own confirmation
      echo "confirmed rotation of $lane on $host (consumed $file)"
      return 0
    fi
    echo "confirmation in $file is stale (${age}s old, window ${CONFIRM_WINDOW_SECONDS}s) — rewriting it" >&2
  fi

  cat > "$file" <<EOF
# Destructive action requires confirmation

Lane:  ${lane}
Host:  ${host}
When:  $(date -Is)

## What this rotation destroys

${breaks}

The current value is NOT recoverable after rotation. Components already
running keep working until they restart, and then fail authentication — so
the breakage is LATENT, not immediate.

## To proceed

Change the verdict line below from 'hold' to 'rotate', then re-run the same
command within ${CONFIRM_WINDOW_SECONDS} seconds. This file is consumed on
use: a second rotation needs a second confirmation.

verdict: hold
EOF
  echo "REFUSED: rotation of $lane on $host needs confirmation." >&2
  echo "         Read and edit: $file" >&2
  return 1
}

gen() { openssl rand -hex 32; }

POSTGRES_PASSWORD=$(gen)
MINIO_ROOT_PASSWORD=$(gen)
NODES_HUMAN_DECISION_TOKEN_SECRET=$(gen)
NODES_CALLBACK_TOKEN_SECRET=$(gen)
NODES_RUNNER_SECRET_THOR=$(gen)
NODES_RUNNER_SECRET_ORIN=$(gen)

install_env() { # host, content-producing function
  local host=$1 content=$2 rc=0
  # A rotation of the live prod.env is the most destructive thing this script
  # can do, so it goes through the confirmation protocol before the ssh runs.
  if [ "${FORCE_PROD:-0}" = "1" ]; then
    require_destructive_confirmation "prod-env" "$host" \
"Rotates POSTGRES_PASSWORD, MINIO_ROOT_PASSWORD, NODES_HUMAN_DECISION_TOKEN_SECRET
and NODES_CALLBACK_TOKEN_SECRET on ${host}.

- PostgreSQL keeps the password from its initdb, so the new value will NOT
  authenticate until the role is altered to match: the api/worker/scheduler
  containers fail to connect on their next restart.
- Outstanding human-decision tokens and attempt callback tokens are
  invalidated; a bridge holding one mid-flight cannot complete its attempt." \
      || return 1
  fi
  # FORCE is evaluated locally and prefixed into the remote command —
  # ssh does not forward env vars, so a bare ${FORCE:-0} inside the
  # single-quoted remote script would always read 0 on the target.
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | ssh "$host" "FORCE=${FORCE_PROD:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/prod.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing prod.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/prod.env' || rc=$?
  # exit 3 is the keep-existing refusal — a re-run on a provisioned host
  # must continue to the later lanes (codex tokens), not abort here.
  if [ "$rc" -eq 3 ]; then echo "kept existing prod.env on $host"; return 0; fi
  [ "$rc" -eq 0 ] && echo "installed ~/.culture-nodes/prod.env on $host"
  return "$rc"
}

common="POSTGRES_USER=nodes
POSTGRES_DB=nodes
POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
MINIO_ROOT_USER=nodesroot
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
NODES_HUMAN_DECISION_TOKEN_SECRET=${NODES_HUMAN_DECISION_TOKEN_SECRET}
NODES_CALLBACK_TOKEN_SECRET=${NODES_CALLBACK_TOKEN_SECRET}
NODES_CALLBACK_BASE_URL=http://thor:18080"

install_env "$THOR" "$common
NODES_RUNNER_SECRET=${NODES_RUNNER_SECRET_THOR}"

install_env "$ORIN" "$common
NODES_RUNNER_SECRET=${NODES_RUNNER_SECRET_ORIN}"

# The runner bearer secrets also land as single-purpose files for
# NODES_RUNNER_SECRET_FILE and for the operator's registry entries on the
# control machine (mode 0600, outside the repo). Guarded like prod.env:
# a re-run keeps an existing runner.secret AND its local mirror in sync —
# rotating the remote file while the mirror kept the old value (or vice
# versa) would break the worker registry's secret_file references.
umask 077
mkdir -p "$HOME/.culture-nodes"
install_runner_secret() { # host, secret, mirror-suffix
  local host=$1 secret=$2 suffix=$3 rc=0
  printf '%s\n' "$secret" | ssh "$host" "FORCE=${FORCE_RUNNER:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/runner.secret ] && [ "$FORCE" != "1" ]; then echo "keeping existing runner.secret" >&2; exit 3; fi; cat > ~/.culture-nodes/runner.secret' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing runner.secret on $host (mirror untouched)"; return 0; fi
  [ "$rc" -eq 0 ] || return "$rc"
  printf '%s\n' "$secret" > "$HOME/.culture-nodes/runner-secret.$suffix"
  echo "runner bearer secret installed on $host and mirrored to ~/.culture-nodes/runner-secret.$suffix"
}
install_runner_secret "$THOR" "$NODES_RUNNER_SECRET_THOR" thor
install_runner_secret "$ORIN" "$NODES_RUNNER_SECRET_ORIN" orin

# --- codex-bridge tokens -------------------------------------------------
# Each host's codex-bridge adapter authenticates inbound requests with its
# own bearer token (~/.culture-nodes/codex-bridge.env,
# CODEX_BRIDGE_AUTH_TOKEN). Either worker may dispatch either host's codex
# actor over the LAN, so both prod.env files also carry *both* tokens as
# NODES_ACTOR_CODEX_THOR_TOKEN / NODES_ACTOR_CODEX_ORIN_TOKEN. Same
# discipline as everything above: tokens are generated locally and ride
# ssh stdin only — the remote command string ssh actually executes (its
# argv) never has a token substituted into it, only a fixed script that
# reads the secret material from its own stdin once it's running on the
# target.
CODEX_BRIDGE_TOKEN_THOR=$(openssl rand -base64 32)
CODEX_BRIDGE_TOKEN_ORIN=$(openssl rand -base64 32)

install_codex_bridge_env() { # host, token — this host's own bridge secret
  local host=$1 token=$2
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  local rc=0
  printf 'CODEX_BRIDGE_AUTH_TOKEN=%s\n' "$token" | ssh "$host" "FORCE=${FORCE_CODEX:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/codex-bridge.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing codex-bridge.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/codex-bridge.env' || rc=$?
  if [ "$rc" -eq 3 ]; then
    echo "kept existing codex-bridge.env on $host — NOT updating prod.env actor tokens with the new value" >&2
    return 3
  fi
  return "$rc"
}

update_actor_token_line() { # key, value — update-in-place or append into BOTH prod.envs
  local key=$1 value=$2 host
  for host in "$THOR" "$ORIN"; do
    # shellcheck disable=SC2029 # the remote path is deliberately remote
    printf '%s=%s\n' "$key" "$value" \
      | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; touch ~/.culture-nodes/prod.env; while IFS= read -r line; do key=${line%%=*}; [ -z "$key" ] && continue; if grep -q "^${key}=" ~/.culture-nodes/prod.env; then sed -i "s|^${key}=.*|${line}|" ~/.culture-nodes/prod.env; else printf "%s\n" "$line" >> ~/.culture-nodes/prod.env; fi; done'
    echo "installed $key into prod.env on $host"
  done
}

update_env_line_on_host() { # host, key, value — update-in-place or append into ONE prod.env
  local host=$1 key=$2 value=$3
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s=%s\n' "$key" "$value" \
    | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; touch ~/.culture-nodes/prod.env; while IFS= read -r line; do k=${line%%=*}; [ -z "$k" ] && continue; if grep -q "^${k}=" ~/.culture-nodes/prod.env; then sed -i "s|^${k}=.*|${line}|" ~/.culture-nodes/prod.env; else printf "%s\n" "$line" >> ~/.culture-nodes/prod.env; fi; done'
  echo "installed $key into prod.env on $host"
}

# A kept (pre-existing) bridge token must not have this run's freshly
# generated value pushed into prod.env — only a token that actually landed
# in a codex-bridge.env propagates, so bridge and workers always agree.
rc=0; install_codex_bridge_env "$THOR" "$CODEX_BRIDGE_TOKEN_THOR" || rc=$?
if [ "$rc" -eq 0 ]; then
  echo "installed ~/.culture-nodes/codex-bridge.env on $THOR"
  update_actor_token_line NODES_ACTOR_CODEX_THOR_TOKEN "$CODEX_BRIDGE_TOKEN_THOR"
elif [ "$rc" -ne 3 ]; then exit "$rc"; fi

rc=0; install_codex_bridge_env "$ORIN" "$CODEX_BRIDGE_TOKEN_ORIN" || rc=$?
if [ "$rc" -eq 0 ]; then
  echo "installed ~/.culture-nodes/codex-bridge.env on $ORIN"
  update_actor_token_line NODES_ACTOR_CODEX_ORIN_TOKEN "$CODEX_BRIDGE_TOKEN_ORIN"
elif [ "$rc" -ne 3 ]; then exit "$rc"; fi

# --- nodes-notifier webhook (thor only — deploy/prod/compose.thor.yml's
# `notifier` service is the only consumer; task t34) ----------------------
# A Discord (or generic) webhook URL is EXTERNALLY ISSUED (created in
# Discord's own UI, or whatever endpoint DISCORD_WEBHOOK_URL/
# CULTURE_NODES_WEBHOOK_URL names) — this script never invents one, unlike
# the openssl-generated secrets above. It only relays a value the operator
# already exported into THIS SCRIPT'S OWN environment before invoking
# install-secrets.sh (e.g. `CULTURE_NODES_WEBHOOK_URL=https://discord.com/
# api/webhooks/... ./install-secrets.sh`). Left unset, nodes-notifier still
# starts and runs — internal/notify.ResolveWebhook simply reports
# webhook_enabled=false and every lifecycle event is journaled as
# delivery-disabled rather than posted (fail-open, per internal/notify's
# own doc comment) — until a later re-run installs the URL.
if [ -n "${CULTURE_NODES_WEBHOOK_URL:-}" ]; then
  update_env_line_on_host "$THOR" CULTURE_NODES_WEBHOOK_URL "$CULTURE_NODES_WEBHOOK_URL"
elif [ -n "${DISCORD_WEBHOOK_URL:-}" ]; then
  update_env_line_on_host "$THOR" DISCORD_WEBHOOK_URL "$DISCORD_WEBHOOK_URL"
else
  echo "CULTURE_NODES_WEBHOOK_URL/DISCORD_WEBHOOK_URL not set in this script's own environment — skipping (nodes-notifier will run with webhook delivery disabled until this is installed)" >&2
fi

# --- human-inbox bridge + tracker secrets (thor only — one logical human
# actor, task t34) ----------------------------------------------------------
# HUMAN_INBOX_BRIDGE_AUTH_TOKEN is a bearer token generated locally exactly
# like the codex-bridge tokens above — nothing a human chooses, just a
# credential this script mints and installs, same FORCE=1 rotation guard.
# GITHUB_TOKEN is externally issued (a GitHub PAT/App token) and is never
# fabricated here: relayed only when the operator already exported it into
# this script's own environment. Left unset, deploy.sh still installs the
# tracker and it uses GitHub's anonymous public-repository lane.
HUMAN_INBOX_BRIDGE_AUTH_TOKEN=$(openssl rand -base64 32)

install_human_inbox_env() { # host
  local host=$1 rc=0
  local content="HUMAN_INBOX_BRIDGE_AUTH_TOKEN=${HUMAN_INBOX_BRIDGE_AUTH_TOKEN}"
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    content="${content}
GITHUB_TOKEN=${GITHUB_TOKEN}"
  fi
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | ssh "$host" "FORCE=${FORCE_HUMAN_INBOX:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/human-inbox.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing human-inbox.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/human-inbox.env' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing human-inbox.env on $host"; return 0; fi
  if [ "$rc" -eq 0 ]; then
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      echo "installed ~/.culture-nodes/human-inbox.env on $host (with GITHUB_TOKEN)"
    else
      echo "installed ~/.culture-nodes/human-inbox.env on $host (no GITHUB_TOKEN — tracker uses anonymous public-repository polling)"
    fi
  fi
  return "$rc"
}
install_human_inbox_env "$THOR"

# --- notify actor bridge bearer token (issue #68) -------------------------
#
# The notify bridge is a kind=agent actor the worker dispatches to, so the
# token has TWO custody points that must agree: the bridge reads it from
# ~/.culture-nodes/notify.env, and the worker reads the same value from
# prod.env under the name the actor row's metadata points at
# (NODES_ACTOR_NOTIFY_TOKEN -- internal/worker/registry.go's authTokenEnvOf).
# Both are written here, from one generated value, because a rotation that
# updated only one side would leave every notify dispatch failing
# authentication with nothing obviously wrong on either host.
#
# Same refuse-by-default posture as every other lane: an existing token is
# KEPT unless FORCE_NOTIFY=1, since re-minting it silently breaks dispatch.
NOTIFY_BRIDGE_AUTH_TOKEN=$(openssl rand -base64 32)

install_notify_env() { # host
  local host=$1 rc=0
  printf 'NOTIFY_BRIDGE_AUTH_TOKEN=%s\n' "$NOTIFY_BRIDGE_AUTH_TOKEN" \
    | ssh "$host" "FORCE=${FORCE_NOTIFY:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/notify.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing notify.env (set FORCE_NOTIFY=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/notify.env' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing notify.env on $host"; fi
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 3 ]; then return "$rc"; fi

  # Mirror whatever token the bridge ENDED UP with -- which is the existing
  # one when the guard above kept it, not the value just generated.
  ssh "$host" 'set -e
tok=$(grep "^NOTIFY_BRIDGE_AUTH_TOKEN=" ~/.culture-nodes/notify.env | cut -d= -f2-)
[ -n "$tok" ] || { echo "notify.env carries no NOTIFY_BRIDGE_AUTH_TOKEN" >&2; exit 1; }
touch ~/.culture-nodes/prod.env; chmod 600 ~/.culture-nodes/prod.env
python3 - "$tok" <<PY
import os, sys
path = os.path.expanduser("~/.culture-nodes/prod.env")
token = sys.argv[1]
line = "NODES_ACTOR_NOTIFY_TOKEN=" + token
lines = open(path).read().splitlines()
if any(l.startswith("NODES_ACTOR_NOTIFY_TOKEN=") for l in lines):
    if line in lines:
        print("control-plane copy of the notify token already matches")
    else:
        lines = [line if l.startswith("NODES_ACTOR_NOTIFY_TOKEN=") else l for l in lines]
        open(path, "w").write("\n".join(lines) + "\n")
        print("re-synced the control-plane copy of the notify token")
else:
    lines.append(line)
    open(path, "w").write("\n".join(lines) + "\n")
    print("installed the control-plane copy of the notify token")
PY'
  echo "notify bridge token in place on $host (bridge + control-plane copies agree)"
  return 0
}
install_notify_env "$THOR"
