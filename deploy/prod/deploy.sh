#!/usr/bin/env bash
# Deploy the current checkout to the production pair (plan t19).
#
#   deploy.sh thor          # full control plane + worker + runner host unit
#   deploy.sh orin          # second worker + runner host unit
#   deploy.sh spark         # bridge lanes only: the claude + qwen bridges (#243)
#
# Ships the working tree's HEAD as a git archive over ssh (no push, no
# registry), builds the image on the target (both machines are aarch64 —
# native builds), installs the runner binary + systemd user unit, installs
# the codex actor bridge (host-resident, beside the containerized worker),
# and starts the stack. All ssh invocations are argv-only; secrets never
# ride in argv (install-secrets.sh puts them in ~/.culture-nodes/prod.env
# and ~/.culture-nodes/codex-bridge.env first — run it once before the
# first deploy).
#
# Since #243 every BRIDGE unit is installed into an engine account's own
# `systemd --user` instance (culture-codex on thor/orin; culture-claude and
# culture-qwen on spark), reached as `ssh culture-<engine>@<host>`; the
# control plane, the runner and prod.env stay with the login user. The
# account lanes live in lanes/unix-user.sh and lanes/account-bridges.sh.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Where an actor is served is a fact about its registration, not about this
# script -- actor-placement.sh is the one place that resolves it, shared with
# install-secrets.sh so the two can never disagree (issue #72).
# shellcheck source=deploy/prod/actor-placement.sh
. "$SCRIPT_DIR/actor-placement.sh"
# The engine-account lanes (#243): the account itself, and what a deploy
# does with one. Sourced before any host step so the spark arm and the
# codex lane share exactly one set of helpers.
# shellcheck source=deploy/prod/lanes/unix-user.sh
source "$SCRIPT_DIR/lanes/unix-user.sh"
# shellcheck source=deploy/prod/lanes/account-bridges.sh
source "$SCRIPT_DIR/lanes/account-bridges.sh"
# The timestamped backup behind every runner grant rewrite (task t5, issue
# #253), shared with install-secrets.sh. Sourced before any lane so the
# runner.env lane below has it in scope.
# shellcheck source=deploy/prod/lanes/env-backup.sh
source "$SCRIPT_DIR/lanes/env-backup.sh"

HOST=${1:?usage: deploy.sh <thor|orin|spark>}
REMOTE_DIR="culture-nodes-prod"
BRANCH=${BRANCH:-HEAD}
REVISION=$(git rev-parse "$BRANCH")

say() { printf '==> %s\n' "$*"; }

# --- thor / orin only: the control plane, its image, the runner host unit --
# spark runs bridge lanes only (its arm in the case at the bottom is the
# whole deploy), so nothing below this line runs there: no preflight against
# a login-user agent checkout it does not have, no archive to its login user,
# no image, no runner binary or unit, no runner.env.
if [[ "$HOST" != spark* ]]; then
  # --- preflight (task t2, spec c25/c28, PR #236 Qodo finding 6) -------------
  # shellcheck source=deploy/prod/lanes/preflight.sh
  source "$SCRIPT_DIR/lanes/preflight.sh"
  # --- grant check (task t5, issue #253) ------------------------------------
  # Still inside preflight's contract — read-only, before the archive, the
  # image build and any `docker compose stop` — because a host missing a
  # grant a startable workflow declares must be refused while there is still
  # nothing to undo. Last of the preflight lanes: it diffs against runner.env,
  # so the checkout and doctor refusals above (which say something is wrong
  # with the host itself) come first.
  # shellcheck source=deploy/prod/lanes/grant-check.sh
  source "$SCRIPT_DIR/lanes/grant-check.sh"

  say "shipping $(git rev-parse --short "$REVISION") to $HOST:$REMOTE_DIR"
  git archive --format=tar "$REVISION" | ssh "$HOST" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -x -C $REMOTE_DIR"

  say "building control-plane image on $HOST (native aarch64)"
  # --build-arg REVISION: the image is built from the shipped tar, which carries
  # no .git, so the binary has no way to discover what it is (task t32, issue
  # #104). Passed here and again through compose's own build args below, because
  # the two build the same Dockerfile by different routes and a revision stamped
  # into only one of them is a control plane whose answer depends on which lane
  # last rebuilt it. VERSION is deliberately left at its Dockerfile default: it
  # is the package's declared version, not this deploy's commit, and conflating
  # the two would make GET /v1alpha1/version report a sha twice.
  #
  # --label culture-nodes.revision: the same fact, readable from OUTSIDE the
  # process (task t2, spec c26). The api serves its revision on
  # GET /v1alpha1/version, but a worker has no HTTP surface at all, so the only
  # revision-bearing fact a running worker exposes is the label on the image its
  # container was created from — `docker inspect` reads it back in the parity
  # check below. The label rides the same `docker build` as the build arg, so
  # the two cannot name different commits. This is also why the compose `up`
  # calls below no longer pass --build: a compose rebuild would re-tag
  # culture-nodes:prod with an image that carries the arg but not the label,
  # and the parity check would read an empty label off a correct binary.
  ssh "$HOST" "cd $REMOTE_DIR && docker build -q --build-arg REVISION=$REVISION --label culture-nodes.revision=$REVISION -t culture-nodes:prod ."

  say "building nodes-runner host binary on $HOST"
  # Issue #17. Two things were wrong here, and only one of them was the shell.
  #
  # 1. The destination is the binary of the RUNNING nodes-runner unit, so
  #    writing it in place fails with ETXTBSY ("scp: dest open ... Failure",
  #    observed on the 2026-08-11 thor deploy). Both paths below therefore
  #    write `nodes-runner.new` and RENAME over the target: a rename is fine
  #    while the old inode is still executing, and needs no unit stop.
  # 2. The failure was swallowed. The fallback used to be a `{ ... }` group on
  #    the right of `||` containing an `&&` chain; every command in an `&&`
  #    list except the last runs with -e ignored, and bash does not exit when
  #    a compound command returns non-zero because a command failed while -e
  #    was being ignored. So the deploy carried on and restarted the unit on
  #    the previous build. `if ! <cond>; then <body>; fi` exempts only the
  #    CONDITION, so every command in the body below is back under `set -e`
  #    and a failed ship aborts the deploy where it happens.
  if ! ssh "$HOST" "bash -lc 'cd $REMOTE_DIR && go build -o ~/.culture-nodes/bin/nodes-runner.new ./cmd/nodes-runner'"; then
    echo "remote Go missing — building here and copying (same arch)"
    go build -o /tmp/nodes-runner ./cmd/nodes-runner
    ssh "$HOST" 'mkdir -p ~/.culture-nodes/bin'
    scp -q /tmp/nodes-runner "$HOST":.culture-nodes/bin/nodes-runner.new
    rm -f /tmp/nodes-runner
  fi
  ssh "$HOST" 'mv -f ~/.culture-nodes/bin/nodes-runner.new ~/.culture-nodes/bin/nodes-runner'

  # The cutover adopter is deliberately a host one-shot: it briefly reads the
  # runner's Jira Basic-auth pair, which must never enter any long-lived
  # control-plane container. It ships as its own binary (nodes-cutover) beside
  # nodes-runner — NOT as ./cmd/nodes, which deviation d1 removed from this
  # lane (tests/deploy/codexdeploylane_test.go): the host query CLI stays the
  # Python nodes package.
  say "building nodes-cutover one-shot host binary on $HOST"
  if ! ssh "$HOST" "bash -lc 'cd $REMOTE_DIR && go build -o ~/.culture-nodes/bin/nodes-cutover.new ./cmd/nodes-cutover'"; then
    echo "remote Go missing — building nodes-cutover here and copying (same arch)"
    go build -o /tmp/nodes-cutover ./cmd/nodes-cutover
    scp -q /tmp/nodes-cutover "$HOST":.culture-nodes/bin/nodes-cutover.new
    rm -f /tmp/nodes-cutover
  fi
  ssh "$HOST" 'mv -f ~/.culture-nodes/bin/nodes-cutover.new ~/.culture-nodes/bin/nodes-cutover'

  say "ensuring headspace CLI on $HOST (uv tool)"
  ssh "$HOST" 'bash -lc "command -v headspace >/dev/null || { command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install headspace-cli || uv tool install headspace-cli; }; command -v headspace"'

  say "installing runner env + systemd user unit on $HOST"
  # shellcheck source=deploy/prod/lanes/runner-env-write.sh
  source "$SCRIPT_DIR/lanes/runner-env-write.sh"
  ssh "$HOST" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  ssh "$HOST" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/nodes-runner.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart nodes-runner && systemctl --user enable nodes-runner"
  ssh "$HOST" 'export XDG_RUNTIME_DIR=/run/user/$(id -u); for i in $(seq 1 15); do st=$(systemctl --user is-active nodes-runner || true); [ "$st" = active ] && { echo "runner: active"; exit 0; }; sleep 2; done; echo "runner failed to become active:"; systemctl --user --no-pager -n 10 status nodes-runner; exit 1'
