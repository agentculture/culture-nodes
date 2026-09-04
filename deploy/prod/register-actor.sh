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
#   register-actor.sh <actor_key> <endpoint_url> [auth_token_env] \
#                     [--metadata KEY=VALUE]... [--os-user NAME]
#   register-actor.sh --engine <actor_id>
#   register-actor.sh --human <actor_key> [--metadata KEY=VALUE]...
#
# Each input can also arrive as an env var (ACTOR_KEY, ENDPOINT_URL,
# AUTH_TOKEN_ENV) so the script composes into other automation without
# positional argv. auth_token_env is the NAME of an environment variable
# the worker reads its credential from at dispatch time
# (internal/worker/registry.go's authTokenEnvOf) -- this script only ever
# handles that name, never the token value itself.
#
# `--metadata KEY=VALUE` (repeatable) carries the OTHER per-actor deployment
# facts the registry holds: `handover_remote`, which scripts/collect-handover.py
# reads to learn where an actor's git remote is, and the repository identity a
# dispatch resolves its checkout from. Both are facts about the deployment, not
# about the graph or the agent -- which is why they live here.
#
# `--os-user NAME` is sugar for `--metadata os_user=NAME`: it is a first-class
# metadata key (issue #204) that records the dedicated Unix account a bridge
# actually runs as (e.g. `culture-codex`, `culture-claude`, `culture-qwen`),
# so the registry can be read as a lane tag -- which actor ran under which
# account -- without inferring it from the endpoint or actor key. The name is
# validated against `^[a-z_][a-z0-9_-]*$`, the same shape `useradd` accepts
# for a login name, and refused otherwise before any Postgres access.
#
# METADATA IS MERGED, NEVER REPLACED, and that is load-bearing. Every
# registration writes a NEW ROW, so a revision built from a hardcoded metadata
# object silently drops every key it does not know about. Once an actor carries
# `handover_remote`, a later endpoint change written that way would erase it and
# handover collection would start failing with nothing to point at. The insert
# below therefore carries the previous revision's metadata forward with `||` and
# overlays only the keys this invocation was actually given. For the same
# reason it carries `kind` and `protocol` forward too, instead of re-asserting
# 'agent'/'http' -- that hardcoding could not register a human or runner actor
# at all, and would have silently rewritten one into an agent.
#
# Reaching Postgres: by default this runs the same
# `docker compose ... exec -T postgres psql -U nodes -d nodes` invocation
# deploy.sh uses on thor. Set PSQL_CMD to override the whole command (a
# test points it at a fake psql executable instead).
#
# Namespace scoping: resolves the oldest namespace through the control-plane
# API, creating the default namespace when the installation has none, unless
# NODES_NAMESPACE_ID is already set.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

