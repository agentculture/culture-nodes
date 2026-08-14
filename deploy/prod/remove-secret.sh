#!/usr/bin/env bash
# remove-secret.sh -- delete ONE named key from ~/.culture-nodes/prod.env on
# one or more hosts. The deliberate counterweight to install-secrets.sh's
# merge (task t11, issue #69 item 1, spec claim c58).
#
# Why this exists. install-secrets.sh used to write prod.env wholesale, so an
# authorized rotation silently deleted every key it did not generate — the
# incident that cost NODES_ACTOR_CLAUDE_TOKEN and ~18 hours of 401s. The fix
# is to merge key by key. But merge-only means nothing in the deploy lane can
# ever remove a line again, and a credential file that can only grow is its
# own problem: a rotated-away actor token, a webhook that moved, a key
# installed under a typo'd name all stay behind, and the next reader cannot
# tell a live credential from a dead one. So removal becomes an explicit,
# named act rather than a side effect of an unrelated rotation.
#
# Usage:
#   remove-secret.sh <KEY> [--yes] [host ...]
#
#   remove-secret.sh NODES_ACTOR_CLAUDE_TOKEN thor          # dry run
#   remove-secret.sh NODES_ACTOR_CLAUDE_TOKEN --yes thor    # actually remove
#   ENV_FILE=codex-bridge.env remove-secret.sh CODEX_BRIDGE_AUTH_TOKEN --yes orin
#
# Hosts default to thor and orin. ENV_FILE defaults to prod.env and names a
# file inside ~/.culture-nodes on the target.
#
# Two guards, sized to what this can actually destroy:
#
#   1. DRY RUN BY DEFAULT. It prints the line it would remove with the value
#      redacted — enough to confirm the right key on the right host, never
#      enough to leak the credential into a terminal scrollback or a CI log —
#      and does nothing until --yes.
#   2. The key name is validated against [A-Za-z_][A-Za-z0-9_]* before it is
#      used. The key (never a value) is interpolated into the remote command
#      and into a sed address, so a metacharacter would let one named key
#      delete lines nobody named — the same class of over-deletion this whole
#      change exists to stop.
#
# It does NOT use install-secrets.sh's windowed confirmation protocol. That
# protocol exists for a rotation that destroys values the operator never
# enumerated; here the operator names the exact key and is shown the exact
# line first. There is no backup file on purpose: a `.bak` beside prod.env
# would be a second, unmanaged copy of live credentials at rest.
set -euo pipefail

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

APPLY=0
KEY=""
KEY_GIVEN=0
HOSTS=()
for arg in "$@"; do
  case "$arg" in
    --yes|-y) APPLY=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *)
      # KEY_GIVEN, not [ -z "$KEY" ]: an empty first argument would otherwise
      # leave the slot open and promote the next positional — the host — into
      # it, and `remove-secret.sh "" thor` would go looking for a key named
      # thor instead of refusing.
      if [ "$KEY_GIVEN" = "0" ]; then KEY=$arg; KEY_GIVEN=1; else HOSTS+=("$arg"); fi
      ;;
  esac
done

if [ "$KEY_GIVEN" = "0" ]; then
  echo "error: no key named" >&2
  echo "usage: remove-secret.sh <KEY> [--yes] [host ...]" >&2
  exit 2
fi
if ! printf '%s' "$KEY" | grep -qE '^[A-Za-z_][A-Za-z0-9_]*$'; then
  echo "refusing to remove '$KEY': a key name must match [A-Za-z_][A-Za-z0-9_]*" >&2
  echo "         (this script removes ONE named key, never a pattern — a regex here would delete lines nobody named)" >&2
  exit 2
fi

ENV_FILE=${ENV_FILE:-prod.env}
if ! printf '%s' "$ENV_FILE" | grep -qE '^[A-Za-z0-9_][A-Za-z0-9_.-]*$'; then
  echo "refusing ENV_FILE='$ENV_FILE': it names one file inside ~/.culture-nodes, not a path" >&2
  exit 2
fi

if [ ${#HOSTS[@]} -eq 0 ]; then
  HOSTS=(thor orin)
fi

status=0
for host in "${HOSTS[@]}"; do
  echo "== $host"
  # KEY/APPLY/ENV_FILE are evaluated locally and prefixed into the remote
  # command — ssh forwards no environment. None of the three is a secret:
  # this lane passes key NAMES through argv and reads values only to redact
  # them, which is what keeps install-secrets.sh's stdin-only discipline
  # (secret VALUES never reach an argv) intact here.
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  ssh "$host" "KEY=$KEY APPLY=$APPLY ENV_FILE=$ENV_FILE; "'
f=~/.culture-nodes/$ENV_FILE
if [ ! -e "$f" ]; then echo "no ~/.culture-nodes/$ENV_FILE on this host — nothing to remove"; exit 0; fi
n=$(grep -c "^${KEY}=" "$f" || true)
if [ "$n" = "0" ]; then echo "$KEY is not present in $ENV_FILE — nothing to remove"; exit 0; fi
grep "^${KEY}=" "$f" | sed "s/=.*/=<redacted>/"
if [ "$APPLY" != "1" ]; then
  echo "dry run: $n line(s) would be removed from $ENV_FILE — re-run with --yes to remove them"
  exit 0
fi
sed -i "/^${KEY}=/d" "$f"
chmod 600 "$f"
echo "removed $n line(s) of $KEY from $ENV_FILE"
' || status=$?
done

if [ "$status" -ne 0 ]; then exit "$status"; fi

if [ "$APPLY" != "1" ]; then
  echo
  echo "nothing was changed (dry run). Re-run with --yes once the lines above are the ones you meant."
else
  echo
  echo "Restart whatever reads $ENV_FILE — a container or unit already running still holds the removed value in memory."
fi
