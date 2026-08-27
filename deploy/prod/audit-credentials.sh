#!/usr/bin/env bash
# Post-deploy credential audit (task t12, issue #69 item 2).
#
#   audit-credentials.sh <thor|orin>
#
# Compare the env keys this host's compose file DECLARES against what
# ~/.culture-nodes/prod.env on that host actually CONTAINS, classify every
# key, and exit non-zero if a required one is missing or empty.
#
# WHY THIS EXISTS. A FORCE rotation destroyed NODES_ACTOR_CLAUDE_TOKEN and
# nothing reported it for about 18 hours: the running worker held the token in
# memory until its next restart, so the deploy looked clean and the failure
# surfaced later as a 401 policy_denied on company/developer. Task t11 fixed
# the CAUSE — every prod.env write now merges key by key (install-secrets.sh,
# tests/deploy/prodenvmerge_test.go). This is the DETECTOR for whatever goes
# missing next by some other mechanism: a hand edit, a restore from an older
# copy, a lane that was never taught to install a key on this host. The manual
# version of this check was about five lines of shell and found the missing
# token immediately; the point of writing it down is that it now runs every
# time instead of when somebody thinks of it.
#
# It is a DETECTOR, not a gate: it runs at the END of deploy.sh, after the
# stack is already up. A failure here does not roll anything back — it says,
# loudly and while an operator is still watching, that the environment just
# shipped is incomplete.
#
# SECRETS. This script reads a file full of live credentials and reports key
# NAMES. No value is ever printed, and no value ever reaches an argv: the
# remote command is a fixed script that emits `KEY<TAB>set|empty` lines, so
# the values never leave the host at all.
#
# Exit codes follow the repo's policy: 0 ok, 1 a required credential is
# missing (a configuration error), 2 the audit could not run (environment).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# AUDIT_COMPOSE_DIR exists so the tests can point the audit at compose files
# they invent, which is how they prove the declared set is READ rather than
# remembered. Same test seam as install-secrets.sh's CONFIRM_DIR.
COMPOSE_DIR=${AUDIT_COMPOSE_DIR:-$SCRIPT_DIR}

HOST=${1:?usage: audit-credentials.sh <thor|orin>}

case "$HOST" in
  thor*) ROLE=thor ;;
  orin*) ROLE=orin ;;
  *) echo "error: unknown host role: $HOST (expected thor or orin)" >&2
     echo "hint: the audit picks a compose file by role, the same way deploy.sh picks a lane" >&2
     exit 2 ;;
esac

HOST_COMPOSE="$COMPOSE_DIR/compose.$ROLE.yml"
[ -r "$HOST_COMPOSE" ] || {
  echo "error: no readable compose file at $HOST_COMPOSE" >&2
  echo "hint: run this from a checkout of deploy/prod, or set AUDIT_COMPOSE_DIR" >&2
  exit 2
}

# Every compose file, not just this host's: `unknown` means declared by NO
# compose file. The two hosts share one generated secret block, so orin's
# prod.env legitimately holds keys only compose.thor.yml names, and scoping
# `unknown` to one file would report half of a correct prod.env as mystery.
ALL_COMPOSE=()
for f in "$COMPOSE_DIR"/compose.*.yml; do
  [ -r "$f" ] && ALL_COMPOSE+=("$f")
done

# --- what compose declares ------------------------------------------------

# compose_refs <file>... -> "NAME<TAB>kind" per `${...}` reference, where kind
# is what the compose file itself says about needing the key:
#
#   required   ${KEY:?msg}         compose refuses to start without it
#   defaulted  ${KEY:-something}   compose supplies a working value
#   open       ${KEY} / ${KEY:-}   compose says nothing either way
#
# `$$` is compose's own escape (the container's shell expands it, e.g. the
# backup service's `$${POSTGRES_USER:-nodes}`), so it is neutralised first: it
# is not an env-file key at all.
compose_refs() {
  awk '
    {
      line = $0
      gsub(/\$\$/, "@@", line)
      while (match(line, /\$\{[A-Za-z_][A-Za-z0-9_]*(:[-?][^}]*)?\}/)) {
        tok = substr(line, RSTART + 2, RLENGTH - 3)
        line = substr(line, RSTART + RLENGTH)
        colon = index(tok, ":")
        if (colon == 0) { print tok "\topen"; continue }
        name = substr(tok, 1, colon - 1)
        op = substr(tok, colon + 1, 1)
        def = substr(tok, colon + 2)
        if (op == "?") print name "\trequired"
        else if (def == "") print name "\topen"
        else print name "\tdefaulted"
      }
    }
  ' "$@"
}

