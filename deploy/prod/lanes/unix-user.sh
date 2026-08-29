# shellcheck shell=bash
# UNIX_USER_LANE_START -- tests/test_deploy_unix_user.py executes these
# functions against fake hosts AND fake accounts; keep the marker on the
# first statement and its mate after the last.
#
# Agents as OS users (#243): one Unix account per engine per host --
# culture-codex on thor and orin, culture-claude (shared by the developer,
# planner, verifier and intake bridges) and culture-qwen on spark. The
# account is the confinement: no sudo, no docker group, a 750 home the login
# user's 750 home cannot be read from, and only what its bridge needs inside.
#
# Two verbs, one boundary between them. `unix_user_bootstrap` is the ONLY
# step that needs root (useradd, linger, the operator's key, the copied
# engine credential) and it is a no-op per step on a second run. Everything
# after it -- `unix_user_provision` and, in deploy.sh, the unit install --
# runs over `ssh culture-<engine>@<host>` AS THE ACCOUNT, never sudo, never
# the login user: the lane can therefore not reach /home/<login>/git even by
# accident, which is what keeps the old agent checkouts (their unpushed
# commits, their 30 unmerged branches) harvestable exactly as before (c30).
# On spark, which accepts no key for its own user, the account is reached as
# culture-<engine>@localhost.
#
# The bootstrap runs through `sudo -n` over ssh where that works (thor is
# NOPASSWD); where sudo wants a password (spark, orin) the lane refuses and
# names the command the operator types on the host instead -- this same
# file, EXECUTED rather than sourced: `sudo bash lanes/unix-user.sh bootstrap
# <engine>...`. Both forms run the one root script below, so the hand-typed
# bootstrap and the scripted one cannot drift apart.
#
# Versions are pinned here, not resolved on the host: the engines live in
# each account's own ~/.local/bin (the login users' engines sit inside 0750
# homes an account cannot read), and "which codex is this actor running" has
# to be a fact this file states, not one the installer picked (c24).
UNIX_USER_CODEX_VERSION=0.147.0
UNIX_USER_CLAUDE_VERSION=2.1.251
UNIX_USER_QWEN_VERSION=0.22.0
UNIX_USER_CODEX_RELEASE_BASE=https://github.com/openai/codex/releases/download
UNIX_USER_CLAUDE_INSTALLER=https://claude.ai/install.sh
UNIX_USER_QWEN_INSTALLER=https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh
UNIX_USER_UV_INSTALLER=https://astral.sh/uv/install.sh
# Overridable so the fake-host test can clone from a local bare repo; the
# value is validated before it is spliced into a remote command.
UNIX_USER_REPO_URL=${UNIX_USER_REPO_URL:-https://github.com/agentculture/culture-nodes}

# The lane is sourced by deploy.sh (which defines say) and executed by hand
# for the bootstrap (which does not).
declare -F say >/dev/null 2>&1 || say() { printf '==> %s\n' "$*"; }

# unix_user_engine_ok <engine> -- the three engines this lane knows. Anything
# else is refused BEFORE it reaches a command line: the engine name is
# spliced into useradd, ssh targets and paths.
unix_user_engine_ok() {
  case "$1" in codex|claude|qwen) return 0 ;; esac
  echo "unix-user: unknown engine '$1' (expected codex, claude or qwen)" >&2
  return 1
}

# unix_user_roles <engine> -- the per-role clones the account holds, named
# ~/git/culture-nodes-<role>. codex keeps the existing `agent` name (the name
# nodes-op.sh's actor table and the bridge's repo_allowlist carry); claude
# gets one clone per bridge because four bridges share the account and a
# worktree of the operator's checkout is unreadable from it (c25).
unix_user_roles() {
  case "$1" in
    codex) echo agent ;;
    claude) echo "developer planner verifier intake" ;;
    qwen) echo qwen-developer ;;
  esac
}

# unix_user_target <host> <engine> -- the ssh target that IS the account.
unix_user_target() {
  local host=$1 engine=$2
  case "$host" in
    spark*) printf 'culture-%s@localhost' "$engine" ;;
    *) printf 'culture-%s@%s' "$engine" "$host" ;;
  esac
}

