#!/usr/bin/env bash
# bind-identity.sh — bind, revoke and list the SSO/service-principal
# identities that let a person (or a service token) act as a registered
# actor (login-from-anywhere task t13; spec c37, c38, c46).
#
#   bind-identity.sh bind   --provider cloudflare-access --subject <sub> \
#                           --actor-key company/alice --roles approver[,viewer]
#   bind-identity.sh revoke --identity <identity-id>
#   bind-identity.sh list   [--actor-key company/alice] [--all]
#
# The table it writes is migrations/0053_actor_identities.sql. Two of that
# migration's rules shape everything below and are enforced here BEFORE any
# Postgres access, so a typo is refused by name rather than by a constraint
# error out of psql:
#
#   - provider is one of 'cloudflare-access' (a person; subject is the `sub`
#     claim of the Access JWT, i.e. the Cloudflare user id) or
#     'cloudflare-service-token' (subject is the token's common name).
#     Never an email: c37 keys bindings by (provider, subject) because bot
#     and service identities may carry fake emails.
#   - roles is a subset of {viewer, approver, namespace_administrator} —
#     the closed vocabulary internal/auth/roles.go parses.
#
# A binding is append-only history, the same way actor rows are. `revoke`
# stamps revoked_at and nothing else; there is no delete, and re-binding the
# same subject after a revoke appends a NEW row (the live-key unique index
# only spans rows with revoked_at IS NULL).
#
# Reaching Postgres: the same shape as deploy/prod/register-actor.sh — a
# PSQL_CMD (default: the thor compose-exec psql invocation, reading
# PROD_ENV_FILE) invoked as `$PSQL_CMD -Atc "<query>"`, and the namespace
# from NODES_NAMESPACE_ID, --namespace, or the control plane's own namespace
# list. Values are interpolated into SQL, so every one is confined to a
# character class that cannot carry a quote or a statement metacharacter
# (the shell-native equivalent of parameterisation register-actor.sh
# already uses). This script prints names and ids and never a credential:
# it never sees one — an Access JWT lives in the browser and a service
# token's secret lives with Cloudflare.
#
# Exit codes follow the repo's policy: 0 ok, 1 user error (bad role,
# unknown provider, unregistered actor, no live binding to revoke),
# 2 environment error (Postgres unreachable, no namespace).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

usage() {
  cat >&2 <<'EOF'
usage: bind-identity.sh bind   --provider <provider> --subject <subject> \
                              --actor-key <actor_key> --roles <role>[,<role>...]
       bind-identity.sh revoke --identity <identity-id>
       bind-identity.sh list   [--actor-key <actor_key>] [--all]

  --provider    cloudflare-access (a person; subject = the JWT `sub` claim,
                the Cloudflare user id shown by GET /v1alpha1/whoami) or
                cloudflare-service-token (subject = the token's common name)
  --subject     the provider's stable subject; never an email address
  --actor-key   a registered actor key, e.g. company/alice; the binding
                points at that key's newest revision
  --roles       comma-separated subset of viewer, approver,
                namespace_administrator (at least one)
  --identity    the id printed by `bind` or `list`
  --all         list revoked bindings too (default: live only)
  --namespace   namespace id (env: NODES_NAMESPACE_ID; default: the control
                plane's first namespace via NODES_API_URL)

Env overrides:
  PSQL_CMD            full command used to reach Postgres (default: the thor
                       compose-exec psql invocation from deploy.sh)
  PROD_ENV_FILE       compose env file for the default PSQL_CMD
  NODES_API_URL       control-plane base URL (default: http://thor:18080)
  NODES_NAMESPACE_ID  skip the namespace lookup and use this id
EOF
}

die() { echo "bind-identity: $1" >&2; exit "${2:-1}"; }

VERB=${1:-}
case "$VERB" in
  bind|revoke|list) shift ;;
  -h|--help|"") usage; exit 1 ;;
  *) die "unknown verb '$VERB' (expected bind, revoke or list)" ;;
