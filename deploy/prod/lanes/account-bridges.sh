# shellcheck shell=bash
# ACCOUNT_BRIDGES_LANE_START -- tests/test_deploy_account_bridges.py runs the
# real deploy.sh against fake hosts and asserts what this lane does per USER;
# keep the marker on the first statement and its mate after the last.
#
# Agents as OS users (#243), the deploy half: every bridge unit is installed
# into an ENGINE ACCOUNT's `systemd --user` instance, not the login user's.
# lanes/unix-user.sh owns the account (root bootstrap, account-side
# provision, the session refusal, the rollback pair); this lane is what
# deploy.sh does with an account once it has one -- ship the archive INTO it,
# stamp + `uv tool install` the bridge there, render its config there, stop
# and disable the login user's unit, start the account's, register the
# account on the actor row, print the rollback pair.
#
# Two hosts shapes, one set of helpers:
#
#   thor / orin   deploy_codex_bridge (deploy.sh) calls account_prepare,
#                 then runs its own steps over `ssh culture-codex@<host>`,
#                 then account_cutover_login_unit + account_register_os_user.
#                 nodes-runner, compose, backups, prod.env stay with the
#                 login user and are not touched here.
#   spark         account_bridges_spark_lane: the whole deploy. Bridge lanes
#                 only -- no image, no compose, no runner, no cutover -- for
#                 culture-claude (developer, planner, verifier, intake) and
#                 culture-qwen (qwen-developer), reached as
#                 culture-<engine>@localhost because spark accepts no ssh to
#                 itself; its login-user half runs locally.
#
# Sourced by deploy.sh AFTER actor-placement.sh and lanes/unix-user.sh, and
# it reads deploy.sh's own helpers at call time: say, REMOTE_DIR, REVISION,
# stamp_revision, assert_unit_healthy, resolve_actor_row_id, SCRIPT_DIR.
#
# Secrets: this lane mints nothing and relays nothing from the operator's
# environment. The two values it carries across a user boundary -- a bridge's
# externally issued auth_token and its registered actor_id, from the login
# user's ~/.config/culture-nodes-bridges/<role>.json on spark -- ride a pipe
# into a python renderer on the account side, never an argv
# (tests/deploy/codexdeploylane_test.go's argv guard reads this file too).
# codex-bridge.env and bridge-push.env under an account are
# install-secrets.sh's to write; this lane only checks they are there.
#
# THOR_HOST is read as ${THOR_HOST:-thor} at call time, never defaulted here:
# lanes/preflight.sh derives it from the deploy target (`deploy.sh thor.lan`)
# after this file is sourced, and a default set now would shadow that.

# account_reachable <target> -- can the operator's key open the account?
# BatchMode so an account that exists but refuses the key fails instead of
# prompting for a password the account does not have.
account_reachable() { # target
  ssh -o BatchMode=yes -o ConnectTimeout=15 "$1" 'id -un' >/dev/null 2>&1
}