fi

# --- codex actor bridge lane (plan t3; as the culture-codex account since #243)
# Both thor and orin run their own managed codex actor (company/codex-thor,
# company/codex-orin), so this lane is host-agnostic and runs once per
# deploy for whichever host was named — it deliberately lives outside the
# case below rather than being duplicated into each branch.
#
# The agent checkout (~/git/culture-nodes-agent) is the ACCOUNT's own since
# #243 and is provisioned by lanes/unix-user.sh (clone when absent, fast-
# forward when clean, REFUSE when dirty or diverged — the account's clone is
# nobody's harvest workspace, so a diff in it is a session that has not
# handed over, and the deploy stops rather than warns). The login user's old
# checkout is never touched: it stays harvestable exactly as before.

# stamp_revision <host> <adapter-dir-name> <package-dir-name> — record WHICH
# REVISION this deploy is about to install into a bridge (task t32, issue #120
# item 4).
#
# The problem it closes is not hypothetical. Three dispatches this cycle
# reported handover=true, committed successfully, and created no handover ref,
# because the bridges on thor and orin predated the code that mints them.
# Nothing reported a problem: internal/handover correctly records nothing when
# there is no fetchable ref, so a stale bridge and an honest refusal produce
# byte-identical evidence. It took `git for-each-ref` over ssh to notice, and
# the reason nothing else could was that no bridge could say what it was
# running.
#
# THIS SCRIPT is the only party that knows. It resolved $REVISION, it shipped
# that exact tree, and `uv tool install` is about to copy it into a venv that
# carries no git and no branch — so if the answer is not written down here it
# does not exist anywhere. The bridge reads it back through
# `<pkg>.deployment.measure_deployment` and advertises it on /v1/capabilities,
# which is what makes "is the fleet current?" a query instead of an ssh.
#
# Two things about it are load-bearing:
#
#   * it runs BEFORE the install, because `uv tool install` COPIES. A stamp
#     written afterwards lands only in the shipped archive that the next
#     deploy's `rm -rf` deletes.
#   * it writes the RESOLVED 40-hex $REVISION, never $BRANCH and never an
#     abbreviation. The bridge refuses anything else (deployment.
#     _full_commit_sha), for the reason internal/handover's validateFullSHA
#     states: a name that means something different tomorrow is not a record.
#
# One helper for every lane rather than an inlined write per lane: three
# copies of one write is exactly how resolve_actor_row_id shipped as the same
# bug in three deploy lanes. tests/deploy/revisionstamp_test.go pins both the
# ordering and the sha shape, and builds a real wheel to check the stamp is
# not silently dropped at build time.
stamp_revision() { # host adapter package
  local host=$1 adapter=$2 package=$3
  ssh "$host" "cat > $REMOTE_DIR/adapters/$adapter/src/$package/_revision.json" <<EOF
{
  "revision": "$REVISION",
  "branch": "$BRANCH",
  "stamped_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source": "deploy/prod/deploy.sh"
}
EOF
  say "stamped $adapter with revision $(git rev-parse --short "$REVISION") on $host"
}

