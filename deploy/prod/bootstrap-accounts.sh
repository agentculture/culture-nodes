#!/usr/bin/env bash
# bootstrap-accounts.sh <spark|orin|thor> — the ONE root step of the
# agents-as-OS-users lane (#243), typed by the operator.
#
#   deploy/prod/bootstrap-accounts.sh spark    # culture-claude + culture-qwen
#   deploy/prod/bootstrap-accounts.sh orin     # culture-codex
#   deploy/prod/bootstrap-accounts.sh thor     # culture-codex (NOPASSWD today,
#                                              # so deploy.sh thor can also do it)
#
# It exists because `sudo -n` wants a password on spark and orin, so
# deploy.sh cannot create the accounts unattended there; this wrapper runs
# the SAME root script the deploy lane runs (lanes/unix-user.sh, executed
# rather than sourced), so a hand-typed bootstrap and a scripted one cannot
# drift apart. Every step is a no-op when already done; it never touches
# /home/<login>/git; it asserts a 750 home and no sudo/docker membership.
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
  orin|thor) ENGINES="codex" ;;
  *) echo "error: unknown host $HOST" >&2; echo "hint: one of spark, orin, thor" >&2; exit 1 ;;
esac

# The operator's machine is spark, which accepts no ssh key for its own
# user (actor-placement.sh has the same local branch): run the root script
# in place there, over ssh with a tty everywhere else.
case "$(hostname)" in
  "$HOST"|"$HOST"-*|"$HOST".*)
    printf '==> %s\n' "bootstrapping $ENGINES on $HOST (local sudo)"
    # shellcheck disable=SC2086
    exec sudo bash "$LANE" bootstrap $ENGINES ;;
esac

STAGED=".culture-nodes-unix-user.sh"
printf '==> %s\n' "staging the lane on $HOST as ~/$STAGED"
scp -q "$LANE" "$HOST:$STAGED"
printf '==> %s\n' "bootstrapping $ENGINES on $HOST (sudo will ask for your password there)"
exec ssh -t "$HOST" "sudo bash ~/$STAGED bootstrap $ENGINES"
