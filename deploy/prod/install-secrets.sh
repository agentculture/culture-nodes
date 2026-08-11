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

gen() { openssl rand -hex 32; }

POSTGRES_PASSWORD=$(gen)
MINIO_ROOT_PASSWORD=$(gen)
NODES_HUMAN_DECISION_TOKEN_SECRET=$(gen)
NODES_CALLBACK_TOKEN_SECRET=$(gen)
NODES_RUNNER_SECRET_THOR=$(gen)
NODES_RUNNER_SECRET_ORIN=$(gen)

install_env() { # host, content-producing function
  local host=$1 content=$2 rc=0
  # FORCE is evaluated locally and prefixed into the remote command —
  # ssh does not forward env vars, so a bare ${FORCE:-0} inside the
  # single-quoted remote script would always read 0 on the target.
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | ssh "$host" "FORCE=${FORCE:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/prod.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing prod.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/prod.env' || rc=$?
  # exit 3 is the keep-existing refusal — a re-run on a provisioned host
  # must continue to the later lanes (codex tokens), not abort here.
  if [ "$rc" -eq 3 ]; then echo "kept existing prod.env on $host"; return 0; fi
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
echo "installed ~/.culture-nodes/prod.env on $THOR"

install_env "$ORIN" "$common
NODES_RUNNER_SECRET=${NODES_RUNNER_SECRET_ORIN}"
echo "installed ~/.culture-nodes/prod.env on $ORIN"

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
  printf '%s\n' "$secret" | ssh "$host" "FORCE=${FORCE:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/runner.secret ] && [ "$FORCE" != "1" ]; then echo "keeping existing runner.secret" >&2; exit 3; fi; cat > ~/.culture-nodes/runner.secret' || rc=$?
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
  printf 'CODEX_BRIDGE_AUTH_TOKEN=%s\n' "$token" | ssh "$host" "FORCE=${FORCE:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/codex-bridge.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing codex-bridge.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/codex-bridge.env' || rc=$?
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