deploy_codex_bridge() { # host — runs identically on thor and orin, AS culture-codex
  local host=$1
  local target remote_home login
  # Every account-side step below addresses the ACCOUNT, never the login
  # user: `~` and $HOME on the far side are /home/culture-codex, so the
  # same relative paths the login-user lane used now land in the account.
  target=$(unix_user_target "$host" codex)

  # The account itself (bootstrapped, or the refusal naming the hand-turn),
  # its pinned codex + uv + clone + inventory, and its own archive copy.
  account_prepare "$host" codex || exit 1

  # install-secrets.sh owns the bridge bearer token exactly as it owns
  # prod.env; deploy.sh only consumes it. Its account step mirrors the login
  # user's copy into the account once the account exists — so a FIRST
  # cutover is two runs by design: this one creates and provisions the
  # account, install-secrets.sh fills it, the re-run installs the unit.
  ssh "$target" 'test -f ~/.culture-nodes/codex-bridge.env' \
    || { echo "~/.culture-nodes/codex-bridge.env missing in $target — run deploy/prod/install-secrets.sh (its culture-codex step mirrors the bridge token into the account), then re-run this deploy; nothing on $host was stopped" >&2; exit 1; }

  # Before the install, never after: `uv tool install` copies (see below).
  stamp_revision "$target" codex codex_bridge

  say "installing the codex-bridge uv tool in $target (archive-independent)"
  # `uv tool install` — NOT --editable, and not `uv run --project` — builds
  # the package and COPIES it into its own tool venv under ~/.local/share/uv,
  # so ~/.local/bin/codex-bridge keeps serving after the next deploy's
  # `rm -rf $REMOTE_DIR` deletes the tree it was installed from (c21/h19).
  # An editable install would keep pointing at the archive and break there.
  # The account's uv is the one unix_user_provision put in its ~/.local/bin.
  ssh "$target" "\$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/codex"

  say "installing the nodes query CLI in $target (PyPI culture-nodes)"
  # Deviation d1: the host-side query CLI is the PYTHON `nodes` CLI from
  # PyPI, not the Go cmd/nodes binary — cmd/nodes has no query verbs, so
  # there is nothing to build or scp here.
  ssh "$target" "\$HOME/.local/bin/uv tool install --force culture-nodes"

  say "installing codex preflight + bridge config in $target"
  ssh "$target" "umask 077; mkdir -p ~/.culture-nodes/bin ~/.culture-nodes/codex-bridge-state && cp $REMOTE_DIR/deploy/prod/codex-preflight.sh ~/.culture-nodes/bin/codex-preflight.sh && chmod +x ~/.culture-nodes/bin/codex-preflight.sh"
  # Same generate-absolute-paths-at-install-time technique runner.env uses:
  # the substitution runs on the TARGET, so __HOME__ becomes the ACCOUNT's
  # own $HOME — /home/culture-codex — which is what makes codex_bin,
  # CODEX_HOME, the state dir and the repo_allowlist all point inside the
  # account. (The bridge config is JSON, not a systemd unit — %h would mean
  # nothing inside it.)
  ssh "$target" "umask 077; sed \"s|__HOME__|\$HOME|g\" $REMOTE_DIR/deploy/prod/codex-bridge.json.template > ~/.culture-nodes/codex-bridge.json"

  # The bridge stamps its actor_id as origin_actor_id on every proposed
  # ledger claim, and ledger_records.origin_actor_id is a FOREIGN KEY into
  # actors(id) — an unregistered id makes every terminal commit roll back
  # (caught live: the default "codex-bridge" id looped the first smoke run).
  # Resolve the registered row id from thor's DB the same way the namespace
  # id is resolved; before first registration there is no row yet, so warn
  # and leave the default — register-actor.sh + a re-deploy completes it.
  # Inlined rather than resolve_actor_row_id: that helper is defined further
  # down, after this lane has already run (bash binds function names at
  # call time), and the lookup reads thor's database as the LOGIN user there
  # — the account holds no database credential and never will.
  ACTOR_ID=$(ssh "${THOR_HOST:-thor}" "cd $REMOTE_DIR/deploy/prod 2>/dev/null && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc \"SELECT id FROM actors WHERE actor_key = 'company/codex-${host}' ORDER BY revision DESC LIMIT 1\"" 2>/dev/null | tr -d '\r' || true)
  if [ -n "$ACTOR_ID" ]; then
    ssh "$target" "python3 - <<PYEOF
import json
p = __import__('os').path.expanduser('~/.culture-nodes/codex-bridge.json')
cfg = json.load(open(p))
cfg['actor_id'] = '$ACTOR_ID'
json.dump(cfg, open(p, 'w'), indent=2)
PYEOF"
    say "bridge actor_id in $target set to registered row $ACTOR_ID"
  else
    say "WARNING: no registered actor row for company/codex-$host yet — bridge keeps its default actor_id; ledger commits will fail until you run register-actor.sh and re-deploy"
  fi

  say "running the non-billable codex preflight as $target"
  # The unit runs this as ExecStartPre anyway; running it once here fails
  # fast at deploy time instead of only at unit start. SKIP_CODEX_PREFLIGHT=1
  # downgrades it to a warning for bootstrap ordering (e.g. codex not logged
  # in yet in a brand-new account).
  # Source the bridge env first — the unit's EnvironmentFile delivers
  # CODEX_BRIDGE_AUTH_TOKEN to ExecStartPre, so the deploy-time run must see
  # the same variable or its non-loopback auth check would falsely fail.
  # Check 8 (no sudo/docker group, checkout owned by the running uid) is
  # measured HERE, as the account, which is the only place it means anything.
  if ! ssh "$target" 'set -a; . ~/.culture-nodes/codex-bridge.env; set +a; ~/.culture-nodes/bin/codex-preflight.sh ~/.culture-nodes/codex-bridge.json'; then
    if [ "${SKIP_CODEX_PREFLIGHT:-0}" = "1" ]; then
      say "WARNING: codex preflight failed in $target but SKIP_CODEX_PREFLIGHT=1 — installing the unit anyway"
    else
      echo "codex preflight failed in $target — fix the reported condition, or re-run with SKIP_CODEX_PREFLIGHT=1 to install anyway" >&2
      exit 1
    fi
  fi

  # The cutover (c31/c32): refuse while a session is in flight as the
  # ACCOUNT (a redeploy after the migration; #249 finding 3) or as the login
  # user, then stop + disable the login unit (file, config and env stay for
  # the rollback pair), then install and start the account's copy on the
  # same port. Linger for the account was enabled by the bootstrap.
  login=$(ssh "$host" 'id -un' | tr -d '\r')
  account_session_guard "$host" codex || exit 1
  account_cutover_login_unit "$host" "$login" codex-bridge || exit 1

  say "installing codex-bridge systemd user unit into $target"
  ssh "$target" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/codex-bridge.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart codex-bridge && systemctl --user enable codex-bridge"
  ssh "$target" 'export XDG_RUNTIME_DIR=/run/user/$(id -u); for i in $(seq 1 15); do st=$(systemctl --user is-active codex-bridge || true); [ "$st" = active ] && { echo "codex-bridge: active"; exit 0; }; sleep 2; done; echo "codex-bridge failed to become active:"; systemctl --user --no-pager -n 10 status codex-bridge; exit 1'

  # The registry row learns which account serves it (#204's lane tag).
  account_register_os_user "company/codex-${host}" culture-codex

  remote_home=$(ssh "$target" 'printf %s "$HOME"' || true)
  # h17: ~/.local/bin is only on PATH in a *login* shell on orin, so the
  # success line prints the absolute path an operator (or a codex session
  # running under a non-login shell) can invoke unconditionally.
  say "codex-bridge active as $target — query CLI at ${remote_home:-\$HOME}/.local/bin/nodes (use the absolute path; ~/.local/bin is on PATH in login shells only)"
}

# --- qwen + pi actor bridge lanes (#294; thor and orin, after the codex lane)
#
# thor and orin each gain a qwen bridge (company/qwen-<host>, :8092) and a pi
# bridge (company/pi-<host>, :8093) beside the codex one, each running as its
# own engine account (culture-qwen, culture-pi) exactly the way the codex
# bridge runs as culture-codex since #243. One helper serves both because the
# two lanes differ only in engine name, template, unit and whether a preflight
# gates startup; a per-engine copy is how resolve_actor_row_id shipped as the
# same bug three times.
#
# Unlike the codex lane these are ADDITIVE and SKIP rather than fail when their
# account is not yet bootstrapped or its bridge secret is not yet installed:
# the qwen/pi accounts are new (#294), a host may run a codex-only deploy
# before they exist, and a hard exit here would refuse an otherwise-fine codex
# deploy. install-secrets.sh writes the account's <engine>-bridge.env (the
# auth token the render bakes into the config) and bridge-push.env; a missing
# one is a printed hint, not a stopped deploy (#289: the caller reads each
# lane's output, not the exit code).

# The account-side config render for a thor/orin engine bridge. sed cannot do
# it: the auth_token must be read from the account's own <engine>-bridge.env
# (so it never crosses an ssh argv), and the port + registered actor_id are
# overlaid too. __HOME__ resolves to the ACCOUNT's home on the far side, the
# same generate-absolute-paths-at-install-time technique the codex config uses.
# shellcheck disable=SC2016 # every expansion is for the remote shell
ACCOUNT_ENGINE_RENDER_REMOTE='set -eu
umask 077
mkdir -p "$HOME/.config/culture-nodes-bridges" "$HOME/.local/state/culture-nodes-bridges/$ROLE"
# The bridge binds 0.0.0.0, so it needs a token; it lives in the env file
# install-secrets.sh wrote, sourced here (not passed as an argv) so the secret
# stays off the process table.
set -a; . "$HOME/.culture-nodes/$ENGINE-bridge.env"; set +a
prog=$(cat <<"RENDERPY"
import json, os, sys

template, dest, engine, port, actor_id = sys.argv[1:6]
home = os.path.expanduser("~")
with open(template) as handle:
    config = json.loads(handle.read().replace("__HOME__", home))
token = os.environ.get(engine.upper() + "_BRIDGE_AUTH_TOKEN") or ""
if token:
    config["auth_token"] = token
if actor_id:
    config["actor_id"] = actor_id
if port:
    config["port"] = int(port)
if not config.get("auth_token"):
    sys.stderr.write("refusing: no auth_token for the " + engine + " bridge (it binds 0.0.0.0 and refuses to start without one); nothing rendered\n")
    raise SystemExit(3)
rendered = json.dumps(config, indent=2, sort_keys=True) + "\n"
tmp = dest + ".new"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as out:
    out.write(rendered)
os.chmod(tmp, 0o600)
os.replace(tmp, dest)
print("rendered " + dest + " (port " + str(port) + ", allowlist " + ", ".join(config.get("repo_allowlist", [])) + ")")
RENDERPY
)
python3 -c "$prog" "$TEMPLATE" "$HOME/.config/culture-nodes-bridges/$ROLE.json" "$ENGINE" "$PORT" "$ACTOR_ID"'

