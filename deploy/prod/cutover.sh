#!/usr/bin/env bash
# cutover.sh — take ONE harness actor on ONE host from secrets to a
# registered actor row, in one command (plan t10, spec c9/c35, issue #298).
#
#   cutover.sh <thor|orin|spark> <qwen|pi|colleague> [--dry-run] [--yes]
#              [--model M] [--model-endpoint URL]
#
# Before this script the sequence was four separate hand-turns, in an order
# only the operator knew (#298):
#
#   1. deploy/prod/bootstrap-accounts.sh HOST   (root: useradd, linger, keys)
#   2. deploy/prod/install-secrets.sh           (the engine's account env)
#   3. deploy/prod/deploy.sh HOST               (deploy_account_engine_bridge)
#   4. deploy/prod/register-actor.sh ...        (the actor row + metadata)
#
# Step 1 STAYS a hand-turn and this script never performs it: it is the only
# step that needs root, `sudo` asks for a typed password on orin and spark,
# and #243's every-hand-turn rule wants that typed sudo recorded rather than
# smuggled into an automation. cutover.sh therefore never invokes
# bootstrap-accounts.sh and never invokes sudo — tests/test_deploy_cutover.py
# greps this file for both, so that is a checked fact and not a promise.
# Steps 2-4 are what this script sequences.
#
# --- posture --------------------------------------------------------------
#
# Every step prints one line, `step <name>: <verdict> — <detail>`, where the
# verdict is run / skip / refuse (or would-run / would-skip / would-refuse
# under --dry-run). The script stops at the FIRST failure, names the failed
# step on stderr and exits non-zero: a half-cutover that reported success is
# exactly the shape of failure #300 and #298 record.
#
# --dry-run performs no ssh, no deploy and no registration. It prints what
# each step would do — the exact command — and exits 0. The two facts a dry
# run cannot know without touching the hosts (whether the account opens, and
# the host's numeric LAN IP) are named as such rather than guessed.
#
# Without --dry-run, --yes is REQUIRED. This script restarts a bridge unit,
# writes a secret into an account and appends an actor revision; the
# nodes-operator skill's assign verb takes the same posture for the same
# reason (#289): a side-effecting fleet command does not act on a bare
# invocation.
#
# --- what it does NOT do --------------------------------------------------
#
# It does not create accounts (step 1 above), it does not edit the compose
# files, and it does not write to install-secrets.sh or deploy.sh. The
# compose files must ALREADY declare NODES_ACTOR_<ENGINE>_<HOST>_TOKEN in
# compose.thor.yml's `api` and `worker` blocks and compose.orin.yml's
# `worker` block — a committed change no runtime script can make, so this
# script checks it and refuses BY NAME instead of deploying a bridge the
# control plane will never hold a credential for.
#
# --- how the three tools are reached (the t10 decisions) ------------------
#
# install-secrets.sh is 999 lines, one under the hard limit
# tests/lint/filelength_test.go enforces, so it cannot grow a subcommand
# entry point — and it has none. Its per-engine lane is already fenced
# between the `# QWEN_PI_ACCOUNT_ENV_START` / `_END` markers precisely so it
# can be lifted out and run on its own (tests/test_deploy_qwen_pi_bridges.py
# has done exactly this since #294). This script lifts the same region and
# sources it, so there is ONE definition of install_bridge_account_env and
# install-secrets.sh gains not one line. Running install-secrets.sh whole
# was the alternative and is wrong here: it rotates or re-checks the entire
# production secret set on BOTH hosts, which is not what bringing one actor
# online should do.
#
# deploy.sh, by contrast, CANNOT be sourced: it reads its host from argv at
# `HOST=${1:?...}` and executes top to bottom, so `deploy_account_engine_bridge`
# is only reachable by running the script. It is therefore invoked as
# `deploy.sh <host>` — the same command step 3 always was. That deploys the
# host's other bridges too; every lane in it is idempotent, and t10 does not
# modify deploy.sh. CUTOVER_DEPLOY_CMD overrides the command (the shim test
# points it at a recorder), the same override register-actor.sh gives PSQL_CMD.
#
# register-actor.sh is already idempotent and append-only: it is CALLED on
# every run and its own "unchanged" answer is what this script reports as a
# skip, rather than second-guessing the registry from outside.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Ports and the registry read: actor-placement.sh is the one place that says
# which port an engine bridge binds, shared with deploy.sh and
# install-secrets.sh so the rendered config and the registered endpoint
# cannot disagree (issue #72).
# shellcheck source=deploy/prod/actor-placement.sh
. "$SCRIPT_DIR/actor-placement.sh"
# unix_user_target (culture-<engine>@<host>, @localhost on spark) and the
# engine allowlist; account_reachable is the same BatchMode probe deploy.sh's
# account lanes use, so "the account exists" means here exactly what it means
# there. Both files are function definitions only when sourced.
# shellcheck source=deploy/prod/lanes/unix-user.sh
. "$SCRIPT_DIR/lanes/unix-user.sh"
# shellcheck source=deploy/prod/lanes/account-bridges.sh
. "$SCRIPT_DIR/lanes/account-bridges.sh"