esac

PROVIDER=""
SUBJECT=""
ACTOR_KEY=""
ROLES=""
IDENTITY_ID=""
NAMESPACE_ID=${NODES_NAMESPACE_ID:-}
LIST_ALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --provider)   [ $# -ge 2 ] || die "--provider needs a value";  PROVIDER=$2;     shift 2 ;;
    --subject)    [ $# -ge 2 ] || die "--subject needs a value";   SUBJECT=$2;      shift 2 ;;
    --actor-key)  [ $# -ge 2 ] || die "--actor-key needs a value"; ACTOR_KEY=$2;    shift 2 ;;
    --roles)      [ $# -ge 2 ] || die "--roles needs a value";     ROLES=$2;        shift 2 ;;
    --identity)   [ $# -ge 2 ] || die "--identity needs a value";  IDENTITY_ID=$2;  shift 2 ;;
    --namespace)  [ $# -ge 2 ] || die "--namespace needs a value"; NAMESPACE_ID=$2; shift 2 ;;
    --all) LIST_ALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1'" ;;
  esac
done

# --- Input validation (strict allowlists, before any Postgres access) -----

validate_provider() {
  case "$1" in
    cloudflare-access|cloudflare-service-token) : ;;
    *) die "unknown provider '$1': expected cloudflare-access or cloudflare-service-token (migrations/0053_actor_identities.sql)" ;;
  esac
}

validate_roles() {
  [ -n "$1" ] || die "--roles is required: at least one of viewer, approver, namespace_administrator"
  local role
  IFS=',' read -r -a ROLE_LIST <<< "$1"
  for role in "${ROLE_LIST[@]}"; do
    case "$role" in
      viewer|approver|namespace_administrator) : ;;
      *) die "unknown role '$role': expected viewer, approver or namespace_administrator (internal/auth/roles.go)" ;;
    esac
  done
}

validate_actor_key() {
  if [[ ! "$1" =~ ^[a-z0-9][a-z0-9._/-]*$ ]]; then
    die "refusing actor key '$1': keys are lowercase [a-z0-9._/-] paths like company/alice"
  fi
}

validate_subject() {
  [ -n "$1" ] || die "--subject is required"
  if [[ ! "$1" =~ ^[A-Za-z0-9._:@+-]+$ ]]; then
    die "refusing subject '$1': only [A-Za-z0-9._:@+-] is allowed"
  fi
  case "$1" in
    *@*.*) echo "bind-identity: warning: subject '$1' looks like an email address; the Cloudflare subject is the JWT sub claim (a user id), not the login email (spec c37)" >&2 ;;
  esac
}

validate_identity_id() {
  [ -n "$1" ] || die "--identity is required"
  if [[ ! "$1" =~ ^[A-Za-z0-9_-]+$ ]]; then
    die "refusing identity id '$1': not a plain identifier"
  fi
}

case "$VERB" in
  bind)
    [ -n "$PROVIDER" ] || die "--provider is required"
    validate_provider "$PROVIDER"
    validate_roles "$ROLES"
    [ -n "$ACTOR_KEY" ] || die "--actor-key is required"
    validate_actor_key "$ACTOR_KEY"
    validate_subject "$SUBJECT"
    ;;
  revoke)
    validate_identity_id "$IDENTITY_ID"
    ;;
  list)
    [ -z "$ACTOR_KEY" ] || validate_actor_key "$ACTOR_KEY"
    ;;
esac

# --- Reaching Postgres --------------------------------------------------
PROD_ENV_FILE=${PROD_ENV_FILE:-${HOME:-}/.culture-nodes/prod.env}
DEFAULT_PSQL_CMD="docker compose --env-file $PROD_ENV_FILE -f $REPO_ROOT/deploy/prod/compose.thor.yml exec -T postgres psql -U nodes -d nodes"
PSQL_CMD=${PSQL_CMD:-$DEFAULT_PSQL_CMD}