usage() {
  cat >&2 <<'EOF'
usage: register-actor.sh <actor_key> <endpoint_url> [auth_token_env] \
                        [--metadata KEY=VALUE]... [--os-user NAME]
       register-actor.sh --engine <actor_id>
       register-actor.sh --human <actor_key> [--metadata KEY=VALUE]...

  actor_key       e.g. company/codex-thor              (env: ACTOR_KEY)
  endpoint_url    must have a numeric IPv4 host, e.g.
                  http://192.168.1.5:17070              (env: ENDPOINT_URL)
  auth_token_env  name of the env var holding the credential -- never the
                  credential itself                      (env: AUTH_TOKEN_ENV)
  --metadata      KEY=VALUE, repeatable. Merged over the previous revision's
                  metadata; keys not named here are carried forward unchanged.
                  harness, model, and model_endpoint are the comparison tags
                  for harness-comparison lanes.
                  e.g. --metadata handover_remote=ssh://thor/~/git/culture-nodes-agent
  --os-user       NAME, sugar for --metadata os_user=NAME. Records the
                  dedicated Unix account a bridge runs as (culture-codex,
                  culture-claude, culture-qwen), so the registry can be read
                  as a lane tag (#204). NAME must match ^[a-z_][a-z0-9_-]*$.
  --engine        register an in-process engine producer with no endpoint;
                  the actor id and actor key are both <actor_id>.
  --human         register a PERSON as a kind=human actor with no endpoint
                  (login-from-anywhere t13). A person is reached through the
                  Access-protected page, never dispatched to, so the row
                  carries protocol 'none' and a NULL endpoint. Bind the
                  person's SSO subject to it with scripts/bind-identity.sh.

Env overrides:
  PSQL_CMD           full command used to reach Postgres (default: the
                      thor compose-exec psql invocation from deploy.sh)
  NODES_API_URL      control-plane base URL (default: http://thor:18080)
  NODES_NAMESPACE_ID skip namespace lookup and use this namespace id
EOF
}

# --- Argument parsing -----------------------------------------------------
#
# Flags are separated from positionals first so `--metadata` may appear
# anywhere, including before the actor key.
METADATA_KEYS=()
METADATA_VALUES=()
POSITIONAL=()
ENGINE_ACTOR=""
HUMAN_ACTOR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --metadata)
      [ $# -ge 2 ] || { echo "register-actor: --metadata needs a KEY=VALUE argument" >&2; exit 1; }
      pair=$2
      case "$pair" in
        *=*) ;;
        *) echo "register-actor: refusing metadata '$pair': expected KEY=VALUE" >&2; exit 1 ;;
      esac
      METADATA_KEYS+=("${pair%%=*}")
      METADATA_VALUES+=("${pair#*=}")
      shift 2
      ;;
    --metadata=*)
      pair=${1#--metadata=}
      case "$pair" in
        *=*) ;;
        *) echo "register-actor: refusing metadata '$pair': expected KEY=VALUE" >&2; exit 1 ;;
      esac
      METADATA_KEYS+=("${pair%%=*}")
      METADATA_VALUES+=("${pair#*=}")
      shift
      ;;
    --os-user)
      [ $# -ge 2 ] || { echo "register-actor: --os-user needs a NAME argument" >&2; exit 1; }
      os_user=$2
      if [[ ! "$os_user" =~ ^[a-z_][a-z0-9_-]*$ ]]; then
        echo "register-actor: refusing os-user '$os_user': must match ^[a-z_][a-z0-9_-]*\$" >&2
        echo "hint: pass the dedicated Unix account name, e.g. culture-codex, culture-claude, or culture-qwen" >&2
        exit 1
      fi
      METADATA_KEYS+=("os_user")
      METADATA_VALUES+=("$os_user")
      shift 2
      ;;
    --os-user=*)
      os_user=${1#--os-user=}
      if [[ ! "$os_user" =~ ^[a-z_][a-z0-9_-]*$ ]]; then
        echo "register-actor: refusing os-user '$os_user': must match ^[a-z_][a-z0-9_-]*\$" >&2
        echo "hint: pass the dedicated Unix account name, e.g. culture-codex, culture-claude, or culture-qwen" >&2
        exit 1
      fi
      METADATA_KEYS+=("os_user")
      METADATA_VALUES+=("$os_user")
      shift
      ;;
    --engine)
      [ $# -ge 2 ] || { echo "register-actor: --engine needs an actor id" >&2; exit 1; }
      ENGINE_ACTOR=$2
      shift 2
      ;;
    --human)
      [ $# -ge 2 ] || { echo "register-actor: --human needs an actor key" >&2; exit 1; }
      HUMAN_ACTOR=$2
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    --) shift; while [ $# -gt 0 ]; do POSITIONAL+=("$1"); shift; done ;;
    -*) echo "register-actor: unknown flag '$1'" >&2; usage; exit 1 ;;
    *) POSITIONAL+=("$1"); shift ;;
  esac
done

ACTOR_KEY=${POSITIONAL[0]:-${ACTOR_KEY:-}}
ENDPOINT_URL=${POSITIONAL[1]:-${ENDPOINT_URL:-}}
AUTH_TOKEN_ENV=${POSITIONAL[2]:-${AUTH_TOKEN_ENV:-}}

