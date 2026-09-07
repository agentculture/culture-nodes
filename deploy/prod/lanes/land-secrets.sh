# shellcheck shell=bash
# The culture-land credential lane of install-secrets.sh (loop-closure t5,
# spec c13/c38, issue #315), in its own file because install-secrets.sh sits
# at 999 lines -- one under the hard limit tests/lint/filelength_test.go
# enforces -- and must not grow (tests/test_deploy_cutover.py pins the count).
#
# It is SOURCED, not executed: it defines install_land_account_env and nothing
# else. install-secrets.sh sources it beside the other lanes and calls it once
# for the runner host; cutover.sh sources it directly for `cutover.sh <host>
# land` (no region to lift, unlike the QWEN_PI_ACCOUNT_ENV lane).
#
# --- what the land account holds, and why two files ------------------------
#
# The land node is deterministic code the runner executes AS culture-land: it
# fetches a handover ref, rebases it onto the PR branch tip, runs the gate
# chain, pushes to the PR branch, replies on the review thread and resolves
# it. Two GitHub operations, two fine-grained tokens, two files:
#
#   ~/.culture-nodes/bridge-push.env   GITHUB_TOKEN_WORKER   Contents: write
#   ~/.culture-nodes/land-pr.env       GITHUB_TOKEN_LAND_PR  Pull requests: write
#
# bridge-push.env is the SAME seam every engine account already carries (#90,
# install_account_push_env): the push credential, Contents-only, so a token
# that leaks from a checkout can move refs and nothing else. land-pr.env is
# NEW with this lane and deliberately separate: replying on a thread and
# resolving it needs pull-requests:write, and a token with BOTH scopes is
# technically able to call the merge API. Keeping the scopes in two files
# means the push step never holds a token that could merge, and the reply
# step never holds one that could push. The land node still never merges a
# PR -- that boundary is enforced by the node (no merge call) and audited by
# the spec's honesty condition, not by token scope; human-merges-pr stays the
# only merge path. deploy/prod/README.md ("The culture-land account") records
# both scopes.
#
# Both tokens are externally issued and RELAYED, never minted (nothing here
# generates random material -- the same rule install_bridge_push_env states):
# the value rides ssh stdin into the account and the remote command string
# names only the file.
# Each file is written under umask 077 and chmod 600, the inventory guard in
# lanes/unix-user.sh admits exactly these two names, and an account that does
# not open with the operator key is skipped by name -- the root bootstrap is
# the hand-turn (deploy/prod/bootstrap-accounts.sh thor) this lane never
# performs.
#
# A token that is NOT set in the caller's environment is a skip with a
# printed reason, not an error: install-secrets.sh runs whole against both
# hosts and an operator rotating the Jira pair should not need the land
# tokens exported to do it. cutover.sh, whose one job is to bring the land
# account online, turns that skip into a refusal by name before calling here
# (tests/deploy/landcutover_test.go).

# install_land_account_env <host> -- both credential files into
# culture-land@<host>. Returns 0 on skip-by-name (unbootstrapped account or
# an unset token), non-zero only when a relay that was attempted failed.
install_land_account_env() { # host
  local host=$1 target rc=0 delivered=0
  target=$(unix_user_target "$host" land)
  if ! ssh -o BatchMode=yes -o ConnectTimeout=15 "$target" 'id -un' >/dev/null 2>&1; then
    echo "culture-land on $host is not bootstrapped or not reachable as $target — skipping its bridge-push.env and land-pr.env (the root bootstrap is a hand-turn: deploy/prod/bootstrap-accounts.sh $host; then re-run this script)" >&2
    return 0
  fi
  # The Contents-write push credential, the shared #90 seam.
  if [ -n "${GITHUB_TOKEN_WORKER:-}" ]; then
    printf 'GITHUB_TOKEN_WORKER=%s\n' "$GITHUB_TOKEN_WORKER" \
      | ssh "$target" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/bridge-push.env; chmod 600 ~/.culture-nodes/bridge-push.env' || rc=$?
    [ "$rc" -eq 0 ] && { echo "installed mode-600 ~/.culture-nodes/bridge-push.env in $target (Contents: write)"; delivered=$((delivered + 1)); }
  else
    echo "GITHUB_TOKEN_WORKER not set in this script's own environment — skipping culture-land's bridge-push.env on $host (the land node cannot push without it)" >&2
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  # The pull-requests:write reply credential, separate on purpose (see above).
  if [ -n "${GITHUB_TOKEN_LAND_PR:-}" ]; then
    printf 'GITHUB_TOKEN_LAND_PR=%s\n' "$GITHUB_TOKEN_LAND_PR" \
      | ssh "$target" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/land-pr.env; chmod 600 ~/.culture-nodes/land-pr.env' || rc=$?
    [ "$rc" -eq 0 ] && { echo "installed mode-600 ~/.culture-nodes/land-pr.env in $target (Pull requests: write; never merges — human-merges-pr is the only merge path)"; delivered=$((delivered + 1)); }
  else
    echo "GITHUB_TOKEN_LAND_PR not set in this script's own environment — skipping culture-land's land-pr.env on $host (the land node cannot reply on a thread without it)" >&2
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  [ "$delivered" -eq 2 ] || echo "culture-land on $host holds $delivered of 2 credential files after this run; export both GITHUB_TOKEN_WORKER and GITHUB_TOKEN_LAND_PR and re-run to complete it" >&2
  return 0
}
