#!/usr/bin/env bash
# Generate the production secret set once and install it on thor and orin
# over ssh stdin — no secret ever appears in an argv, a shell history
# line, or this repo (plan t19; credential pattern per spec assumption c8,
# cited from reachy-mini-cli's argv-only ssh discipline).
#
# Usage: install-secrets.sh [thor-host] [orin-host]
# Idempotent per machine: refuses to overwrite an existing prod.env unless
# FORCE_PROD=1, so a re-deploy never silently rotates a live database
# password. The codex-bridge lane (~/.culture-nodes/codex-bridge.env)
# carries its own FORCE_CODEX=1 guard so a re-run never silently rotates a
# live bridge token either; the NODES_ACTOR_CODEX_*_TOKEN lines it adds to
# prod.env are updated in place (or appended) instead, since they mirror
# rather than gate access.
#
# EVERY prod.env write in this file MERGES KEY BY KEY (task t11, issue #69
# item 1). prod.env holds two populations: the six secrets this script
# generates, and roughly eight more that accrete afterwards —
# NODES_NAMESPACE_ID and THOR_IP from deploy.sh, NODES_ACTOR_*_TOKEN from
# this script's own later lanes and from actor registration,
# DISCORD_WEBHOOK_URL and NODES_ACTOR_CLAUDE_TOKEN relayed from outside.
# The prod lane used to `cat >` the whole file from the generated block, so
# an authorized rotation deleted the second population without saying so:
# a FORCE=1 rotation destroyed NODES_ACTOR_CLAUDE_TOKEN and the failure
# stayed latent for ~18 hours (company/developer succeeded at 13:03, then
# answered 401 policy_denied at 06:42 the next morning, after a restart).
# A rotation now replaces only the keys it actually generates.
#
# Merging means nothing here can DELETE a key, which would leave prod.env
# able only to grow. deploy/prod/remove-secret.sh is the explicit removal
# path: it names one key, shows the redacted line it would drop, and takes
# a --yes to act.
#
# The FORCE_PROD guard covers the GENERATED population only. The non-secret
# deployment settings have their own UNGUARDED, add-if-absent lane
# (install_deployment_settings, below the two install_env calls), because the
# guard returns before the merge: a setting added to the generated block after
# a host was provisioned could otherwise reach that host only by rotating every
# secret alongside it (issue #124).
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Shared with deploy.sh: which host serves an actor is resolved in exactly one
# place, so a secret can never be installed on a host the deploy will not use
# (issue #72).
# shellcheck source=deploy/prod/actor-placement.sh
. "$SCRIPT_DIR/actor-placement.sh"
# The engine-account targets (#243): unix_user_target names the ssh target
# that IS an account (culture-<engine>@<host>, @localhost on spark). Sourced
# for that one resolver; the lane's bootstrap/provision verbs are deploy.sh's.
# shellcheck source=deploy/prod/lanes/unix-user.sh
. "$SCRIPT_DIR/lanes/unix-user.sh"
# The timestamped backup behind every runner grant rewrite (task t5, issue
# #253). Shared with deploy.sh so the two lanes that can rewrite
# runner-secrets.env and runner.env leave the same recoverable trail.
# shellcheck source=deploy/prod/lanes/env-backup.sh
. "$SCRIPT_DIR/lanes/env-backup.sh"
# The add-if-absent deployment-settings lane (issue #124), in its own file since
# task t25 pushed this script past the 1000-line source limit. Sourced here,
# CALLED further down where the flow needs it — its body reads $PROD_ENV_MERGE,
# $UI_BASE_URL and $CALLBACK_BASE_URL from this script at call time.
# shellcheck source=deploy/prod/lanes/deployment-settings.sh
. "$SCRIPT_DIR/lanes/deployment-settings.sh"

THOR=${1:-thor}
ORIN=${2:-orin}

# --- the ticket page origin (task t16, spec c10) ---------------------------
#
# NODES_UI_BASE_URL decides whether the page-link comment culture-nodes posts
# on a Jira ticket is clickable. Set nowhere, it rendered `/tickets/SCRUM-N`:
# a path with no origin, which Jira shows as text.
#
# Resolved HERE, once, and installed on BOTH hosts with the SAME value,
# deliberately: orin runs a worker and serves no API, but the engine renders
# this comment inside whichever process minted the run — so a value present
# on thor only would make the link's correctness depend on which worker
# claimed the node, the same divergence #224 records for the actor tokens.
#
# Since the login-from-anywhere cycle (task t19, spec c44) the default is the
# PUBLIC name the page is served under behind Cloudflare Access,
# https://nodes.culture.dev — a link a person off the LAN can open. It is
# therefore NOT derived from $THOR any more: the host this deploy was told
# about is where the API listens for MACHINES (NODES_API_URL,
# NODES_CALLBACK_BASE_URL keep their LAN forms), not where a person is sent.
# See docs/operations/nodes-culture-dev.md, "Step 5", which also names the
# hand-turn this add-if-absent lane needs on a host that already carries the
# old LAN value.
#
# It is a non-secret address, which is why it may ride the argv to the remote
# shell alongside HOST/DB_HOST/PROFILES.
UI_BASE_URL=${NODES_UI_BASE_URL:-}
if [ -n "$UI_BASE_URL" ]; then
  UI_BASE_URL_SOURCE="exported for this run"
else
  UI_BASE_URL="https://nodes.culture.dev"
  UI_BASE_URL_SOURCE="defaulted to the public SSO origin"
fi
# A trailing slash is what an operator types; the renderer trims it too, but a
# normalised value is what the next reader of prod.env sees.
UI_BASE_URL=${UI_BASE_URL%/}

