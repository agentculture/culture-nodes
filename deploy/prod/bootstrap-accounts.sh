#!/usr/bin/env bash
# bootstrap-accounts.sh <spark|orin|thor> — the ONE root step of the
# agents-as-OS-users lane (#243), typed by the operator.
#
#   deploy/prod/bootstrap-accounts.sh spark    # culture-claude + culture-qwen
#   deploy/prod/bootstrap-accounts.sh orin     # culture-codex + culture-qwen + culture-pi
#   deploy/prod/bootstrap-accounts.sh thor     # culture-codex + culture-qwen + culture-pi
#                                              # (thor is NOPASSWD today, so
#                                              # deploy.sh thor can also do it)
#
# It exists because `sudo -n` wants a password on spark and orin, so
# deploy.sh cannot create the accounts unattended there; this wrapper runs
# the SAME root script the deploy lane runs (lanes/unix-user.sh, executed
# rather than sourced), so a hand-typed bootstrap and a scripted one cannot
# drift apart. Every step is a no-op when already done; it never touches
# /home/<login>/git; it asserts a 750 home and no sudo/docker membership.
#
# thor and orin carry three engines since #294 (codex qwen pi): the qwen and
# pi accounts are the harness-comparison lanes beside codex, each with its
# own pinned engine and its own copy of the login user's provider config
# (~/.qwen/settings.json, ~/.pi/agent/models.json). spark keeps claude +
# qwen: its culture-qwen and qwen-developer bridge stay as they are, and it
# gets a pi account only if the operator asks.
#
# Each invocation is an operator hand-turn: record it as a comment on the
# tracking issue (CLAUDE.md, "Every piece of operator work opens or updates
# an issue").
set -euo pipefail

HOST=${1:?usage: bootstrap-accounts.sh <spark|orin|thor>}
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LANE="$SCRIPT_DIR/lanes/unix-user.sh"
[ -f "$LANE" ] || { echo "error: $LANE missing" >&2; echo "hint: run from a checkout that carries deploy/prod/lanes/unix-user.sh" >&2; exit 2; }

case "$HOST" in
  spark) ENGINES="claude qwen" ;;
  orin|thor) ENGINES="codex qwen pi" ;;
  *) echo "error: unknown host $HOST" >&2; echo "hint: one of spark, orin, thor" >&2; exit 1 ;;
esac

# `nodes doctor` BEFORE the root step (#249 review, finding 8): the four
# agent-identity checks, non-strict, so only the error-severity one (the
# prompt file the account's bridge will load) refuses -- the other three
# print as warnings and the bootstrap goes on. Locally it runs in the
# checkout this script lives in; over ssh it is best effort in the host's
# shipped archive (~/culture-nodes-prod): a host with no archive yet, or no
# uv, is a WARNING, an unreachable host or a doctor that ran and failed is
# a refusal, and nothing is bootstrapped after a refusal.
doctor_local() {
  local checkout
  checkout=$(cd "$SCRIPT_DIR/../.." && pwd)
  if ! command -v uv >/dev/null 2>&1; then
    printf '==> %s\n' "WARNING: no uv on PATH — skipping nodes doctor in $checkout (best effort)"
    return 0
  fi
  printf '==> %s\n' "nodes doctor in $checkout (non-strict: the error-severity check refuses, the rest warn)"
  (cd "$checkout" && uv run nodes doctor) || {
    echo "error: nodes doctor reports the agent identity unhealthy in $checkout (its error-severity check failed)" >&2
    echo "hint: fix the reported check (uv run nodes doctor), then re-run; nothing was bootstrapped" >&2
    exit 1
  }
}

doctor_remote() { # host
  local host=$1 rc=0
  printf '==> %s\n' "nodes doctor on $host in ~/culture-nodes-prod (best effort, over ssh)"
  ssh "$host" "bash -lc 'if [ -d ~/culture-nodes-prod ]; then cd ~/culture-nodes-prod && uv run nodes doctor; else echo \"no ~/culture-nodes-prod on $host (never deployed) — nothing to doctor\"; exit 42; fi'" || rc=$?
  case "$rc" in
    0) ;;
    42|127) printf '==> %s\n' "WARNING: nodes doctor could not run on $host (no shipped checkout, or no uv in a login shell; exit $rc) — bootstrapping anyway; deploy.sh $host doctors the account after" ;;
    255) echo "error: cannot reach $host (ssh exit 255)" >&2; echo "hint: fix ssh to $host, then re-run" >&2; exit 1 ;;
    *)
      echo "error: nodes doctor reports the agent identity unhealthy on $host (exit $rc; its error-severity check failed)" >&2
      echo "hint: fix the reported check on $host (cd ~/culture-nodes-prod && uv run nodes doctor), then re-run; nothing was bootstrapped" >&2
      exit 1 ;;
  esac
}

# The operator's machine is spark, which accepts no ssh key for its own
# user (actor-placement.sh has the same local branch): run the root script
# in place there, over ssh with a tty everywhere else.
case "$(hostname)" in
  "$HOST"|"$HOST"-*|"$HOST".*)
    doctor_local
    printf '==> %s\n' "bootstrapping $ENGINES on $HOST (local sudo)"
    # shellcheck disable=SC2086
    exec sudo bash "$LANE" bootstrap $ENGINES ;;
esac

doctor_remote "$HOST"
STAGED=".culture-nodes-unix-user.sh"
printf '==> %s\n' "staging the lane on $HOST as ~/$STAGED"
scp -q "$LANE" "$HOST:$STAGED"
printf '==> %s\n' "bootstrapping $ENGINES on $HOST (sudo will ask for your password there)"
exec ssh -t "$HOST" "sudo bash ~/$STAGED bootstrap $ENGINES"