# One kind per key: the strictest thing any occurrence said.
host_kinds=$(compose_refs "$HOST_COMPOSE" | awk -F'\t' '
  {
    if ($2 == "required" || kind[$1] == "required") kind[$1] = "required"
    else if ($2 == "defaulted" || kind[$1] == "defaulted") kind[$1] = "defaulted"
    else kind[$1] = "open"
  }
  END { for (n in kind) print n "\t" kind[n] }
' | sort)

declared_anywhere=$(compose_refs "${ALL_COMPOSE[@]}" | cut -f1 | sort -u)

# --- the hand-classified keys: ONE list, one reason each ------------------
#
# Everything compose can decide for itself is decided from compose above:
# `${KEY:?}` is required by construction, `${KEY:-value}` works without the
# key by construction. Neither statement needs a human, and neither can drift.
#
# What DOES need a human is `${KEY:-}` — an empty default. Compose writes that
# for every credential, because compose has no way to say "this deployment is
# broken without it" and a hard `:?` would stop the stack rather than report.
# That is exactly the shape of the key the incident destroyed: compose
# tolerated its absence and so did everything else, for 18 hours.
#
# So this list carries those keys and only those, plus overrides where compose
# says something that is true of compose but not of this deployment. Each
# entry says why it is where it is. Lines starting with # are comments.
#
# The classes:
#   required  the service cannot work without it
#   optional  absence is a legitimate deployment choice that CLOSES a feature
#             rather than breaking one — reporting these as failures is how an
#             operator learns to ignore the whole audit
audit_classification() {
  cat <<'EOF'
# --- required ------------------------------------------------------------

# The bearer for the claude-code bridges on spark (company/intake, planner,
# developer, verifier). Externally issued and relayed by install-secrets.sh,
# never minted — so nothing regenerates it if it goes. Without it every
# dispatch to those four actors answers 401 policy_denied, which is the
# incident this whole audit was written for.
NODES_ACTOR_CLAUDE_TOKEN required

# The two codex bridges' bearers. install-secrets.sh installs BOTH on BOTH
# hosts on purpose: either worker may claim a node run for either host's codex
# actor, so a worker missing one 401s on work it legitimately claimed.
NODES_ACTOR_CODEX_THOR_TOKEN required
NODES_ACTOR_CODEX_ORIN_TOKEN required

# The notify bridge's bearer, in its second custody point: the bridge holds
# the same value in ~/.culture-nodes/notify.env. install-secrets.sh mints and
# installs both halves unconditionally, so an absent control-plane copy is not
# a deployment choice — it is loss, and it makes every notify dispatch fail
# authentication with nothing obviously wrong on either host.
NODES_ACTOR_NOTIFY_TOKEN required

# The qwen bridge's bearer (adapters/qwen on spark, registered as
# company/qwen-developer). Required for the same reason the four bearers
# above are: the actor is registered and every dispatch to it presents this
# token, so an absent value is not a closed feature — it is an actor that
# answers 401 on work the worker legitimately claimed, with nothing in the
# run or the ledger naming the cause. Registration does not check that a
# compose line exists for the auth_token_env it accepts (issue #222), which
# is precisely why the audit has to.
NODES_ACTOR_QWEN_TOKEN required

# Not an open default — an override. compose.thor.yml defaults this to the
# literal string "default", which is a bootstrap placeholder rather than a
# namespace id: a worker started with it polls a namespace that does not exist
# and claims nothing, silently, forever. deploy.sh writes the real id before
# the stack comes up, so an absence at audit time is a defect, not a choice.
# (compose.orin.yml already says `:?`; this makes the two hosts agree.)
NODES_NAMESPACE_ID required

# Where the authoritative database is. Task t15 made this a deployment input
# in every profile rather than four inlined copies of one URL, which means
# there is no longer a value to fall back to: unset, compose refuses to render
# at all. Required is therefore a description of what already happens, not a
# policy this audit adds.
NODES_DATABASE_URL required

# The bundled database's password. It stopped being unconditionally required
# when t15 put postgres behind a profile — compose now gives it an open
# default so a deployment pointing at an EXTERNAL database can render without
# supplying a password for a container it never starts. It stays required
# here because a deployment that does run the bundled database and leaves this
# at its default is running its authoritative store on a published default
# credential, which is the one outcome this audit exists to prevent.
POSTGRES_PASSWORD required

# --- optional, closed by default -----------------------------------------

# NOT a credential at all — the git commit this deploy is building the control
# plane image from (task t32, issue #104). It reaches compose from deploy.sh's
# OWN environment (`NODES_BUILD_REVISION=$REVISION docker compose ...`), never
# from prod.env, so an absence here is the normal state for anyone running
# `docker compose up` by hand rather than through the deploy.
#
# What an absence costs is worth stating, because it is quiet: the image is
# built with no revision stamp, GET /v1alpha1/version answers that its
# revision cannot be established, and a live test against it can say what it
# measured but not which code it measured. That is a degraded answer rather
# than a broken deployment, which is exactly what `optional` means here.
NODES_BUILD_REVISION optional


# Off-host backups (task t14, issue #30). Unset is the deployment that keeps
# its dumps only on the host they came from — which is every install without
# an AWS account, and was this deployment until today. Set, each pg_dump is
# also copied to object storage. The backup loop reads BACKUP_S3_BUCKET and
# skips the upload entirely when it is empty, so absence closes the off-host
# copy and breaks nothing; the local seven-dump rotation is unaffected.
BACKUP_S3_BUCKET optional

# The credentials that off-host copy uses, and nothing else. They are optional
# for the same reason BACKUP_S3_BUCKET is: without a bucket there is nothing to
# authenticate to. AWS_REGION carries a default in compose, so it is optional
# even when the other two are set.
AWS_ACCESS_KEY_ID optional
AWS_SECRET_ACCESS_KEY optional
AWS_REGION optional

# The notifier's webhook. Either name enables delivery, neither is invented
# here, and internal/notify.ResolveWebhook is fail-open by design: unset, the
# daemon still runs and journals every lifecycle event as delivery-disabled.
# Absence closes notifications; it breaks nothing.
CULTURE_NODES_WEBHOOK_URL optional
DISCORD_WEBHOOK_URL optional

# The three bearer secrets the attempts-evidence-humans-loops batch added.
# compose.thor.yml says it in as many words: "Optional on purpose:
# closed-by-default means an unset secret leaves that route refusing 401,
# never mounted-authless." An unset one closes a route; it does not break the
# control plane.
NODES_ACTOR_REGISTRATION_TOKEN_SECRET optional
NODES_EVENT_TOKEN_SECRET optional
NODES_ADHOC_RUN_TOKEN_SECRET optional

# The dial-in credential issuance secret (issue #111's dial-in half). Same
# closed-by-default reasoning: unset, POST /v1alpha1/inbound/credentials
# refuses 401 and nothing else changes — a bridge that already holds an
# issued credential keeps dialling, because admission reads the stored
# verifier, not this key. Absence closes issuance; it breaks nothing.
NODES_INBOUND_ISSUANCE_TOKEN_SECRET optional

# Runner-service placement. Unset keeps the in-process CodeRunner path, which
# is a complete deployment — these select the host runner service instead.
# deploy.sh's own comment for the sibling pr-upkeep grants states the same
# principle: "Unset is a legitimate state, not a misconfiguration."
#
# NODES_CODE_RUNNER_NAME joined this list when the compose files stopped
# hardcoding it. Each of the tuple's three keys is individually optional here,
# and that is correct for THIS script — the audit asks whether a key's absence
# breaks the service, and any one of them absent is survivable. What is NOT
# survivable is a PARTIAL tuple, which cmd/nodes/worker.go refuses at startup.
# That is a relationship BETWEEN keys, and this audit is deliberately per-key:
# it reads its declared set from the compose files precisely so it cannot drift
# into encoding rules the compose files do not state. install-secrets.sh is
# where the relationship is enforced, by writing the name only to a host that
# already has the other two.
NODES_CODE_RUNNER_NAME optional
NODES_CODE_RUNNER_REVISION optional
NODES_CODE_RUNNER_ACTOR_ID optional
NODES_RUNNER_SERVICES_FILE optional

# The pr-upkeep sweep grants are the same shape (absent = this host does not
# run the sweep, and the runner boundary refuses that one operation by name).
# They live in ~/.culture-nodes/runner.env rather than prod.env and no compose
# file declares them, so they are normally out of this audit's reach; they are
# classified here so that an operator who does put them in prod.env sees them
# called optional rather than unknown.
PR_UPKEEP_SWEEP_SOURCE_URL optional
PR_UPKEEP_SWEEP_SOURCE_SHA256 optional

# The bundled database's password (task t15 made the database a deployment
# input). It became an OPEN default the moment the bundled postgres service
# moved behind the `bundled-postgres` profile: compose interpolates disabled
# profiles too, so a deployment running against an external database must not
# be made to supply a password nothing reads. It is therefore optional here —
# and NODES_DATABASE_URL, which every profile does read, is the required key
# that replaced it. A bundled-postgres deployment absent this value gets a
# database with no password, which install-secrets.sh is what prevents.
POSTGRES_PASSWORD optional

# The telemetry collector endpoint (task t13, issue #5). Not a credential at
# all: it is an address, and its absence is the OFF state
# (internal/telemetry.New returns NoOp() when it is unset — no exporter, no
# dial). Classified so an operator who sets it sees it called optional rather
# than unknown.
OTEL_EXPORTER_OTLP_ENDPOINT optional
EOF
}

class_of() { # <key> <compose-kind> -> required|optional|unclassified
  local key=$1 kind=$2 hand
  hand=$(audit_classification | awk -v k="$key" '$1 == k { print $2; exit }')
  if [ -n "$hand" ]; then printf '%s' "$hand"; return 0; fi
  case "$kind" in
    required) printf 'required' ;;
    defaulted) printf 'optional' ;;
    # An open default nobody classified. Fail closed: every key of this shape
    # in the two shipped compose files is a credential, and treating a new one
    # as harmless is the assumption that cost 18 hours. One line in the list
    # above, with a reason, clears it.
    *) printf 'unclassified' ;;
  esac
}