# unix_user_xdg -- the XDG_RUNTIME_DIR export every `systemctl --user` over
# ssh needs, evaluated on the far side for whichever user answers.
UNIX_USER_XDG='export XDG_RUNTIME_DIR=/run/user/$(id -u)'

# unix_user_login_exec <host> <command> -- the LOGIN user's side of a host.
# Plain ssh everywhere except spark, which accepts no key for its own user:
# there the command runs here, from $HOME, exactly as actor-placement.sh's
# local branch does (every relative path in these remote strings is written
# against the login user's home). The account side never takes this path --
# an account is always an ssh target, localhost included.
unix_user_login_exec() { # host command
  case "$1" in
    spark*) (cd "$HOME" && bash -c "$2") ;;
    *) ssh "$1" "$2" ;;
  esac
}

# --- bootstrap (root) ---------------------------------------------------------
#
# The one root script. Positional arguments are engines. It runs under sudo
# on the host (SUDO_USER names the login user whose key and credentials are
# copied), and refuses to run any other way: it never reads /home/<login>/git
# and never writes anywhere but the account it is creating.
#
# Per step, present means no-op: an existing account is not re-added, a key
# already in authorized_keys is not appended twice, a credential whose bytes
# already match is not rewritten. The home mode is ASSERTED after chmod, not
# assumed from login.defs (HOME_MODE 0750 on all three hosts today, but a
# default is not a fact about this account).
UNIX_USER_BOOTSTRAP_ROOT_REMOTE='set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "unix-user bootstrap: must run as root (through sudo) — this is the ONLY root step of the lane" >&2; exit 1; }
login=${UNIX_USER_LOGIN_USER:-${SUDO_USER:-}}
[ -n "$login" ] || { echo "unix-user bootstrap: cannot tell which login user is bootstrapping (run it through sudo, or set UNIX_USER_LOGIN_USER)" >&2; exit 1; }
login_home=$(getent passwd "$login" | cut -d: -f6)
[ -n "$login_home" ] && [ -d "$login_home" ] || { echo "unix-user bootstrap: no home for login user $login" >&2; exit 1; }
pubkey_file=${UNIX_USER_PUBKEY_FILE:-}
if [ -z "$pubkey_file" ]; then
  for c in "$login_home/.ssh/authorized_keys" "$login_home"/.ssh/id_*.pub; do
    [ -s "$c" ] && { pubkey_file=$c; break; }
  done
fi
[ -n "$pubkey_file" ] && [ -s "$pubkey_file" ] || { echo "unix-user bootstrap: no operator public key found ($login_home/.ssh/authorized_keys or an id_*.pub); set UNIX_USER_PUBKEY_FILE" >&2; exit 1; }
[ $# -gt 0 ] || { echo "unix-user bootstrap: no engine named" >&2; exit 1; }
for engine in "$@"; do
  case "$engine" in codex|claude|qwen) ;; *) echo "unix-user bootstrap: unknown engine $engine" >&2; exit 1 ;; esac