# account_ensure_bootstrapped <host> <engine>... -- "already bootstrapped" is
# the normal path: an account the operator's key opens is left alone (the
# root step is a no-op per step anyway; re-copying a refreshed engine
# credential is a hand-typed re-run of the bootstrap, named in the summary).
# A missing account is created inline where `sudo -n` works (thor is
# NOPASSWD) and REFUSED with the hand-typed command where it does not (orin,
# spark) -- the operator bootstraps first and re-runs; #243's every-hand-turn
# rule wants that typed sudo recorded.
account_ensure_bootstrapped() { # host engine...
  local host=$1; shift
  local engine target
  local missing=()
  for engine in "$@"; do
    unix_user_engine_ok "$engine" || return 1
    target=$(unix_user_target "$host" "$engine")
    if account_reachable "$target"; then
      say "engine account culture-$engine on $host: bootstrapped (reachable as $target); root step skipped"
    else
      missing+=("$engine")
    fi
  done
  [ ${#missing[@]} -gt 0 ] || return 0
  case "$host" in
    spark*)
      # No ssh to itself, so no `sudo -n` over ssh either: the root step is
      # typed on this host, from this checkout (spark ships no archive).
      echo "refusing: engine account(s) ${missing[*]} on $host are not bootstrapped (culture-<engine>@localhost does not open with the operator key), and $host cannot run the root step over ssh to itself" >&2
      echo "hint: on $host run   sudo bash $SCRIPT_DIR/lanes/unix-user.sh bootstrap ${missing[*]}" >&2
      echo "hint: then re-run deploy.sh $host; record the typed command on #243 (every-hand-turn rule)" >&2
      return 1 ;;
  esac
  unix_user_bootstrap "$host" "${missing[@]}" || return 1
  for engine in "${missing[@]}"; do
    target=$(unix_user_target "$host" "$engine")
    account_reachable "$target" || {
      echo "bootstrap on $host reported culture-$engine created, but $target still does not open with the operator key; check sshd on $host and the key the bootstrap installed" >&2
      return 1
    }
  done
}

# account_ship_archive <target> -- the account's OWN copy of the shipped
# tree. The login user's $REMOTE_DIR sits in a 0750 home the account cannot
# read, and `uv tool install` needs a source tree to copy from; the archive
# is the same git object the login user got, so the stamp that follows names
# the same revision. Nothing here is a secret, so the pipe is plain.
account_ship_archive() { # target
  local target=$1
  say "shipping $(git rev-parse --short "$REVISION") into $target:$REMOTE_DIR (the account's own copy; the login user's is unreadable from it)"
  git archive --format=tar "$REVISION" | ssh "$target" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -x -C $REMOTE_DIR"
}

# account_prepare <host> <engine> -- everything a bridge install needs an
# account to have: the account (or the refusal), its engine + clones +
# inventory (unix_user_provision, as the account), and the archive.
account_prepare() { # host engine
  local host=$1 engine=$2 target
  account_ensure_bootstrapped "$host" "$engine" || return 1
  unix_user_provision "$host" "$engine" || return 1
  target=$(unix_user_target "$host" "$engine")
  account_ship_archive "$target"
}

# account_install_unit <target> <unit> -- the unit FILE, byte-for-byte, into
# the account's `systemd --user` instance: XDG_RUNTIME_DIR is evaluated on
# the far side from the account's own `id -u`, which is the whole point of
# reaching the host as the account rather than as the login user.
# shellcheck disable=SC2029 # the XDG export is deliberately remote
account_install_unit() { # target unit
  local target=$1 unit=$2
  say "installing $unit.service into $target's systemd --user instance"
  ssh "$target" "$UNIX_USER_XDG; mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/$unit.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart $unit && systemctl --user enable $unit"
  assert_unit_healthy "$target" "$unit"
}

# account_cutover_login_unit <host> <login> <unit>... -- stop and disable
# the login user's copies, behind the session-in-flight refusal (#230
# hand-turn 8: a restart mid-session kills the run and leaves it `running`
# in the ledger). The unit files, configs and env files STAY: the printed
# rollback pair (c32) restores the previous posture without a deploy, and
# `disable` alone is what keeps a reboot from starting both copies on one
# port. A unit that was never installed under the login user (a fresh host)
# is not an error here.
account_cutover_login_unit() { # host login unit...
  local host=$1 login=$2; shift 2
  local unit
  unix_user_session_check "$host" "$login" || return 1
  for unit in "$@"; do
    say "stopping + disabling $unit under $login on $host (file, config and env stay in place for the rollback pair)"
    unix_user_login_exec "$host" "$UNIX_USER_XDG; systemctl --user stop $unit 2>/dev/null || true; systemctl --user disable $unit 2>/dev/null || true"
  done
}

# account_session_guard <host> <engine>... -- the account half of the
# session-in-flight refusal (#249 review, finding 3): after a migration the
# sessions run AS the engine account, so before the login units are stopped
# and the account units restarted, every account this deploy is about to
# restart is asked, as itself, whether a session is in flight. Runs BEFORE
# account_cutover_login_unit so a refusal here stops nothing on either side.
account_session_guard() { # host engine...
  local host=$1; shift
  local engine
  for engine in "$@"; do
    unix_user_account_session_check "$host" "$engine" || return 1
  done
}

# account_psql_thor -- register-actor.sh's PSQL_CMD, pointed at thor's
# database the way resolve_actor_row_id reaches it: the same compose exec on
# the login user's shipped archive, over ssh from wherever deploy.sh runs.
# Exported as a function so register-actor.sh (a separate bash process that
# word-splits PSQL_CMD into argv) finds it; the SQL argument is %q-quoted for
# the remote shell.
account_psql_thor() {
  local args
  args=$(printf ' %q' "$@")
  ssh "${THOR_HOST:-thor}" "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes$args"
}

# account_register_os_user <actor_key> <account> -- merge os_user=<account>
# onto the actor's newest revision (register-actor.sh carries every prior
# key forward; #204 reads it as a lane tag). The endpoint is READ from the
# registry, never typed here: a registration that does not resolve is a
# printed hand command, not a guessed row.
account_register_os_user() { # actor_key account
  local actor_key=$1 account=$2 registration endpoint
  registration=$(actor_registration "$actor_key") || registration=""
  if [ -z "$registration" ]; then
    say "WARNING: $actor_key does not resolve in the actor registry at $NODES_API_URL — os_user=$account NOT recorded; register it by hand on ${THOR_HOST:-thor}: deploy/prod/register-actor.sh $actor_key http://<ipv4>:<port> --os-user $account"
    return 0
  fi
  endpoint=$(printf '%s' "$registration" | cut -d'|' -f3)
  say "registering os_user=$account on $actor_key (metadata merge, new revision only if it changed)"
  export -f account_psql_thor
  if ! THOR_HOST="${THOR_HOST:-thor}" REMOTE_DIR="$REMOTE_DIR" PSQL_CMD=account_psql_thor \
      "$SCRIPT_DIR/register-actor.sh" "$actor_key" "$endpoint" --os-user "$account"; then
    say "WARNING: register-actor.sh could not record os_user=$account on $actor_key (reason above); run by hand on ${THOR_HOST:-thor}: deploy/prod/register-actor.sh $actor_key $endpoint --os-user $account"
  fi
}

# --- spark: the four claude bridges and the qwen bridge -----------------------

# account_role_actor_key <role> -- the registered actor behind a spark role.
account_role_actor_key() { printf 'company/%s' "$1"; }

# The account-side renderer. Reads the template the archive shipped,
# substitutes __HOME__ with the ACCOUNT's home (and __NODES_API_URL__), then
# overlays exactly two keys carried over stdin from the login user's config:
# actor_id (the registered row id the bridge stamps on ledger claims) and
# auth_token (externally issued; the control plane holds the matching
# NODES_ACTOR_*_TOKEN). Nothing else from the old file crosses -- q5: the
# developer session must not carry NODES_HUMAN_DECISION_TOKEN, and that is
# asserted on the rendered document, not assumed from the allowlist.
# Written 0600 under umask 077, prepare-then-replace. The program rides -c
# (built by a here-document into a variable, as issue-dialin-credential.sh
# does) because stdin is already carrying the pair.
# shellcheck disable=SC2016 # every expansion is for the remote shell
ACCOUNT_RENDER_REMOTE='set -eu
umask 077
mkdir -p "$HOME/.config/culture-nodes-bridges" "$HOME/.local/state/culture-nodes-bridges/$ROLE"
prog=$(cat <<"RENDERPY"
import json, os, sys

template, role, api_url = sys.argv[1:4]
home = os.path.expanduser("~")
with open(template) as handle:
    body = handle.read().replace("__HOME__", home).replace("__NODES_API_URL__", api_url)
config = json.loads(body)
carried = json.loads(sys.stdin.read() or "{}")
for key in ("actor_id", "auth_token"):
    value = carried.get(key)
    if isinstance(value, str) and value:
        config[key] = value
engine_env = carried.get("qwen_env")
if isinstance(engine_env, dict) and engine_env:
    merged = dict(config.get("qwen_env") or {})
    merged.update({str(k): str(v) for k, v in engine_env.items()})
    config["qwen_env"] = merged
if not config.get("auth_token"):
    sys.stderr.write("refusing: no auth_token for " + role + " (the bridge binds 0.0.0.0 and refuses to start without one); nothing rendered\n")
    raise SystemExit(3)
if not config.get("actor_id"):
    sys.stderr.write("refusing: no actor_id for " + role + " (the bridge stamps it as origin_actor_id, a FOREIGN KEY into actors); nothing rendered\n")
    raise SystemExit(3)
rendered = json.dumps(config, indent=2, sort_keys=True) + "\n"
if "NODES_HUMAN_DECISION_TOKEN" in rendered:
    sys.stderr.write("refusing: the rendered " + role + " config would carry NODES_HUMAN_DECISION_TOKEN (q5); nothing rendered\n")
    raise SystemExit(3)
dest = os.path.join(home, ".config", "culture-nodes-bridges", role + ".json")
tmp = dest + ".new"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as out:
    out.write(rendered)
os.chmod(tmp, 0o600)
os.replace(tmp, dest)
print("rendered " + dest + " (allowlist " + ", ".join(config.get("repo_allowlist", [])) + ")")
RENDERPY
)
python3 -c "$prog" "$TEMPLATE" "$ROLE" "$NODES_API_URL"'

# account_render_bridge_config <target> <role> <template> [actor_key]
#
# The login user's ~/.config/culture-nodes-bridges/<role>.json is the custody
# point of the externally issued auth_token (install-secrets.sh's claude lane
# relays the control plane's copy FROM it), so the account copy is carried
# from there: two keys, extracted locally by name, piped to the renderer.
# A missing actor_id there is resolved from the registry instead; a missing
# auth_token is a refusal, because a bridge on 0.0.0.0 with no token either
# refuses to start or answers everyone.
#
# The qwen role carries one more thing (#249 review, finding 1): the API-key
# variable its session authenticates with. The login user's
# ~/.qwen/settings.json names it (modelProviders.*[].envKey) and holds its
# value (env.<NAME>); the pair rides the same pipe into the template's
# qwen_env slot, so the account's session carries exactly what the login
# user's did. It is read from THAT FILE, never from this shell's environment:
# the bridge merges qwen_env into every session, and an operator variable
# that happened to share a name would otherwise cross the account boundary.
# A named key with no value is a refusal -- the session would start and
# fail every turn on auth.
account_render_bridge_config() { # target role template [actor_key]
  local target=$1 role=$2 template=$3 actor_key=${4:-$(account_role_actor_key "$2")}
  local source="$HOME/.config/culture-nodes-bridges/$role.json" row_id="" engine_settings=""
  case "$role" in qwen*) engine_settings="$HOME/.qwen/settings.json" ;; esac
  if [ -n "$engine_settings" ] && [ ! -s "$engine_settings" ]; then
    echo "refusing: $engine_settings is missing — it names the API-key variable a qwen session authenticates with (modelProviders.*[].envKey) and holds its value; nothing rendered for $role" >&2
    return 1
  fi
  if [ ! -s "$source" ]; then
    echo "refusing: $source is missing — it is where $role's externally issued auth_token lives (the value the control plane holds as NODES_ACTOR_*_TOKEN), and this lane mints none; nothing rendered for $role" >&2
    return 1
  fi
  if ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$source" 2>/dev/null; then
    echo "refusing: $source is not valid JSON; nothing rendered for $role" >&2
    return 1
  fi
  if [ -z "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("actor_id") or "")' "$source")" ]; then
    row_id=$(resolve_actor_row_id "$actor_key")
    [ -n "$row_id" ] || say "WARNING: $source carries no actor_id and $actor_key has no registered row on ${THOR_HOST:-thor} — the renderer will refuse"
  fi
  say "rendering $role config into $target from deploy/prod/$template (auth_token + actor_id carried from $source, nothing else)"
  # The carried pair rides stdin. ROLE/TEMPLATE/NODES_API_URL are non-secret
  # and ride the argv prefix the way install-secrets.sh prefixes FORCE.
  # shellcheck disable=SC2029 # the prefix is deliberately expanded here
  python3 -c 'import json,sys
cfg = json.load(open(sys.argv[1])); row = sys.argv[2]; settings_path = sys.argv[3]
out = {k: cfg[k] for k in ("actor_id", "auth_token") if isinstance(cfg.get(k), str) and cfg[k]}
if row and "actor_id" not in out: out["actor_id"] = row
if settings_path:
    settings = json.load(open(settings_path))
    values = settings.get("env") if isinstance(settings.get("env"), dict) else {}
    names = sorted({p["envKey"] for ps in (settings.get("modelProviders") or {}).values() if isinstance(ps, list)
                    for p in ps if isinstance(p, dict) and isinstance(p.get("envKey"), str) and p["envKey"]})
    missing = [n for n in names if not isinstance(values.get(n), str) or not values[n]]
    if missing:
        sys.stderr.write("refusing: " + settings_path + " names " + ", ".join(missing) + " as the API-key variable but its env block holds no value for it; the session could not authenticate\n")
        raise SystemExit(3)
    if names: out["qwen_env"] = {n: values[n] for n in names}
print(json.dumps(out))' "$source" "$row_id" "$engine_settings" \
    | ssh "$target" "ROLE='$role'; TEMPLATE='$REMOTE_DIR/deploy/prod/$template'; NODES_API_URL='$NODES_API_URL'; $ACCOUNT_RENDER_REMOTE"
}