# --- what prod.env contains -----------------------------------------------

# Key names and set/empty only. The values stay on the host: they are never
# printed, never returned over the ssh channel, and never in an argv.
# shellcheck disable=SC2016 # every expansion here is for the remote shell
REMOTE_PROD_ENV_KEYS='f="$HOME/.culture-nodes/prod.env"
[ -r "$f" ] || exit 4
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in ""|"#"*) continue ;; esac
  k=${line%%=*}
  [ "$k" = "$line" ] && continue
  case "$k" in ""|*[!A-Za-z0-9_]*) continue ;; esac
  v=${line#*=}
  if [ -n "$v" ]; then printf "%s\tset\n" "$k"; else printf "%s\tempty\n" "$k"; fi
done < "$f"'

# shellcheck disable=SC2029 # the client-side expansion is the point: it puts a FIXED script on the remote argv, with no value in it
prod_keys=$(ssh "$HOST" "$REMOTE_PROD_ENV_KEYS") || {
  echo "error: cannot read ~/.culture-nodes/prod.env on $HOST" >&2
  echo "hint: run deploy/prod/install-secrets.sh $HOST (or check ssh to $HOST), then re-run the audit" >&2
  exit 2
}

declare -A present=()
while IFS=$'\t' read -r key state; do
  [ -n "$key" ] && present["$key"]=$state
done <<< "$prod_keys"

declare -A declared=()
while read -r key; do
  [ -n "$key" ] && declared["$key"]=1
done <<< "$declared_anywhere"

# --- classify -------------------------------------------------------------

missing_required=(); empty_required=(); present_required=()
present_optional=(); absent_optional=(); unclassified_keys=(); unknown_keys=()

while IFS=$'\t' read -r key kind; do
  [ -n "$key" ] || continue
  class=$(class_of "$key" "$kind")
  state=${present[$key]:-absent}
  case "$class" in
    unclassified)
      unclassified_keys+=("$key")
      class=required
      ;;
  esac
  case "$class:$state" in
    required:set)     present_required+=("$key") ;;
    required:empty)   empty_required+=("$key") ;;
    required:absent)  missing_required+=("$key") ;;
    optional:set)     present_optional+=("$key") ;;
    optional:*)       absent_optional+=("$key") ;;
  esac