done
for engine in "$@"; do
  account=culture-$engine
  if getent passwd "$account" >/dev/null; then
    echo "account $account: present"
  else
    useradd -m -s /bin/bash "$account"
    echo "account $account: created"
  fi
  home=$(getent passwd "$account" | cut -d: -f6)
  [ -n "$home" ] && [ -d "$home" ] || { echo "unix-user bootstrap: $account has no home directory" >&2; exit 1; }
  case "$home" in "$login_home"|"$login_home"/*) echo "unix-user bootstrap: $account home $home is inside the login home $login_home — refusing" >&2; exit 1 ;; esac
  chmod 750 "$home"
  mode=$(stat -c %a "$home")
  [ "$mode" = 750 ] || { echo "unix-user bootstrap: $home is mode $mode after chmod 750" >&2; exit 1; }
  groups=$(id -nG "$account")
  for g in sudo docker wheel admin; do
    case " $groups " in *" $g "*) echo "unix-user bootstrap: $account is in group $g — an account with $g cannot carry the confinement claim; remove it (gpasswd -d $account $g) and re-run" >&2; exit 1 ;; esac
  done
  loginctl enable-linger "$account"
  mkdir -p "$home/.ssh"
  chmod 700 "$home/.ssh"
  touch "$home/.ssh/authorized_keys"
  while IFS= read -r key || [ -n "$key" ]; do
    [ -n "$key" ] || continue
    grep -qxF -- "$key" "$home/.ssh/authorized_keys" || printf "%s\n" "$key" >> "$home/.ssh/authorized_keys"
  done < "$pubkey_file"
  chmod 600 "$home/.ssh/authorized_keys"
  chown -R "$account:$account" "$home/.ssh"
  case "$engine" in
    codex) cred_dir=.codex; cred_file=auth.json ;;
    claude) cred_dir=.claude; cred_file=.credentials.json ;;
    *) cred_dir=; cred_file= ;;
  esac
  if [ -n "$cred_dir" ]; then
    src=$login_home/$cred_dir/$cred_file
    dst=$home/$cred_dir/$cred_file
    if [ -f "$src" ]; then
      mkdir -p "$home/$cred_dir"
      chmod 700 "$home/$cred_dir"
      if cmp -s "$src" "$dst" 2>/dev/null; then
        echo "credential $cred_dir/$cred_file: already in $account"
      else
        cp "$src" "$dst"
        echo "credential $cred_dir/$cred_file: copied into $account"
      fi
      chmod 600 "$dst"
      chown -R "$account:$account" "$home/$cred_dir"
    else
      echo "credential $src: absent on this host — $account will need its own $engine login before its bridge can start"
    fi
  fi
  echo "account $account: home $home mode 750, linger on, key installed, groups: $groups"
done'

# unix_user_bootstrap <host> <engine>... -- the root step, over ssh + sudo -n.
# The script rides the command line as one bash -c argument (printf %q), so
# nothing on the host reads stdin and nothing the operator typed is spliced
# in unvalidated: engines are checked here AND inside the script.
unix_user_bootstrap() {
  local host=$1; shift
  local engine
  [ $# -gt 0 ] || { echo "unix_user_bootstrap: no engine named for $host" >&2; return 1; }
  for engine in "$@"; do unix_user_engine_ok "$engine" || return 1; done
  say "bootstrapping engine account(s) $* on $host (the lane's only root step, via sudo -n)"
  local status=0
  ssh "$host" "sudo -n bash -c $(printf %q "$UNIX_USER_BOOTSTRAP_ROOT_REMOTE") unix-user-bootstrap $*" || status=$?
  case "$status" in
    0) say "bootstrap on $host: done" ;;
    255)
      echo "unix-user bootstrap failed: cannot reach $host (ssh exit 255); nothing was created" >&2
      return 1 ;;
    *)
      echo "unix-user bootstrap failed on $host (exit $status)." >&2
      echo "hint: if sudo asked for a password, this host is a hand-turn (#243): on $host run" >&2
      echo "hint:   sudo bash ${REMOTE_DIR:-culture-nodes-prod}/deploy/prod/lanes/unix-user.sh bootstrap $*" >&2
      echo "hint: then re-run this deploy; record the typed command on #243 per the every-hand-turn rule" >&2
      return 1 ;;
  esac
}

# --- provision (as the account) ------------------------------------------------

# Read-only guard the account runs on itself before anything is installed:
# the confinement facts the deploy is about to rely on (h28, the no-sudo /
# no-docker honesty condition). Mode is measured, not assumed.
UNIX_USER_ACCOUNT_GUARD_REMOTE='set -euo pipefail
me=$(id -un)
[ "$me" = "$ACCOUNT" ] || { echo "refusing: ssh landed as $me, not $ACCOUNT" >&2; exit 3; }
mode=$(stat -c %a "$HOME")
[ "$mode" = 750 ] || { echo "refusing: $HOME is mode $mode, expected 750 (chmod 750 it; the login user must not be able to read an engine account, nor the reverse)" >&2; exit 3; }
groups=$(id -nG)
for g in sudo docker wheel admin; do
  case " $groups " in *" $g "*) echo "refusing: $ACCOUNT is in group $g — an account with $g cannot carry the confinement claim" >&2; exit 3 ;; esac
done
umask 077
mkdir -p "$HOME/.culture-nodes" "$HOME/.local/bin"
chmod 700 "$HOME/.culture-nodes"
echo "account $ACCOUNT: home mode 750, groups: $groups"'

# The engine install, idempotent on the pinned version: an engine already
# answering --version with the pin is left alone (nothing is re-downloaded),
# anything else is (re)installed and then checked again. codex is the
# standalone musl release binary (what ~/.codex/packages/standalone holds on
# orin, without the npm the login user's install went through); claude is
# its native installer at a named version; qwen is its standalone installer,
# which bundles node -- orin has no node, and no account needs one.
UNIX_USER_ENGINE_INSTALL_REMOTE='set -euo pipefail
bin=$HOME/.local/bin/$ENGINE
mkdir -p "$HOME/.local/bin"
have() {
  [ -x "$bin" ] || return 1
  case "$("$bin" --version 2>/dev/null || true)" in *"$VERSION"*) ;; *) return 1 ;; esac
  # codex 0.147 spawns codex-code-mode-host from beside its own binary; a bare
  # binary answers --version and then blocks every session ("failed to spawn
  # code-mode host", run 01M17DN04BTX9JAS3W3H9NJ7ZP) -- so the pin is the
  # package, not the binary.
  if [ "$ENGINE" = codex ]; then [ -x "$(dirname "$(readlink -f "$bin")")/codex-code-mode-host" ] || return 1; fi
  return 0
}
if have; then
  echo "$ENGINE: $VERSION already installed at $bin"
  exit 0
fi
case "$ENGINE" in
  codex)
    case "$(uname -m)" in
      aarch64|arm64) triple=aarch64-unknown-linux-musl ;;
      x86_64) triple=x86_64-unknown-linux-musl ;;
      *) echo "refusing: no codex release for $(uname -m)" >&2; exit 3 ;;
    esac
    # The standalone PACKAGE (bin/codex + bin/codex-code-mode-host +
    # codex-path/rg + codex-resources), laid out exactly as the OpenAI
    # standalone installer lays it out under ~/.codex/packages/standalone, so
    # the account codex is byte-for-byte what the login users run.
    asset=codex-package-$triple.tar.gz
    rel=$HOME/.codex/packages/standalone/releases/$VERSION-$triple
    tmp=$(mktemp -d "$HOME/.local/codex-install.XXXXXX")
    trap "rm -rf \"$tmp\"" EXIT
    curl -fsSL "$CODEX_RELEASE_BASE/rust-v$VERSION/$asset" -o "$tmp/$asset"
    mkdir -p "$tmp/pkg"
    tar -xzf "$tmp/$asset" -C "$tmp/pkg"
    root=$(dirname "$(find "$tmp/pkg" -type f -path "*/bin/codex" | head -n 1)")
    [ -n "$root" ] && [ -x "$root/codex" ] && [ -x "$root/codex-code-mode-host" ] || { echo "refusing: $asset held no bin/codex + bin/codex-code-mode-host pair" >&2; exit 3; }
    root=$(dirname "$root")
    rm -rf "$rel"; mkdir -p "$(dirname "$rel")"
    mv "$root" "$rel"
    ln -sfn "$rel" "$HOME/.codex/packages/standalone/current"
    ln -sfn "$HOME/.codex/packages/standalone/current/bin/codex" "$bin"
    ;;
  claude) curl -fsSL "$CLAUDE_INSTALLER" | bash -s "$VERSION" ;;
  qwen) curl -fsSL "$QWEN_INSTALLER" | bash -s -- --version "$VERSION" ;;