if [ -n "$ENGINE_ACTOR" ] && [ -n "$HUMAN_ACTOR" ]; then
  echo "register-actor: --engine and --human are mutually exclusive" >&2
  exit 1
fi
if [ -n "$ENGINE_ACTOR" ]; then
  [ ${#POSITIONAL[@]} -eq 0 ] || { echo "register-actor: --engine does not accept endpoint arguments" >&2; exit 1; }
  ACTOR_KEY=$ENGINE_ACTOR
  ENDPOINT_URL=""
  AUTH_TOKEN_ENV=""
fi
if [ -n "$HUMAN_ACTOR" ]; then
  [ ${#POSITIONAL[@]} -eq 0 ] || { echo "register-actor: --human does not accept endpoint arguments (a person has no endpoint)" >&2; exit 1; }
  ACTOR_KEY=$HUMAN_ACTOR
  ENDPOINT_URL=""
  AUTH_TOKEN_ENV=""
fi
# NO_ENDPOINT covers both endpoint-less shapes so the endpoint checks below
# read as one condition rather than two.
NO_ENDPOINT=""
if [ -n "$ENGINE_ACTOR" ] || [ -n "$HUMAN_ACTOR" ]; then NO_ENDPOINT=1; fi

if [ -z "$ACTOR_KEY" ] || { [ -z "$NO_ENDPOINT" ] && [ -z "$ENDPOINT_URL" ]; }; then
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

# Metadata keys and values are interpolated into a JSON literal which is then
# interpolated into SQL, so both are confined to character classes that can
# contain neither a JSON metacharacter (quote, backslash) nor a SQL one. That
# is the same shell-native parameterization the checks above use, applied one
# layer deeper because there are two nested quoting contexts here rather than
# one. A value that needs a quote is a value this script should refuse rather
# than escape.
for i in "${!METADATA_KEYS[@]}"; do
  key=${METADATA_KEYS[$i]}
  value=${METADATA_VALUES[$i]}
  if [[ ! "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
    echo "register-actor: refusing metadata key '$key': must be a plain identifier" >&2
    exit 1
  fi
  if [ -z "$value" ]; then
    echo "register-actor: refusing empty metadata value for '$key': an empty string is a value a reader could mistake for a configured one -- omit the key instead" >&2
    exit 1
  fi
  if [[ ! "$value" =~ ^[A-Za-z0-9:/@._~-]+$ ]]; then
    echo "register-actor: refusing metadata value for '$key': only [A-Za-z0-9:/@._~-] is allowed (got '$value')" >&2
    exit 1
  fi
done
if [ -z "$NO_ENDPOINT" ] && [[ ! "$ENDPOINT_URL" =~ ^https?://[A-Za-z0-9:/._-]+$ ]]; then
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

if [ -z "$NO_ENDPOINT" ] && [[ ! "$host" =~ $ipv4_regex ]]; then
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
  NODES_API_URL=${NODES_API_URL:-http://thor:18080}
  namespaces_url="${NODES_API_URL%/}/v1alpha1/namespaces"
  namespace_rows=$(curl -fsS "$namespaces_url") || {
    echo "register-actor: no namespace row found (seed a namespace first, or set NODES_NAMESPACE_ID)" >&2
    exit 1
  }
  NAMESPACE_ID=$(python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0]["id"] if rows else "")' <<<"$namespace_rows")
  if [ -z "$NAMESPACE_ID" ]; then
    namespace_row=$(curl -fsS -X POST -H 'Content-Type: application/json' \
      --data '{"name":"Default","slug":"default"}' "$namespaces_url") || {
      echo "register-actor: no namespace row found (seed a namespace first, or set NODES_NAMESPACE_ID)" >&2
      exit 1
    }
    NAMESPACE_ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("id", ""))' <<<"$namespace_row")
  fi
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

# --- The overlay this invocation asks for ---------------------------------
#
# Only keys actually supplied appear here. An absent auth_token_env is NOT
# written as an empty string: that would overwrite a previously registered
# credential name with a value the worker cannot resolve.
overlay_pairs=()
if [ -n "$AUTH_TOKEN_ENV" ]; then
  overlay_pairs+=("\"auth_token_env\": \"$AUTH_TOKEN_ENV\"")
fi
for i in "${!METADATA_KEYS[@]}"; do
  overlay_pairs+=("\"${METADATA_KEYS[$i]}\": \"${METADATA_VALUES[$i]}\"")
done
overlay_json="{$(IFS=,; echo "${overlay_pairs[*]}")}"

# --- Read the newest revision --------------------------------------------
#
# The comparison reads the metadata this invocation would OVERLAY, rendered
# from the stored row the same way the overlay is rendered above, so a
# metadata-only change is visible to the idempotency check. Comparing only
# endpoint and auth_token_env would report "unchanged" for a registration whose
# whole purpose was to add handover_remote.
current=$(run_psql "SELECT revision, endpoint_ref, coalesce((metadata || '$overlay_json'::jsonb) = metadata, false) FROM actors WHERE namespace_id = '$NAMESPACE_ID' AND actor_key = '$ACTOR_KEY' ORDER BY revision DESC LIMIT 1")

current_revision=""
current_endpoint=""
overlay_is_noop=""
if [ -n "$current" ]; then
  IFS='|' read -r current_revision current_endpoint overlay_is_noop <<< "$current"
fi

# --- Idempotent no-op -----------------------------------------------------
if [ -n "$current_revision" ] && [ "$current_endpoint" = "$ENDPOINT_URL" ] && [ "$overlay_is_noop" = "t" ]; then
  echo "register-actor: unchanged (revision $current_revision)"
  exit 0
fi

# --- New revision -----------------------------------------------------
next_revision=$(( ${current_revision:-0} + 1 ))
actor_id="actor_register_$(date +%s%N)_$$"

if [ -n "$current_revision" ]; then
  # Carry the previous revision's metadata, kind and protocol forward and
  # overlay only what was asked for. INSERT ... SELECT does the merge inside
  # Postgres so the stored JSON never round-trips through the shell -- which
  # also means no stored value can be re-interpolated into this statement.
  run_psql "INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata) SELECT '$actor_id', '$NAMESPACE_ID', '$ACTOR_KEY', $next_revision, kind, protocol, nullif('$ENDPOINT_URL', ''), metadata || '$overlay_json'::jsonb FROM actors WHERE namespace_id = '$NAMESPACE_ID' AND actor_key = '$ACTOR_KEY' ORDER BY revision DESC LIMIT 1" >/dev/null
else
  # First revision: there is nothing to carry forward, so the kind/protocol
  # defaults apply. An actor that is not an http agent is registered by
  # amending its first revision, not by guessing here.
  if [ -n "$ENGINE_ACTOR" ]; then
    actor_id=$ENGINE_ACTOR
    run_psql "INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata) VALUES ('$actor_id', '$NAMESPACE_ID', '$ACTOR_KEY', $next_revision, 'engine', 'internal', NULL, '$overlay_json'::jsonb)" >/dev/null
  elif [ -n "$HUMAN_ACTOR" ]; then
    run_psql "INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata) VALUES ('$actor_id', '$NAMESPACE_ID', '$ACTOR_KEY', $next_revision, 'human', 'none', NULL, '$overlay_json'::jsonb)" >/dev/null
  else
    run_psql "INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, metadata) VALUES ('$actor_id', '$NAMESPACE_ID', '$ACTOR_KEY', $next_revision, 'agent', 'http', '$ENDPOINT_URL', '$overlay_json'::jsonb)" >/dev/null
  fi
fi

echo "register-actor: registered $ACTOR_KEY at revision $next_revision ($ENDPOINT_URL)"