done <<< "$host_kinds"

# --- credentials that must not be HERE ------------------------------------
#
# The classes above ask "is this key present?". This one asks the opposite,
# and it exists because one credential in this deployment is designed to have
# exactly ONE custody point.
#
# A dial-in credential (issue #111's dial-in half) is presented BY a bridge TO
# the control plane. The control plane never presents it, so it keeps only a
# SHA-256 verifier and needs no plaintext — unlike every NODES_ACTOR_*_TOKEN
# above, each of which legitimately has two copies that a whole lane of
# install-secrets.sh exists to keep in step. One copy is what makes a stale
# rotation impossible rather than merely unlikely, so the only inconsistency
# prod.env can express about such a credential is HOLDING ONE AT ALL — and
# holding one is not harmless here: notify-bridge.service lists prod.env as an
# EnvironmentFile, so a *_DIAL_TOKEN written here would really be read, by a
# bridge that was supposed to read its own file.
#
# That is the two-copies-diverge shape issue #133 is about, caught by name
# instead of discovered when a bridge quietly stops being dispatchable.
forbidden_keys=()
for key in "${!present[@]}"; do
  case "$key" in
    *_DIAL_TOKEN) forbidden_keys+=("$key") ;;
  esac
done

declare -A forbidden=()
for key in ${forbidden_keys[@]+"${forbidden_keys[@]}"}; do forbidden["$key"]=1; done