# --- the deployment's own addresses (task t25, issue #135) -----------------
#
# Two values the settings lane writes are facts about THIS deployment rather
# than about this script, and both were literals inside the lane until now.
#
# NODES_CALLBACK_BASE_URL is the origin a bridge posts an attempt result back
# to. Its `thor` is a CONTAINER-resolved name, not this script's ssh target --
# compose.orin.yml resolves it through the extra_hosts entry it builds from
# THOR_IP -- which is why it does NOT follow $THOR the way UI_BASE_URL above
# does, and why an override is the only way to serve the api under another
# name.
#
# NODES_COMPOSE_PROFILES is thor's profile list. deploy/prod/README's
# external-database path tells an operator to remove `bundled-postgres` from it
# by hand-editing prod.env on the host afterwards; that is an operator hand-turn
# of exactly issue #124's shape, and a parameter removes the need for it.
#
# Both default to what this deployment uses today, so an operator who sets
# neither gets a byte-identical prod.env.
CALLBACK_BASE_URL=${NODES_CALLBACK_BASE_URL:-http://thor:18080}
THOR_COMPOSE_PROFILES=${NODES_COMPOSE_PROFILES:-bundled-postgres,backup}

# --- destructive-action confirmation protocol ------------------------------
# Rotating a live secret is irreversible: the old value is gone, and every
# component still holding it keeps working until it restarts and then fails
# auth. This lane therefore refuses a rotation the FIRST time it is asked,
# writes a confirmation file naming exactly what would be destroyed and what
# breaks, and only proceeds once a human (or agent) has READ that file and
# edited its verdict line — and only within a short window, so a stale
# confirmation from last week cannot authorize today's rotation.
#
# Written after a real incident: `FORCE=1` was passed intending to add one
# key to one file, and — because FORCE was a single global switch across
# every lane — it rotated prod.env, codex-bridge.env and runner.secret on a
# live host. Nothing broke immediately (running processes hold their creds in
# memory); the damage was latent until the next restart. Per-lane FORCE_*
# variables fixed the scoping; this protocol makes the remaining destructive
# path unrepeatable-by-accident.
CONFIRM_DIR=${CONFIRM_DIR:-$HOME/.culture-nodes}
CONFIRM_WINDOW_SECONDS=${CONFIRM_WINDOW_SECONDS:-900}   # 15 minutes

# require_destructive_confirmation <lane> <host> <what-breaks>
require_destructive_confirmation() {
  local lane=$1 host=$2 breaks=$3
  local file="$CONFIRM_DIR/CONFIRM-rotate-${lane}-${host}.md"
  mkdir -p "$CONFIRM_DIR"

  if [ -f "$file" ] && grep -qiE '^verdict:[[:space:]]*rotate[[:space:]]*$' "$file"; then
    local age now mtime
    now=$(date +%s); mtime=$(stat -c %Y "$file" 2>/dev/null || echo 0)
    age=$(( now - mtime ))
    if [ "$age" -le "$CONFIRM_WINDOW_SECONDS" ]; then
      rm -f "$file"   # single-use: the next rotation needs its own confirmation
      echo "confirmed rotation of $lane on $host (consumed $file)"
      return 0
    fi
    echo "confirmation in $file is stale (${age}s old, window ${CONFIRM_WINDOW_SECONDS}s) — rewriting it" >&2
  fi

  cat > "$file" <<EOF
# Destructive action requires confirmation

Lane:  ${lane}
Host:  ${host}
When:  $(date -Is)

## What this rotation destroys

${breaks}

The current value is NOT recoverable after rotation. Components already
running keep working until they restart, and then fail authentication — so
the breakage is LATENT, not immediate.

## To proceed

Change the verdict line below from 'hold' to 'rotate', then re-run the same
command within ${CONFIRM_WINDOW_SECONDS} seconds. This file is consumed on
use: a second rotation needs a second confirmation.

verdict: hold
EOF
  echo "REFUSED: rotation of $lane on $host needs confirmation." >&2
  echo "         Read and edit: $file" >&2
  return 1
}

gen() { openssl rand -hex 32; }

# PROD_ENV_MERGE -- the remote half of every prod.env write in this file.
#
# It reads KEY=VALUE lines from its own stdin and, for each one, replaces
# that key's line in ~/.culture-nodes/prod.env if it is already there and
# appends it if it is not. Keys nobody sent are not touched, which is the
# whole point: prod.env accretes keys from deploy.sh, from actor
# registration, and from token relays, and a lane that rewrites the file
# from its own block deletes all of them (see this script's header).
#
# It lives in ONE variable rather than being pasted into each lane. The
# idiom was already here — the single-key actor-token helpers below used
# it — and pasting a third copy is how the copies start disagreeing: the
# trailing-newline guard on the line below was missing from the pasted
# copies, so appending a key to a hand-edited file whose last line had no
# newline concatenated the new assignment onto the old value and destroyed
# it (tests/deploy/prodenvmerge_test.go pins that case).
#
# The replacement is done by REWRITING THE FILE LINE BY LINE in the shell,
# not by `sed -i "s|^${k}=.*|${line}|"`, which is what this loop used to do.
# sed's s/// delimiter is part of the expression, so a value carrying that
# delimiter ends the expression early: with `|` as the delimiter,
#
#   line='NODES_DATABASE_URL=postgres://nodes:pa|ss@thor:5432/nodes'
#   sed -i "s|^${k}=.*|${line}|" prod.env
#   -> sed: unknown option to `s'   (exit 1, file BYTE-IDENTICAL)
#
# How that surfaces depends on how many keys are being merged, and the
# multi-key case is the bad one. This loop runs on the remote side with no
# `set -e`, so a later iteration's exit status overwrites the failed one:
# merging twelve keys where the third carries a pipe skips that key and
# still exits 0, and the lane prints its success line. (A single-key merge
# happens to end on the failed sed, so ssh returns 1 and the local
# `set -euo pipefail` aborts — loud, but the old value is still in place.)
# Both were reproduced before this was changed. Every key survives today
# only by accident — `openssl rand -hex 32` and `-base64 32` emit no pipe.
# The exposed values are the ones this script RELAYS from the operator's
# environment rather than generates, and NODES_DATABASE_URL, whose password
# an external database hands out, is the one most likely to carry one.
# Picking a different delimiter only moves the hazard to another character;
# `case "$cur" in "$k"=*)` has no delimiter at all, and a quoted case
# pattern is literal, so NO value and NO key can collide with it.
#
# The rewrite is written to a sibling temp file and moved into place, so a
# merge interrupted midway leaves the previous prod.env intact instead of a
# half-written one. The temp file is chmod 600 BEFORE it holds any secret.
#
# Single-quoted on purpose: $line, ${k}, $cur, $$ and the command
# substitution are for the remote shell to expand, not this one. Expanding
# "$PROD_ENV_MERGE" into an ssh argv does not re-scan them.
# shellcheck disable=SC2016 # the expansions are deliberately remote
PROD_ENV_MERGE='touch ~/.culture-nodes/prod.env; chmod 600 ~/.culture-nodes/prod.env; if [ -s ~/.culture-nodes/prod.env ] && [ -n "$(tail -c1 ~/.culture-nodes/prod.env)" ]; then echo >> ~/.culture-nodes/prod.env; fi; while IFS= read -r line; do k=${line%%=*}; [ -z "$k" ] && continue; tmp=~/.culture-nodes/prod.env.merge.$$; : > "$tmp"; chmod 600 "$tmp"; found=0; while IFS= read -r cur || [ -n "$cur" ]; do case "$cur" in "$k"=*) printf "%s\n" "$line" >> "$tmp"; found=1;; *) printf "%s\n" "$cur" >> "$tmp";; esac; done < ~/.culture-nodes/prod.env; [ "$found" = 1 ] || printf "%s\n" "$line" >> "$tmp"; mv "$tmp" ~/.culture-nodes/prod.env; done'

# PROD_ENV_URL_CAPTURE / PROD_ENV_URL_REFRESH -- the rotation's other half
# (task t25, issue #133).
#
# NODES_DATABASE_URL carries POSTGRES_PASSWORD inline. The settings lane below
# composes it ON THE HOST, once, add-if-absent, and never revisits it -- so a
# FORCE_PROD rotation used to replace POSTGRES_PASSWORD and leave the URL
# holding the value the database no longer accepts. Both keys present, both
# non-empty, deploy log clean: the api/worker/scheduler containers then fail
# authentication on their NEXT restart, which is the same latency that made the
# NODES_ACTOR_CLAUDE_TOKEN loss an 18-hour outage rather than a deploy failure.
#
# CAPTURE runs before the merge and remembers the pre-rotation password and the
# URL beside it; REFRESH runs after it and rewrites the URL's password -- and
# ONLY its password -- through the same shared merge. Both halves stay on the
# host: neither value crosses the ssh channel, enters an argv, or is echoed.
# Only key names are ever printed.
#
# The refresh is CONDITIONAL, and the condition is proof of authorship: the
# URL's own password must be the value this rotation is replacing. On an
# external-database host (deploy/prod/README's documented path) the URL carries
# a password the provider issued while POSTGRES_PASSWORD is a bundled-database
# credential nothing reads, so rewriting it would point the whole stack at a
# credential no database has ever accepted -- turning the rotation of an unused
# key into an outage. That case is REFUSED BY NAME on stderr instead, which is
# also what an already-divergent pair gets; audit-credentials.sh then reports
# the same pair at the end of the deploy.
#
# shellcheck disable=SC2016 # every expansion here is for the remote shell
PROD_ENV_URL_CAPTURE='oldpw=; url=; if [ -e ~/.culture-nodes/prod.env ]; then while IFS= read -r cur || [ -n "$cur" ]; do case "$cur" in POSTGRES_PASSWORD=*) oldpw=${cur#*=} ;; NODES_DATABASE_URL=*) url=${cur#*=} ;; esac; done < ~/.culture-nodes/prod.env; fi'

# The merge is SPLICED IN here at definition time rather than referenced as
# $PROD_ENV_MERGE, which would expand on the remote side where it does not
# exist. The loop therefore still has exactly one definition in this file.
# shellcheck disable=SC2016 # every expansion here is for the remote shell
PROD_ENV_URL_REFRESH='newpw=; while IFS= read -r cur || [ -n "$cur" ]; do case "$cur" in POSTGRES_PASSWORD=*) newpw=${cur#*=} ;; esac; done < ~/.culture-nodes/prod.env; urlpw=; prefix=; rest=; case "$url" in *@*) creds=${url%%@*}; rest=${url#*@}; case "$creds" in *://*:*) urlpw=${creds##*:}; prefix=${creds%:*} ;; esac ;; esac; if [ -z "$url" ] || [ "$newpw" = "$oldpw" ]; then :; elif [ -n "$oldpw" ] && [ "$urlpw" = "$oldpw" ]; then printf "NODES_DATABASE_URL=%s\n" "$prefix:$newpw@$rest" | { '"$PROD_ENV_MERGE"'; }; echo "refreshed NODES_DATABASE_URL on $HOST so it carries the rotated POSTGRES_PASSWORD"; else echo "REFUSED on $HOST: NODES_DATABASE_URL was left as it is, so it does NOT carry the rotated POSTGRES_PASSWORD. The password embedded in that URL is not the value this rotation replaced, so the URL names a database this script did not provision (the documented external-database path) or the two copies had already diverged. Rewriting it would point the stack at a credential no database accepts, which turns a rotation into an outage. deploy/prod/audit-credentials.sh reports the pair by name; correct the URL by hand, or remove it with deploy/prod/remove-secret.sh NODES_DATABASE_URL and re-run this script to have it composed again." >&2; fi'

POSTGRES_PASSWORD=$(gen)
MINIO_ROOT_PASSWORD=$(gen)
NODES_HUMAN_DECISION_TOKEN_SECRET=$(gen)
NODES_CALLBACK_TOKEN_SECRET=$(gen)
NODES_JIRA_WEBHOOK_SECRET=$(gen)
NODES_JIRA_WEBHOOK_TOKEN=$(gen)
NODES_RUNNER_SECRET_THOR=$(gen)
NODES_RUNNER_SECRET_ORIN=$(gen)

install_env() { # host, content-producing function
  local host=$1 content=$2 rc=0
  # A rotation of the live prod.env is the most destructive thing this script
  # can do, so it goes through the confirmation protocol before the ssh runs.
  if [ "${FORCE_PROD:-0}" = "1" ]; then
    require_destructive_confirmation "prod-env" "$host" \
"Rotates POSTGRES_PASSWORD, MINIO_ROOT_PASSWORD, NODES_HUMAN_DECISION_TOKEN_SECRET
and NODES_CALLBACK_TOKEN_SECRET on ${host}. Only those keys: every other line
of prod.env is merged around, not overwritten.

- PostgreSQL keeps the password from its initdb, so the new value will NOT
  authenticate until the role is altered to match: the api/worker/scheduler
  containers fail to connect on their next restart.
- Outstanding human-decision tokens and attempt callback tokens are
  invalidated; a bridge holding one mid-flight cannot complete its attempt." \
      || return 1
  fi
  # FORCE and HOST are evaluated locally and prefixed into the remote command —
  # ssh does not forward env vars, so a bare ${FORCE:-0} inside the
  # single-quoted remote script would always read 0 on the target, and the
  # URL-refresh messages below would name no host. Both are non-secret (a 0/1
  # gate and an ssh target), which is why they may ride the argv.
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | ssh "$host" "FORCE=${FORCE_PROD:-0}; HOST='$host'; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/prod.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing prod.env (set FORCE_PROD=1 to rotate)" >&2; exit 3; fi; '"$PROD_ENV_URL_CAPTURE; $PROD_ENV_MERGE; $PROD_ENV_URL_REFRESH" || rc=$?
  # exit 3 is the keep-existing refusal — a re-run on a provisioned host
  # must continue to the later lanes (codex tokens), not abort here.
  if [ "$rc" -eq 3 ]; then echo "kept existing prod.env on $host"; return 0; fi
  [ "$rc" -eq 0 ] && echo "installed ~/.culture-nodes/prod.env on $host"
  return "$rc"
}

# This block is the GENERATED population and nothing else: every value in it
# is minted by gen() above, and every one of them is what the FORCE_PROD guard
# in install_env exists to protect.
#
# The NON-secret settings that used to ride along here — POSTGRES_USER,
# POSTGRES_DB, DATABASE_SSLMODE, MINIO_ROOT_USER, NODES_CALLBACK_BASE_URL,
# COMPOSE_PROFILES and NODES_DATABASE_URL — moved to the unguarded
# add-if-absent lane below (issue #124). They were unreachable here: the guard
# returns BEFORE the merge on any host that already has a prod.env, so a key
# added to this block after a host was provisioned could only reach that host
# by rotating every secret in the block along with it. That cost two operator
# hand-turns — thor edited mid-deploy, and orin's deploy aborting outright at
# `error while interpolating services.worker.environment.NODES_DATABASE_URL`.
common="POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
NODES_HUMAN_DECISION_TOKEN_SECRET=${NODES_HUMAN_DECISION_TOKEN_SECRET}
NODES_CALLBACK_TOKEN_SECRET=${NODES_CALLBACK_TOKEN_SECRET}
NODES_JIRA_WEBHOOK_SECRET=${NODES_JIRA_WEBHOOK_SECRET}
NODES_JIRA_WEBHOOK_TOKEN=${NODES_JIRA_WEBHOOK_TOKEN}"

install_env "$THOR" "$common
NODES_RUNNER_SECRET=${NODES_RUNNER_SECRET_THOR}"

install_env "$ORIN" "$common
NODES_RUNNER_SECRET=${NODES_RUNNER_SECRET_ORIN}"

# Announced before the lane runs, because the two branches at the top of this
# file (exported versus defaulted) produce identically-shaped prod.env lines:
# an operator reading the deploy log has no other way to tell the public SSO
# origin the script chose from an origin they exported (a LAN or tailscale
# address for a deployment without the tunnel, say). See deploy/prod/README.md,
# "Ticket page links are the public SSO origin".
echo "ticket page links: NODES_UI_BASE_URL=$UI_BASE_URL ($UI_BASE_URL_SOURCE) — every Jira page-link comment will read $UI_BASE_URL/tickets/<KEY>"

# thor's database is the bundled compose service `postgres`; orin reaches the
# same database as `thor`, the name compose.orin.yml resolves through its
# extra_hosts entry from THOR_IP (containers do not inherit /etc/hosts). These
# are container-resolved database hostnames, NOT this script's ssh targets —
# which is why they are literals here and not "$THOR"/"$ORIN".
install_deployment_settings "$THOR" postgres "$THOR_COMPOSE_PROFILES"
install_deployment_settings "$ORIN" thor ""

# --- dial-in credential issuance bearer (issue #111's dial-in half) --------
#
# The bearer that authorises POST /v1alpha1/inbound/credentials, and NOT the
# credential that route issues. The distinction is the whole point of the
# split, so it is worth stating here rather than only in the sibling script:
#
#   this key        one copy, on the control plane host, read by the api
#                   container. Generated here.
#   the CREDENTIAL  one copy, on the BRIDGE host, in a per-bridge file.
#                   Generated by the control plane with crypto/rand, revealed
#                   once, and written by deploy/prod/issue-dialin-credential.sh
#                   and nothing else. It is not in this file, in this host's
#                   prod.env, or in the repo, and audit-credentials.sh fails
#                   by name if one ever turns up in a prod.env.
#
# Deliberately NOT in the guarded `common=` block above. Two reasons, and the
# second is the one plan task t13 is about:
#
#  - issue #124: the FORCE_PROD guard returns BEFORE the merge, so a key added
#    to that block after a host was provisioned can reach the host only by
#    rotating every secret beside it. That cost two operator hand-turns last
#    cycle.
#  - issue #133: that same block is where a rotation can update one copy of a
#    value and leave another stale. Nothing about dial-in may acquire that
#    shape while #133 is open — and this lane cannot, because it mints
#    add-if-absent and the value it mints has no second copy anywhere.
#
# THOR only: compose.thor.yml is the only file that declares it, because the
# api service runs there. orin runs a worker, which issues nothing.
ISSUANCE_TOKEN_SECRET=$(gen)

install_issuance_secret() { # host
  local host=$1 rc=0
  # Rotating it invalidates no issued credential — a bridge that already holds
  # one keeps dialling, because admission reads the stored verifier and not
  # this key. What it does break is the operator's own issuance lane, silently,
  # if some other copy of the bearer is in use. Same confirmation protocol as
  # every other rotation in this file.
  if [ "${FORCE_ISSUANCE:-0}" = "1" ]; then
    require_destructive_confirmation "issuance-bearer" "$host" \
"Rotates NODES_INBOUND_ISSUANCE_TOKEN_SECRET on ${host}. Only that key.

- Already-issued dial-in credentials are UNAFFECTED: admission reads each
  party's stored verifier, not this bearer.
- Any operator or script holding the previous bearer starts getting 401 from
  POST /v1alpha1/inbound/credentials, with nothing else changing.
- deploy/prod/issue-dialin-credential.sh reads the bearer from this host's
  prod.env on each run, so it picks the new value up by itself." \
      || return 1
  fi
  # The remote script reads its whole stdin before deciding, so the keep path
  # cannot break the local pipeline with EPIPE.
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf 'NODES_INBOUND_ISSUANCE_TOKEN_SECRET=%s\n' "$ISSUANCE_TOKEN_SECRET" \
    | ssh "$host" "FORCE=${FORCE_ISSUANCE:-0}; "'umask 077; mkdir -p ~/.culture-nodes; touch ~/.culture-nodes/prod.env; chmod 600 ~/.culture-nodes/prod.env; payload=$(cat); have=0; while IFS= read -r cur || [ -n "$cur" ]; do case "$cur" in "NODES_INBOUND_ISSUANCE_TOKEN_SECRET="?*) have=1 ;; esac; done < ~/.culture-nodes/prod.env; if [ "$have" = 1 ] && [ "$FORCE" != "1" ]; then exit 3; fi; printf "%s\n" "$payload" | { '"$PROD_ENV_MERGE"'; }' || rc=$?
  if [ "$rc" -eq 3 ]; then
    echo "kept the existing dial-in issuance bearer on $host (set FORCE_ISSUANCE=1 to rotate)"
    return 0
  fi
  [ "$rc" -eq 0 ] && echo "installed NODES_INBOUND_ISSUANCE_TOKEN_SECRET into prod.env on $host"
  return "$rc"
}

install_issuance_secret "$THOR"

# The runner bearer secrets also land as single-purpose files for
# NODES_RUNNER_SECRET_FILE and for the operator's registry entries on the
# control machine (mode 0600, outside the repo). Guarded like prod.env:
# a re-run keeps an existing runner.secret AND its local mirror in sync —
# rotating the remote file while the mirror kept the old value (or vice
# versa) would break the worker registry's secret_file references.
umask 077
mkdir -p "$HOME/.culture-nodes"
install_runner_secret() { # host, secret, mirror-suffix
  local host=$1 secret=$2 suffix=$3 rc=0
  printf '%s\n' "$secret" | ssh "$host" "FORCE=${FORCE_RUNNER:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/runner.secret ] && [ "$FORCE" != "1" ]; then echo "keeping existing runner.secret" >&2; exit 3; fi; cat > ~/.culture-nodes/runner.secret' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing runner.secret on $host (mirror untouched)"; return 0; fi
  [ "$rc" -eq 0 ] || return "$rc"
  printf '%s\n' "$secret" > "$HOME/.culture-nodes/runner-secret.$suffix"
  echo "runner bearer secret installed on $host and mirrored to ~/.culture-nodes/runner-secret.$suffix"
}
install_runner_secret "$THOR" "$NODES_RUNNER_SECRET_THOR" thor
install_runner_secret "$ORIN" "$NODES_RUNNER_SECRET_ORIN" orin

# --- codex-bridge tokens -------------------------------------------------
# Each host's codex-bridge adapter authenticates inbound requests with its
# own bearer token (~/.culture-nodes/codex-bridge.env,
# CODEX_BRIDGE_AUTH_TOKEN). Either worker may dispatch either host's codex
# actor over the LAN, so both prod.env files also carry *both* tokens as
# NODES_ACTOR_CODEX_THOR_TOKEN / NODES_ACTOR_CODEX_ORIN_TOKEN. Same
# discipline as everything above: tokens are generated locally and ride
# ssh stdin only — the remote command string ssh actually executes (its
# argv) never has a token substituted into it, only a fixed script that
# reads the secret material from its own stdin once it's running on the
# target.
CODEX_BRIDGE_TOKEN_THOR=$(openssl rand -base64 32)
CODEX_BRIDGE_TOKEN_ORIN=$(openssl rand -base64 32)

install_codex_bridge_env() { # host, token — this host's own bridge secret
  local host=$1 token=$2
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  local rc=0
  printf 'CODEX_BRIDGE_AUTH_TOKEN=%s\n' "$token" | ssh "$host" "FORCE=${FORCE_CODEX:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/codex-bridge.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing codex-bridge.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/codex-bridge.env' || rc=$?
  if [ "$rc" -eq 3 ]; then
    echo "kept existing codex-bridge.env on $host — NOT updating prod.env actor tokens with the new value" >&2
    return 3
  fi
  return "$rc"
}

update_actor_token_line() { # key, value — update-in-place or append into BOTH prod.envs
  local key=$1 value=$2 host
  for host in "$THOR" "$ORIN"; do
    # shellcheck disable=SC2029 # the remote path is deliberately remote
    printf '%s=%s\n' "$key" "$value" \
      | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; '"$PROD_ENV_MERGE"
    echo "installed $key into prod.env on $host"
  done
}

update_env_line_on_host() { # host, key, value — update-in-place or append into ONE prod.env
  local host=$1 key=$2 value=$3
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s=%s\n' "$key" "$value" \
    | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; '"$PROD_ENV_MERGE"
  echo "installed $key into prod.env on $host"
}

# A kept (pre-existing) bridge token must not have this run's freshly
# generated value pushed into prod.env — only a token that actually landed
# in a codex-bridge.env propagates, so bridge and workers always agree.
rc=0; install_codex_bridge_env "$THOR" "$CODEX_BRIDGE_TOKEN_THOR" || rc=$?
if [ "$rc" -eq 0 ]; then
  echo "installed ~/.culture-nodes/codex-bridge.env on $THOR"
  update_actor_token_line NODES_ACTOR_CODEX_THOR_TOKEN "$CODEX_BRIDGE_TOKEN_THOR"
elif [ "$rc" -ne 3 ]; then exit "$rc"; fi

rc=0; install_codex_bridge_env "$ORIN" "$CODEX_BRIDGE_TOKEN_ORIN" || rc=$?
if [ "$rc" -eq 0 ]; then
  echo "installed ~/.culture-nodes/codex-bridge.env on $ORIN"
  update_actor_token_line NODES_ACTOR_CODEX_ORIN_TOKEN "$CODEX_BRIDGE_TOKEN_ORIN"
elif [ "$rc" -ne 3 ]; then exit "$rc"; fi

# --- codex-bridge token, the ACCOUNT's copy (#243) -------------------------
# Since #243 the codex bridge runs as culture-codex, and its unit reads
# ~/.culture-nodes/codex-bridge.env in THAT home — a 0750 home the login user
# cannot write and the account cannot read the login user's from. The login
# user's file above stays the custody point the FORCE_CODEX guard protects
# (it is also the rollback posture); this step MIRRORS whatever token that
# file ended up with — kept or fresh — into the account, so the bridge and
# the control plane's NODES_ACTOR_CODEX_*_TOKEN copy always agree.
#
# The token crosses two ssh sessions through this script's own pipe: read on
# the login-user side by a fixed command, written on the account side by a
# fixed command reading its stdin. No argv on either hop carries it, and no
# variable here ever holds it.
#
# Guarded like every other bridge file in that nothing is overwritten
# without FORCE_CODEX=1 — but UNLIKE the others, a copy that differs is a
# FAILURE, not a kept file (#249 review, finding 5). The worker dispatches
# with the login copy's token (mirrored into prod.env above) and the
# account's bridge authenticates callers with its own file: an account copy
# that differs is a bridge that rejects every dispatch, and a script that
# reported success over it left that 401 for the next deploy to find. An
# account copy that already matches is left alone; FORCE_CODEX=1 — the same
# switch that rotates the login user's copy — re-syncs it, so a rotation
# carries the account along in the same run. An account that is not
# bootstrapped yet is skipped by name: deploy.sh creates it, and this script
# is re-run after (a first cutover is two runs).
CODEX_BRIDGE_ENV_REL=.culture-nodes/codex-bridge.env

install_codex_account_env() { # host
  local host=$1 target rc=0
  target=$(unix_user_target "$host" codex)
  if ! ssh -o BatchMode=yes -o ConnectTimeout=15 "$target" 'id -un' >/dev/null 2>&1; then
    echo "culture-codex on $host is not bootstrapped or not reachable as $target — skipping the account copy of codex-bridge.env (run deploy.sh $host, or the bootstrap by hand, then re-run this script)" >&2
    return 0
  fi
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  ssh "$host" "cat ~/$CODEX_BRIDGE_ENV_REL" | ssh "$target" "FORCE=${FORCE_CODEX:-0}; "'umask 077; mkdir -p ~/.culture-nodes; new=$(cat); [ -n "$new" ] || { echo "empty codex-bridge.env relayed from the login user" >&2; exit 1; }; if [ -e ~/.culture-nodes/codex-bridge.env ] && [ "$FORCE" != "1" ] && [ "$(cat ~/.culture-nodes/codex-bridge.env)" != "$new" ]; then echo "the account codex-bridge.env DIFFERS from the login user copy and was left as it is (set FORCE_CODEX=1 to re-sync)" >&2; exit 3; fi; printf "%s\n" "$new" > ~/.culture-nodes/codex-bridge.env; chmod 600 ~/.culture-nodes/codex-bridge.env' || rc=$?
  if [ "$rc" -eq 3 ]; then
    echo "error: the codex-bridge.env in $target DIFFERS from $host's login copy — the worker dispatches with the login copy's token, so that bridge rejects every dispatch until the two agree" >&2
    echo "hint: re-run with FORCE_CODEX=1 to re-sync the account copy from the login user's (the same switch that rotates the login copy, so a rotation carries the account along)" >&2
    return 3
  fi
  [ "$rc" -eq 0 ] && echo "mirrored ~/.culture-nodes/codex-bridge.env into $target (mode 600)"
  return "$rc"
}
install_codex_account_env "$THOR"
install_codex_account_env "$ORIN"

# --- merge-gate actor token (login-from-anywhere t11, spec c45) ------------
#
# The merge-gate scripts (scripts/merge-gate.py, scripts/collect-handover.py
# --gate) used to post suite verdicts and gate reports with the HUMAN
# decision secret. They now authenticate as their own registered agent actor,
# company/merge-gate, whose bearer is NODES_ACTOR_MERGE_GATE_TOKEN: the actor
# row's metadata.auth_token_env names this variable
# (deploy/prod/register-actor.sh company/merge-gate <endpoint> NODES_ACTOR_MERGE_GATE_TOKEN),
# and the api service reads it from prod.env to recognise the bearer —
# the same lookup the worker uses for outbound credentials.
#
# Two custody points must agree: the control plane's copy here, and the copy
# the gate runs with (a granted environment value on the lane, or the
# operator's ~/.culture-nodes/operator.env). So an EXISTING value is kept
# unless FORCE_MERGE_GATE=1 — a silent re-mint on every run would leave every
# operator copy stale and every gate post answering 401. Same discipline as
# every lane above: the token rides ssh stdin into the umask-077 merge, never
# an ssh argv.
MERGE_GATE_TOKEN=$(gen)

install_merge_gate_token() { # host
  local host=$1 rc=0
  printf 'NODES_ACTOR_MERGE_GATE_TOKEN=%s\n' "$MERGE_GATE_TOKEN" \
    | ssh "$host" "FORCE=${FORCE_MERGE_GATE:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/prod.env ] && grep -q "^NODES_ACTOR_MERGE_GATE_TOKEN=" ~/.culture-nodes/prod.env && [ "$FORCE" != "1" ]; then echo "keeping existing NODES_ACTOR_MERGE_GATE_TOKEN (set FORCE_MERGE_GATE=1 to rotate)" >&2; exit 3; fi; '"$PROD_ENV_MERGE" || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing NODES_ACTOR_MERGE_GATE_TOKEN on $host"; return 0; fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "installed NODES_ACTOR_MERGE_GATE_TOKEN into prod.env on $host"
}
install_merge_gate_token "$THOR"
install_merge_gate_token "$ORIN"

# --- nodes-notifier webhook (thor only — deploy/prod/compose.thor.yml's
# `notifier` service is the only consumer; task t34) ----------------------
# A Discord (or generic) webhook URL is EXTERNALLY ISSUED (created in
# Discord's own UI, or whatever endpoint DISCORD_WEBHOOK_URL/
# CULTURE_NODES_WEBHOOK_URL names) — this script never invents one, unlike
# the openssl-generated secrets above. It only relays a value the operator
# already exported into THIS SCRIPT'S OWN environment before invoking
# install-secrets.sh (e.g. `CULTURE_NODES_WEBHOOK_URL=https://discord.com/
# api/webhooks/... ./install-secrets.sh`). Left unset, nodes-notifier still
# starts and runs — internal/notify.ResolveWebhook simply reports
# webhook_enabled=false and every lifecycle event is journaled as
# delivery-disabled rather than posted (fail-open, per internal/notify's
# own doc comment) — until a later re-run installs the URL.
if [ -n "${CULTURE_NODES_WEBHOOK_URL:-}" ]; then
  update_env_line_on_host "$THOR" CULTURE_NODES_WEBHOOK_URL "$CULTURE_NODES_WEBHOOK_URL"
elif [ -n "${DISCORD_WEBHOOK_URL:-}" ]; then
  update_env_line_on_host "$THOR" DISCORD_WEBHOOK_URL "$DISCORD_WEBHOOK_URL"
else
  echo "CULTURE_NODES_WEBHOOK_URL/DISCORD_WEBHOOK_URL not set in this script's own environment — skipping (nodes-notifier will run with webhook delivery disabled until this is installed)" >&2
fi

# --- human-inbox bridge + tracker secrets (task t34; host derivation, t10) --
# HUMAN_INBOX_BRIDGE_AUTH_TOKEN is a bearer token generated locally exactly
# like the codex-bridge tokens above — nothing a human chooses, just a
# credential this script mints and installs, same FORCE=1 rotation guard.
# GITHUB_TOKEN is externally issued (a GitHub PAT/App token) and is never
# fabricated here: relayed only when the operator already exported it into
# this script's own environment. Left unset, deploy.sh still installs the
# tracker and it uses GitHub's anonymous public-repository lane.
#
# WHICH HOST gets them is derived, not declared. This lane used to install the
# bridge token on $THOR because a comment said the bridge was thor-only, while
# company/human-ops was registered at another machine's address entirely
# (issue #72) — so the token landed on a host that ran no bridge, and the host
# that did run one was never provisioned by this script at all. The bearer
# token belongs wherever the bridge runs, and the actor's registration is the
# only artifact that says where that is. deploy.sh resolves it the same way,
# through the same library, so the secret and the unit cannot disagree.
HUMAN_INBOX_ACTOR_KEY=${HUMAN_INBOX_ACTOR_KEY:-company/human-ops}
HUMAN_INBOX_BRIDGE_AUTH_TOKEN=$(openssl rand -base64 32)

# human_inbox_secret_host — the host serving HUMAN_INBOX_ACTOR_KEY.
#
# HUMAN_INBOX_HOST overrides it for the bootstrap case only: on a brand-new
# cluster there is no control plane to ask and no actor row to ask about, and
# an operator who knows where the bridge will run can say so. Everything else
# resolves from the registry, and an unresolvable actor installs nothing.
human_inbox_secret_host() {
  if [ -n "${HUMAN_INBOX_HOST:-}" ]; then
    printf '%s' "$HUMAN_INBOX_HOST"
    return 0
  fi
  local registration
  registration=$(actor_registration "$HUMAN_INBOX_ACTOR_KEY") || return 1
  endpoint_address "$(printf '%s' "$registration" | cut -d'|' -f3)"
}

install_human_inbox_env() { # host
  local host=$1 rc=0
  local content="HUMAN_INBOX_BRIDGE_AUTH_TOKEN=${HUMAN_INBOX_BRIDGE_AUTH_TOKEN}"
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    content="${content}
GITHUB_TOKEN=${GITHUB_TOKEN}"
  fi
  # shellcheck disable=SC2029 # the remote path is deliberately remote
  printf '%s\n' "$content" | actor_host_exec "$host" "FORCE=${FORCE_HUMAN_INBOX:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/human-inbox.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing human-inbox.env (set FORCE=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/human-inbox.env' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing human-inbox.env on $host"; return 0; fi
  if [ "$rc" -eq 0 ]; then
    if [ -n "${GITHUB_TOKEN:-}" ]; then
      echo "installed ~/.culture-nodes/human-inbox.env on $host (with GITHUB_TOKEN)"
    else
      echo "installed ~/.culture-nodes/human-inbox.env on $host (no GITHUB_TOKEN — tracker uses anonymous public-repository polling)"
    fi
  fi
  return "$rc"
}

HUMAN_INBOX_TARGET=$(human_inbox_secret_host) || HUMAN_INBOX_TARGET=""
if [ -n "$HUMAN_INBOX_TARGET" ]; then
  echo "$HUMAN_INBOX_ACTOR_KEY is served at $HUMAN_INBOX_TARGET — installing the human-inbox bridge secret there"
  install_human_inbox_env "$HUMAN_INBOX_TARGET"
else
  echo "$HUMAN_INBOX_ACTOR_KEY does not resolve in the actor registry at $NODES_API_URL — skipping the human-inbox bridge secret rather than installing it on a guessed host. Register the actor (deploy/prod/register-actor.sh) and re-run, or set HUMAN_INBOX_HOST=<address> to bootstrap a host before its actor row exists" >&2
fi

# --- bridge git-push credential, relayed not minted -----------------------
# GITHUB_TOKEN_WORKER is externally issued and deliberately distinct from
# the human-inbox tracker's read-only GITHUB_TOKEN above.  It is relayed only
# when the operator exported it into this script's environment.  The target
# is the host serving company/developer according to the actor registry: the
# registration, not a hostname declaration, is authoritative (issue #72).
CLAUDE_PUSH_ACTOR_KEY=${CLAUDE_PUSH_ACTOR_KEY:-company/developer}
install_bridge_push_env() { # host
  local host=$1
  [ -n "${GITHUB_TOKEN_WORKER:-}" ] || {
    echo "GITHUB_TOKEN_WORKER not set in this script's own environment — skipping bridge push credential relay" >&2
    return 0
  }
  printf 'GITHUB_TOKEN_WORKER=%s\n' "$GITHUB_TOKEN_WORKER" \
    | actor_host_exec "$host" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/bridge-push.env; chmod 600 ~/.culture-nodes/bridge-push.env'
  echo "installed mode-600 ~/.culture-nodes/bridge-push.env on the registered $CLAUDE_PUSH_ACTOR_KEY host"
}

CLAUDE_PUSH_REGISTRATION=$(actor_registration "$CLAUDE_PUSH_ACTOR_KEY") || CLAUDE_PUSH_REGISTRATION=""
CLAUDE_PUSH_TARGET=""
if [ -n "$CLAUDE_PUSH_REGISTRATION" ]; then
  CLAUDE_PUSH_TARGET=$(endpoint_address "$(printf '%s' "$CLAUDE_PUSH_REGISTRATION" | cut -d'|' -f3)")
  install_bridge_push_env "$CLAUDE_PUSH_TARGET"
else
  echo "$CLAUDE_PUSH_ACTOR_KEY does not resolve in the actor registry at $NODES_API_URL — skipping GITHUB_TOKEN_WORKER rather than installing it on a guessed host" >&2
fi

# The ENGINE ACCOUNTS' copies (#243, c27): the bridges that push handover
# refs now run as culture-codex (thor, orin) and culture-claude (the
# registered developer host), each with its own ~/.culture-nodes that the
# login user's bridge-push.env above does not reach. Same relay, same
# stdin-only discipline, same mode 600; an account that does not open with
# the operator key is skipped by name, never guessed at.
install_account_push_env() { # target
  local target=$1
  [ -n "${GITHUB_TOKEN_WORKER:-}" ] || return 0
  if ! ssh -o BatchMode=yes -o ConnectTimeout=15 "$target" 'id -un' >/dev/null 2>&1; then
    echo "$target is not bootstrapped or not reachable — skipping its bridge-push.env (bootstrap the account, then re-run this script)" >&2
    return 0
  fi
  printf 'GITHUB_TOKEN_WORKER=%s\n' "$GITHUB_TOKEN_WORKER" \
    | ssh "$target" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/bridge-push.env; chmod 600 ~/.culture-nodes/bridge-push.env'
  echo "installed mode-600 ~/.culture-nodes/bridge-push.env in $target"
}
install_account_push_env "$(unix_user_target "$THOR" codex)"
install_account_push_env "$(unix_user_target "$ORIN" codex)"
if [ -n "$CLAUDE_PUSH_TARGET" ]; then
  # The developer bridge's account: culture-claude on whichever host serves
  # company/developer — @localhost when that host is this machine (spark).
  if address_is_local "$CLAUDE_PUSH_TARGET"; then
    install_account_push_env culture-claude@localhost
  else
    install_account_push_env "culture-claude@$CLAUDE_PUSH_TARGET"
  fi
fi

# --- Jira Cloud read credential, relayed not minted ----------------------
# Jira Cloud REST v3 requires the externally-issued account email AND API
# token as one Basic-auth pair. Refuse a partial pair; relay both over stdin
# to the runner's separate mode-600 EnvironmentFile so deploy.sh can safely
# rewrite its non-secret runner.env without erasing them.
#
# This lane MERGES, for the same reason every prod.env write in this file
# does, and it learned it the same way (task t5, issue #253). runner-secrets.env
# holds two populations: the Jira pair this lane owns, and the sweep
# credentials an operator grants by hand -- GITHUB_TOKEN, SONAR_TOKEN,
# NODES_EVENT_TOKEN, which NO lane in this repo writes. The lane used to
# `cat >` the whole file from its own two lines, so the #243 cutover deploy,
# run by a shell holding no Jira pair, reduced thor's runner-secrets.env to 36
# bytes of empty grants. The runner boundary then refused every sweep attempt
# with `rejected_input: environment_refs names GITHUB_TOKEN, SONAR_TOKEN,
# NODES_EVENT_TOKEN, not set in this worker process's own environment` -- 183
# of 275 runs across the next sixteen hours, on a digest that had completed 92
# times before the deploy.
#
# RUNNER_SECRETS_MERGE is $PROD_ENV_MERGE retargeted at the other file rather
# than a second copy of the loop: the copies of that loop had already drifted
# once (the trailing-newline guard existed in one and not the other), so the
# one place it is written is the one place it can be fixed. Everything the
# original says about why the replacement is a `case` and not a `sed s///`
# applies here unchanged, and applies harder -- these values are relayed from
# outside, not generated, so a value carrying the delimiter is an operator's
# token rather than hex from `openssl rand`.
RUNNER_SECRETS_MERGE=${PROD_ENV_MERGE//prod.env/runner-secrets.env}

install_jira_runner_env() { # host
  local host=$1 exists
  exists=$(ssh "$host" 'if [ -f ~/.culture-nodes/runner-secrets.env ]; then echo yes; else echo no; fi')
  if [ -z "${JIRA_ACCOUNT_EMAIL:-}" ] && [ -z "${JIRA_API_TOKEN:-}" ]; then
    if [ "$exists" = yes ]; then
      # The #253 path. An unset pair is not an instruction to blank the two
      # names -- on a host that already carries the file it is an instruction
      # to do NOTHING, because the names are already granted there (by an
      # earlier deploy, or by hand) and writing empty values over them is
      # exactly the truncation this lane caused.
      #
      # Refusing the WRITE, not the deploy: the file already holds the grants,
      # so there is nothing broken to stop for, and aborting install-secrets.sh
      # here would take the notify and issuance lanes below it down with every
      # deploy that has no Jira configured.
      echo "refusing to rewrite ~/.culture-nodes/runner-secrets.env on $host: JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN are unset in this shell and the file already exists — its current grants (which may include GITHUB_TOKEN, SONAR_TOKEN, NODES_EVENT_TOKEN, granted by hand and written by no lane) were left untouched" >&2
      echo "hint: export JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN to rotate the pair; leaving them unset is not a way to clear them" >&2
      return 0
    fi
    # Grant the NAMES with empty values rather than skipping the file.
    #
    # The runner boundary refuses an operation whose environment_refs name
    # anything absent from the runner process (headspace/bridge.go's
    # resolveEnv) -- deliberately, and before the operation runs. pr-upkeep's
    # sweep node names the Jira pair unconditionally, so on a deployment with
    # no Jira configured the sweep was refused as rejected_input in 1ms and
    # the whole flow failed. Found live: run 01M02J59XEF9RB30ZDTYRD1ADQ, the
    # first pr-upkeep run ever attempted in production.
    #
    # Empty is the honest grant here, not a workaround: sweep.py only reads
    # these when a repository entry carries a `jira_site`, so an unconfigured
    # deployment skips the Jira source entirely and never looks at them. What
    # the boundary needs is for the name to EXIST; what the script needs is to
    # know Jira is off. Both are true of an empty value.
    #
    # Reachable only when the host has NO runner-secrets.env at all, which is
    # a first deploy: there is nothing here to overwrite.
    echo "JIRA_ACCOUNT_EMAIL/JIRA_API_TOKEN not set — granting empty values on $host so the sweep's environment_refs resolve" >&2
    printf 'JIRA_ACCOUNT_EMAIL=\nJIRA_API_TOKEN=\n' \
      | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; cat > ~/.culture-nodes/runner-secrets.env; chmod 600 ~/.culture-nodes/runner-secrets.env'
    return 0
  fi
  if [ -z "${JIRA_ACCOUNT_EMAIL:-}" ] || [ -z "${JIRA_API_TOKEN:-}" ]; then
    echo "JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN must both be set" >&2
    return 1
  fi
  backup_env_file "$host" runner-secrets.env
  printf 'JIRA_ACCOUNT_EMAIL=%s\nJIRA_API_TOKEN=%s\n' "$JIRA_ACCOUNT_EMAIL" "$JIRA_API_TOKEN" \
    | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; '"$RUNNER_SECRETS_MERGE"
  echo "merged the Jira Basic-auth pair into mode-600 runner-secrets.env on $host (every other key in the file was left as it was)"
}

install_jira_runner_env "$THOR"
install_jira_runner_env "$ORIN"

# --- notify actor bridge bearer token (issue #68) -------------------------
#
# The notify bridge is a kind=agent actor the worker dispatches to, so the
# token has TWO custody points that must agree: the bridge reads it from
# ~/.culture-nodes/notify.env, and the worker reads the same value from
# prod.env under the name the actor row's metadata points at
# (NODES_ACTOR_NOTIFY_TOKEN -- internal/worker/registry.go's authTokenEnvOf).
# Both are written here, from one generated value, because a rotation that
# updated only one side would leave every notify dispatch failing
# authentication with nothing obviously wrong on either host.
#
# Same refuse-by-default posture as every other lane: an existing token is
# KEPT unless FORCE_NOTIFY=1, since re-minting it silently breaks dispatch.
NOTIFY_BRIDGE_AUTH_TOKEN=$(openssl rand -base64 32)

install_notify_env() { # host
  local host=$1 rc=0
  printf 'NOTIFY_BRIDGE_AUTH_TOKEN=%s\n' "$NOTIFY_BRIDGE_AUTH_TOKEN" \
    | ssh "$host" "FORCE=${FORCE_NOTIFY:-0}; "'umask 077; mkdir -p ~/.culture-nodes; if [ -e ~/.culture-nodes/notify.env ] && [ "$FORCE" != "1" ]; then echo "keeping existing notify.env (set FORCE_NOTIFY=1 to rotate)" >&2; exit 3; fi; cat > ~/.culture-nodes/notify.env' || rc=$?
  if [ "$rc" -eq 3 ]; then echo "kept existing notify.env on $host"; fi
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 3 ]; then return "$rc"; fi

  # Mirror whatever token the bridge ENDED UP with -- which is the existing
  # one when the guard above kept it, not the value just generated.
  ssh "$host" 'set -e
tok=$(grep "^NOTIFY_BRIDGE_AUTH_TOKEN=" ~/.culture-nodes/notify.env | cut -d= -f2-)
[ -n "$tok" ] || { echo "notify.env carries no NOTIFY_BRIDGE_AUTH_TOKEN" >&2; exit 1; }
touch ~/.culture-nodes/prod.env; chmod 600 ~/.culture-nodes/prod.env
python3 - "$tok" <<PY
import os, sys
path = os.path.expanduser("~/.culture-nodes/prod.env")
token = sys.argv[1]
line = "NODES_ACTOR_NOTIFY_TOKEN=" + token
lines = open(path).read().splitlines()
if any(l.startswith("NODES_ACTOR_NOTIFY_TOKEN=") for l in lines):
    if line in lines:
        print("control-plane copy of the notify token already matches")
    else:
        lines = [line if l.startswith("NODES_ACTOR_NOTIFY_TOKEN=") else l for l in lines]
        open(path, "w").write("\n".join(lines) + "\n")
        print("re-synced the control-plane copy of the notify token")
else:
    lines.append(line)
    open(path, "w").write("\n".join(lines) + "\n")
    print("installed the control-plane copy of the notify token")
PY'
  echo "notify bridge token in place on $host (bridge + control-plane copies agree)"
  return 0
}
install_notify_env "$THOR"

# --- claude-code bridge token, relayed not minted --------------------------
#
# The claude-code bridges run on SPARK (company/intake, planner, developer,
# verifier — their actor rows point at spark's address), while the control
# plane runs on thor. That split is the point: the dashboard's machine and the
# node agents' machines are deliberately separable, and the engine only ever
# resolves an endpoint, never a host.
#
# It also means the credential is EXTERNALLY ISSUED — spark's bridge configs
# already hold it, exactly as GITHUB_TOKEN is issued outside this script. So
# this lane relays a value from its own environment and never invents one; a
# generated token here would simply not match the four bridges and every
# dispatch would 401.
#
# Found live: the actors were registered but this token was never installed,
# so the first cross-machine run failed with
#   auth_or_policy (HTTP 401): actor answered Unauthorized
# — a run that reached spark, was refused, and reported honestly.
#
#   export NODES_ACTOR_CLAUDE_TOKEN=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.config/culture-nodes-bridges/developer.json')))['auth_token'])")
#   deploy/prod/install-secrets.sh thor
install_claude_actor_token() { # host
  local host=$1
  if [ -z "${NODES_ACTOR_CLAUDE_TOKEN:-}" ]; then
    echo "no NODES_ACTOR_CLAUDE_TOKEN in this script's environment — leaving the control plane's copy untouched (the spark claude bridges will keep answering 401 until it is relayed; see this lane's comment)"
    return 0
  fi
  printf '%s' "$NODES_ACTOR_CLAUDE_TOKEN" | ssh "$host" 'set -e
tok=$(cat)
[ -n "$tok" ] || { echo "empty NODES_ACTOR_CLAUDE_TOKEN relayed" >&2; exit 1; }
umask 077; touch ~/.culture-nodes/prod.env; chmod 600 ~/.culture-nodes/prod.env
TOK="$tok" python3 - <<PY
import os
path = os.path.expanduser("~/.culture-nodes/prod.env")
line = "NODES_ACTOR_CLAUDE_TOKEN=" + os.environ["TOK"]
lines = open(path).read().splitlines()
if line in lines:
    print("control-plane copy of the claude actor token already matches")
elif any(l.startswith("NODES_ACTOR_CLAUDE_TOKEN=") for l in lines):
    lines = [line if l.startswith("NODES_ACTOR_CLAUDE_TOKEN=") else l for l in lines]
    open(path, "w").write("\n".join(lines) + "\n")
    print("re-synced the control-plane copy of the claude actor token")
else:
    lines.append(line)
    open(path, "w").write("\n".join(lines) + "\n")
    print("installed the control-plane copy of the claude actor token")
PY'
}
# BOTH hosts, not just thor. Either worker may dispatch either actor, so
# each needs every actor credential its compose file declares — which is why
# the codex lanes above call update_actor_token_line, whose whole body is a
# loop over both hosts. This lane installed to thor alone until task t12's
# credential audit reported NODES_ACTOR_CLAUDE_TOKEN missing from orin's
# prod.env on its first live run, while compose.orin.yml declared it: orin's
# worker would answer 401 policy_denied on every claude node dispatched to
# it, and no deploy would say so. tests/deploy/claudetokenplacement_test.go
# derives the required hosts from the compose files, so a third host is
# covered without editing the test.
install_claude_actor_token "$THOR"
install_claude_actor_token "$ORIN"