run_psql() {
  # Word-splitting PSQL_CMD is intentional (see register-actor.sh).
  # shellcheck disable=SC2206
  local cmd_arr=($PSQL_CMD)
  "${cmd_arr[@]}" -Atc "$1"
}

# --- Namespace scoping ---------------------------------------------------
if [ -z "$NAMESPACE_ID" ]; then
  NODES_API_URL=${NODES_API_URL:-http://thor:18080}
  namespaces_url="${NODES_API_URL%/}/v1alpha1/namespaces"
  namespace_rows=$(curl -fsS "$namespaces_url") || die "no namespace row found (set NODES_NAMESPACE_ID or --namespace)" 2
  NAMESPACE_ID=$(python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0]["id"] if rows else "")' <<<"$namespace_rows")
fi
[ -n "$NAMESPACE_ID" ] || die "no namespace row found (set NODES_NAMESPACE_ID or --namespace)" 2
if [[ ! "$NAMESPACE_ID" =~ ^[A-Za-z0-9_-]+$ ]]; then
  die "refusing namespace id '$NAMESPACE_ID': not a plain identifier"
fi

# --- Verbs ---------------------------------------------------------------
case "$VERB" in
  bind)
    actor_id=$(run_psql "SELECT id FROM actors WHERE namespace_id = '$NAMESPACE_ID' AND actor_key = '$ACTOR_KEY' ORDER BY revision DESC LIMIT 1")
    [ -n "$actor_id" ] || die "no actor registered under key '$ACTOR_KEY' in namespace $NAMESPACE_ID (register one first: deploy/prod/register-actor.sh --human $ACTOR_KEY)"
    if [[ ! "$actor_id" =~ ^[A-Za-z0-9_-]+$ ]]; then
      die "refusing actor id '$actor_id' read from the registry: not a plain identifier" 2
    fi
    roles_sql=""
    for role in "${ROLE_LIST[@]}"; do
      roles_sql="${roles_sql:+$roles_sql,}'$role'"
    done
    identity_id="identity_$(date +%s%N)_$$"
    inserted=$(run_psql "INSERT INTO actor_identities (id, namespace_id, provider, subject, actor_id, roles) VALUES ('$identity_id', '$NAMESPACE_ID', '$PROVIDER', '$SUBJECT', '$actor_id', ARRAY[$roles_sql]::TEXT[]) RETURNING id") || die "bind refused by Postgres (a live binding for $PROVIDER/$SUBJECT may already exist: list it, revoke it, then bind again)" 2
    [ -n "$inserted" ] || die "bind returned no row" 2
    echo "bind-identity: bound $PROVIDER/$SUBJECT to $ACTOR_KEY (actor $actor_id, identity $identity_id, roles $ROLES)"
    ;;
  revoke)
    revoked=$(run_psql "UPDATE actor_identities SET revoked_at = now() WHERE id = '$IDENTITY_ID' AND namespace_id = '$NAMESPACE_ID' AND revoked_at IS NULL RETURNING id")
    [ -n "$revoked" ] || die "no live binding with id '$IDENTITY_ID' in namespace $NAMESPACE_ID (already revoked, or a different namespace)"
    echo "bind-identity: revoked identity $IDENTITY_ID"
    ;;
  list)
    where="i.namespace_id = '$NAMESPACE_ID'"
    [ "$LIST_ALL" -eq 1 ] || where="$where AND i.revoked_at IS NULL"
    [ -z "$ACTOR_KEY" ] || where="$where AND a.actor_key = '$ACTOR_KEY'"
    printf 'id|provider|subject|actor_key|actor_id|roles|created_at|revoked_at\n'
    run_psql "SELECT i.id, i.provider, i.subject, a.actor_key, i.actor_id, array_to_string(i.roles, ','), i.created_at, coalesce(i.revoked_at::text, '') FROM actor_identities i JOIN actors a ON a.id = i.actor_id WHERE $where ORDER BY i.created_at"
    ;;
esac
