# shellcheck shell=bash
# The land account's toolchain DETECTOR (loop-closure t7, spec c36, risk r2;
# issue #315). Sourced by deploy.sh, which calls land_toolchain_check once at
# the tail of the thor and orin arms, after `nodes doctor` and before the
# summary -- the same posture as audit-credentials.sh: a detector that speaks
# once the stack is up, never a gate that leaves it half-shipped.
#
# The land node's gate (examples/land/land_gate.py) runs the operator's
# pre-push chain AS culture-land on the runner host, and that chain needs four
# binaries on the account's PATH: go (go test ./tests/lint/...), uv (the target
# pytest and lint-all's linters), node and markdownlint-cli2 (lint-all root's
# markdown step). On 2026-09-07 `ssh thor 'command -v go'` found nothing (spec
# s26): the gate would refuse by name with a `toolchain_missing` record at the
# first landing, and until this lane existed nothing on the deploy side said
# so. Now the deploy asks the account, over the same ssh the other lanes use,
# and prints one line per binary -- present with its path, or MISSING -- plus
# one WARNING naming every missing binary. The check itself FAILS
# (returns 1) naming the missing binaries, but it never fails the DEPLOY:
# deploy.sh guards the call, because installing Go for culture-land is a
# counted hand-turn (CLAUDE.md) and a deploy cannot type it.
#
# The list is one variable so a test (tests/test_deploy_land_toolchain.py) can
# probe an arbitrary name, and so the four stay in one place beside
# land_gate.py's REQUIRED_TOOLCHAINS -- widen both or neither.
LAND_TOOLCHAIN_BINARIES=${LAND_TOOLCHAIN_BINARIES:-"go uv node markdownlint-cli2"}

# land_toolchain_check <host> -- one line per binary for culture-land@<host>,
# one WARNING for the missing set. Returns 1 when a binary is missing (the
# check fails, by name) and 0 otherwise -- including when the account is not
# bootstrapped yet, where there is nothing to measure, which is a skip and
# not a failure. deploy.sh calls it guarded: a detector reports, it does not
# abort.
land_toolchain_check() { # host
  local host=$1 target bin found missing=()
  target=$(unix_user_target "$host" land)
  if ! ssh -o BatchMode=yes -o ConnectTimeout=15 "$target" 'id -un' >/dev/null 2>&1; then
    say "land toolchain: culture-land on $host is not bootstrapped or not reachable as $target — the gate chain has no account to run in yet; skipping the toolchain check (bootstrap is the hand-turn: deploy/prod/bootstrap-accounts.sh $host)"
    return 0
  fi
  for bin in $LAND_TOOLCHAIN_BINARIES; do
    # `command -v` in the account's plain ssh shell: the PATH the runner's
    # operation inherits, which is the PATH the spec's probe measured.
    found=$(ssh "$target" "command -v $bin" 2>/dev/null | tr -d '\r' || true)
    if [ -n "$found" ]; then
      say "land toolchain: $bin present in $target ($found)"
    else
      say "land toolchain: $bin MISSING in $target"
      missing+=("$bin")
    fi
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    say "WARNING: the land node's gate cannot run on $host until ${missing[*]} is on culture-land's PATH — land_gate.py refuses by name (toolchain_missing record) before running any step; installing it for the account is a counted hand-turn, record it on the tracking issue"
    return 1
  else
    say "land toolchain: $LAND_TOOLCHAIN_BINARIES all present in $target — the gate chain can run"
  fi
  return 0
}
