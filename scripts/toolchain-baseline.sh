#!/usr/bin/env bash
# scripts/toolchain-baseline.sh — capture and re-check what each dispatch
# host's toolchains actually are (issue #96, task t19).
#
#   toolchain-baseline.sh capture [host ...]   write docs/baselines/toolchains/<host>.json
#   toolchain-baseline.sh check   [host ...]   re-measure and diff; non-zero on drift
#
# With no host arguments both verbs use HOSTS below.
#
# # Why this exists
#
# The capability surface reports what can EXECUTE under the dispatch
# posture. Half of that is measured live by each bridge; the other half —
# whether a given mode grants writes, egress, or the ability to start a
# nested confinement helper — comes from three dispatched probe runs:
#
#   01M03374VAKH0KHN0GDZ466NP4  thor, snap-packaged uv vs snap-confine
#   01M0342X60F3NY8MH150G48AZ6  orin, standalone uv vs a read-only cache
#   01M0356BK8QYR3119R8VY1YY9Q  orin, nothing writable at all under read-only
#
# Those findings are pinned to a moment: this uv, this codex-cli, this
# kernel. `check` is what notices the moment has passed. It compares each
# host's measured toolchains against the committed baseline and fails on any
# difference — a uv upgrade, a snap replaced by a standalone binary, a
# codex-cli bump, a tool appearing or vanishing. A red diff means re-running
# the three probes, not editing the baseline.
#
# # How it measures
#
# By piping the bridges' own `preflight.py` over ssh into `python3 -`. That
# module is byte-identical in all four adapters and imports nothing outside
# the stdlib, so the host being measured needs no install, no checkout and
# no bridge upgrade — and the measurement is the SAME CODE the surface uses
# rather than a second implementation of `which` and `readlink` in shell,
# which is exactly the duplication tests/lint/preflightsurface_test.go
# exists to prevent.
#
# PATH matters and is recorded. `on_path` is relative to the PATH of the
# measuring process, and a dispatched session inherits the BRIDGE's PATH,
# not a login shell's. So a remote measurement is run under the systemd user
# manager's PATH where one is available (orin's carries ~/.local/bin and an
# ssh login shell's does not, which would otherwise report orin's uv absent
# on a host that has it).
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BASELINE_DIR="${TOOLCHAIN_BASELINE_DIR:-$REPO_ROOT/docs/baselines/toolchains}"

# The instrument. Any bridge's copy would do -- they are byte-identical and
# a Go lint test fails the build if they stop being -- so naming one is a
# choice of address, not of behaviour.
PROBE="$REPO_ROOT/adapters/codex/src/codex_bridge/preflight.py"

# The hosts a dispatch actually lands on, plus spark, which is where the
# operator lane and the claude bridges run.
HOSTS=(thor orin spark)

# The toolchains measured on every host: the three the probe runs tested,
# plus each backend CLI whose version bump would re-open them.
# node and npm are here because the TDD merge gate this repo is making a node
# (#101) includes a web suite -- `cd web && npm run build && npm test`. A
# capability surface that never measures npm cannot answer "can this lane run
# the web build?", which is the question that decides where the gate node runs.
# Measured by task t15 (run 01M05ZGNT86MAFDHATB6W5VYPN): preflight.py is
# tool-agnostic (`names = list(argv)`), so naming them here is the entire
# change -- the probe needs no edit at all.
TOOLS=(uv go gh git node npm codex claude colleague)

usage() {
	sed -n '2,12p' "${BASH_SOURCE[0]}" >&2
	exit 2
}

# measure <host> prints the probe's JSON for that host.
#
# "spark" (or "local") is measured in this process rather than over ssh: it
# is where this script runs, and an ssh loopback would only add a different
# PATH to explain.
measure() {
	local host=$1
	if [ "$host" = "spark" ] || [ "$host" = "local" ]; then
		python3 "$PROBE" "${TOOLS[@]}"
		return
	fi
	# Prefer the systemd user manager's PATH -- the one a bridge process,
	# and therefore a dispatched session, actually gets.
	ssh -o BatchMode=yes "$host" '
		userpath=$(systemctl --user show-environment 2>/dev/null | sed -n "s/^PATH=//p" | head -1)
		[ -n "$userpath" ] && export PATH="$userpath"
		python3 - '"${TOOLS[*]}" <"$PROBE"
}

capture() {
	mkdir -p "$BASELINE_DIR"
	local host
	for host in "$@"; do
		printf 'capturing %s ... ' "$host"
		measure "$host" >"$BASELINE_DIR/$host.json"
		printf 'wrote %s\n' "$BASELINE_DIR/$host.json"
	done
}

# facts strips the one field that legitimately varies between two honest
# measurements of an unchanged host: `search_path`. It is captured because
# `on_path` is relative to it and a baseline that did not say which PATH it
# used could not be compared at all — but an operator running `check` from a
# shell with a different PATH has not found drift, and a check that cried
# wolf about that would stop being run. A PATH difference is reported
# separately, below, where it can be read rather than acted on.
facts() {
	python3 -c 'import json,sys; d=json.load(sys.stdin); d.pop("search_path", None); print(json.dumps(d, indent=2))'
}

check() {
	local host drift=0 baseline current
	for host in "$@"; do
		baseline="$BASELINE_DIR/$host.json"
		if [ ! -f "$baseline" ]; then
			printf 'no baseline for %s (%s); run "capture %s" first\n' "$host" "$baseline" "$host" >&2
			drift=1
			continue
		fi
		current=$(measure "$host")
		if diff -u <(facts <"$baseline") <(printf '%s\n' "$current" | facts) >/tmp/toolchain-drift.$$; then
			printf 'ok   %s\n' "$host"
			if [ "$(jq -r '.search_path' <"$baseline")" != "$(printf '%s\n' "$current" | jq -r '.search_path')" ]; then
				printf '     note: measured under a different PATH than the baseline was; the toolchain facts agree anyway\n'
			fi
		else
			printf 'DRIFT %s\n' "$host"
			cat /tmp/toolchain-drift.$$
			drift=1
		fi
		rm -f /tmp/toolchain-drift.$$
	done
	if [ "$drift" -ne 0 ]; then
		cat >&2 <<'EOF'

A toolchain changed on a dispatch host. The three probe runs behind the
capability surface's posture map (01M03374VAKH0KHN0GDZ466NP4,
01M0342X60F3NY8MH150G48AZ6, 01M0356BK8QYR3119R8VY1YY9Q) measured the OLD
state, so re-run them before trusting the surface again, then re-capture:

  scripts/toolchain-baseline.sh capture <host>

Editing the baseline to match without re-probing is how a surface starts
reporting what someone believes instead of what a dispatch measured.
EOF
		return 1
	fi
}

[ $# -ge 1 ] || usage
verb=$1
shift
hosts=("$@")
[ ${#hosts[@]} -gt 0 ] || hosts=("${HOSTS[@]}")

case "$verb" in
capture) capture "${hosts[@]}" ;;
check) check "${hosts[@]}" ;;
*) usage ;;
esac
