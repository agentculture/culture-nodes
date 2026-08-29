# PREFLIGHT_START -- tests/test_deploy_two_host.py drives this block and the
# TWO_HOST_LANE block below against fake hosts; keep the marker on the first
# statement of the block and its mate after the last.
#
# The two production hosts, named once. The thor lane's migrate + cutover
# window has to quiesce orin's worker too, and the orin lane's parity check
# reads thor's api and resumes thor's scheduler — so each lane needs the
# OTHER host's name. SKIP_ORIN_QUIESCE=1 declares that there IS no second
# worker (a single-host deployment); it is not a way around an unreachable
# orin, which the probe below refuses.
# The host this lane targets is that host by definition (`deploy.sh thor.lan`
# must not then ssh to a bare `thor`); the other one is named by default.
if [[ "$HOST" == thor* ]]; then
  THOR_HOST=${THOR_HOST:-$HOST}
elif [[ "$HOST" == orin* ]]; then
  ORIN_HOST=${ORIN_HOST:-$HOST}
fi
THOR_HOST=${THOR_HOST:-thor}
ORIN_HOST=${ORIN_HOST:-orin}
#
# Everything in this block is READ-ONLY on the target, and it runs before the
# archive ship, before the image build, and (in the thor lane) before any
# `docker compose stop`. Until task t2 the only hard check on the agent
# checkout was `nodes doctor` at the END of the lane (after migrate, cutover
# and stack-up), and the checkout lane's own fast-forward refusal was a
# warning — so a detached or dirty ~/git/culture-nodes-agent surfaced only
# after thor had already been modified. A deploy that is going to be refused
# must be refused while there is still nothing to undo.
#
# Two states are refused here and both are OPERATOR states, not bugs: a dirty
# tree (a write session's diff waiting to be harvested) and a detached HEAD
# (the hand-modified prod checkouts spec q3 discards by hand). Deciding what
# to do with either is a human's call — the runbook is harvest, then reset —
# and this script's job is to stop, name the state, and change nothing.
# An ABSENT checkout passes: the codex lane below clones it, and on a first
# deploy there is nothing to protect yet.
AGENT_CHECKOUT_PREFLIGHT_REMOTE='repo=$HOME/git/culture-nodes-agent
if [ ! -d "$repo/.git" ]; then
  echo "agent checkout: absent at $repo (first deploy: the codex lane clones it)"
  exit 0
fi
if [ -n "$(git -C "$repo" status --porcelain)" ]; then
  echo "agent checkout $repo is DIRTY (uncommitted changes) — harvest the diff, then reset it to the deployed revision, per the runbook (deploy/prod/README.md)" >&2
  exit 3
fi
if ! git -C "$repo" symbolic-ref -q HEAD >/dev/null 2>&1; then
  echo "agent checkout $repo is DETACHED at $(git -C "$repo" rev-parse --short HEAD) — check out its tracking branch (or reset it to the deployed revision, spec q3) before deploying" >&2
  exit 3
fi
echo "agent checkout: clean on $(git -C "$repo" symbolic-ref --short HEAD) at $(git -C "$repo" rev-parse --short HEAD)"'

say "preflight: agent checkout state on $HOST (read-only, before anything is shipped or stopped)"
ssh "$HOST" "$AGENT_CHECKOUT_PREFLIGHT_REMOTE" || {
  echo "preflight failed on $HOST: the agent checkout is not in a deployable state (reason above); nothing on $HOST was changed" >&2
  exit 1
}

