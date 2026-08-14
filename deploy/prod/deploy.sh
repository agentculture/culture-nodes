#!/usr/bin/env bash
# Deploy the current checkout to the production pair (plan t19).
#
#   deploy.sh thor          # full control plane + worker + runner host unit
#   deploy.sh orin          # second worker + runner host unit
#
# Ships the working tree's HEAD as a git archive over ssh (no push, no
# registry), builds the image on the target (both machines are aarch64 —
# native builds), installs the runner binary + systemd user unit, installs
# the codex actor bridge (host-resident, beside the containerized worker),
# and starts the stack. All ssh invocations are argv-only; secrets never
# ride in argv (install-secrets.sh puts them in ~/.culture-nodes/prod.env
# and ~/.culture-nodes/codex-bridge.env first — run it once before the
# first deploy).
set -euo pipefail

HOST=${1:?usage: deploy.sh <thor|orin>}
REMOTE_DIR="culture-nodes-prod"
BRANCH=${BRANCH:-HEAD}

say() { printf '==> %s\n' "$*"; }

say "shipping $(git rev-parse --short "$BRANCH") to $HOST:$REMOTE_DIR"
git archive --format=tar "$BRANCH" | ssh "$HOST" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -x -C $REMOTE_DIR"

say "building control-plane image on $HOST (native aarch64)"
ssh "$HOST" "cd $REMOTE_DIR && docker build -q -t culture-nodes:prod ."

say "building nodes-runner host binary on $HOST"
ssh "$HOST" "bash -lc 'cd $REMOTE_DIR && go build -o ~/.culture-nodes/bin/nodes-runner ./cmd/nodes-runner'" \
  || { echo "remote Go missing — building here and copying (same arch)"; \
       go build -o /tmp/nodes-runner ./cmd/nodes-runner && \
       ssh "$HOST" 'mkdir -p ~/.culture-nodes/bin' && \
       scp -q /tmp/nodes-runner "$HOST":.culture-nodes/bin/nodes-runner && rm /tmp/nodes-runner; }

say "ensuring headspace CLI on $HOST (uv tool)"
ssh "$HOST" 'bash -lc "command -v headspace >/dev/null || { command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install headspace-cli || uv tool install headspace-cli; }; command -v headspace"'

say "installing runner env + systemd user unit on $HOST"
# Single-quoted remote script: $HOME expands on the TARGET, giving the env
# file absolute paths (EnvironmentFile values get no %h expansion).
ssh "$HOST" 'umask 077; mkdir -p ~/.culture-nodes/bin ~/.culture-nodes/runner-state
{ echo "NODES_RUNNER_LISTEN=:17070"
  echo "NODES_RUNNER_SECRET_FILE=$HOME/.culture-nodes/runner.secret"
  echo "NODES_RUNNER_STATE_DIR=$HOME/.culture-nodes/runner-state"
  echo "NODES_RUNNER_HEADSPACE_PROFILES=sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de=python3.12"
  echo "NODES_RUNNER_HEADSPACE_BIN=$HOME/.local/bin/headspace"
} > ~/.culture-nodes/runner.env'
ssh "$HOST" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
ssh "$HOST" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/nodes-runner.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart nodes-runner && systemctl --user enable nodes-runner"
ssh "$HOST" 'export XDG_RUNTIME_DIR=/run/user/$(id -u); for i in $(seq 1 15); do st=$(systemctl --user is-active nodes-runner || true); [ "$st" = active ] && { echo "runner: active"; exit 0; }; sleep 2; done; echo "runner failed to become active:"; systemctl --user --no-pager -n 10 status nodes-runner; exit 1'

# --- codex actor bridge lane (plan t3) ------------------------------------
# Both thor and orin run their own managed codex actor (company/codex-thor,
# company/codex-orin), so this lane is host-agnostic and runs once per
# deploy for whichever host was named — it deliberately lives outside the
# case below rather than being duplicated into each branch.
#
# The checkout-provisioning step below WARNS rather than fails: see the
# comment on that step.
CODEX_AGENT_CHECKOUT_REMOTE='repo=$HOME/git/culture-nodes-agent
if [ ! -d "$repo/.git" ]; then
  mkdir -p "$HOME/git" || exit 3
  git clone https://github.com/agentculture/culture-nodes "$repo" || exit 3
  echo "agent checkout: cloned $repo"
  exit 0