INSTALL_SECRETS="$SCRIPT_DIR/install-secrets.sh"
SECRETS_LANE_START='# QWEN_PI_ACCOUNT_ENV_START'
SECRETS_LANE_END='# QWEN_PI_ACCOUNT_ENV_END'

# Lift install-secrets.sh's per-engine account lane without executing the
# rest of that script. The region is comments plus three function
# definitions; anything else in it would be a change to install-secrets.sh
# that this script must not silently absorb, so an empty or unfenced region
# is a hard refusal rather than a fallback.
source_secrets_lane() {
  local block
  block=$(sed -n "/^${SECRETS_LANE_START}/,/^${SECRETS_LANE_END}/p" "$INSTALL_SECRETS")
  case "$block" in
    *"$SECRETS_LANE_START"*"$SECRETS_LANE_END"*) ;;
    *)
      echo "cutover: cannot find the ${SECRETS_LANE_START#\# } region in $INSTALL_SECRETS" >&2
      echo "hint: that marker pair fences install_bridge_account_env; restore it (or update this script) before running a cutover" >&2
      exit 2 ;;
  esac
  eval "$block"
  if ! declare -F install_bridge_account_env >/dev/null 2>&1; then
    echo "cutover: install-secrets.sh's engine lane no longer defines install_bridge_account_env" >&2
    echo "hint: read the region between the QWEN_PI_ACCOUNT_ENV markers in $INSTALL_SECRETS" >&2
    exit 2
  fi
}

usage() {
  cat >&2 <<'EOF'
usage: cutover.sh <thor|orin|spark> <qwen|pi|colleague> [options]

  Brings ONE harness actor online on ONE host: the engine account's bridge
  secret, the deploy of its bridge, and the actor registration -- in that
  order, stopping at the first failure.

  --dry-run              print every step it would run and exit 0; no ssh,
                         no deploy, no registration, no side effects
  --yes                  required for a real run (it restarts a bridge and
                         writes a secret); ignored with --dry-run
  --model M              actor metadata model=M
                         (default: unsloth/Qwen3.8-27B-NVFP4)
  --model-endpoint URL   actor metadata model_endpoint=URL
                         (default: http://thor:8000/v1)

  Creating the culture-<engine> account is NOT part of this script: it needs
  root and stays a recorded hand-turn --
    sudo bash deploy/prod/lanes/unix-user.sh bootstrap <engine>
  typed on the host, then re-run this.

Env:
  FORCE_QWEN / FORCE_PI / FORCE_COLLEAGUE=1   rotate an existing bridge
                         secret instead of keeping it (install-secrets.sh's
                         own guard, mirrored here)
  CUTOVER_DEPLOY_CMD     command run for the deploy step
                         (default: deploy/prod/deploy.sh)
  BRANCH                 revision the deploy would ship (default: HEAD)
EOF
}

# --- arguments ------------------------------------------------------------
HOST=""
ENGINE=""
DRY_RUN=0
ASSUME_YES=0
MODEL=${CUTOVER_MODEL:-unsloth/Qwen3.8-27B-NVFP4}
MODEL_ENDPOINT=${CUTOVER_MODEL_ENDPOINT:-http://thor:8000/v1}
POSITIONAL=()

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    --model) [ $# -ge 2 ] || { echo "cutover: --model needs a value" >&2; usage; exit 1; }
             MODEL=$2; shift 2 ;;
    --model=*) MODEL=${1#--model=}; shift ;;
    --model-endpoint) [ $# -ge 2 ] || { echo "cutover: --model-endpoint needs a value" >&2; usage; exit 1; }
             MODEL_ENDPOINT=$2; shift 2 ;;
    --model-endpoint=*) MODEL_ENDPOINT=${1#--model-endpoint=}; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "cutover: unknown option '$1'" >&2; usage; exit 1 ;;
    *) POSITIONAL+=("$1"); shift ;;
  esac
done

if [ "${#POSITIONAL[@]}" -ne 2 ]; then
  echo "cutover: expected exactly two arguments (host and engine), got ${#POSITIONAL[@]}" >&2
  usage
  exit 1
fi
HOST=${POSITIONAL[0]}
ENGINE=${POSITIONAL[1]}

# The engines this script knows are the HARNESS engines: the three the
# comparison measures. codex and claude are deployed by their own lanes and
# have no per-host token key of this shape.
case "$ENGINE" in
  qwen|pi|colleague) ;;
  *) echo "cutover: unknown engine '$ENGINE' (expected qwen, pi or colleague)" >&2
     echo "hint: codex and claude are deployed by their own lanes in deploy.sh, not by a harness cutover" >&2
     usage; exit 1 ;;
