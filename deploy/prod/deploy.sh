#!/usr/bin/env bash
# Deploy the current checkout to the production pair (plan t19).
#
#   deploy.sh thor          # full control plane + worker + runner host unit
#   deploy.sh orin          # second worker + runner host unit
#
# Ships the working tree's HEAD as a git archive over ssh (no push, no
# registry), builds the image on the target (both machines are aarch64 —
# native builds), installs the runner binary + systemd user unit, and
# starts the stack. All ssh invocations are argv-only; secrets never ride
# in argv (install-secrets.sh puts them in ~/.culture-nodes/prod.env
# first — run it once before the first deploy).
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