fi
if [ -n "$(git -C "$repo" status --porcelain)" ]; then
  echo "agent checkout $repo has uncommitted changes — refusing to touch it (harvest the diff, then reset, per the runbook)" >&2
  exit 3
fi
upstream=$(git -C "$repo" rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null || echo origin/main)
git -C "$repo" fetch "${upstream%%/*}" || { echo "agent checkout $repo: git fetch ${upstream%%/*} failed — leaving it untouched" >&2; exit 3; }
git -C "$repo" merge --ff-only "$upstream" || { echo "agent checkout $repo has diverged from $upstream — refusing to touch it (no rebase, no reset)" >&2; exit 3; }
echo "agent checkout: fast-forwarded $repo to $upstream"'

deploy_codex_bridge() { # host — runs identically on thor and orin
  local host=$1
  local remote_home

  # install-secrets.sh owns the bridge bearer token exactly as it owns
  # prod.env; deploy.sh only consumes it. Fail early with the same kind of
  # "run the other script first" message prod.env's absence produces.
  ssh "$host" 'test -f ~/.culture-nodes/codex-bridge.env' \
    || { echo "~/.culture-nodes/codex-bridge.env missing on $host — run deploy/prod/install-secrets.sh first" >&2; exit 1; }

  say "installing the codex-bridge uv tool on $host (archive-independent)"
  # `uv tool install` — NOT --editable, and not `uv run --project` — builds
  # the package and COPIES it into its own tool venv under ~/.local/share/uv,
  # so ~/.local/bin/codex-bridge keeps serving after the next deploy's
  # `rm -rf $REMOTE_DIR` deletes the tree it was installed from (c21/h19).
  # An editable install would keep pointing at the archive and break there.
  ssh "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/codex || uv tool install --force ./$REMOTE_DIR/adapters/codex'"

  say "installing the nodes query CLI on $host (PyPI culture-nodes)"
  # Deviation d1: the host-side query CLI is the PYTHON `nodes` CLI from
  # PyPI, not the Go cmd/nodes binary — cmd/nodes has no query verbs, so
  # there is nothing to build or scp here.
  ssh "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force culture-nodes || uv tool install --force culture-nodes'"

  say "provisioning the codex agent checkout on $host (~/git/culture-nodes-agent)"
  # Warn, don't fail: a dirty checkout is an EXPECTED operator state — write
  # sessions leave diffs the operator harvests and then resets (harvest/reset
  # runbook). Refusing to touch such a checkout must not block the bridge
  # deploy itself, so a refusal here is reported and the lane continues.
  ssh "$host" "$CODEX_AGENT_CHECKOUT_REMOTE" \
    || say "WARNING: agent checkout on $host left untouched (reason above) — continuing deploy"

  say "installing codex preflight + bridge config on $host"
  ssh "$host" "umask 077; mkdir -p ~/.culture-nodes/bin ~/.culture-nodes/codex-bridge-state && cp $REMOTE_DIR/deploy/prod/codex-preflight.sh ~/.culture-nodes/bin/codex-preflight.sh && chmod +x ~/.culture-nodes/bin/codex-preflight.sh"
  # Same generate-absolute-paths-at-install-time technique runner.env uses:
  # the substitution runs on the TARGET, so __HOME__ becomes that host's own
  # $HOME. (The bridge config is JSON, not a systemd unit — %h would mean
  # nothing inside it.)
  ssh "$host" "sed \"s|__HOME__|\$HOME|g\" $REMOTE_DIR/deploy/prod/codex-bridge.json.template > ~/.culture-nodes/codex-bridge.json"

  # The bridge stamps its actor_id as origin_actor_id on every proposed
  # ledger claim, and ledger_records.origin_actor_id is a FOREIGN KEY into
  # actors(id) — an unregistered id makes every terminal commit roll back
  # (caught live: the default "codex-bridge" id looped the first smoke run).
  # Resolve the registered row id from thor's DB the same way the namespace
  # id is resolved; before first registration there is no row yet, so warn
  # and leave the default — register-actor.sh + a re-deploy completes it.
  ACTOR_ID=$(ssh thor "cd $REMOTE_DIR/deploy/prod 2>/dev/null && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc \"SELECT id FROM actors WHERE actor_key = 'company/codex-${host}' ORDER BY revision DESC LIMIT 1\"" 2>/dev/null || true)
  if [ -n "$ACTOR_ID" ]; then
    ssh "$host" "python3 - <<PYEOF
import json
p = __import__('os').path.expanduser('~/.culture-nodes/codex-bridge.json')
cfg = json.load(open(p))
cfg['actor_id'] = '$ACTOR_ID'
json.dump(cfg, open(p, 'w'), indent=2)
PYEOF"
    say "bridge actor_id on $host set to registered row $ACTOR_ID"
  else
    say "WARNING: no registered actor row for company/codex-$host yet — bridge keeps its default actor_id; ledger commits will fail until you run register-actor.sh and re-deploy"
  fi

  say "running the non-billable codex preflight on $host"
  # The unit runs this as ExecStartPre anyway; running it once here fails
  # fast at deploy time instead of only at unit start. SKIP_CODEX_PREFLIGHT=1
  # downgrades it to a warning for bootstrap ordering (e.g. codex not logged
  # in yet on a brand-new host).
  # Source the bridge env first — the unit's EnvironmentFile delivers
  # CODEX_BRIDGE_AUTH_TOKEN to ExecStartPre, so the deploy-time run must see
  # the same variable or its non-loopback auth check would falsely fail.
  if ! ssh "$host" 'set -a; . ~/.culture-nodes/codex-bridge.env; set +a; ~/.culture-nodes/bin/codex-preflight.sh ~/.culture-nodes/codex-bridge.json'; then
    if [ "${SKIP_CODEX_PREFLIGHT:-0}" = "1" ]; then
      say "WARNING: codex preflight failed on $host but SKIP_CODEX_PREFLIGHT=1 — installing the unit anyway"
    else
      echo "codex preflight failed on $host — fix the reported condition, or re-run with SKIP_CODEX_PREFLIGHT=1 to install anyway" >&2
      exit 1
    fi
  fi

  say "installing codex-bridge systemd user unit on $host"
  ssh "$host" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && cp $REMOTE_DIR/deploy/prod/codex-bridge.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user restart codex-bridge && systemctl --user enable codex-bridge"
  ssh "$host" 'export XDG_RUNTIME_DIR=/run/user/$(id -u); for i in $(seq 1 15); do st=$(systemctl --user is-active codex-bridge || true); [ "$st" = active ] && { echo "codex-bridge: active"; exit 0; }; sleep 2; done; echo "codex-bridge failed to become active:"; systemctl --user --no-pager -n 10 status codex-bridge; exit 1'

  remote_home=$(ssh "$host" 'printf %s "$HOME"' || true)
  # h17: ~/.local/bin is only on PATH in a *login* shell on orin, so the
  # success line prints the absolute path an operator (or a codex session
  # running under a non-login shell) can invoke unconditionally.
  say "codex-bridge active on $host — query CLI at ${remote_home:-\$HOME}/.local/bin/nodes (use the absolute path; ~/.local/bin is on PATH in login shells only)"
}