esac

# `thor.lan`, `thor-fake` and `thor` all name the same production host; the
# BASE name is what the compose token key and the actor key are built from,
# while $HOST stays whatever the operator typed (it is the ssh target).
HOST_BASE=${HOST%%.*}
HOST_BASE=${HOST_BASE%-fake}
case "$HOST_BASE" in
  thor|orin|spark) ;;
  *) echo "cutover: unknown host '$HOST' (expected thor, orin or spark)" >&2; usage; exit 1 ;;
esac

if [ "$ENGINE" = colleague ] && [ "$HOST_BASE" != spark ]; then
  echo "cutover: the colleague harness lane is spark's (#298 t5) — there is no culture-colleague account on $HOST_BASE" >&2
  echo "hint: run 'cutover.sh spark colleague', or pick qwen/pi for thor and orin" >&2
  exit 1
fi

if [ "$DRY_RUN" = 0 ] && [ "$ASSUME_YES" = 0 ]; then
  echo "cutover: refusing to act without --yes" >&2
  echo "hint: this restarts a bridge unit, writes a secret into culture-$ENGINE on $HOST and appends an actor revision — run 'cutover.sh $HOST $ENGINE --dry-run' first, then re-run with --yes" >&2
  exit 1
fi

# --- derived facts --------------------------------------------------------
engine_upper=$(printf '%s' "$ENGINE" | tr '[:lower:]' '[:upper:]')
host_upper=$(printf '%s' "$HOST_BASE" | tr '[:lower:]' '[:upper:]')
TOKEN_KEY="NODES_ACTOR_${engine_upper}_${host_upper}_TOKEN"
ACTOR_KEY="company/${ENGINE}-${HOST_BASE}"
OS_USER="culture-${ENGINE}"
TARGET=$(unix_user_target "$HOST" "$ENGINE")
# shellcheck disable=SC2088 # the tilde is expanded by the REMOTE shell, not here
BRIDGE_ENV="~/.culture-nodes/${ENGINE}-bridge.env"
FORCE_VAR="FORCE_${engine_upper}"
FORCE=${!FORCE_VAR:-0}
DEPLOY_CMD=${CUTOVER_DEPLOY_CMD:-$SCRIPT_DIR/deploy.sh}

# actor-placement.sh knows the thor/orin engine ports. colleague is spark's
# only harness bridge and its port lives in its own config template (#298
# t5), so it is read from there rather than duplicated as a literal here.
if ! PORT=$(actor_bridge_port "$ENGINE" 2>/dev/null); then
  PORT=$(sed -n 's/^[[:space:]]*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
    "$SCRIPT_DIR/${ENGINE}-developer.json.template" | head -n1)
fi
if [ -z "${PORT:-}" ]; then
  echo "cutover: no port for engine '$ENGINE' (neither actor_bridge_port nor ${ENGINE}-developer.json.template answered)" >&2
  exit 2
fi

# --- step reporting -------------------------------------------------------
#
# Results on stdout, diagnostics on stderr, never mixed -- the same contract
# the nodes CLI states in culture_nodes/cli/_output.py.
step() { # name verdict detail
  local verdict=$2
  [ "$DRY_RUN" = 1 ] && verdict="would-$2"
  printf 'step %s: %s — %s\n' "$1" "$verdict" "$3"
}

fail() { # name reason [exit-code]
  echo "cutover: step $1 failed: $2" >&2
  echo "hint: nothing after '$1' ran; fix that step and re-run 'cutover.sh $HOST $ENGINE --yes' (every step is idempotent, so a re-run resumes rather than repeats)" >&2
  exit "${3:-1}"
}

