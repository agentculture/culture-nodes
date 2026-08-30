#!/usr/bin/env bash
# The deployment-settings lane of install-secrets.sh (issue #124), split into
# its own file when install-secrets.sh crossed the repo's 1000-line hard limit
# (tests/lint/filelength_test.go) during task t25.
#
# It is SOURCED, not executed: it defines install_deployment_settings and
# nothing else, and install-secrets.sh calls it once per host after the guarded
# prod lane has run. The function body reads $PROD_ENV_MERGE, $UI_BASE_URL and
# $CALLBACK_BASE_URL from its caller at call time, which is why the source line
# may sit at the top of that script alongside the other lanes while the calls
# stay where they belong in the flow.

# --- deployment settings, add-if-absent (issue #124) -----------------------
#
# The non-secret half of prod.env. This lane runs UNGUARDED and mints nothing:
# every value below is a deployment setting, so delivering one to an
# already-provisioned host must not require FORCE_PROD=1 and must not rotate a
# live credential to do it. `./install-secrets.sh` with nothing set is the
# whole answer to "compose says a variable is missing on a host I already
# installed".
#
# ADD-IF-ABSENT, deliberately asymmetric: a key prod.env does not have is
# written, a key it has is left alone however wrong it is. deploy/prod/README's
# "Bundled or external PostgreSQL" section tells an operator to point the stack
# at an external database by hand-editing NODES_DATABASE_URL and
# COMPOSE_PROFILES on the host; a lane that re-asserted its own values every
# run would silently revert that documented choice on the next deploy and bring
# the stack back up against the bundled database having reported nothing.
# The cost is stated in the README too: correcting a wrong value is
# remove-secret.sh followed by a re-run, not a re-run.
#
# The surviving keys go through the ONE shared $PROD_ENV_MERGE — no second
# copy of the merge loop. The copies had already drifted once (only one of them
# normalised a missing trailing newline), and tests/deploy/prodenvmerge_test.go
# pins the loop to a single definition.
install_deployment_settings() { # host, database-host, compose-profiles ("" = none)
  local host=$1 db_host=$2 profiles=$3
  # HOST/DB_HOST/PROFILES/UI_BASE_URL/CALLBACK_BASE_URL are prefixed into the
  # remote command exactly the way FORCE is in install_env — ssh forwards no
  # environment, so a bare $HOST inside the single-quoted body would be empty on
  # the target. All five are non-secret by construction (an ssh target, a
  # database hostname, a profile list, a page origin, a callback origin), which
  # is why they may ride the argv at all.
  # POSTGRES_PASSWORD may not, and never does: it is read on the far side and
  # never comes back.
  # shellcheck disable=SC2029,SC2016 # both are deliberately remote: the prefix
  # is expanded here on purpose, the body is expanded on the target on purpose
  ssh "$host" "HOST='$host'; DB_HOST='$db_host'; PROFILES='$profiles'; UI_BASE_URL='$UI_BASE_URL'; CALLBACK_BASE_URL='$CALLBACK_BASE_URL'; "'
set -eu
umask 077
mkdir -p ~/.culture-nodes
touch ~/.culture-nodes/prod.env
chmod 600 ~/.culture-nodes/prod.env

# ONE last-wins reader, plus a presence test defined in terms of it (issue
# #135). env_get scans to the end of the file and keeps the LAST assignment of
# the key, which is what the docker compose env_file reader itself does. env_has
# used to return on the FIRST line whose key matched and call the key present
# whatever it held, so it could answer from a line no reader anywhere uses:
# a key assigned twice whose winning line is empty, or a bare `KEY=`, both read
# as present here and as unset everywhere else. prod.env is hand-edited in
# practice -- that is how half its keys got there -- so both shapes are
# reachable, and the consequence is not cosmetic: an empty
# NODES_CODE_RUNNER_ACTOR_ID counted as present makes this lane write
# NODES_CODE_RUNNER_NAME beside it, which is the PARTIAL runner tuple
# cmd/nodes/worker.go refuses at startup and the outage the tuple rule below
# exists for.
#
# The reader matches with a QUOTED case pattern and no delimiter anywhere, for
# the same reason PROD_ENV_MERGE stopped using sed: a value carrying the s///
# delimiter ends the expression early and the key is skipped in silence. A
# quoted case pattern is literal, so no key and no value can collide with it.
env_get() {
  v=
  while IFS= read -r cur || [ -n "$cur" ]; do
    case "$cur" in "$1"=*) v=${cur#*=} ;; esac
  done < ~/.culture-nodes/prod.env
  printf %s "$v"
}
env_has() { [ -n "$(env_get "$1")" ]; }

# sslmode is resolved HERE and written into the URL as a LITERAL value, never
# as a ${DATABASE_SSLMODE} placeholder. Found by probe: docker compose
# interpolates env-file values recursively, but only backwards — a placeholder
# resolves only while the key it names happens to sit EARLIER in the file. In
# the other order compose resolves sslmode= to the empty string and reports no
# error at all; libpq then falls back to its own default, the stack connects,
# and nobody learns the TLS mode was never applied. An add-if-absent lane
# appends in whatever order the host is missing keys, so it can produce exactly
# that file. Resolving on the host removes the ordering dependency instead of
# documenting it.
sslmode=$(env_get DATABASE_SSLMODE)
# A present-but-empty DATABASE_SSLMODE takes the same default as an absent one:
# sslmode= in a URL is the exact string this lane exists never to write.
[ -n "$sslmode" ] || sslmode=disable

settings="POSTGRES_USER=nodes
POSTGRES_DB=nodes
MINIO_ROOT_USER=nodesroot
NODES_CALLBACK_BASE_URL=$CALLBACK_BASE_URL
NODES_UI_BASE_URL=$UI_BASE_URL"

# DATABASE_SSLMODE is an INPUT to composing the URL and has no other reader --
# no compose service and no line of Go, as deploy/prod/README states. So it is
# delivered only where it can still be used: to a host whose NODES_DATABASE_URL
# does not already name an sslmode. Writing it beside a URL that does would add
# a second copy of a TLS decision nothing consults and that can contradict the
# one actually in force -- an external-database host would say `verify-full` in
# its URL and `disable` in the key beside it, with nothing to say which the
# stack uses. That is the two-copies-diverge shape of issue #133 arriving from the
# settings side, so this lane declines to create it (issue #135).
url_sslmode=
case "$(env_get NODES_DATABASE_URL)" in
  *sslmode=*)
    url_sslmode=$(env_get NODES_DATABASE_URL)
    url_sslmode=${url_sslmode##*sslmode=}
    url_sslmode=${url_sslmode%%&*}
    ;;
esac
if [ -z "$url_sslmode" ]; then
  settings="$settings
DATABASE_SSLMODE=$sslmode"
fi
# COMPOSE_PROFILES is thor-only: it starts the bundled database and the backup
# loop, both of which live in compose.thor.yml. orin is only a worker against
# the database on thor and has no profile of its own to select.
if [ -n "$PROFILES" ]; then
  settings="$settings
COMPOSE_PROFILES=$PROFILES"
fi

# NODES_DATABASE_URL is composed ON THE HOST, from the POSTGRES_PASSWORD line
# already in this file, and only when the key is missing.
#
# It cannot be composed locally: the password this script generated is NOT the
# live one on a provisioned host — the guarded lane above kept the existing
# file — so a locally composed URL would carry a password the database does not
# accept, and the stack would fail auth on the next restart with a prod.env
# that looks correct. Reading it here also keeps it under the same discipline
# as every other secret in this file: it crosses no wire, enters no argv, and
# is never echoed. Only the KEY NAME is ever printed.
if ! env_has NODES_DATABASE_URL; then
  pw=$(env_get POSTGRES_PASSWORD)
  if [ -n "$pw" ]; then
    settings="$settings
NODES_DATABASE_URL=postgres://nodes:$pw@$DB_HOST:5432/nodes?sslmode=$sslmode"
  else
    # Refused BY NAME. A URL with an empty password authenticates as nobody
    # while reading, to anything that greps for the key, as configured — the
    # same class of quiet wrongness as the empty sslmode above.
    echo "REFUSED on $HOST: prod.env carries no POSTGRES_PASSWORD, so NODES_DATABASE_URL cannot be composed from it. Refusing rather than writing a URL with an empty password. Install the generated population first (a host with no prod.env gets it from the guarded lane of this script; FORCE_PROD=1 rotates an existing one), then re-run." >&2
  fi
fi

# NODES_CODE_RUNNER_NAME is delivered ONLY to a host that already carries
# NODES_CODE_RUNNER_REVISION and NODES_CODE_RUNNER_ACTOR_ID.
#
# cmd/nodes/worker.go refuses a PARTIAL tuple — one set, another empty — because
# that combination attributes a code operation to a runner nobody can identify.
# Setting NONE of the three is legitimate and means "this deployment runs no
# code nodes". Both compose files used to hardcode the name, which made that
# legitimate state unreachable: the worker always saw exactly one of the three
# set. thor survived only because someone had hand-installed the other two;
# orin had none, so the first deploy that brought it to a revision carrying the
# check left it crash-looping after 46 hours of running fine without them.
#
# The condition cannot live in compose — it has no conditionals — so it lives
# here, where prod.env is already being read. This lane does not INVENT the
# other two: a revision and a registered actor row are facts about a
# deployment, not defaults, and guessing either would attribute evidence to a
# runner that never produced it.
if env_has NODES_CODE_RUNNER_REVISION && env_has NODES_CODE_RUNNER_ACTOR_ID; then
  settings="$settings
NODES_CODE_RUNNER_NAME=headspace"
fi

# Filter to the keys this host does NOT already have. What survives is what
# gets merged, and what gets merged is what is reported — a lane that printed
# the same success line either way would be a second place claiming success
# without evidence.
pending=
added=
while IFS= read -r kv; do
  [ -n "$kv" ] || continue
  key=${kv%%=*}
  if env_has "$key"; then continue; fi
  if [ -z "$pending" ]; then pending=$kv; else pending="$pending
$kv"; fi
  added="$added $key"
done <<SETTINGS
$settings
SETTINGS

if [ -n "$pending" ]; then
  printf "%s\n" "$pending" | { '"$PROD_ENV_MERGE"'; }
  echo "added deployment settings to prod.env on $HOST:$added"
else
  echo "no deployment settings to add on $HOST — prod.env already carries all of them (this lane never replaces a value: correct a wrong one with remove-secret.sh, then re-run)"
fi
'
}