deploy_account_engine_bridge() { # host engine — runs on thor and orin, AS culture-<engine>
  local host=$1 engine=$2
  local role adapter package template unit preflight actor_key target port ACTOR_ID remote_home

  case "$engine" in
    qwen) role=qwen-developer; adapter=qwen; package=qwen_bridge
          template=qwen-developer.json.template; unit=culture-nodes-qwen-developer; preflight="" ;;
    pi)   role=pi-developer; adapter=pi; package=pi_bridge
          template=pi-developer.json.template; unit=culture-nodes-pi-developer; preflight=pi-preflight.sh ;;
    *) echo "deploy_account_engine_bridge: unknown engine $engine" >&2; return 1 ;;
  esac
  actor_key="company/${engine}-${host}"
  target=$(unix_user_target "$host" "$engine")
  port=$(actor_bridge_port "$engine")

  # Additive: an account that is not bootstrapped is not this deploy's failure
  # (a codex-only deploy is a valid deploy). Skip loudly, name the hand-turn.
  if ! account_reachable "$target"; then
    say "WARNING: culture-$engine on $host is not bootstrapped ($target does not open with the operator key) — skipping the $engine bridge lane (#294 accounts are additive; bootstrap it and run install-secrets.sh, then re-deploy). Nothing on $host was stopped"
    return 0
  fi

  # The account, its pinned engine + clone + inventory (as the account), and
  # its own archive copy — exactly the codex lane's account_prepare.
  account_prepare "$host" "$engine" || exit 1

  # install-secrets.sh owns the bridge auth token; deploy.sh only consumes it.
  ssh "$target" "test -f ~/.culture-nodes/${engine}-bridge.env" || {
    say "WARNING: ~/.culture-nodes/${engine}-bridge.env missing in $target — skipping the $engine bridge (run deploy/prod/install-secrets.sh, whose install_${engine}_account_env step writes it, then re-deploy). Nothing on $host was stopped"
    return 0
  }

  # pi's adapter (adapters/pi, task t2) may not be in every shipped tree yet;
  # the stamp's parent dir is created so stamp_revision's write lands whether
  # or not the archive already carried the package. mkdir -p is a no-op when
  # the adapter is present (qwen always, pi once t2 lands).
  ssh "$target" "mkdir -p $REMOTE_DIR/adapters/$adapter/src/$package"
  # Before the install, never after: `uv tool install` copies (stamp_revision).
  stamp_revision "$target" "$adapter" "$package"

  say "installing the $engine-bridge uv tool in $target (archive-independent copy)"
  ssh "$target" "\$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/$adapter"

  if [ -n "$preflight" ]; then
    say "installing $engine preflight in $target (with its pinned-version dependency beside it)"
    # pi-preflight.sh reads the pinned version out of lanes/unix-user.sh next
    # to itself, so it needs that file at a stable path (the unit's
    # ExecStartPre points at the installed copy, never the archive the next
    # deploy rm -rf's). It reads ONLY the UNIX_USER_PI_VERSION= line, so a
    # stub carrying just that line is installed rather than the whole lane:
    # the lane's own body contains the operator-material strings the account
    # inventory greps for (its inventory pattern names NODES_DATABASE_URL),
    # and copying the whole file into the account would trip that guard. The
    # stub is extracted from the real lane at deploy time, so the pin cannot
    # drift. Inventory allows the whole `bin` entry and does not recurse into
    # bin/lanes/, so the stub is invisible to the entry list.
    ssh "$target" "umask 077; mkdir -p ~/.culture-nodes/bin/lanes && cp $REMOTE_DIR/deploy/prod/$preflight ~/.culture-nodes/bin/$preflight && chmod +x ~/.culture-nodes/bin/$preflight && grep -m1 '^UNIX_USER_PI_VERSION=' $REMOTE_DIR/deploy/prod/lanes/unix-user.sh > ~/.culture-nodes/bin/lanes/unix-user.sh"
  fi

  # The registered row id the bridge stamps as origin_actor_id (a FOREIGN KEY
  # into actors), resolved from thor's DB as the LOGIN user the way the codex
  # lane resolves it (inlined for the same reason: resolve_actor_row_id is
  # defined further down and this lane runs before it).
  ACTOR_ID=$(ssh "${THOR_HOST:-thor}" "cd $REMOTE_DIR/deploy/prod 2>/dev/null && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc \"SELECT id FROM actors WHERE actor_key = '$actor_key' ORDER BY revision DESC LIMIT 1\"" 2>/dev/null | tr -d '\r' || true)
  [ -n "$ACTOR_ID" ] || say "WARNING: no registered actor row for $actor_key yet — the $engine bridge keeps no actor_id; ledger commits will fail until you run register-actor.sh and re-deploy"

  say "rendering $engine bridge config into $target from deploy/prod/$template (auth_token from ~/.culture-nodes/$engine-bridge.env, port $port)"
  # shellcheck disable=SC2029 # ROLE/TEMPLATE/ENGINE/PORT/ACTOR_ID are deliberately expanded here; the token is read on the far side
  ssh "$target" "ROLE='$role'; TEMPLATE='$REMOTE_DIR/deploy/prod/$template'; ENGINE='$engine'; PORT='$port'; ACTOR_ID='$ACTOR_ID'; $ACCOUNT_ENGINE_RENDER_REMOTE"

  # Refuse a restart while a session is in flight AS the account (a redeploy
  # kills the run and leaves it `running` in the ledger). No login-user unit to
  # cut over: thor/orin never ran a qwen or pi bridge as the login user.
  account_session_guard "$host" "$engine" || exit 1

  say "installing $unit systemd user unit into $target"
  ssh "$target" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/$unit.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart $unit && systemctl --user enable $unit"
  ssh "$target" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); for i in \$(seq 1 15); do st=\$(systemctl --user is-active $unit || true); [ \"\$st\" = active ] && { echo \"$unit: active\"; exit 0; }; sleep 2; done; echo \"$unit failed to become active:\"; systemctl --user --no-pager -n 10 status $unit; exit 1"

  # The registry row learns which account serves it (#204's lane tag).
  account_register_os_user "$actor_key" "culture-$engine"

  remote_home=$(ssh "$target" 'printf %s "$HOME"' || true)
  say "$engine-bridge active as $target — config at ${remote_home:-\$HOME}/.config/culture-nodes-bridges/$role.json"
}

deploy_qwen_bridge() { deploy_account_engine_bridge "$1" qwen; }
deploy_pi_bridge() { deploy_account_engine_bridge "$1" pi; }

# Bridge lanes run for the codex hosts only; spark has no codex/qwen/pi thor
# actor here (spark's qwen bridge is account_bridges_spark_lane's, in the case).
if [[ "$HOST" != spark* ]]; then
  deploy_codex_bridge "$HOST"
  deploy_qwen_bridge "$HOST"
  deploy_pi_bridge "$HOST"
fi

# --- human-inbox actor bridge lane (task t34: deploy wiring for the t16
# kind=human bridge + its GitHub merge tracker; task t10: host derivation) --
#
# WHERE this lane deploys is DERIVED, never declared. There is one logical
# human actor, its registration carries an endpoint_ref, and that endpoint is
# the address the engine dispatches human work to. The bridge answering there
# and the tracker watching its parked tasks are one pair on one machine: the
# tracker reads those tasks off the local filesystem, so "same host" is the
# mechanism, not a preference.
#
# This lane used to say THOR ONLY, in a comment, in three files, while the
# actor was registered at a different machine's address (issue #72). The
# engine parked human tasks on the bridge at the registered endpoint; the
# tracker on the declared host watched its own empty state directory and
# logged pending=0 for as long as anyone left it running. Nothing failed --
# two config values that had to agree were agreeing only by luck.
#
# Task t8 made the tracker REFUSE TO START against a bridge that does not
# serve its actor. This lane refuses to INSTALL that arrangement in the first
# place (assert_human_inbox_colocated). Neither half is sufficient alone: a
# wrong deploy that never starts is still a wrong deploy, and a right deploy
# that nothing rechecks drifts on the next endpoint move.
#
# Host-resident Python, installed as a uv tool the way the codex bridge is --
# see the long comment at the install step for why running it out of the
# agent checkout was wrong.
HUMAN_INBOX_ACTOR_KEY=${HUMAN_INBOX_ACTOR_KEY:-company/human-ops}