# --- step 1: the engine account exists ------------------------------------
#
# deploy.sh only WARNS and skips an unbootstrapped account (a codex-only
# deploy is a valid deploy); a cutover of that very engine must refuse
# instead, or it would report success having deployed nothing.
ACCOUNT_PROBE="ssh -o BatchMode=yes -o ConnectTimeout=15 $TARGET 'id -un'"
if [ "$DRY_RUN" = 1 ]; then
  step account-exists run "$ACCOUNT_PROBE (not run under --dry-run: a dry run touches no host)"
elif account_reachable "$TARGET"; then
  step account-exists run "$TARGET opens with the operator key"
else
  step account-exists refuse "$TARGET does not open with the operator key — $OS_USER on $HOST is not bootstrapped"
  echo "cutover: the root bootstrap is the one hand-turn this script never performs (#243, #298)" >&2
  echo "hint: type it on $HOST_BASE — sudo bash deploy/prod/lanes/unix-user.sh bootstrap $ENGINE — then re-run this" >&2
  fail account-exists "$TARGET is not reachable" 2
fi

# --- step 2: both compose files declare the actor token key ---------------
#
# The control plane reads an actor's bearer from the environment variable
# named in its registration (internal/worker/registry.go's authTokenEnvOf).
# That variable reaches the api and worker containers only if the compose
# file passes it through, and BOTH hosts' workers may dispatch either host's
# actor (#224), so both files must carry it. compose.orin.yml declares a
# worker only -- it runs no api (see that file's header).
compose_block_declares() { # file service key
  awk -v service="$2" -v key="$3" '
    /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ { inside = ($0 == "  " service ":"); next }
    inside && $0 ~ ("^[[:space:]]+" key ":") { found = 1 }
    END { exit found ? 0 : 1 }
  ' "$1"
}

compose_missing=()
for spec in "compose.thor.yml:api" "compose.thor.yml:worker" "compose.orin.yml:worker"; do
  file=${spec%%:*}
  service=${spec#*:}
  compose_block_declares "$SCRIPT_DIR/$file" "$service" "$TOKEN_KEY" \
    || compose_missing+=("$file's $service block")
done

if [ "${#compose_missing[@]}" -eq 0 ]; then
  step compose-declares-token-key run "$TOKEN_KEY is declared in compose.thor.yml (api, worker) and compose.orin.yml (worker)"
else
  step compose-declares-token-key refuse "$TOKEN_KEY is not declared in $(IFS=', '; echo "${compose_missing[*]}")"
  if [ "$DRY_RUN" = 1 ]; then
    echo "cutover: $TOKEN_KEY is a COMMITTED change no runtime script can make; a real run would refuse here" >&2
  else
    echo "cutover: $TOKEN_KEY is a COMMITTED change no runtime script can make — add it to the api and worker environment blocks and merge that first" >&2
    echo "hint: copy the NODES_ACTOR_PI_THOR_TOKEN lines in deploy/prod/compose.thor.yml and compose.orin.yml; a bridge deployed without it answers 401 to every dispatch" >&2
    fail compose-declares-token-key "$TOKEN_KEY is undeclared" 2
  fi
fi

# --- step 3: the engine account's bridge secret ---------------------------
#
# install-secrets.sh's own guard is "keep an existing file unless the
# engine's FORCE flag is set", because the rendered bridge config and the
# control plane's prod.env copy have to agree: a silent re-mint is a bridge
# that 401s every dispatch until the next deploy. The probe below mirrors
# that guard OUTSIDE the lane so the skip is visible as a step, and then the
# lane is run (it re-checks on its own; the two cannot disagree).
SECRETS_CMD="install-secrets.sh's install_bridge_account_env $HOST $ENGINE $FORCE (lifted from the QWEN_PI_ACCOUNT_ENV region)"
if [ "$DRY_RUN" = 1 ]; then
  step secrets run "$SECRETS_CMD"
else
  source_secrets_lane
  secret_present=0
  # shellcheck disable=SC2029 # the path is deliberately expanded on the far side
  ssh -o BatchMode=yes "$TARGET" "test -e $BRIDGE_ENV" >/dev/null 2>&1 && secret_present=1
  if [ "$secret_present" = 1 ] && [ "$FORCE" != 1 ]; then
    step secrets skip "$BRIDGE_ENV already exists in $TARGET (set $FORCE_VAR=1 to rotate it)"
  else
    step secrets run "$SECRETS_CMD"
    install_bridge_account_env "$HOST" "$ENGINE" "$FORCE" || fail secrets "install_bridge_account_env returned non-zero"
  fi
fi

# --- step 4: deploy the bridge into the account ---------------------------
#
# The deploy is skipped when the bridge is ALREADY serving the revision this
# checkout would ship. Asking the bridge what it is running, rather than
# assuming, is the discipline #104/#120 record: thor's and orin's bridges are
# `uv tool install` COPIES that go stale silently, so its /v1/capabilities
# deployment block is the only honest answer.
resolve_lan_ip() {
  # register-actor.sh refuses a hostname endpoint outright (c20): worker
  # containers do not inherit the host's /etc/hosts, so the address has to be
  # numeric. Derived on the target the way deploy.sh derives THOR_IP, never
  # hardcoded here.
  if [[ "$HOST" == spark* ]]; then
    getent hosts "$HOST_BASE" 2>/dev/null | awk '{print $1; exit}'
    return
  fi
  # shellcheck disable=SC2029 # the getent runs on the far side on purpose
  ssh "$HOST" "getent hosts $HOST_BASE | awk '{print \$1; exit}'" 2>/dev/null
}

bridge_revision() { # lan-ip
  curl -fsS --max-time 10 "http://$1:$PORT/v1/capabilities" 2>/dev/null \
    | python3 -c 'import json,sys
try:
    doc = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
print((doc.get("deployment") or {}).get("revision") or "")' 2>/dev/null || true
}

if [ "$DRY_RUN" = 1 ]; then
  step deploy run "$DEPLOY_CMD $HOST (reaches deploy_account_engine_bridge $HOST $ENGINE; skipped when http://<$HOST_BASE-lan-ip>:$PORT/v1/capabilities already reports this checkout's revision)"
  LAN_IP="<$HOST_BASE-lan-ip>"
else
  LAN_IP=$(resolve_lan_ip | tr -d '\r' || true)
  case "$LAN_IP" in
    [0-9]*.[0-9]*.[0-9]*.[0-9]*) ;;
    *) fail deploy "cannot resolve a numeric LAN IP for $HOST_BASE from $HOST (got '${LAN_IP:-nothing}'); register-actor.sh accepts only a numeric IPv4 endpoint" 2 ;;
  esac
  want_revision=$(git -C "$SCRIPT_DIR" rev-parse "${BRANCH:-HEAD}" 2>/dev/null || true)
  have_revision=$(bridge_revision "$LAN_IP")
  if [ -n "$want_revision" ] && [ "$have_revision" = "$want_revision" ]; then
    step deploy skip "the $ENGINE bridge on $LAN_IP:$PORT already serves ${want_revision:0:12}"
  else
    step deploy run "$DEPLOY_CMD $HOST (bridge reports '${have_revision:-no revision}', this checkout is ${want_revision:0:12})"
    "$DEPLOY_CMD" "$HOST" || fail deploy "$DEPLOY_CMD $HOST returned non-zero"
  fi