deploy_codex_bridge "$HOST"

# --- human-inbox actor bridge lane (task t34: deploy wiring for the t16
# kind=human bridge + its GitHub merge tracker) ---------------------------
# One logical human actor (company/human-ops), so this lane is THOR ONLY --
# a second bridge/tracker pair on orin would just race the same GitHub PRs
# and the same inbox tasks against the same actor row. Host-resident Python,
# installed as a uv tool the way the codex bridge is -- see the long comment
# at the install step for why running it out of the agent checkout was wrong.

# assert_unit_healthy <host> <unit>
#
# Waits for a user unit to reach active AND STAY there. The staying part is
# the point: a unit whose process dies immediately spends its life in
# `activating (auto-restart)`, and a naive one-shot `is-active` check taken
# between restarts can catch it mid-start and call that success. The tracker
# crash-looped 6272 times on thor while the deploy reported clean.
assert_unit_healthy() { # host unit
  local host=$1 unit=$2
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u)
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

deploy_human_inbox() { # host
  local host=$1
  case "$host" in
    thor*) ;;
    *) say "human-inbox bridge is thor-only (one logical human actor: company/human-ops) -- skipping on $host"; return 0 ;;
  esac

  # A missing human-inbox.env skips this lane rather than failing the deploy:
  # the bridge and tracker need their shared bridge auth token, and an absent
  # optional daemon secret file must never block the control plane from
  # shipping (found live — the hard exit here aborted the whole thor deploy
  # before the compose step ever ran).
  ssh "$host" 'test -f ~/.culture-nodes/human-inbox.env' || {
    say "WARNING: ~/.culture-nodes/human-inbox.env missing on $host — skipping the human-inbox bridge and tracker (run deploy/prod/install-secrets.sh, then deploy.sh again, to enable human nodes and auto-submit-on-merge)"
    return 0
  }

  say "ensuring uv on $host (human-inbox lane)"
  ssh "$host" 'bash -lc "command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh"'

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
  ssh "$host" "bash -lc 'command -v uv >/dev/null || curl -LsSf https://astral.sh/uv/install.sh | sh; \$HOME/.local/bin/uv tool install --force ./$REMOTE_DIR/adapters/human-inbox || uv tool install --force ./$REMOTE_DIR/adapters/human-inbox'"

  # Resolve the REAL absolute paths of the installed console scripts and
  # substitute them into the units at install time. A systemd ExecStart takes
  # no PATH lookup, so the unit must carry the path this host actually has —
  # the same lesson %h/.local/bin/uv taught when thor turned out to keep uv at
  # /snap/bin/uv and the units died with 203/EXEC.
  BRIDGE_BIN=$(ssh "$host" 'bash -lc "command -v human-inbox-bridge"' | tr -d '\r')
  TRACKER_BIN=$(ssh "$host" 'bash -lc "command -v human-inbox-tracker"' | tr -d '\r')
  [ -n "$BRIDGE_BIN" ] && [ -n "$TRACKER_BIN" ] || {
    echo "human-inbox console scripts not on PATH on $host after uv tool install (bridge='$BRIDGE_BIN' tracker='$TRACKER_BIN')" >&2
    return 1
  }
  say "human-inbox units will exec $BRIDGE_BIN and $TRACKER_BIN on $host"

  say "installing human-inbox non-secret config on $host"
  # Same generate-absolute-paths-at-install-time technique runner.env and
  # codex-bridge.json use: $HOME expands on the TARGET, so
  # EnvironmentFile values (which get no %h expansion once systemd reads
  # them as plain KEY=VALUE lines) still resolve to real absolute paths.
  ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes
{ echo "HUMAN_INBOX_BRIDGE_HOST=0.0.0.0"
  echo "HUMAN_INBOX_BRIDGE_PORT=8087"
  echo "HUMAN_INBOX_BRIDGE_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
  echo "HUMAN_INBOX_BRIDGE_ACTOR_ID=company/human-ops"
} > ~/.culture-nodes/human-inbox-bridge.env
{ echo "HUMAN_INBOX_TRACKER_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
  echo "HUMAN_INBOX_TRACKER_BRIDGE_URL=http://127.0.0.1:8087"
  echo "HUMAN_INBOX_BRIDGE_STATE_DIR=$HOME/.culture-nodes/human-inbox-state"
} > ~/.culture-nodes/human-inbox-tracker.env'

  say "installing human-inbox-bridge systemd user unit on $host"
  ssh "$host" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/human-inbox-bridge#$BRIDGE_BIN#\" $REMOTE_DIR/deploy/prod/human-inbox-bridge.service > ~/.config/systemd/user/human-inbox-bridge.service && systemctl --user daemon-reload && systemctl --user restart human-inbox-bridge && systemctl --user enable human-inbox-bridge"
  assert_unit_healthy "$host" human-inbox-bridge

  # GITHUB_TOKEN is optional: the public-repository lane polls anonymously at
  # half the 60/hour ceiling (the quota is per source IP, so the tracker must
  # leave room for whatever else on this host talks to GitHub), while a token
  # selects the 5,000/hour authenticated lane. Both install the same unit.
  say "installing human-inbox-tracker systemd user unit on $host"
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/human-inbox-tracker#$TRACKER_BIN#\" $REMOTE_DIR/deploy/prod/human-inbox-tracker.service > ~/.config/systemd/user/human-inbox-tracker.service && systemctl --user daemon-reload && systemctl --user restart human-inbox-tracker && systemctl --user enable human-inbox-tracker"
  assert_unit_healthy "$host" human-inbox-tracker
}

