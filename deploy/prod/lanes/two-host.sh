# TWO_HOST_LANE_START -- tests/test_deploy_two_host.py executes these
# functions against fake hosts and asserts their ORDER; keep the marker on
# the first definition and its mate after the last.
#
# migrations/README.md's 0041 entry states the fail-closed deploy order: stop
# the sweep, migrate, nodes-cutover, resume. Until this task the thor lane
# implemented it on ONE host: it stopped thor's scheduler/worker/api, and
# orin's worker — the second consumer of the same database — kept running
# through migrate and cutover. Nothing took a dump before the migration ran,
# and "resume" was `compose up`, which brought the scheduler back whether or
# not the two workers were running the same code as the api. This block is
# that order across both hosts, with the dump in it, and with the resume
# gated on revision parity.
#
# "The sweep schedule" is paused and resumed as the `scheduler` compose
# service: it is the only process that fires schedules (cmd/nodes/scheduler.go),
# nothing in compose.thor.yml depends on it, and a service that is not running
# cannot fire. `up -d --scale scheduler=0` brings every other service up
# without it; `up -d scheduler` is the resume. No schedule row is touched, so a
# deploy that stops early leaves nothing half-armed in the database.

# compose_thor / compose_orin <args...> — one compose invocation on the named
# host. NODES_BUILD_REVISION rides along so that if compose ever DOES build
# (it should not — see the image build step — but the tag missing would make
# it), the binary is still stamped (tests/deploy/revisionstamp_test.go).
compose_thor() {
  ssh "$THOR_HOST" "cd $REMOTE_DIR/deploy/prod && NODES_BUILD_REVISION=$REVISION docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml $*"
}
compose_orin() {
  ssh "$ORIN_HOST" "cd $REMOTE_DIR/deploy/prod && NODES_BUILD_REVISION=$REVISION docker compose --env-file ~/.culture-nodes/prod.env -f compose.orin.yml $*"
}

# predeploy_pg_dump — a FORCED full dump of the authoritative database, taken
# before anything is stopped and before migrate runs. Sets PREDEPLOY_DUMP.
#
# The `backup` compose profile dumps on an interval, so the newest scheduled
# dump can be up to BACKUP_INTERVAL_SECONDS (6h) behind the moment the
# migration runs — and RESTORE.md's rollback is dump-restore ONLY (there is
# no contract-migration reverse). This dump is what a rollback of THIS deploy
# restores from. It reuses the backup service's own definition (its image, its
# NODES_DATABASE_URL, its ~/.culture-nodes/backups bind mount), so it dumps
# the same database the loop does in both bundled and external-Postgres mode;
# --no-deps because the database is already running and `run` must not try to
# start (or, in external mode, find) a bundled postgres. The name is
# predeploy-*, which the loop's `nodes-*` rotation never prunes: a pre-migrate
# dump must not age out on a schedule, so pruning these is an operator's
# decision, by hand.
predeploy_pg_dump() {
  local name
  name="predeploy-${REVISION:0:12}-$(date -u +%Y%m%dT%H%M%SZ).dump"
  PREDEPLOY_DUMP="~/.culture-nodes/backups/$name"
  say "forcing a pre-migrate pg_dump on $THOR_HOST -> $PREDEPLOY_DUMP"
  compose_thor "run --rm --no-deps -T backup 'pg_dump \"\$NODES_DATABASE_URL\" -Fc -f /backups/$name'" || {
    echo "pre-migrate pg_dump failed on $THOR_HOST; refusing to migrate without a dump to restore from — nothing was stopped" >&2
    exit 1
  }
  ssh "$THOR_HOST" "test -s ~/.culture-nodes/backups/$name" || {
    echo "pre-migrate pg_dump reported success but $PREDEPLOY_DUMP is missing or empty on $THOR_HOST; refusing to migrate — nothing was stopped" >&2
    exit 1
  }
}

# quiesce_orin_worker / restart_orin_worker — the second consumer of the
# database, stopped for the migrate + cutover window and started again after
# thor's stack is up. ORIN_WORKER_STACK was established in preflight, so
# "absent" here is a first deploy, never an unreachable host.
quiesce_orin_worker() {
  if [ "$ORIN_WORKER_STACK" = present ]; then
    say "stopping orin's worker on $ORIN_HOST across migrate/cutover"
    compose_orin "stop worker"
  else
    say "no orin worker stack to quiesce"
  fi
}
restart_orin_worker() {
  if [ "$ORIN_WORKER_STACK" = present ]; then
    say "restarting orin's worker on $ORIN_HOST"
    compose_orin "up -d worker"
  fi
}