for key in "${!present[@]}"; do
  [ -n "${declared[$key]:-}" ] && continue
  [ -n "${forbidden[$key]:-}" ] && continue
  unknown_keys+=("$key")
done

# --- report ---------------------------------------------------------------

keys() { # sorted, space-separated; nothing when the list is empty
  [ "$#" -eq 0 ] && return 0
  printf '%s\n' "$@" | sort | tr '\n' ' ' | sed 's/ $//'
}

printf '==> credential audit on %s against %s\n' "$HOST" "$(basename "$HOST_COMPOSE")"
printf '    required: %d present, %d missing, %d empty\n' \
  "${#present_required[@]}" "${#missing_required[@]}" "${#empty_required[@]}"
printf '    optional: %d present, %d absent (closed by default)\n' \
  "${#present_optional[@]}" "${#absent_optional[@]}"
printf '    unknown:  %d present in prod.env, declared by no compose file\n' "${#unknown_keys[@]}"
if [ "${#present_required[@]}" -gt 0 ]; then
  printf '    required (present): %s\n' "$(keys ${present_required[@]+"${present_required[@]}"})"
fi
if [ "${#present_optional[@]}" -gt 0 ]; then
  printf '    optional (present): %s\n' "$(keys ${present_optional[@]+"${present_optional[@]}"})"
fi
if [ "${#absent_optional[@]}" -gt 0 ]; then
  printf '    optional (absent, closed by default): %s\n' "$(keys ${absent_optional[@]+"${absent_optional[@]}"})"
fi
if [ "${#unknown_keys[@]}" -gt 0 ]; then
  # Reported, never removed: prod.env legitimately carries keys compose never
  # mentions (NODES_RUNNER_SECRET is one on both hosts today), and deleting a
  # key nobody could explain is how the incident happened.
  printf '    unknown (declared by no compose file, left untouched): %s\n' "$(keys ${unknown_keys[@]+"${unknown_keys[@]}"})"
  printf '    note: unknown keys are reported only. deploy/prod/remove-secret.sh is the deliberate removal path.\n'
fi

if [ "${#unclassified_keys[@]}" -gt 0 ]; then
  echo "warning: compose declares these with an open default and audit-credentials.sh does not classify them: $(keys ${unclassified_keys[@]+"${unclassified_keys[@]}"})" >&2
  echo "hint: add each to audit_classification() with a comment saying why it is required or optional; until then they are treated as required" >&2
fi

rc=0
if [ "${#forbidden_keys[@]}" -gt 0 ]; then
  echo "error: dial-in credential in prod.env: $(keys ${forbidden_keys[@]+"${forbidden_keys[@]}"})" >&2
  echo "hint: a dial-in credential has exactly ONE custody point — the bridge's own per-bridge file, written by deploy/prod/issue-dialin-credential.sh, which is the only thing that may write one. prod.env is a second copy (and notify-bridge.service reads it), which is the two-copies-diverge shape of issue #133. Remove it with deploy/prod/remove-secret.sh <key> and re-issue that bridge's credential." >&2
  rc=1
fi
if [ "${#missing_required[@]}" -gt 0 ]; then
  echo "error: missing (required): $(keys ${missing_required[@]+"${missing_required[@]}"})" >&2
  rc=1
fi
if [ "${#empty_required[@]}" -gt 0 ]; then
  echo "error: empty (required): $(keys ${empty_required[@]+"${empty_required[@]}"})" >&2
  rc=1
fi
# The "incomplete" summary belongs to the missing/empty findings only: a
# prod.env that carries a key it must NOT carry is wrong in the other
# direction, and its own message above says so.
if [ "${#missing_required[@]}" -gt 0 ] || [ "${#empty_required[@]}" -gt 0 ]; then
  echo "error: ~/.culture-nodes/prod.env on $HOST is incomplete for $(basename "$HOST_COMPOSE")" >&2
  echo "hint: the containers already running hold their credentials in memory and will keep working until they restart — this failure is LATENT, so fix it now: re-run deploy/prod/install-secrets.sh $HOST, or relay the externally-issued value (see that script's NODES_ACTOR_CLAUDE_TOKEN lane)" >&2
fi
if [ "$rc" -ne 0 ]; then
  exit "$rc"
fi

printf 'credential audit passed on %s\n' "$HOST"