# --- notify actor bridge lane (issue #68) ---------------------------------
# THOR ONLY, like the human-inbox lane and for the same reason: one logical
# notification actor. A second bridge on orin would be a second identity
# posting into the same channel.
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

  say "installing notify non-secret config on $host"
  ssh "$host" 'umask 077; mkdir -p ~/.culture-nodes
{ echo "NOTIFY_BRIDGE_HOST=0.0.0.0"
  echo "NOTIFY_BRIDGE_PORT=8088"
  echo "NOTIFY_BRIDGE_STATE_DIR=$HOME/.culture-nodes/notify-state"
  echo "NOTIFY_BRIDGE_ACTOR_ID=company/notify-discord"
} > ~/.culture-nodes/notify-bridge.env'

  say "installing notify-bridge systemd user unit on $host"
  ssh "$host" "loginctl enable-linger \$(id -un) 2>/dev/null || true"
  ssh "$host" "export XDG_RUNTIME_DIR=/run/user/\$(id -u); mkdir -p ~/.config/systemd/user && sed \"s#%h/.local/bin/notify-bridge#$NOTIFY_BIN#\" $REMOTE_DIR/deploy/prod/notify-bridge.service > ~/.config/systemd/user/notify-bridge.service && systemctl --user daemon-reload && systemctl --user restart notify-bridge && systemctl --user enable notify-bridge"
  assert_unit_healthy "$host" notify-bridge
}