esac
have || { echo "refusing: $bin does not report $VERSION after install ($("$bin" --version 2>&1 | head -n 1))" >&2; exit 3; }
echo "$ENGINE: installed $VERSION at $bin"'

# The codex sandbox posture of an ACCOUNT (#243, deviation d2): the account is
# the fence, so a workspace-write session keeps its file confinement (writes
# stay in the workspace; .git opens only for a handover) and gets NETWORK --
# codex's own default denies egress in workspace-write, which is exactly the
# #230 "Could not resolve host" wall. Rendered into the account's own
# ~/.codex/config.toml once; codex appends its [projects.*] trust entries to
# the same file and this never rewrites those.
UNIX_USER_CODEX_CONFIG_REMOTE='set -euo pipefail
mkdir -p "$HOME/.codex"; chmod 700 "$HOME/.codex"
cfg=$HOME/.codex/config.toml
if [ -f "$cfg" ] && grep -q "^\[sandbox_workspace_write\]" "$cfg"; then
  echo "codex config: sandbox_workspace_write section present in $cfg"
else
  printf "%s\n" "" "[sandbox_workspace_write]" "# #243: the account is the fence; workspace-write keeps its file confinement and gains network" "network_access = true" >> "$cfg"
  chmod 600 "$cfg"
  echo "codex config: sandbox_workspace_write.network_access = true written to $cfg"
