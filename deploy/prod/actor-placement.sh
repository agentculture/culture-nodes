#!/usr/bin/env bash
# actor-placement.sh -- WHICH HOST serves an actor, and the refusal that
# keeps a deployment from answering that question two different ways.
# Sourced by deploy.sh and install-secrets.sh; never executed directly.
#
# Why this exists (issue #72). The human-inbox bridge and its merge tracker
# were deployed "thor only" by a comment in three files, while
# company/human-ops was registered at a different machine's address. The
# engine dispatches an actor's work to its REGISTERED ENDPOINT, so human
# tasks parked on the bridge at that endpoint while thor's tracker watched
# thor's own empty state directory and logged pending=0 for as long as anyone
# left it running. Nothing was broken in a way anything reported: two config
# values that had to agree were agreeing only by luck, and then stopped.
#
# The fix is not a second, better-maintained host name. It is having only one
# value: an actor's registration says where it is served, so the deploy reads
# it rather than declaring it. A hardcoded name can drift from the registry;
# the registry cannot drift from itself.
#
# This is the deploy-time half of the invariant. The runtime half is task
# t8's: the tracker refuses to run when its bridge is not the actor's bridge
# (adapters/human-inbox/src/human_inbox_bridge/tracker.py,
# verify_bridge_serves_actor).
#
# The two halves no longer read the same surface, and that is a known,
# temporary asymmetry rather than the original bug returning. Task t7
# converted the runtime half off addresses entirely (issue #121): it proves
# co-location from the bridge's own store identity and reads
# GET /v1alpha1/dial-in-presence, which is keyed by actor_key and holds no
# address. This script still needs one, because "which host do I ssh to" is
# a question presence cannot answer. Migration 0036 drops endpoint_ref, so
# before it is applied this lookup must move to a deployment fact on
# actor.metadata -- the shape scripts/collect-handover.py's handover_remote
# already uses. See docs/decisions/transport-inversion.md.

