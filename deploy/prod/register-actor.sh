#!/usr/bin/env bash
# register-actor.sh — idempotent actor registration for the thor+orin pair
# (plan t6, codex-bridges-thor-orin).
#
# Actor rows are append-only (migrations/0001_namespaces_and_identity.sql:
# "a new capability or endpoint change is a new row (revision), never an
# update to an existing one"), so this script only ever reads the newest
# revision and, when the desired state genuinely differs, inserts one more.
# It never touches an existing row.
#
#   register-actor.sh <actor_key> <endpoint_url> [auth_token_env]
#
# Each input can also arrive as an env var (ACTOR_KEY, ENDPOINT_URL,
# AUTH_TOKEN_ENV) so the script composes into other automation without
# positional argv. auth_token_env is the NAME of an environment variable
# the worker reads its credential from at dispatch time
# (internal/worker/registry.go's authTokenEnvOf) -- this script only ever
# handles that name, never the token value itself.
#
# Reaching Postgres: by default this runs the same
# `docker compose ... exec -T postgres psql -U nodes -d nodes` invocation
# deploy.sh uses on thor. Set PSQL_CMD to override the whole command (a
# test points it at a fake psql executable instead).
#
# Namespace scoping: resolves the namespace id the same way deploy.sh does
# (the oldest namespaces row) unless NODES_NAMESPACE_ID is already set.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

usage() {
  cat >&2 <<'EOF'
usage: register-actor.sh <actor_key> <endpoint_url> [auth_token_env]

  actor_key       e.g. company/codex-thor              (env: ACTOR_KEY)
  endpoint_url    must have a numeric IPv4 host, e.g.
                  http://192.168.1.5:17070              (env: ENDPOINT_URL)
  auth_token_env  name of the env var holding the credential -- never the
                  credential itself                      (env: AUTH_TOKEN_ENV)

Env overrides:
  PSQL_CMD           full command used to reach Postgres (default: the
                      thor compose-exec psql invocation from deploy.sh)
  NODES_NAMESPACE_ID skip namespace lookup and use this namespace id
EOF
}

ACTOR_KEY=${1:-${ACTOR_KEY:-}}
ENDPOINT_URL=${2:-${ENDPOINT_URL:-}}
AUTH_TOKEN_ENV=${3:-${AUTH_TOKEN_ENV:-}}

if [ -z "$ACTOR_KEY" ] || [ -z "$ENDPOINT_URL" ]; then
  usage
  exit 1
fi

# --- Input validation (strict allowlists) --------------------------------
#
# These values are interpolated into SQL below, so each is confined to a
# character class that cannot contain a quote, backslash, or statement
# metacharacter -- allowlist validation is the shell-native equivalent of
# parameterization here, and it doubles as a schema sanity check (PR #20
# review). Refusals happen before any Postgres access.
if [[ ! "$ACTOR_KEY" =~ ^[a-z0-9][a-z0-9._/-]*$ ]]; then
  echo "register-actor: refusing actor key '$ACTOR_KEY': keys are lowercase [a-z0-9._/-] paths like company/codex-thor" >&2
  exit 1