case "$HOST" in
  thor*)
    say "starting thor control plane"
    ssh "$HOST" "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml up -d --build"
    say "waiting for readyz"
    ssh "$HOST" 'for i in $(seq 1 60); do curl -fsS http://localhost:18080/v1alpha1/readyz >/dev/null 2>&1 && echo READY && exit 0; sleep 2; done; echo NOT_READY; exit 1'
    say "resolving namespace id and (re)starting worker with it"
    NS=$(ssh "$HOST" "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc 'SELECT id FROM namespaces ORDER BY created_at LIMIT 1'")
    [ -n "$NS" ] || { echo "no namespace row found" >&2; exit 1; }
    ssh "$HOST" "grep -q '^NODES_NAMESPACE_ID=' ~/.culture-nodes/prod.env && sed -i 's/^NODES_NAMESPACE_ID=.*/NODES_NAMESPACE_ID=$NS/' ~/.culture-nodes/prod.env || echo NODES_NAMESPACE_ID=$NS >> ~/.culture-nodes/prod.env"
    ssh "$HOST" "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml up -d worker"
    # The human-inbox bridge and tracker come up AFTER the control plane:
    # the tracker submits into the bridge and the bridge calls back into the
    # API, so standing them up first only means they start against a stack
    # that is still restarting.
    deploy_human_inbox "$HOST"
    deploy_notify "$HOST"
    say "thor deploy complete (namespace $NS)"
    ;;
  orin*)
    say "resolving thor's address from $HOST and starting the orin worker"
    THOR_IP=$(ssh "$HOST" "getent hosts thor | awk '{print \$1; exit}'")
    [ -n "$THOR_IP" ] || { echo "orin cannot resolve thor" >&2; exit 1; }
    NS=$(ssh thor "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc 'SELECT id FROM namespaces ORDER BY created_at LIMIT 1'")
    [ -n "$NS" ] || { echo "thor has no namespace yet — deploy thor first" >&2; exit 1; }
    ssh "$HOST" "grep -q '^THOR_IP=' ~/.culture-nodes/prod.env && sed -i 's/^THOR_IP=.*/THOR_IP=$THOR_IP/' ~/.culture-nodes/prod.env || echo THOR_IP=$THOR_IP >> ~/.culture-nodes/prod.env"
    ssh "$HOST" "grep -q '^NODES_NAMESPACE_ID=' ~/.culture-nodes/prod.env && sed -i 's/^NODES_NAMESPACE_ID=.*/NODES_NAMESPACE_ID=$NS/' ~/.culture-nodes/prod.env || echo NODES_NAMESPACE_ID=$NS >> ~/.culture-nodes/prod.env"
    ssh "$HOST" "cd $REMOTE_DIR/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.orin.yml up -d --build"
    say "orin deploy complete (worker joined namespace $NS)"
    ;;
  *)
    echo "unknown host role: $HOST (expected thor or orin)" >&2; exit 1;;
esac