# The control plane's public read surface. GET /v1alpha1/actors is
# unauthenticated by design (spec decision c45: only registration and human
# decisions are gated), which is why resolving placement needs no credential
# and can run from install-secrets.sh before any deploy has happened.
NODES_API_URL=${NODES_API_URL:-http://thor:18080}

# actor_registration <actor_key>
#
# Echoes "id|revision|endpoint_ref|auth_token_env" for the actor's NEWEST
# REVISION, or fails (non-zero, no output) when the actor is unregistered or
# the registry cannot be read.
#
# Newest revision, not any matching row: actor identity is append-only
# (migrations/0001_namespaces_and_identity.sql -- "an endpoint change is a new
# row, never an update"), so an actor that MOVED has both its old and new
# address in the list. Picking the wrong one is precisely the split this
# library exists to prevent.
#
# Failure is never a fallback. A caller that cannot resolve an actor must
# install nothing: a pair deployed to a guessed host is the defect, whereas a
# pair not deployed at all is a reported gap.
actor_registration() { # actor_key
  local actor_key=$1 body=""

  body=$(curl -fsS --max-time "${NODES_API_TIMEOUT_SECONDS:-10}" \
    "${NODES_API_URL%/}/v1alpha1/actors" 2>/dev/null) || return 1

  ACTOR_PLACEMENT_KEY="$actor_key" ACTOR_PLACEMENT_BODY="$body" python3 -c '
import json, os, sys

key = os.environ["ACTOR_PLACEMENT_KEY"]
try:
    payload = json.loads(os.environ["ACTOR_PLACEMENT_BODY"])
except ValueError:
    sys.exit(1)
items = payload.get("items") if isinstance(payload, dict) else None
if not isinstance(items, list):
    sys.exit(1)
rows = [
    row
    for row in items
    if isinstance(row, dict)
    and row.get("actor_key") == key
    and isinstance(row.get("revision"), int)
    and not isinstance(row.get("revision"), bool)
]
if not rows:
    sys.exit(1)
row = max(rows, key=lambda candidate: candidate["revision"])
metadata = row.get("metadata") if isinstance(row.get("metadata"), dict) else {}
print("|".join([
    str(row.get("id") or ""),
    str(row["revision"]),
    str(row.get("endpoint_ref") or ""),
    str(metadata.get("auth_token_env") or ""),
]))
' || return 1
}

# endpoint_address <endpoint_ref> -- the host part of a registered endpoint.
# register-actor.sh refuses anything but a numeric IPv4 host (worker
# containers do not inherit the host's /etc/hosts), so this is always a plain
# address rather than a name needing resolution.
endpoint_address() { # endpoint_ref
  local rest=${1#*://}
  local host_port=${rest%%/*}
  printf '%s' "${host_port%%:*}"
}

# endpoint_port <endpoint_ref> -- the port part, or a failure.
#
# A missing port is a failure rather than a default: the bridge binds the port
# the engine dispatches to, so inventing one here would re-create the very
# thing this file removes -- a second value that has to agree with the
# registration by luck.
endpoint_port() { # endpoint_ref
  local rest=${1#*://}
  local host_port=${rest%%/*}
  case "$host_port" in
    *:*) printf '%s' "${host_port##*:}" ;;
    *) return 1 ;;
  esac
}

# local_addresses -- every address THIS machine answers on.
local_addresses() {
  {
    ip -4 -o addr show 2>/dev/null | tr -s ' ' | cut -d' ' -f4 | cut -d/ -f1
    hostname -I 2>/dev/null | tr ' ' '\n'
  } | tr -d '\r' | grep -v '^$' | sort -u
}

# address_is_local <address> -- whether this machine is the one at <address>.
address_is_local() { # address
  local address=$1
  [ -n "$address" ] || return 1
  case "$address" in
    127.*|localhost|::1) return 0 ;;
  esac
  local_addresses | grep -Fxq "$address"
}

# actor_host_exec <host> <command>
#
# Runs <command> on <host>, over ssh -- except when <host> is an address this
# machine itself answers on, in which case it runs here.
#
# That local branch is not a convenience. The deploy runs from the operator's
# own machine, and an actor registered at that machine's address is a normal
# arrangement (it is the live one for company/human-ops). ssh'ing to yourself
# is not: the operator's host may accept no key for its own user, so a
# derived-host lane that could only reach hosts over ssh would resolve the
# right machine and then fail to deploy to it. Stdin passes through both
# branches identically, which is what keeps the "secrets ride stdin, never
# argv" discipline intact either way.
# The local branch starts in $HOME on purpose: a command sent over ssh runs
# from the remote user's home directory, so every relative path in this
# repo's remote scripts (culture-nodes-prod/..., ~/.culture-nodes/...) is
# written against $HOME. Running the same string from whatever directory the
# operator happened to invoke deploy.sh in would quietly resolve those paths
# somewhere else -- including inside this checkout.
actor_host_exec() { # host command
  local host=$1 command=$2
  if address_is_local "$host"; then
    (cd "$HOME" && bash -c "$command")
  else
    ssh "$host" "$command"
  fi
}

# host_addresses <host> -- the addresses <host> reports for itself.
host_addresses() { # host
  actor_host_exec "$1" \
    'ip -4 -o addr show 2>/dev/null | tr -s " " | cut -d" " -f4 | cut -d/ -f1; hostname -I 2>/dev/null | tr " " "\n"' \
    2>/dev/null | tr -d '\r' | tr -s ' \t' '\n' | grep -v '^$' | sort -u
}

# host_owns_address <host> <address> -- does <host> actually answer on
# <address>? Asked OF THE HOST rather than assumed from its name, because the
# name is exactly what was wrong before.
host_owns_address() { # host address
  host_addresses "$1" | grep -Fxq "$2"
}

# _placement_env_value <file-text> <KEY> -- the value of KEY=... in an
# env-file body, last assignment wins, surrounding quotes stripped.
_placement_env_value() { # text key
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | tail -n 1 | sed "s/^['\"]//; s/['\"]\$//"
}

# _placement_refuse <message> -- fail the deploy, loudly, naming both sides.
_placement_refuse() { # message
  printf 'SPLIT DEPLOYMENT REFUSED: %s\n' "$1" >&2
  exit 1
}

# assert_human_inbox_colocated <host> <actor_key> <endpoint_ref>
#
# Run after the env files are written and BEFORE either systemd unit is
# installed. Reads back what was actually written on the host -- not what the
# caller meant to write -- and refuses the deploy unless the bridge and the
# tracker are one pair, on the actor's own host.
#
# Each check below is a way this deployment has split or could split:
#
#   1. The pair is going somewhere the actor is not served. The engine
#      dispatches to the registered endpoint; a bridge anywhere else never
#      receives the work, and the tracker beside it watches an empty store.
#   2. The bridge binds a port other than the registered one. Same failure,
#      one field over -- and this was live: the registration said :8090 while
#      the deploy wrote :8087.
#   3. The tracker submits to some other bridge. Its submissions then bypass
#      the idempotency store that would have deduplicated them.
#   4. The tracker reads a different state directory. It is a filesystem
#      reader; a second directory is a second inbox, permanently empty.
#   5/6. The bridge's actor id and the tracker's are the same variable name
#      with two different required values: the bridge stamps origin.actor_id
#      on a ledger record whose FOREIGN KEY points at actors(id), while the
#      tracker resolves its copy as an actor_KEY against the registry. Swap
#      them and either every terminal commit rolls back on a constraint
#      nothing names, or the tracker refuses to start.
#   7. The tracker has no control plane to check itself against, which
#      disables task t8's startup refusal -- the runtime half of this
#      invariant. Installing that is installing a pair nothing guards.
assert_human_inbox_colocated() { # host actor_key endpoint_ref
  local host=$1 actor_key=$2 endpoint=$3
  local address port bridge_env tracker_env
  local bridge_port bridge_state bridge_actor
  local tracker_url tracker_port tracker_state tracker_actor tracker_control

  address=$(endpoint_address "$endpoint")
  port=$(endpoint_port "$endpoint") || port=""
  if [ -z "$address" ] || [ -z "$port" ]; then
    _placement_refuse "actor '$actor_key' is registered at '$endpoint', which has no host:port to check a deployment against. An endpoint that cannot be checked has not been checked -- register the actor with an explicit http://<ipv4>:<port> endpoint_ref (deploy/prod/register-actor.sh) before deploying its bridge"
  fi

  if ! host_owns_address "$host" "$address"; then
    _placement_refuse "about to install the human-inbox bridge and tracker on '$host', but actor '$actor_key' is registered at $endpoint and '$host' does not answer on $address. It answers on: $(host_addresses "$host" | tr '\n' ' '). The engine dispatches this actor's work to $address, so a bridge on '$host' would never receive it and the tracker beside it would watch an empty store"
  fi

  bridge_env=$(actor_host_exec "$host" 'cat ~/.culture-nodes/human-inbox-bridge.env 2>/dev/null') || bridge_env=""
  tracker_env=$(actor_host_exec "$host" 'cat ~/.culture-nodes/human-inbox-tracker.env 2>/dev/null') || tracker_env=""
  if [ -z "$bridge_env" ] || [ -z "$tracker_env" ]; then
    _placement_refuse "one of ~/.culture-nodes/human-inbox-bridge.env and ~/.culture-nodes/human-inbox-tracker.env is missing or empty on '$host'; the pair's configuration cannot be compared, and an unverified pairing is not a verified one"
  fi

  bridge_port=$(_placement_env_value "$bridge_env" HUMAN_INBOX_BRIDGE_PORT)
  bridge_state=$(_placement_env_value "$bridge_env" HUMAN_INBOX_BRIDGE_STATE_DIR)
  bridge_actor=$(_placement_env_value "$bridge_env" HUMAN_INBOX_BRIDGE_ACTOR_ID)
  tracker_url=$(_placement_env_value "$tracker_env" HUMAN_INBOX_TRACKER_BRIDGE_URL)
  tracker_state=$(_placement_env_value "$tracker_env" HUMAN_INBOX_TRACKER_STATE_DIR)
  tracker_actor=$(_placement_env_value "$tracker_env" HUMAN_INBOX_BRIDGE_ACTOR_ID)
  tracker_control=$(_placement_env_value "$tracker_env" HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL)
  tracker_port=$(endpoint_port "$tracker_url") || tracker_port=""

  if [ "$bridge_port" != "$port" ]; then
    _placement_refuse "the bridge on '$host' is configured to bind port '$bridge_port', but actor '$actor_key' is registered at $endpoint -- the engine dispatches to port $port, so a bridge on $bridge_port answers nothing and the tracker watching it sees nothing"
  fi

  if [ "$tracker_port" != "$bridge_port" ]; then
    _placement_refuse "the tracker submits to '$tracker_url' (port '$tracker_port') while the bridge serving actor '$actor_key' binds port '$bridge_port'. The bridge's idempotency store is per-process and file-based, so submissions through a second bridge are deduplicated by nothing"
  fi

  if [ -z "$bridge_state" ] || [ "$bridge_state" != "$tracker_state" ]; then
    _placement_refuse "the bridge parks tasks in '$bridge_state' while the tracker watches '$tracker_state'. The tracker is a filesystem reader: a second directory is a second inbox, and it stays empty while real work waits in the first"
  fi

  if [ -z "$tracker_actor" ] || [ "$tracker_actor" != "$actor_key" ]; then
    _placement_refuse "the tracker's HUMAN_INBOX_BRIDGE_ACTOR_ID is '$tracker_actor', but it resolves that value as an actor_KEY against the control plane's actor list -- it must be '$actor_key'. A row id here makes the startup identity check refuse every single start"
  fi

  if [ -z "$bridge_actor" ] || [ "$bridge_actor" = "$actor_key" ]; then
    _placement_refuse "the bridge's HUMAN_INBOX_BRIDGE_ACTOR_ID is '$bridge_actor', which is the actor_key '$actor_key' rather than a registered actors(id) row id. The bridge stamps this value as origin.actor_id on every proposed ledger claim, and ledger_records.origin_actor_id is a FOREIGN KEY into actors(id): the actor would do its real work, answer correctly, and every terminal commit would roll back"
  fi

  if [ -z "$tracker_control" ]; then
    _placement_refuse "the tracker carries no HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL, which disables its startup identity check (task t8). That check is the half of this invariant that runs after the deploy is over; without it, a later split runs silently exactly as issue #72 did"
  fi

  printf 'human-inbox pairing verified: bridge and tracker on %s, serving %s at %s (state %s)\n' \
    "$host" "$actor_key" "$endpoint" "$bridge_state"
}