fi
if [ -n "$AUTH_TOKEN_ENV" ] && [[ ! "$AUTH_TOKEN_ENV" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  echo "register-actor: refusing auth token env name '$AUTH_TOKEN_ENV': must be a valid environment variable name" >&2
  exit 1
fi
if [[ ! "$ENDPOINT_URL" =~ ^https?://[A-Za-z0-9:/._-]+$ ]]; then
  echo "register-actor: refusing endpoint '$ENDPOINT_URL': must be an explicit http:// or https:// URL (a scheme-less endpoint would be persisted and then fail when the worker builds requests from it)" >&2
  exit 1
fi

# --- IP-only refusal --------------------------------------------------
#
# Worker containers do not inherit the host's /etc/hosts (deploy/prod's
# README notes deploy.sh has to resolve THOR_IP explicitly for exactly this
# reason), so an actor endpoint that names a LAN hostname would resolve
# nowhere from inside the worker container. Refuse anything whose host is
# not a plain numeric IPv4 address, before any Postgres access happens.
without_scheme=${ENDPOINT_URL#*://}
host_port=${without_scheme%%/*}
host=${host_port%%:*}

octet='(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])'
ipv4_regex="^${octet}\\.${octet}\\.${octet}\\.${octet}\$"

if [[ ! "$host" =~ $ipv4_regex ]]; then
  echo "register-actor: refusing endpoint '$ENDPOINT_URL': host '$host' is not a numeric IPv4 address -- worker containers cannot resolve LAN hostnames, so an actor endpoint must be a plain IPv4 address" >&2
  exit 1
fi

# --- Reaching Postgres --------------------------------------------------
PROD_ENV_FILE=${PROD_ENV_FILE:-${HOME:-}/.culture-nodes/prod.env}
DEFAULT_PSQL_CMD="docker compose --env-file $PROD_ENV_FILE -f $SCRIPT_DIR/compose.thor.yml exec -T postgres psql -U nodes -d nodes"
PSQL_CMD=${PSQL_CMD:-$DEFAULT_PSQL_CMD}

run_psql() {
  # Word-splitting PSQL_CMD is intentional: it turns the configured command
  # string (the docker compose invocation above, or a test's fake psql
  # path) into argv.
  # shellcheck disable=SC2206
  local cmd_arr=($PSQL_CMD)
  "${cmd_arr[@]}" -Atc "$1"
}

# --- Namespace scoping ---------------------------------------------------
NAMESPACE_ID=${NODES_NAMESPACE_ID:-}
if [ -z "$NAMESPACE_ID" ]; then
  NAMESPACE_ID=$(run_psql "SELECT id FROM namespaces ORDER BY created_at LIMIT 1")
fi
if [ -z "$NAMESPACE_ID" ]; then
  echo "register-actor: no namespace row found (seed a namespace first, or set NODES_NAMESPACE_ID)" >&2
  exit 1
fi
# Same allowlist rationale as above -- this value is interpolated into SQL,
# whether it came from the environment or the namespace lookup.
if [[ ! "$NAMESPACE_ID" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "register-actor: refusing namespace id '$NAMESPACE_ID': not a plain identifier" >&2
  exit 1
fi

# --- Read the newest revision --------------------------------------------
current=$(run_psql "SELECT revision, endpoint_ref, metadata->>'auth_token_env' FROM actors WHERE namespace_id = '$NAMESPACE_ID' AND actor_key = '$ACTOR_KEY' ORDER BY revision DESC LIMIT 1")

current_revision=""
current_endpoint=""
current_auth_env=""
if [ -n "$current" ]; then
  IFS='|' read -r current_revision current_endpoint current_auth_env <<< "$current"
fi

# --- Idempotent no-op -----------------------------------------------------
if [ -n "$current_revision" ] && [ "$current_endpoint" = "$ENDPOINT_URL" ] && [ "$current_auth_env" = "$AUTH_TOKEN_ENV" ]; then
  echo "register-actor: unchanged (revision $current_revision)"
  exit 0
fi

# --- New revision -----------------------------------------------------
next_revision=$(( ${current_revision:-0} + 1 ))
actor_id="actor_register_$(date +%s%N)_$$"
metadata_json=$(printf '{"auth_token_env": "%s"}' "$AUTH_TOKEN_ENV")

run_psql "INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata) VALUES ('$actor_id', '$NAMESPACE_ID', '$ACTOR_KEY', $next_revision, 'agent', 'http', '$ENDPOINT_URL', '$metadata_json'::jsonb)" >/dev/null

echo "register-actor: registered $ACTOR_KEY at revision $next_revision ($ENDPOINT_URL)"