fi

# --- step 5: register the actor -------------------------------------------
#
# register-actor.sh is append-only and idempotent: it reads the newest
# revision, no-ops when endpoint and metadata already match, and inserts one
# more revision when they do not. It is CALLED on every run and its own
# answer is what this step reports -- second-guessing the registry from out
# here would be a fifth opinion about a fact the registry already holds.
REGISTER_ARGS=(
  "$ACTOR_KEY" "http://$LAN_IP:$PORT" "$TOKEN_KEY"
  --os-user "$OS_USER"
  --metadata "harness=$ENGINE"
  --metadata "model=$MODEL"
  --metadata "model_endpoint=$MODEL_ENDPOINT"
  --metadata "repository_identity=agentculture/culture-nodes"
)
REGISTER_CMD="$SCRIPT_DIR/register-actor.sh ${REGISTER_ARGS[*]}"

if [ "$DRY_RUN" = 1 ]; then
  step register run "$REGISTER_CMD"
  printf 'cutover: dry run only — nothing was changed on %s. Re-run with --yes to act.\n' "$HOST"
  exit 0
fi

register_out=$("$SCRIPT_DIR/register-actor.sh" "${REGISTER_ARGS[@]}") \
  || fail register "register-actor.sh returned non-zero"
printf '%s\n' "$register_out"
case "$register_out" in
  *unchanged*) step register skip "register-actor.sh reports the newest revision already matches" ;;
  *) step register run "$REGISTER_CMD" ;;
esac

printf 'cutover: %s is online on %s as %s (bridge http://%s:%s, bearer %s)\n' \
  "$ACTOR_KEY" "$HOST_BASE" "$OS_USER" "$LAN_IP" "$PORT" "$TOKEN_KEY"
