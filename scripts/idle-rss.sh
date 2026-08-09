#!/usr/bin/env bash
# idle-rss.sh — measure the resident set size of an IDLE `nodes all` process.
#
# PRD §21.1 asks for an idle-memory profile to be recorded. This is the
# measurement: start an ephemeral PostgreSQL, migrate it, start `nodes all`
# (API + scheduler + worker in one process, the local-development topology),
# let it sit doing nothing for a settling period, and read VmRSS straight out
# of /proc/<pid>/status.
#
# Why /proc and not a Go benchmark: the figure wanted is the whole process's
# resident memory — Go runtime, pgx pool, HTTP server, embedded schemas — and
# a benchmark inside the test binary measures a different process entirely.
#
# The number is RECORDED, never gated. See docs/benchmarks.md for the measured
# figures and the host caveat.
#
# Usage:
#   scripts/idle-rss.sh [--settle SECONDS] [--json]
#
# Requires: docker, go, and a Linux /proc. It exits 2 (environment error) when
# any of those is missing, rather than printing a number it could not measure.

set -euo pipefail

SETTLE_SECONDS=30
JSON_OUTPUT=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --settle)
            SETTLE_SECONDS="${2:?--settle needs a number of seconds}"
            shift 2
            ;;
        --json)
            JSON_OUTPUT=1
            shift
            ;;
        -h | --help)
            sed -n '2,25p' "$0"
            exit 0
            ;;
        *)
            echo "error: unknown argument $1" >&2
            echo "hint: usage is scripts/idle-rss.sh [--settle SECONDS] [--json]" >&2
            exit 1
            ;;
    esac
done

die_env() {
    echo "error: $1" >&2
    echo "hint: $2" >&2
    exit 2
}

command -v docker > /dev/null 2>&1 || die_env "docker is not on PATH" "install Docker; this script starts an ephemeral postgres:17-alpine"
command -v go > /dev/null 2>&1 || die_env "go is not on PATH" "install Go 1.26+ to build the nodes binary"
[[ -r /proc/self/status ]] || die_env "/proc is not readable on this host" "this measurement reads VmRSS from /proc/<pid>/status, which is Linux-only"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="$(mktemp -d)"
PG_NAME="nodes-idle-rss-$$"
NODES_PID=""

cleanup() {
    if [[ -n "$NODES_PID" ]] && kill -0 "$NODES_PID" 2> /dev/null; then
        kill -TERM "$NODES_PID" 2> /dev/null || true
        wait "$NODES_PID" 2> /dev/null || true
    fi
    docker stop "$PG_NAME" > /dev/null 2>&1 || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

echo "idle-rss: building the nodes binary" >&2
go build -o "$WORK_DIR/nodes" "$REPO_ROOT/cmd/nodes"

echo "idle-rss: starting an ephemeral postgres:17-alpine" >&2
docker run -d --rm --name "$PG_NAME" \
    -e POSTGRES_PASSWORD=nodes -e POSTGRES_DB=nodes \
    -p 5432 postgres:17-alpine > /dev/null

PG_PORT="$(docker port "$PG_NAME" 5432/tcp | head -1 | sed 's/.*://')"
export NODES_DATABASE_URL="postgres://postgres:nodes@127.0.0.1:${PG_PORT}/nodes?sslmode=disable"

echo "idle-rss: waiting for postgres on port ${PG_PORT}" >&2
for _ in $(seq 1 60); do
    if docker exec "$PG_NAME" pg_isready -U postgres > /dev/null 2>&1; then
        break
    fi
    sleep 1
done

echo "idle-rss: migrating" >&2
"$WORK_DIR/nodes" migrate > /dev/null

# A high port so a developer's own :8080 is never disturbed.
export NODES_LISTEN="127.0.0.1:18080"
echo "idle-rss: starting 'nodes all' on ${NODES_LISTEN}" >&2
"$WORK_DIR/nodes" all > "$WORK_DIR/nodes.log" 2>&1 &
NODES_PID=$!

# The process must actually be serving before the clock starts: a measurement
# taken during startup is a measurement of startup.
READY=0
for _ in $(seq 1 60); do
    if curl -fsS "http://${NODES_LISTEN}/v1alpha1/healthz" > /dev/null 2>&1; then
        READY=1
        break
    fi
    if ! kill -0 "$NODES_PID" 2> /dev/null; then
        echo "error: 'nodes all' exited during startup" >&2
        sed 's/^/  /' "$WORK_DIR/nodes.log" >&2
        exit 2
    fi
    sleep 1
done
[[ "$READY" == "1" ]] || die_env "'nodes all' never answered /v1alpha1/healthz" "inspect the log above; the process may not have bound its port"

echo "idle-rss: idling for ${SETTLE_SECONDS}s" >&2
sleep "$SETTLE_SECONDS"

if ! kill -0 "$NODES_PID" 2> /dev/null; then
    echo "error: 'nodes all' exited while idling" >&2
    sed 's/^/  /' "$WORK_DIR/nodes.log" >&2
    exit 2
fi

VM_RSS_KB="$(awk '/^VmRSS:/ {print $2}' "/proc/${NODES_PID}/status")"
VM_HWM_KB="$(awk '/^VmHWM:/ {print $2}' "/proc/${NODES_PID}/status")"
GO_VERSION="$(go version | awk '{print $3}')"
KERNEL="$(uname -sr)"
ARCH="$(uname -m)"

if [[ "$JSON_OUTPUT" == "1" ]]; then
    printf '{"mode":"all","settle_seconds":%s,"vm_rss_kb":%s,"vm_rss_mib":%.1f,"vm_hwm_kb":%s,"vm_hwm_mib":%.1f,"go":"%s","kernel":"%s","arch":"%s"}\n' \
        "$SETTLE_SECONDS" "$VM_RSS_KB" "$(echo "$VM_RSS_KB" | awk '{print $1/1024}')" \
        "$VM_HWM_KB" "$(echo "$VM_HWM_KB" | awk '{print $1/1024}')" \
        "$GO_VERSION" "$KERNEL" "$ARCH"
else
    printf 'nodes all, idle for %ss\n' "$SETTLE_SECONDS"
    printf '  VmRSS  %s kB (%.1f MiB)\n' "$VM_RSS_KB" "$(echo "$VM_RSS_KB" | awk '{print $1/1024}')"
    printf '  VmHWM  %s kB (%.1f MiB)  peak since start\n' "$VM_HWM_KB" "$(echo "$VM_HWM_KB" | awk '{print $1/1024}')"
    printf '  host   %s %s, %s\n' "$KERNEL" "$ARCH" "$GO_VERSION"
fi