fi'

# The per-role clone, mirroring lanes/preflight.sh's agent-checkout states:
# absent -> clone; clean -> fetch + ff-only; dirty or detached or diverged ->
# refuse and change nothing. A refusal here FAILS the provision (unlike
# deploy.sh's codex lane, which warns): the clone is the account's own, no
# operator harvests it by hand, and a diff sitting in it is a session that
# has not handed over yet.
UNIX_USER_CHECKOUT_REMOTE='set -euo pipefail
repo=$HOME/git/culture-nodes-$ROLE
if [ ! -d "$repo/.git" ]; then
  mkdir -p "$HOME/git"
  git clone -q "$REPO_URL" "$repo"
  echo "checkout $repo: cloned"
  exit 0
fi
if [ -n "$(git -C "$repo" status --porcelain)" ]; then
  echo "checkout $repo has uncommitted changes — refusing to touch it (the session that made them has not handed over)" >&2
  exit 3
fi
if ! git -C "$repo" symbolic-ref -q HEAD >/dev/null 2>&1; then
  echo "checkout $repo is DETACHED at $(git -C "$repo" rev-parse --short HEAD) — refusing to touch it" >&2
  exit 3
fi
upstream=$(git -C "$repo" rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null || echo origin/main)
git -C "$repo" fetch -q "${upstream%%/*}" || { echo "checkout $repo: git fetch ${upstream%%/*} failed — leaving it untouched" >&2; exit 3; }
git -C "$repo" merge -q --ff-only "$upstream" || { echo "checkout $repo has diverged from $upstream — refusing to touch it (no rebase, no reset)" >&2; exit 3; }
echo "checkout $repo: fast-forwarded to $upstream ($(git -C "$repo" rev-parse --short HEAD))"'

# The inventory (h29): an engine account holds only what its bridge needs.
# ~/.culture-nodes may contain the bridge env and config, the worker push
# credential, the dial-in credential, the bridge state and the preflight bin
# -- and nothing else. prod.env, runner.env, runner secrets, backups: any of
# those under an account is the operator secret bundle leaking across the
# boundary the account exists to draw, so the lane refuses rather than
# warns. Every env file must be 600 (umask 077 writers). The decision-token
# grep is q5: the developer session must not carry the bearer that makes
# human decisions.
UNIX_USER_INVENTORY_REMOTE='set -euo pipefail
cn=$HOME/.culture-nodes
bad=
for entry in "$cn"/* "$cn"/.[!.]*; do
  [ -e "$entry" ] || continue
  name=${entry##*/}
  case "$name" in
    *-bridge.env|*-bridge.json|bridge-push.env|dialin|*-state|bin) ;;
    *) bad="$bad $name" ;;
  esac