# resolve_actor_row_id <actor_key>
#
# Echoes the newest registered actors(id) for a key, or nothing.
#
# A bridge reports `origin.actor_id` on the ledger claim it emits, and
# ledger_records.origin_actor_id is a FOREIGN KEY into actors(id) — so the
# bridge must carry the ROW ID, never the human-readable actor_key. Get that
# wrong and the actor does its real work, answers correctly, and every
# terminal commit then rolls back on a foreign-key violation. Nothing about
# the symptom points at identity.
#
# The codex lane learned this live and inlined the lookup; its comment records
# the incident ("the default codex-bridge id looped the first smoke run").
# Then the human-inbox lane shipped `company/human-ops` and the notify lane
# shipped `company/notify-discord`, both keys, both broken the same way —
# which is what an inlined fix rather than a shared one buys you. Hence this
# helper, and hence the deploy-lane test asserting no lane hardcodes a key.
resolve_actor_row_id() { # actor_key
  local actor_key=$1
  ssh thor "cd $REMOTE_DIR/deploy/prod 2>/dev/null && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc \"SELECT id FROM actors WHERE actor_key = '$actor_key' ORDER BY revision DESC LIMIT 1\"" 2>/dev/null | tr -d '\r' || true
}

# assert_unit_healthy <host> <unit>
#
# Waits for a user unit to reach active AND STAY there. The staying part is
# the point: a unit whose process dies immediately spends its life in
# `activating (auto-restart)`, and a naive one-shot `is-active` check taken
# between restarts can catch it mid-start and call that success. The tracker
# crash-looped 6272 times on thor while the deploy reported clean.
assert_unit_healthy() { # host unit
  local host=$1 unit=$2
  actor_host_exec "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u)
for i in \$(seq 1 15); do
  [ \"\$(systemctl --user is-active $unit || true)\" = active ] && break
  sleep 2
done
if [ \"\$(systemctl --user is-active $unit || true)\" != active ]; then
  echo '$unit failed to become active:' >&2
  systemctl --user --no-pager -n 20 status $unit >&2 || true
  exit 1
fi
# Active once proves it started; active still, after a restart interval, is
# what proves it is running rather than flapping.
before=\$(systemctl --user show $unit -p NRestarts --value)
sleep 8
after=\$(systemctl --user show $unit -p NRestarts --value)
if [ \"\$before\" != \"\$after\" ] || [ \"\$(systemctl --user is-active $unit || true)\" != active ]; then
  echo \"$unit is restarting in a loop (NRestarts \$before -> \$after) — it starts and dies:\" >&2
  journalctl --user -u $unit -n 30 --no-pager >&2 || true
  exit 1
fi
echo \"$unit: active (NRestarts \$after)\""
}

report_port_conflict() { # host port failed-unit
  local host=$1 port=$2 failed_unit=$3
  actor_host_exec "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u)
pid=\$(ss -H -ltnp 'sport = :$port' 2>/dev/null | sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p' | head -n1)
conflict=unknown
if [ -n \"\$pid\" ]; then
  conflict=\$(systemctl --user status \"\$pid\" --no-pager 2>/dev/null | sed -n 's#.*[ /]\([^ /]*\\.service\).*#\1#p' | head -n1)
  [ -n \"\$conflict\" ] || conflict=pid-\$pid
fi
echo 'Address already in use: $failed_unit could not bind registered port $port; conflicting unit: '\"\$conflict\" >&2
exit 1"
}

deploy_human_inbox() { # no argument: the host comes from the registration
  local registration row_id revision endpoint auth_env
  local host port bridge_bin tracker_bin

  registration=$(actor_registration "$HUMAN_INBOX_ACTOR_KEY") || registration=""
  if [ -z "$registration" ]; then
    say "WARNING: $HUMAN_INBOX_ACTOR_KEY does not resolve in the actor registry at $NODES_API_URL — skipping the human-inbox bridge and tracker. Nothing is installed rather than installed somewhere guessed: the registration is the only artifact that says which host serves this actor (register it with deploy/prod/register-actor.sh, then re-run this deploy)"
    return 0
  fi
  IFS='|' read -r row_id revision endpoint auth_env <<< "$registration"

  host=$(endpoint_address "$endpoint")
  port=$(endpoint_port "$endpoint") || port=""
  if [ -z "$host" ] || [ -z "$port" ]; then
    echo "$HUMAN_INBOX_ACTOR_KEY (revision $revision) is registered at '$endpoint', which names no host:port — there is nowhere to deploy its bridge to. Re-register it with an explicit http://<ipv4>:<port> endpoint_ref" >&2
    exit 1
  fi
  say "$HUMAN_INBOX_ACTOR_KEY (revision $revision) is served at $endpoint — deploying its bridge and tracker there"

  # The bridge's caller credential has two custody points that must hold the
  # same string: this bridge authenticates inbound requests with the token in
  # ~/.culture-nodes/human-inbox.env, and the worker dispatches with the value
  # of whichever env var the actor row's metadata names. Reported, not
  # enforced -- the registration is authoritative about its own credential,
  # and a deploy silently assuming otherwise is how the last mismatch got in.
  if [ -n "$auth_env" ] && [ "$auth_env" != "HUMAN_INBOX_BRIDGE_AUTH_TOKEN" ]; then
    say "NOTE: the control plane dispatches to $HUMAN_INBOX_ACTOR_KEY with \$$auth_env (the actor row's metadata.auth_token_env), while this bridge authenticates callers with HUMAN_INBOX_BRIDGE_AUTH_TOKEN from ~/.culture-nodes/human-inbox.env — those two values must be the same string or every dispatch answers 401"
  fi

  # A missing human-inbox.env skips this lane rather than failing the deploy:
  # the bridge and tracker need their shared bridge auth token, and an absent
  # optional daemon secret file must never block the control plane from
  # shipping (found live — the hard exit here aborted the whole thor deploy
  # before the compose step ever ran).
  actor_host_exec "$host" 'test -f ~/.culture-nodes/human-inbox.env' || {
    say "WARNING: ~/.culture-nodes/human-inbox.env missing on $host — skipping the human-inbox bridge and tracker (run deploy/prod/install-secrets.sh, then deploy.sh again, to enable human nodes and auto-submit-on-merge)"
    return 0
  }

  # The archive: this lane's host is derived, so it is not necessarily the
  # host this deploy shipped to. When it is, the tree is already unpacked and
  # re-shipping it would `rm -rf` the directory the running deploy is using.
  if host_owns_address "$HOST" "$host"; then
    say "the human-inbox host is this deploy's own target — reusing the archive already at $REMOTE_DIR"
  else
    say "shipping $(git rev-parse --short "$BRANCH") to $host:$REMOTE_DIR (the actor's host is not this deploy's target)"
    git archive --format=tar "$BRANCH" | actor_host_exec "$host" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -x -C $REMOTE_DIR"
  fi

  say "ensuring uv on $host (human-inbox lane)"
  actor_host_exec "$host" 'bash -lc "command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh"'

  # Install the adapter as a uv TOOL, exactly as the codex-bridge lane does
  # and for a second reason on top of that one.
  #
  # The first reason is archive independence: `uv tool install` copies the
  # package into its own venv, so the units keep serving after the next
  # deploy's `rm -rf $REMOTE_DIR` removes the tree they were built from.
  #
  # The second was found live on thor. These units used to exec
  # `uv run --directory ~/git/culture-nodes-agent/adapters/human-inbox`, and
  # that checkout is the CODEX AGENT WORKSPACE — the lane above fast-forwards
  # it to its upstream tracking branch, which is main. So deploying a branch
  # installed units that exec code living only on that branch, out of a
  # checkout pinned to another one. The tracker died on
  # `No module named human_inbox_bridge.tracker` and systemd restarted it
  # 6272 times over nine hours while merge-as-action silently did nothing. An
  # agent workspace and a deployment artifact source are different things and
  # must not be the same directory.
  say "installing the human-inbox adapter as a uv tool on $host (archive-independent)"
  actor_host_exec "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/human-inbox || uv tool install --force ./$REMOTE_DIR/adapters/human-inbox'"

  # Resolve the REAL absolute paths of the installed console scripts and
  # substitute them into the units at install time. A systemd ExecStart takes
  # no PATH lookup, so the unit must carry the path this host actually has —
  # the same lesson %h/.local/bin/uv taught when one host turned out to keep
  # uv at /snap/bin/uv and the units died with 203/EXEC.
  bridge_bin=$(actor_host_exec "$host" 'bash -lc "command -v human-inbox-bridge"' | tr -d '\r')
  tracker_bin=$(actor_host_exec "$host" 'bash -lc "command -v human-inbox-tracker"' | tr -d '\r')
  [ -n "$bridge_bin" ] && [ -n "$tracker_bin" ] || {
    echo "human-inbox console scripts not on PATH on $host after uv tool install (bridge='$bridge_bin' tracker='$tracker_bin')" >&2
    return 1
  }
  say "human-inbox units will exec $bridge_bin and $tracker_bin on $host"

  say "installing human-inbox non-secret config on $host"
  # Same generate-absolute-paths-at-install-time technique runner.env and
  # codex-bridge.json use: $HOME expands on the TARGET, so
  # EnvironmentFile values (which get no %h expansion once systemd reads
  # them as plain KEY=VALUE lines) still resolve to real absolute paths.
  #
  # The port is the registered one, not a number written here: the engine
  # dispatches to the port in endpoint_ref, so a bridge that binds any other
  # port is a bridge nothing talks to. That was live -- the registration said
  # one port while this lane wrote another.
  #
  # HUMAN_INBOX_BRIDGE_ACTOR_ID is written into BOTH files, with DIFFERENT
  # values, and that is deliberate. The bridge stamps its copy as
  # origin.actor_id on every proposed ledger claim, and
  # ledger_records.origin_actor_id is a FOREIGN KEY into actors(id) -- so the
  # bridge needs the ROW ID. The tracker resolves its copy as an actor_KEY
  # against the control plane (task t8's startup identity check, which since
  # task t7 resolves it against GET /v1alpha1/dial-in-presence rather than the
  # actor list), so it needs the KEY. Same variable name, two required values,
  # two separate env files; assert_human_inbox_colocated refuses the swap.
  #
  # The tracker knows about this split rather than being broken by it: its
  # startup check compares the bridge's reported actor_id through the presence
  # row, which carries both the actor_key and the current revision's row id.
  actor_host_exec "$host" 'umask 077; mkdir -p ~/.culture-nodes
{ echo "HUMAN_INBOX_BRIDGE_HOST=0.0.0.0"
  echo "HUMAN_INBOX_BRIDGE_PORT='"$port"'"
  echo "HUMAN_INBOX_BRIDGE_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
  echo "HUMAN_INBOX_BRIDGE_ACTOR_ID='"$row_id"'"
} > ~/.culture-nodes/human-inbox-bridge.env'
  say "human-inbox bridge origin.actor_id set to registered row $row_id"

  # The canonical bridge unit reads the same JSON config as the already
  # running culture-nodes-human-inbox service.  Generate it on the target so
  # absolute paths and the registry-derived port/row id cannot drift.
  actor_host_exec "$host" 'umask 077; mkdir -p ~/.config/culture-nodes-bridges
PORT='"$port"' ACTOR_ID='"$row_id"' python3 -c '\''import json, os
print(json.dumps({"host":"0.0.0.0","port":int(os.environ["PORT"]),"state_dir":os.path.expanduser("~/.culture-nodes/human-inbox-state"),"actor_id":os.environ["ACTOR_ID"]}, indent=2))'\'' > ~/.config/culture-nodes-bridges/human-ops.json'

  # HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL is what ARMS task t8's startup
  # refusal: unset, the tracker logs a warning and runs unguarded, which is
  # the state issue #72 went unnoticed in. The deploy-time assertion below and
  # that startup check are the two halves of one invariant, and this line is
  # what connects them.
  actor_host_exec "$host" 'umask 077; mkdir -p ~/.culture-nodes
{ echo "HUMAN_INBOX_TRACKER_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
  echo "HUMAN_INBOX_BRIDGE_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
  echo "HUMAN_INBOX_TRACKER_BRIDGE_URL=http://127.0.0.1:'"$port"'"
  echo "HUMAN_INBOX_BRIDGE_ACTOR_ID='"$HUMAN_INBOX_ACTOR_KEY"'"
  echo "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL='"$NODES_API_URL"'"
} > ~/.culture-nodes/human-inbox-tracker.env'

  # The tracker makes exactly one control-plane call, at startup, and refuses
  # to run if it fails. Proving that read works from THIS host now turns a
  # crash-loop the health assertion would report as "unit not active" into a
  # deploy-time message naming the actual cause.
  actor_host_exec "$host" "curl -fsS --max-time 10 '${NODES_API_URL%/}/v1alpha1/dial-in-presence' >/dev/null" || {
    echo "$host cannot read dial-in presence at $NODES_API_URL, which the tracker resolves its actor against at startup — it would refuse to start (issue #72's runtime half). Fix reachability from $host, or set NODES_API_URL to an address that host can reach. A 404 here means the control plane predates task t6 and must be deployed first" >&2
    exit 1
  }

  # Acceptance criterion 2: refuse, loudly, before anything is installed, if
  # the bridge and tracker about to be installed would not be one pair on the
  # actor's own host. Reads back what was actually written above rather than
  # what this function meant to write.
  assert_human_inbox_colocated "$host" "$HUMAN_INBOX_ACTOR_KEY" "$endpoint"

  # Adopt the canonical culture-nodes-* names on every deploy.  Removing the
  # legacy files is essential: disabling alone lets the next archive copy
  # and re-enable them, recreating the :8090 conflict on a second deploy.
  say "removing legacy human-inbox unit names on $host"
  actor_host_exec "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); systemctl --user stop human-inbox-bridge.service 2>/dev/null || true; systemctl --user stop human-inbox-tracker.service 2>/dev/null || true; systemctl --user disable human-inbox-bridge.service 2>/dev/null || true; systemctl --user disable human-inbox-tracker.service 2>/dev/null || true; rm -f ~/.config/systemd/user/human-inbox-bridge.service; rm -f ~/.config/systemd/user/human-inbox-tracker.service; systemctl --user daemon-reload"

  say "installing culture-nodes-human-inbox systemd user unit on $host"
  actor_host_exec "$host" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  actor_host_exec "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/human-inbox-bridge#$bridge_bin#\" $REMOTE_DIR/deploy/prod/culture-nodes-human-inbox.service > ~/.config/systemd/user/culture-nodes-human-inbox.service && systemctl --user daemon-reload && systemctl --user restart culture-nodes-human-inbox && systemctl --user enable culture-nodes-human-inbox"
  assert_unit_healthy "$host" culture-nodes-human-inbox || {
    report_port_conflict "$host" "$port" culture-nodes-human-inbox.service
  }

  # GITHUB_TOKEN is optional: the public-repository lane polls anonymously at
  # half the 60/hour ceiling (the quota is per source IP, so the tracker must
  # leave room for whatever else on this host talks to GitHub), while a token
  # selects the 5,000/hour authenticated lane. Both install the same unit.
  say "installing culture-nodes-human-inbox-tracker systemd user unit on $host"
  actor_host_exec "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/human-inbox-tracker#$tracker_bin#\" $REMOTE_DIR/deploy/prod/culture-nodes-human-inbox-tracker.service > ~/.config/systemd/user/culture-nodes-human-inbox-tracker.service && systemctl --user daemon-reload && systemctl --user restart culture-nodes-human-inbox-tracker && systemctl --user enable culture-nodes-human-inbox-tracker"
  assert_unit_healthy "$host" culture-nodes-human-inbox-tracker
}

# --- notify actor bridge lane (issue #68) ---------------------------------
# THOR ONLY: one logical notification actor, and a second bridge on orin
# would be a second identity posting into the same channel.
#
# Note what this lane does NOT share with the human-inbox lane any more. That
# one used to carry the same declared-host comment, and the declaration went
# stale against the actor's registered endpoint without anything noticing
# (issue #68's sibling, #72). company/notify-discord happens to be registered
# on this host today, which is agreement by luck, not by construction — if
# that actor ever moves, give this lane the same actor_registration treatment
# rather than editing the host name in a comment.
#
# This bridge is the ONLY thing here that both speaks the actor protocol and
# holds the webhook URL. It reads that URL from prod.env — the file the
# notifier container already uses — rather than a copy of its own, so the
# secret keeps one custody point.
deploy_notify() { # host
  local host=$1
  case "$host" in
    thor*) ;;
    *) say "notify bridge is thor-only (one logical notify actor) -- skipping on $host"; return 0 ;;
  esac

  ssh "$host" 'test -f ~/.culture-nodes/prod.env' || {
    say "WARNING: ~/.culture-nodes/prod.env missing on $host — skipping the notify bridge (it reads CULTURE_NODES_WEBHOOK_URL from there)"
    return 0
  }
  ssh "$host" 'grep -q "^CULTURE_NODES_WEBHOOK_URL=." ~/.culture-nodes/prod.env' || {
    say "WARNING: no CULTURE_NODES_WEBHOOK_URL in ~/.culture-nodes/prod.env on $host — skipping the notify bridge (a bridge with no webhook would accept dispatches and drop every message)"
    return 0
  }

  # Same helper, same reason, same ordering as the codex lane: this bridge is
  # a copy too, and a notify bridge silently running last month's message
  # shape is exactly as invisible as a stale codex bridge was in #120.
  stamp_revision "$host" notify notify_bridge

  say "installing the notify adapter as a uv tool on $host (archive-independent)"
  ssh "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/notify || uv tool install --force ./$REMOTE_DIR/adapters/notify'"

  NOTIFY_BIN=$(ssh "$host" 'bash -lc "command -v notify-bridge"' | tr -d '\r')
  [ -n "$NOTIFY_BIN" ] || { echo "notify-bridge console script not on PATH on $host after uv tool install" >&2; return 1; }
  say "notify unit will exec $NOTIFY_BIN on $host"

  # This lane CONSUMES the bearer token; it never mints one. Secret material
  # is install-secrets.sh's alone (a boundary two deploy-lane tests enforce),
  # and there is a second reason here: the control plane holds the matching
  # value, so a token re-minted on every deploy would silently break dispatch.
  ssh "$host" 'test -s ~/.culture-nodes/notify.env' || {
    say "WARNING: ~/.culture-nodes/notify.env missing on $host — skipping the notify bridge (run deploy/prod/install-secrets.sh, then deploy.sh again, to enable workflow-step notifications)"
    return 0
  }

  NOTIFY_ACTOR_ID=$(resolve_actor_row_id "company/notify-discord")
  if [ -z "$NOTIFY_ACTOR_ID" ]; then
    say "WARNING: no registered actor row for company/notify-discord yet — skipping the notify bridge (it would report an actor_key as origin.actor_id and every terminal commit would roll back on the ledger FK); run deploy/prod/register-actor.sh and re-deploy"
    return 0
  fi
  say "notify bridge origin.actor_id set to registered row $NOTIFY_ACTOR_ID"
  say "installing notify non-secret config on $host"
  ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes
{ echo "NOTIFY_BRIDGE_HOST=0.0.0.0"
  echo "NOTIFY_BRIDGE_PORT=8088"
  echo "NOTIFY_BRIDGE_STATE_DIR=$HOME/.culture-nodes/notify-state"
  echo "NOTIFY_BRIDGE_ACTOR_ID='"$NOTIFY_ACTOR_ID"'"
} > ~/.culture-nodes/notify-bridge.env'

  say "installing notify-bridge systemd user unit on $host"
  ssh "$host" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/notify-bridge#$NOTIFY_BIN#\" $REMOTE_DIR/deploy/prod/notify-bridge.service > ~/.config/systemd/user/notify-bridge.service && systemctl --user daemon-reload && systemctl --user restart notify-bridge && systemctl --user enable notify-bridge"
  assert_unit_healthy "$host" notify-bridge
}

# --- Jira comment actor lane ----------------------------------------------
# An ordinary actor registration supplies routing and the worker-facing
# token name. Jira Basic auth is not consumed here and never enters control-
# plane configuration; systemd loads it from jira-bridge-jira.env directly.
deploy_jira() { # host
  local host=$1
  local transition_targets=${JIRA_TRANSITION_TARGETS:-In Progress,Pending,In Review,Done}
  local transition_project_prefix=${JIRA_TRANSITION_PROJECT_PREFIX:-SCRUM-}
  # The REST base the bridge's four verbs authenticate at. Empty means the
  # site URL; a scoped Jira Cloud service-account token needs the Atlassian
  # gateway base instead, because the site URL answers 401 for it.
  #
  # Deploy-managed like the two keys above, with one difference a reader has
  # to notice: those have safe DEFAULTS, so writing them on every deploy
  # restores the intended value. This one does not -- an empty base is a
  # working configuration for an unscoped token and a broken one for a scoped
  # token, and the two are indistinguishable from here. So the lane writes it
  # when this shell SETS it (empty included, which is how it is deliberately
  # cleared) and leaves the host's alone when it does not, the same rule
  # install-secrets.sh applies to the same key. An ordinary deploy therefore
  # cannot silently send every bridge verb back to a 401.
  local write_api_base=no
  if [ -n "${JIRA_API_BASE+set}" ]; then write_api_base=yes; fi
  case "$host" in thor*) ;; *) return 0 ;; esac
  if [ -z "${JIRA_SITE:-}" ]; then
    say "JIRA_SITE unset: Jira comment actor is not configured on $host"
    return 0
  fi
  ssh "$host" 'test -s ~/.culture-nodes/jira-bridge-jira.env && test -s ~/.culture-nodes/jira-bridge-auth.env' || {
    say "WARNING: Jira actor env files missing on $host — run install-secrets.sh"
    return 0
  }
  JIRA_ACTOR_ID=$(resolve_actor_row_id "company/jira-comment")
  if [ -z "$JIRA_ACTOR_ID" ]; then
    say "WARNING: company/jira-comment is not registered; use register-actor.sh and re-deploy"
    return 0
  fi
  say "installing the single-verb Jira comment actor on $host"
  ssh "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/jira || uv tool install --force ./$REMOTE_DIR/adapters/jira'"
  JIRA_BIN=$(ssh "$host" 'bash -lc "command -v jira-bridge"' | tr -d '\r')
  [ -n "$JIRA_BIN" ] || { echo "jira-bridge not on PATH after install" >&2; return 1; }
  printf 'JIRA_BRIDGE_HOST=0.0.0.0\nJIRA_BRIDGE_PORT=8089\nJIRA_BRIDGE_ACTOR_ID=%s\nJIRA_SITE=%s\n' "$JIRA_ACTOR_ID" "$JIRA_SITE" \
    | ssh "$host" 'umask 077; cat > ~/.culture-nodes/jira-bridge.env; chmod 600 ~/.culture-nodes/jira-bridge.env'
  # This file also holds the Jira Basic-auth credential pair installed by
  # install-secrets.sh. Merge only the deploy-owned, non-secret keys: a
  # whole-file write here would erase the bridge's Jira authority on every
  # ordinary deploy. Values travel over stdin and output names only.
  say "merging the deploy-managed Jira bridge keys on $host (JIRA_API_BASE included: $write_api_base)"
  {
    printf 'JIRA_TRANSITION_TARGETS=%s\nJIRA_TRANSITION_PROJECT_PREFIX=%s\n' "$transition_targets" "$transition_project_prefix"
    if [ "$write_api_base" = yes ]; then printf 'JIRA_API_BASE=%s\n' "$JIRA_API_BASE"; fi
  } \
    | ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes; touch ~/.culture-nodes/jira-bridge-jira.env; chmod 600 ~/.culture-nodes/jira-bridge-jira.env; if [ -s ~/.culture-nodes/jira-bridge-jira.env ] && [ -n "$(tail -c1 ~/.culture-nodes/jira-bridge-jira.env)" ]; then echo >> ~/.culture-nodes/jira-bridge-jira.env; fi; while IFS= read -r line; do k=${line%%=*}; [ -z "$k" ] && continue; tmp=~/.culture-nodes/jira-bridge-jira.env.merge.$$; : > "$tmp"; chmod 600 "$tmp"; found=0; while IFS= read -r cur || [ -n "$cur" ]; do case "$cur" in "$k"=*) printf "%s\n" "$line" >> "$tmp"; found=1;; *) printf "%s\n" "$cur" >> "$tmp";; esac; done < ~/.culture-nodes/jira-bridge-jira.env; [ "$found" = 1 ] || printf "%s\n" "$line" >> "$tmp"; mv "$tmp" ~/.culture-nodes/jira-bridge-jira.env; done'
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); sed \"s#%h/.local/bin/jira-bridge#$JIRA_BIN#\" $REMOTE_DIR/deploy/prod/jira-bridge.service > ~/.config/systemd/user/jira-bridge.service && systemctl --user daemon-reload && systemctl --user restart jira-bridge && systemctl --user enable jira-bridge"
  assert_unit_healthy "$host" jira-bridge
}