# worker_revision <host> <compose-file> — the revision a running worker was
# built from. A worker has no HTTP surface, so the only revision-bearing fact
# it exposes is the `culture-nodes.revision` label on the image its container
# was created from (stamped by the explicit `docker build` above). Prints the
# label, or nothing when there is no worker container.
WORKER_REVISION_REMOTE='cd __REMOTE_DIR__/deploy/prod || exit 1
cid=$(docker compose --env-file ~/.culture-nodes/prod.env -f __COMPOSE__ ps -q worker | head -n 1)
[ -n "$cid" ] || exit 0
docker inspect --format "{{index .Config.Labels \"culture-nodes.revision\"}}" "$cid"'
worker_revision() { # host compose-file
  local snippet=${WORKER_REVISION_REMOTE//__REMOTE_DIR__/$REMOTE_DIR}
  ssh "$1" "${snippet//__COMPOSE__/$2}" | tr -d '\r'
}

# revision_parity_check — the gate on resuming the sweep (spec c26). Returns 0
# when thor's api, thor's worker and (when present) orin's worker all report
# exactly $REVISION; 1 otherwise, after printing every reading. Two workers on
# one namespace race for the same dispatches, so a worker one revision behind
# is not "mostly deployed": it is a run whose outcome depends on which host
# polled first.
revision_parity_check() {
  local api thor_worker orin_worker="(skipped)" ok=yes
  say "revision parity: api and workers against $REVISION"
  api=$(ssh "$THOR_HOST" "curl -fsS http://localhost:18080/v1alpha1/version" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("revision",""))' || true)
  thor_worker=$(worker_revision "$THOR_HOST" compose.thor.yml || true)
  [ "$ORIN_WORKER_STACK" != present ] || orin_worker=$(worker_revision "$ORIN_HOST" compose.orin.yml || true)
  printf '    api (%s, GET /v1alpha1/version): %s\n' "$THOR_HOST" "${api:-(none)}"
  printf '    worker (%s, image label):        %s\n' "$THOR_HOST" "${thor_worker:-(none)}"
  printf '    worker (%s, image label):        %s\n' "$ORIN_HOST" "${orin_worker:-(none)}"
  [ "$api" = "$REVISION" ] || ok=no
  [ "$thor_worker" = "$REVISION" ] || ok=no
  [ "$ORIN_WORKER_STACK" != present ] || [ "$orin_worker" = "$REVISION" ] || ok=no
  [ "$ok" = yes ]
}

resume_sweep_schedule() {
  say "resuming the sweep schedule (starting thor's scheduler)"
  compose_thor "up -d scheduler"
}

# thor_two_host_lane — the ordered sequence itself. Sets NS, PREDEPLOY_DUMP
# and SWEEP_RESUMED; deploy_summary reads them.
thor_two_host_lane() {
  say "starting thor control plane (two-host r4 sequence)"
  predeploy_pg_dump
  # Stop history-producing services on BOTH hosts, apply migrations alone,
  # then adopt all pending Jira heads before any scheduler/worker can resume
  # the sweep.
  say "stopping thor's scheduler, worker and api"
  compose_thor "stop scheduler worker api || true"
  quiesce_orin_worker
  compose_thor "up -d postgres"
  compose_thor "run --rm migrate"
  # systemd parses the two EnvironmentFiles without shell-evaluating secret
  # values, and applies them only to this transient host-side process.
  say "running the nodes-cutover adopter on $THOR_HOST"
  # nodes-cutover is a HOST process (custody: the Jira read pair never enters
  # a long-lived container), but prod.env's NODES_DATABASE_URL names the
  # compose service `postgres`, which only resolves inside the compose
  # network -- t7 measured "lookup postgres ... server misbehaving" with the
  # api already stopped. compose.thor.yml publishes postgres on the host's
  # 5432, so the adopter reads a third env file that rewrites only the host
  # part; it is written 0600 on the target and later files override earlier.
  ssh "$THOR_HOST" 'umask 077; grep "^NODES_DATABASE_URL=" $HOME/.culture-nodes/prod.env | sed "s#@postgres:#@127.0.0.1:#" > $HOME/.culture-nodes/cutover.env; systemd-run --user --wait --pipe --collect --property=EnvironmentFile=$HOME/.culture-nodes/prod.env --property=EnvironmentFile=$HOME/.culture-nodes/runner-secrets.env --property=EnvironmentFile=$HOME/.culture-nodes/cutover.env $HOME/.culture-nodes/bin/nodes-cutover; rc=$?; rm -f $HOME/.culture-nodes/cutover.env; exit $rc'
  # Everything except the scheduler; no --build (see the image build step:
  # a compose rebuild would drop the label the parity check reads).
  compose_thor "up -d --scale scheduler=0"
  say "waiting for readyz"
  ssh "$THOR_HOST" 'for i in $(seq 1 60); do curl -fsS http://localhost:18080/v1alpha1/readyz >/dev/null 2>&1 && echo READY && exit 0; sleep 2; done; echo NOT_READY; exit 1'
  say "resolving namespace id and (re)starting worker with it"
  NS=$(ssh "$THOR_HOST" "curl -fsS http://localhost:18080/v1alpha1/namespaces | python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0][\"id\"] if rows else \"\")'")
  [ -n "$NS" ] || { echo "no namespace row found" >&2; exit 1; }
  ssh "$THOR_HOST" "grep -q '^NODES_NAMESPACE_ID=' ~/.culture-nodes/prod.env && sed -i 's/^NODES_NAMESPACE_ID=.*/NODES_NAMESPACE_ID=$NS/' ~/.culture-nodes/prod.env || echo NODES_NAMESPACE_ID=$NS >> ~/.culture-nodes/prod.env"
  compose_thor "up -d worker"
  restart_orin_worker
  if revision_parity_check; then
    resume_sweep_schedule
    SWEEP_RESUMED=yes
  else
    # Not fatal HERE: on a revision change orin's worker is EXPECTED to be
    # behind until `deploy.sh orin` rebuilds it, and that lane's own parity
    # check is what resumes the sweep. The lane finishes its detectors, and
    # deploy_summary exits non-zero so the paused sweep cannot be overlooked.
    say "revision parity does not hold — the sweep schedule stays PAUSED (scheduler not started)"
    SWEEP_RESUMED=no
  fi
}

# orin_two_host_lane — the orin lane's half: after its worker is up on the
# freshly built image, the same parity check decides whether thor's scheduler
# may run. This is the resume step of an upgrade, since the thor lane above
# necessarily left the sweep paused when orin was still on the old image.
orin_two_host_lane() {
  ORIN_WORKER_STACK=present
  if revision_parity_check; then
    resume_sweep_schedule
    SWEEP_RESUMED=yes
  else
    say "revision parity does not hold — refusing to resume the sweep schedule"
    SWEEP_RESUMED=no
  fi
}

# deploy_summary <lane> — the last lines of either lane: the dump path (the
# thing a rollback needs, printed where the operator will see it) and the
# sweep state. A paused sweep is exit 3: the deploy of this host succeeded,
# but the r4 procedure is not complete until both workers match the api and
# the sweep is running again, and a zero exit here is how that gets forgotten.
deploy_summary() { # lane
  say "$1 deploy complete (namespace ${NS:-?})"
  [ -z "${PREDEPLOY_DUMP:-}" ] || say "pre-migrate dump: $PREDEPLOY_DUMP on $THOR_HOST (rollback: deploy/prod/RESTORE.md; not rotated by the backup loop — prune by hand)"
  if [ "${SWEEP_RESUMED:-no}" = yes ]; then
    say "sweep schedule: resumed (api and workers all at ${REVISION:0:12})"
  else
    say "sweep schedule: PAUSED — thor's scheduler is not running, so no schedule fires"
    if [[ "$1" == thor* ]]; then
      say "next: run deploy.sh orin; its parity check resumes the sweep once orin's worker is on ${REVISION:0:12}"
    else
      say "next: run deploy.sh thor for the revision orin should match, or re-run deploy.sh orin after fixing the reading above"
    fi
    exit 3
  fi
}
# TWO_HOST_LANE_END
