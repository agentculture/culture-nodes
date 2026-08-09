#!/usr/bin/env bash
# Generate the production secret set once and install it on thor and orin
# over ssh stdin — no secret ever appears in an argv, a shell history
# line, or this repo (plan t19; credential pattern per spec assumption c8,
# cited from reachy-mini-cli's argv-only ssh discipline).
#
# Usage: install-secrets.sh [thor-host] [orin-host]
# Idempotent per machine: refuses to overwrite an existing prod.env unless
# FORCE=1, so a re-deploy never silently rotates a live database password.
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
  local host=$1 content=$2
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/prod.env ] && [ "${FORCE:-0}" != "1" ]; then echo "refusing to overwrite existing prod.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/prod.env'
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
# control machine (mode 0600, outside the repo).
umask 077
mkdir -p "$HOME/.culture-nodes"
printf '%s\n' "$NODES_RUNNER_SECRET_THOR" > "$HOME/.culture-nodes/runner-secret.thor"
printf '%s\n' "$NODES_RUNNER_SECRET_ORIN" > "$HOME/.culture-nodes/runner-secret.orin"
printf '%s\n' "$NODES_RUNNER_SECRET_THOR" | ssh "$THOR" 'umask 077; cat > ~/.culture-nodes/runner.secret'
printf '%s\n' "$NODES_RUNNER_SECRET_ORIN" | ssh "$ORIN" 'umask 077; cat > ~/.culture-nodes/runner.secret'
echo "runner bearer secrets installed (thor, orin) and mirrored to ~/.culture-nodes/ here"