# account_bridges_spark_lane <host> -- the whole of `deploy.sh spark`.
# Order: every refusal that can happen (bootstrap, provision, missing auth
# token) happens BEFORE the first login-user unit is stopped, so a refused
# spark deploy leaves the five bridges exactly as they were.
account_bridges_spark_lane() { # host
  local host=$1 login claude qwen target role
  login=$(id -un)
  claude=$(unix_user_target "$host" claude)
  qwen=$(unix_user_target "$host" qwen)

  say "spark: bridge lanes only — no image, compose, runner or cutover on this host (#243)"
  account_ensure_bootstrapped "$host" claude qwen || exit 1
  unix_user_provision "$host" claude || exit 1
  unix_user_provision "$host" qwen || exit 1
  account_ship_archive "$claude"
  account_ship_archive "$qwen"

  # Before the install, never after: `uv tool install` copies (deploy.sh's
  # stamp_revision comment). Each adapter's stamp names this exact revision,
  # and the bridges read it back on /v1/capabilities as install_mode=copy.
  target=$claude
  stamp_revision "$target" claude-code claude_code_bridge
  say "installing the claude-code bridge as a uv tool in $target (archive-independent copy)"
  ssh "$target" "\$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/claude-code"
  target=$qwen
  stamp_revision "$target" qwen qwen_bridge
  say "installing the qwen bridge as a uv tool in $target (archive-independent copy)"
  ssh "$target" "\$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/qwen"

  for role in developer planner verifier intake; do
    account_render_bridge_config "$claude" "$role" "claude-$role.json.template" || exit 1
  done
  account_render_bridge_config "$qwen" qwen-developer qwen-developer.json.template || exit 1

  # The developer's dial-in credential moves with the account (c27): one
  # plaintext custody point, re-issued INTO culture-claude. A failure here
  # is reported with the exact re-run, not fatal -- the unit reads the file
  # with EnvironmentFile=- and still serves outbound dispatch without it.
  say "re-issuing company/developer's dial-in credential into $claude"
  "$SCRIPT_DIR/issue-dialin-credential.sh" company/developer "$claude" \
    || say "WARNING: dial-in re-issue failed (reason above); re-run by hand: deploy/prod/issue-dialin-credential.sh company/developer $claude"

  # The cutover: one session check per account (as the account) and one for
  # the login user, then stop + disable its five units, then start the five
  # under their accounts. Same port per unit either side, so the login copy
  # must be down before the account copy can bind.
  account_session_guard "$host" claude qwen || exit 1
  account_cutover_login_unit "$host" "$login" \
    culture-nodes-claude-developer culture-nodes-claude-planner \
    culture-nodes-claude-verifier culture-nodes-claude-intake \
    culture-nodes-qwen-developer || exit 1
  for role in developer planner verifier intake; do
    account_install_unit "$claude" "culture-nodes-claude-$role"
  done
  account_install_unit "$qwen" culture-nodes-qwen-developer

  for role in developer planner verifier intake; do
    account_register_os_user "$(account_role_actor_key "$role")" culture-claude
  done
  account_register_os_user company/qwen-developer culture-qwen

  account_bridges_summary "$host"
}

# account_bridges_summary <host> -- the rollback pair per migrated unit
# (printed, never run), plus the two hand-turns an account may still need.
account_bridges_summary() { # host
  local host=$1 role
  case "$host" in
    spark*)
      say "spark deploy complete: five bridges running under culture-claude / culture-qwen"
      for role in developer planner verifier intake; do
        unix_user_rollback_pair claude "culture-nodes-claude-$role" "$host"
      done
      unix_user_rollback_pair qwen culture-nodes-qwen-developer "$host"
      say "re-copy a refreshed engine credential into an account: sudo bash $SCRIPT_DIR/lanes/unix-user.sh bootstrap claude qwen   (on $host; idempotent, root)"
      ;;
    *)
      unix_user_rollback_pair codex codex-bridge "$host"
      say "re-copy a refreshed codex credential into culture-codex: sudo bash $REMOTE_DIR/deploy/prod/lanes/unix-user.sh bootstrap codex   (on $host; idempotent, root)"
      ;;
  esac
}
# ACCOUNT_BRIDGES_LANE_END