done
[ -z "$bad" ] || { echo "refusing: $cn holds entries outside the engine-account inventory:$bad — an engine account carries its bridge env/config, bridge-push.env, dialin/, *-state/ and bin/ only (never prod.env, runner*, backups)" >&2; exit 3; }
for f in "$cn"/*.env "$cn"/dialin/*.env; do
  [ -f "$f" ] || continue
  m=$(stat -c %a "$f")
  [ "$m" = 600 ] || { echo "refusing: $f is mode $m, expected 600 (write env files under umask 077)" >&2; exit 3; }
done
hits=$(grep -rlE "NODES_DATABASE_URL=|NODES_HUMAN_DECISION_TOKEN" "$cn" "$HOME/.config/culture-nodes-bridges" 2>/dev/null || true)
[ -z "$hits" ] || { echo "refusing: operator-only material under $HOME (prod.env keys or the human decision token NODES_HUMAN_DECISION_TOKEN):" >&2; printf "  %s\n" $hits >&2; exit 3; }
for f in "$HOME"/.ssh/id_*; do
  [ -f "$f" ] || continue
  case "$f" in *.pub) continue ;; esac
  echo "refusing: $f looks like a private key inside an engine account; the operator key goes in authorized_keys only" >&2; exit 3
done
[ ! -e "$HOME/.config/gh" ] || { echo "refusing: $HOME/.config/gh exists — the operator gh credential must not live in an engine account" >&2; exit 3; }
echo "inventory ok: $(ls -A "$cn" | tr "\n" " ")"'

# unix_user_provision <host> <engine> -- everything after root, as the
# account. Order: guard (read-only) -> uv -> engine -> role clones ->
# inventory. Role clones are sequential on purpose: four clones under
# culture-claude share one uv cache, and the plan parks the parallel-install
# lock question rather than measuring it here.
unix_user_provision() {
  local host=$1 engine=$2
  unix_user_engine_ok "$engine" || return 1
  local target account version role status
  target=$(unix_user_target "$host" "$engine")
  account=culture-$engine
  case "$engine" in
    codex) version=$UNIX_USER_CODEX_VERSION ;;
    claude) version=$UNIX_USER_CLAUDE_VERSION ;;
    qwen) version=$UNIX_USER_QWEN_VERSION ;;
  esac
  # Values spliced into remote commands are shapes this lane owns, checked
  # before the splice: a version is a version, a repo URL is URL characters.
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "unix_user_provision: pinned $engine version '$version' is not a version" >&2; return 1; }
  [[ "$UNIX_USER_REPO_URL" =~ ^[A-Za-z0-9._:/@+-]+$ ]] || { echo "unix_user_provision: UNIX_USER_REPO_URL contains characters that will not be spliced into a remote command" >&2; return 1; }

  say "provisioning $account on $host as $target (no root from here on)"
  status=0
  ssh "$target" "ACCOUNT=$account; $UNIX_USER_ACCOUNT_GUARD_REMOTE" || status=$?
  case "$status" in
    0) ;;
    255)
      echo "provision refused: cannot reach $target (ssh exit 255) — run the bootstrap for $engine on $host first (unix_user_bootstrap, or by hand: sudo bash ${REMOTE_DIR:-culture-nodes-prod}/deploy/prod/lanes/unix-user.sh bootstrap $engine), and check the operator key reaches the account" >&2
      return 1 ;;
    *)
      echo "provision refused on $host: $account is not in a confinable state (reason above); nothing was installed" >&2
      return 1 ;;
  esac

  say "ensuring uv in $account's ~/.local/bin"
  ssh "$target" "[ -x \$HOME/.local/bin/uv ] || curl -LsSf $UNIX_USER_UV_INSTALLER | sh" || {
    echo "provision failed on $host: uv install as $account failed" >&2
    return 1
  }

  say "ensuring $engine $version in $account's ~/.local/bin (pinned; standalone install, no node/npm)"
  ssh "$target" "ENGINE=$engine; VERSION=$version; CODEX_RELEASE_BASE=$UNIX_USER_CODEX_RELEASE_BASE; CLAUDE_INSTALLER=$UNIX_USER_CLAUDE_INSTALLER; QWEN_INSTALLER=$UNIX_USER_QWEN_INSTALLER; $UNIX_USER_ENGINE_INSTALL_REMOTE" || {
    echo "provision failed on $host: $engine $version did not install for $account (reason above)" >&2
    return 1
  }

  if [ "$engine" = codex ]; then
    say "ensuring $account's codex sandbox posture: workspace-write keeps file confinement and gains network (config.toml)"
    ssh "$target" "$UNIX_USER_CODEX_CONFIG_REMOTE" || { echo "provision failed on $host: codex config.toml for $account (reason above)" >&2; return 1; }
  fi
  for role in $(unix_user_roles "$engine"); do
    say "checkout ~/git/culture-nodes-$role for $account (clone, or fast-forward a clean tree)"
    ssh "$target" "ROLE=$role; REPO_URL=$UNIX_USER_REPO_URL; $UNIX_USER_CHECKOUT_REMOTE" || {
      echo "provision refused on $host: $account's culture-nodes-$role checkout is not in a deployable state (reason above)" >&2
      return 1
    }
  done

  say "asserting $account's home inventory on $host"
  ssh "$target" "$UNIX_USER_INVENTORY_REMOTE" || {
    echo "provision refused on $host: $account's home holds more than its bridge needs (reason above) — remove the named entries; the lane never deletes" >&2
    return 1
  }
  say "$account provisioned on $host: $engine $version, $(unix_user_roles "$engine" | wc -w | tr -d ' ') checkout(s), inventory asserted"
}

# --- cutover guards ----------------------------------------------------------------

# unix_user_session_check <host> <login-user> -- refuse to stop a login-user
# bridge unit while one of its sessions is in flight. A restart mid-session
# kills the run and leaves it `running` in the ledger (#230 hand-turn 8,
# exit 143). Three states, like preflight's orin probe: pgrep found one (0:
# refuse, or warn under SKIP_SESSION_CHECK=1), found none (1: proceed), or
# ssh never reached the host (anything else: refuse -- unreachable is not
# the same as idle).
unix_user_session_check() {
  local host=$1 login=$2 status=0
  [[ "$login" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "unix_user_session_check: '$login' is not a login user name" >&2; return 1; }
  say "session check: any claude -p / codex exec / qwen session running as $login on $host?"
  unix_user_login_exec "$host" "pgrep -u $login -f '[c]laude -p|[c]odex exec|qwen_bridge[.]qwen_cli' >/dev/null" || status=$?
  case "$status" in
    0)
      if [ "${SKIP_SESSION_CHECK:-0}" = 1 ]; then
        say "WARNING: a session is running as $login on $host and SKIP_SESSION_CHECK=1 — stopping its unit will kill that run mid-session"
        return 0
      fi
      echo "refusing: a session (claude -p / codex exec / qwen) is running as $login on $host; stopping its unit now would kill the run and leave it running in the ledger" >&2
      echo "hint: wait for it to finish (pgrep -u $login -af 'claude -p|codex exec|qwen'), or export SKIP_SESSION_CHECK=1 to accept killing it" >&2
      return 1 ;;
    1)
      say "session check: none running as $login on $host"
      return 0 ;;
    *)
      echo "refusing: cannot reach $host (ssh exit $status) to ask whether a session is in flight as $login; an unreachable host is not an idle one" >&2
      return 1 ;;
  esac
}

# unix_user_rollback_pair <engine> <unit> [host] -- the one-command-per-host
# rollback (c32) for the summary: the login-user unit was stopped and
# disabled but its file, config and env stay in place, so stopping the
# account's unit and starting the login user's restores the previous
# posture without a deploy. Prints; runs nothing. On spark the login half
# is typed on spark itself (no ssh to itself), so it is printed bare.
unix_user_rollback_pair() {
  local engine=$1 unit=$2 host=${3:-$HOST}
  local target login_half
  target=$(unix_user_target "$host" "$engine")
  case "$host" in
    spark*) login_half="$UNIX_USER_XDG; systemctl --user start $unit   (typed on $host as the login user)" ;;
    *) login_half="ssh $host '$UNIX_USER_XDG; systemctl --user start $unit'" ;;
  esac
  say "rollback $unit on $host: ssh $target '$UNIX_USER_XDG; systemctl --user stop $unit' && $login_half"
}

# Executed (not sourced): `sudo bash lanes/unix-user.sh bootstrap <engine>...`
# on the host itself -- the hand-turn form for spark and orin, where sudo
# needs a typed password and cannot be driven over ssh.
if [ "${BASH_SOURCE[0]:-}" = "${0:-}" ]; then
  case "${1:-}" in
    bootstrap)
      shift
      for _engine in "$@"; do unix_user_engine_ok "$_engine" || exit 1; done
      exec bash -c "$UNIX_USER_BOOTSTRAP_ROOT_REMOTE" unix-user-bootstrap "$@" ;;
    *)
      echo "usage: sudo bash ${0} bootstrap <codex|claude|qwen>..." >&2
      exit 1 ;;
  esac
fi
# UNIX_USER_LANE_END
