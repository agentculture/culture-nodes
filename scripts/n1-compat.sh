#!/usr/bin/env bash
# scripts/n1-compat.sh
#
# N-1 binary compatibility harness (docs/adr/0002-migration-policy.md).
#
# A k8s rolling upgrade runs the previous binary version (N-1) and the new
# one (N) against the same PostgreSQL database at the same time, because
# the migration Job applies the schema for N before any pod running N-1 has
# been replaced. This script proves that window is safe: it builds the
# `nodes` binary from the previous git tag, migrates a fresh database with
# the CURRENT (HEAD) binary, then runs the previous-tag binary's `migrate`
# against that already-migrated schema. If the old binary errors out, a
# real rollout would break every pod still running it.
#
# No git tag exists in this repository yet (task t21 is where the first
# tagged release ships). Until then this script has nothing to check
# against and exits 0 having done nothing destructive: the harness is
# armed -- wired up and ready to run the real check the moment a tag
# exists -- but vacuous today. That is expected, not a failure.
#
# Usage:
#   scripts/n1-compat.sh
#
# Env:
#   NODES_TEST_DATABASE_URL   reuse an existing empty/scratch database
#                             instead of starting an ephemeral Docker
#                             postgres:17-alpine container.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

log() { printf 'n1-compat: %s\n' "$*" >&2; }

previous_tag="$(git tag --list --sort=-creatordate | head -n1 || true)"

if [[ -z "$previous_tag" ]]; then
  log "no git tag exists in this repository yet"
  log "no previous tag — harness armed but vacuous"
  log "once a release is tagged (task t21), this script builds that tag's" \
      "nodes binary and runs its migrate command against a schema" \
      "migrated by HEAD"
  exit 0
fi

log "found previous tag: $previous_tag"

if ! command -v go >/dev/null 2>&1; then
  log "error: go is not on PATH"
  exit 2
fi

worktree_dir=""
container_name=""
tmp_bin_dir="$(mktemp -d)"

cleanup() {
  if [[ -n "$container_name" ]]; then
    docker stop "$container_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$worktree_dir" ]]; then
    git worktree remove --force "$worktree_dir" >/dev/null 2>&1 || rm -rf "$worktree_dir"
  fi
  rm -rf "$tmp_bin_dir"
}
trap cleanup EXIT

worktree_dir="$(mktemp -u)"
git worktree add --detach "$worktree_dir" "$previous_tag" >/dev/null

if [[ ! -d "$worktree_dir/cmd/nodes" ]]; then
  log "tag $previous_tag predates cmd/nodes — nothing to smoke-test against"
  log "harness armed but vacuous for this tag"
  exit 0
fi

prev_binary="$tmp_bin_dir/nodes-prev"
log "building $previous_tag's nodes binary"
(cd "$worktree_dir" && go build -o "$prev_binary" ./cmd/nodes)

# The migrate subcommand is itself new as of this task (t6). A tag cut
# before it existed has nothing this harness can smoke-test yet -- that is
# not a compatibility failure, just a binary too old to have the feature.
if ! "$prev_binary" 2>&1 | grep -q 'migrate'; then
  log "tag $previous_tag's nodes binary has no migrate subcommand — nothing to smoke-test against"
  log "harness armed but vacuous for this tag"
  exit 0
fi

current_binary="$tmp_bin_dir/nodes-head"
log "building HEAD's nodes binary"
go build -o "$current_binary" ./cmd/nodes

db_url="${NODES_TEST_DATABASE_URL:-}"
if [[ -z "$db_url" ]]; then
  if ! command -v docker >/dev/null 2>&1; then
    log "error: docker is not on PATH and NODES_TEST_DATABASE_URL is not set"
    exit 2
  fi
  container_name="nodes-n1-compat-$$"
  log "starting an ephemeral postgres:17-alpine container"
  docker run -d --rm --name "$container_name" \
    -e POSTGRES_PASSWORD=nodes -e POSTGRES_DB=nodes -p 5432 postgres:17-alpine >/dev/null
  host_port="$(docker port "$container_name" 5432/tcp | head -n1 | sed 's/.*://')"
  db_url="postgres://postgres:nodes@127.0.0.1:${host_port}/nodes?sslmode=disable"
fi

log "migrating the schema to HEAD with the current binary (also serves as the readiness wait)"
migrated=false
for _ in $(seq 1 60); do
  if NODES_DATABASE_URL="$db_url" "$current_binary" migrate >/dev/null 2>&1; then
    migrated=true
    break
  fi
  sleep 1
done
if [[ "$migrated" != true ]]; then
  log "FAIL — the current (HEAD) binary could not migrate the database within 60s"
  exit 2
fi

log "running the $previous_tag binary's migrate against the HEAD-migrated schema"
if ! NODES_DATABASE_URL="$db_url" "$prev_binary" migrate; then
  log "FAIL — the $previous_tag binary errored against a schema already migrated by HEAD"
  log "a rolling upgrade would break pods still running $previous_tag"
  exit 1
fi

log "PASS — $previous_tag's binary ran cleanly against HEAD's migrated schema"