# The same four checks the end-of-lane doctor runs (prompt file, skills kit,
# API reachability, userns sysctl), but BEFORE the stack is touched, so a host
# the agent lane cannot work on is refused while the stack is still the old
# one. Only prompt_file_present decides the exit code (CLAUDE.md, "Mesh
# identity"), and it reads the checkout — so on a first deploy, where there
# is no checkout and no ~/.local/bin/nodes yet, there is nothing for it to
# read and the preflight says so and passes; the end-of-lane doctor still
# gates that deploy once the codex lane has installed both.
# Fail closed: a host with no agent checkout or no nodes CLI cannot be
# doctored, and a deploy that skips the doctor is exactly the path #63's
# userns fact was added to close. FIRST_DEPLOY=1 is the explicit, named
# exception for a host that has never been deployed (nothing to doctor yet);
# it is declared by the operator, never inferred from a missing file.
# The declaration is made in the OPERATOR's shell and the check runs on the
# HOST: ssh carries none of the operator's environment across (sshd forwards
# only what its AcceptEnv admits — LANG/LC_* by default), so the remote
# script would read its own, unset, FIRST_DEPLOY and refuse every first
# deploy while the fake-ssh harness, which inherits the environment, said
# the exception worked (#244 review). The lane therefore carries it inside
# the remote command — as a normalised 0/1 it computed itself, never the raw
# variable, so nothing the operator typed is spliced into a command line on
# a production host.
preflight_doctor() {
  local host=$1 first_deploy=0
  if [ "${FIRST_DEPLOY:-}" = 1 ]; then
    first_deploy=1
  fi
  say "preflight: nodes doctor on $host"
  ssh "$host" "FIRST_DEPLOY=$first_deploy; "'repo=$HOME/git/culture-nodes-agent; cli=$HOME/.local/bin/nodes
if [ ! -d "$repo/.git" ] || [ ! -x "$cli" ]; then
  if [ "$FIRST_DEPLOY" = 1 ]; then
    echo "nodes doctor: no agent checkout or nodes CLI on this host and FIRST_DEPLOY=1 declared — proceeding; the post-deploy doctor gates this deploy"
    exit 0
  fi
  echo "preflight refused: no agent checkout or no nodes CLI on this host, so nodes doctor cannot run before the deploy; declare FIRST_DEPLOY=1 for a host that has never been deployed, otherwise restore the checkout first" >&2
  exit 1
fi
cd "$repo" && "$cli" doctor' || {
    echo "preflight failed on $host: nodes doctor reports the agent lane unhealthy BEFORE the deploy; fix the reported check first — nothing was changed" >&2
    exit 1
  }
}
preflight_doctor "$HOST"

# The thor lane is about to stop orin's worker across migrate/cutover
# (TWO_HOST_LANE below), so whether orin is reachable and whether it has a
# worker stack at all are facts to establish NOW, while thor is still
# untouched. Three answers: `present` (quiesce it), `absent` (a first deploy —
# orin has never been deployed, there is no worker to stop), or nothing (the
# ssh itself failed — refuse; an unreachable second worker is not the same as
# no second worker, and the difference is a worker that keeps producing history
# through the cutover window).
ORIN_WORKER_STACK=absent
if [[ "$HOST" == thor* ]]; then
  if [ "${SKIP_ORIN_QUIESCE:-0}" = "1" ]; then
    say "preflight: SKIP_ORIN_QUIESCE=1 — treating this as a single-host deployment with no orin worker"
  else
    say "preflight: orin worker stack on $ORIN_HOST (read-only)"
    # Signalled by exit status, not parsed output: the remote `test` answers
    # 0 (present) or 1 (absent), and ssh itself answers 255 when it never
    # reached the host — three states that must not collapse into two.
    orin_probe_status=0
    ssh "$ORIN_HOST" "test -f $REMOTE_DIR/deploy/prod/compose.orin.yml" || orin_probe_status=$?
    case "$orin_probe_status" in
      0) ORIN_WORKER_STACK=present
         say "preflight: $ORIN_HOST has a worker stack; it will be stopped across migrate/cutover and restarted after"
         # orin is about to be modified by this lane too (its worker is
         # stopped and restarted), so it gets the same pre-modification
         # doctor as thor, before anything on either host changes.
         preflight_doctor "$ORIN_HOST" ;;
      1) say "preflight: $ORIN_HOST has no worker stack yet (first deploy) — nothing there to quiesce; deploy.sh orin comes after this lane" ;;
      *)
        echo "preflight failed: cannot reach $ORIN_HOST (ssh exit $orin_probe_status) to ask whether it runs a worker; nothing on $HOST was changed" >&2
        echo "hint: fix ssh to $ORIN_HOST (or set ORIN_HOST), or export SKIP_ORIN_QUIESCE=1 ONLY if this deployment has no second worker" >&2
        exit 1
        ;;
    esac
  fi
fi
# PREFLIGHT_END