# --- the two-host r4 sequence (task t2, spec c25/c26/c28, #230) -----------
# shellcheck source=deploy/prod/lanes/two-host.sh
source "$SCRIPT_DIR/lanes/two-host.sh"

case "$HOST" in
  thor*)
    thor_two_host_lane
    # The human-inbox bridge and tracker come up AFTER the control plane:
    # the tracker submits into the bridge, the bridge calls back into the API,
    # and the lane itself resolves the actor registry to learn which host it
    # deploys to — so standing them up first only means they start against a
    # stack that is still restarting. The lane takes no host argument: it
    # reads that from the actor's registration, which may well be a machine
    # other than this deploy's target.
    deploy_human_inbox
    deploy_notify "$HOST"
    deploy_jira "$HOST"
    # LAST, on purpose: the audit reports on the environment this deploy
    # actually shipped, so it runs after every lane above has had its say. It
    # is a detector, not a gate — the stack is already up when it speaks, and
    # a non-zero exit here means an operator hears about a missing credential
    # now instead of 18 hours later from a 401 (issue #69 item 2).
    "$SCRIPT_DIR/audit-credentials.sh" "$HOST"
    # Doctor is the second detector (PR #208 review finding 2): after the
    # stack is up, the Python nodes CLI's four checks say whether the agent
    # lane this deploy just reconfigured can actually work — prompt file,
    # skills kit, API reachability, and the userns sysctl a workspace-write
    # dispatch silently loses writes without (#63). Same posture as the
    # credential audit above: a detector that fails the deploy LOUDLY at
    # the end, not a gate that leaves the stack half-shipped.
    # As the ACCOUNT since #243: the agent lane is culture-codex's checkout
    # and culture-codex's nodes CLI, so that is where the four checks mean
    # something. The login user's copies are the rollback posture, not the
    # lane this deploy shipped.
    say "running nodes doctor as culture-codex on $HOST"
    ssh "$(unix_user_target "$HOST" codex)" "cd \$HOME/git/culture-nodes-agent && \$HOME/.local/bin/nodes doctor" || { echo "nodes doctor reports unhealthy in culture-codex on $HOST" >&2; exit 1; }
    account_bridges_summary "$HOST"
    deploy_summary thor
    ;;
  orin*)
    say "resolving thor's address from $HOST and starting the orin worker"
    THOR_IP=$(ssh "$HOST" "getent hosts thor | awk '{print \$1; exit}'")
    [ -n "$THOR_IP" ] || { echo "orin cannot resolve thor" >&2; exit 1; }
    NS=$(ssh "$THOR_HOST" "curl -fsS http://localhost:18080/v1alpha1/namespaces | python3 -c 'import json,sys; rows=json.load(sys.stdin); print(rows[0][\"id\"] if rows else \"\")'")
    [ -n "$NS" ] || { echo "$THOR_HOST has no namespace yet — deploy thor first" >&2; exit 1; }
    ssh "$HOST" "grep -q '^THOR_IP=' ~/.culture-nodes/prod.env && sed -i 's/^THOR_IP=.*/THOR_IP=$THOR_IP/' ~/.culture-nodes/prod.env || echo THOR_IP=$THOR_IP >> ~/.culture-nodes/prod.env"
    ssh "$HOST" "grep -q '^NODES_NAMESPACE_ID=' ~/.culture-nodes/prod.env && sed -i 's/^NODES_NAMESPACE_ID=.*/NODES_NAMESPACE_ID=$NS/' ~/.culture-nodes/prod.env || echo NODES_NAMESPACE_ID=$NS >> ~/.culture-nodes/prod.env"
    # --force-recreate (#300): a plain `up -d` only recreates the worker
    # container when compose itself detects an interpolated-config change,
    # and #300 reproduced that detection not firing for a prod.env
    # actor-token addition — the worker kept its stale environment until a
    # manual --force-recreate. compose.orin.yml declares no `api` service
    # (orin runs a worker only, see that file's header), so unlike the thor
    # lane's explicit "api worker" recreate this names "worker" alone. No
    # --build: the image was built and labelled above (see the image build
    # step); a compose rebuild would drop the label the parity check reads.
    compose_orin "up -d --force-recreate worker"
    # The orin half of the r4 sequence: parity across thor's api and both
    # workers, and the sweep resumed only when it holds (TWO_HOST_LANE).
    orin_two_host_lane
    # Same detector, same reason, against compose.orin.yml's own declared set
    # (see the thor lane's comment).
    "$SCRIPT_DIR/audit-credentials.sh" "$HOST"
    # Same doctor detector as the thor lane (PR #208 review finding 2).
    say "running nodes doctor as culture-codex on $HOST"
    ssh "$(unix_user_target "$HOST" codex)" "cd \$HOME/git/culture-nodes-agent && \$HOME/.local/bin/nodes doctor" || { echo "nodes doctor reports unhealthy in culture-codex on $HOST" >&2; exit 1; }
    account_bridges_summary "$HOST"
    deploy_summary orin
    ;;
  spark*)
    # Bridge lanes only (#243): the four claude bridges into culture-claude,
    # qwen-developer into culture-qwen, over ssh culture-<engine>@localhost.
    # No compose, no runner, no cutover, no two-host lane, no audit — the
    # control plane is not on this host. The lane prints its own summary.
    # The pre-deploy doctor first, as on thor/orin (#249 finding 7): a host
    # the agent lane cannot work on is refused while every bridge is still
    # the old one.
    account_spark_preflight_doctor "$HOST" || exit 1
    account_bridges_spark_lane "$HOST"
    ;;
  *)
    echo "unknown host role: $HOST (expected thor, orin or spark)" >&2; exit 1;;
esac
