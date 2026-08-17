#!/usr/bin/env bash
# scripts/lint-all.sh — every lint job CI runs, run the way CI runs it
# (issue #123, task t8).
#
#   lint-all.sh                       every job
#   lint-all.sh root                  the tests.yml `lint` job
#   lint-all.sh adapter-codex         the adapter-codex.yml `lint` job
#   lint-all.sh adapter-claude-code   the adapter-claude-code.yml `lint` job
#   lint-all.sh --list                print the job names and exit
#
# # Why this exists
#
# Three CI jobs are literally named `lint`, and they invoke the SAME four
# linters DIFFERENTLY. The difference is not incidental, it is the whole
# problem:
#
#   tests.yml               root scope, from the repo root
#   adapter-codex.yml       from the REPO ROOT against adapter paths --
#                           so the ROOT black/isort/flake8 config applies
#   adapter-claude-code.yml from the ADAPTER DIRECTORY -- so that adapter's
#                           OWN config applies
#
# Run the adapter-directory form for codex and it passes; CI runs the
# repo-root form and it fails. PR #122 went red on three jobs named `lint`
# after a fully green local run for exactly that reason. There was no single
# command an author could run that meant "CI's lint will be green", so the
# author ran a plausible subset and CI ran the real thing.
#
# So this script is not a convenience wrapper around the workflows: it is the
# definition, and the three workflows call it. A green run of this script and
# a red CI lint job cannot coexist by construction, because there is only one
# copy of the commands. Same shape as scripts/check-zero-runtime-deps.sh and
# scripts/check-vendored-skill-diff.py, which CI likewise invokes rather than
# inlines.
#
# # Scope
#
# The three jobs named `lint`, and only those. Whether go.yml's `go vet` and
# web.yml's `webglass` job belong here is a real question and a parked one
# (the easy-pickings spec records it as an open non-blocking unknown); adding
# them unilaterally would change what "lint is green" means for every caller.
#
# # Two steps need context a laptop does not have
#
#   vendored-skills  needs a merge RANGE. CI passes the PR base sha; locally
#                    it falls back to the merge-base with origin/main.
#   triage           needs an authenticated `gh` and a reachable GitHub.
#
# Neither is quietly dropped. A step that cannot run is reported as SKIPPED,
# named in the summary, and the summary says plainly that CI will still run
# it -- because a silent skip is how the gap this script closes reopens.
# `LINT_ALL_SKIP="triage vendored-skills"` skips them deliberately.
#
# # Failures accumulate
#
# CI stops at the first failing step. This runs them all and reports every
# failure, which is strictly safer: it can report more red than CI, never
# less, and an author fixing four findings in one pass beats four round trips.
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 2

# The version tests.yml pins. Pinned here too, and asserted rather than
# assumed: linting with a different markdownlint than CI's is the same
# green-here/red-there gap in miniature.
MARKDOWNLINT_VERSION=0.21.0

JOBS=(root adapter-codex adapter-claude-code)

FAILED=()
# Two different facts, deliberately not one list. WAIVED is "the operator asked
# me not to run this"; UNRUNNABLE is "this environment could not run it". Only
# the second is a measurement that did not happen, and folding it into a green
# exit is the exact defect #146 is about one directory over: an instrument that
# reports success without having measured anything. So it gets its own exit
# code, matching the policy the rest of the repo already uses --
# scripts/merge-gate.py's exit 2 is `measurement_incomplete`, and
# culture_nodes/cli/_errors.py reserves 2 for an environment error.
WAIVED=()
UNRUNNABLE=()

usage() {
	sed -n '2,10p' "${BASH_SOURCE[0]}" >&2
	exit 2
}

# step <name> <command...> runs one workflow step, verbatim, and records it.
step() {
	local name=$1
	shift
	if [[ " ${LINT_ALL_SKIP:-} " == *" $name "* ]]; then
		printf '\n>>> SKIP %s (LINT_ALL_SKIP)\n' "$name"
		WAIVED+=("$name")
		return 0
	fi
	printf '\n>>> %s\n' "$name"
	local rc=0
	"$@" || rc=$?
	if [ "$rc" -eq 0 ]; then
		return 0
	fi
	# A step is allowed to say "I could not measure" rather than "this is
	# wrong", and the repo has one code for it: merge-gate.py exits 2 for
	# measurement_incomplete, culture_nodes/cli/_errors.py reserves 2 for an
	# environment error, and triage-report.py returns 2 when GitHub cannot be
	# read. Treating that as FAILED is the conflation this script was just
	# corrected for, one level up -- and it is not hypothetical: a transient
	# GitHub 503 turned PR #159 red with `FAILED: triage` on a tree whose
	# table was fine.
	if [ "$rc" -eq 2 ]; then
		printf '<<< UNRUNNABLE %s (exit 2 -- could not measure, not a finding)\n' "$name"
		UNRUNNABLE+=("$name")
		return 0
	fi
	printf '<<< FAILED %s\n' "$name"
	FAILED+=("$name")
}

# skip <name> <reason> records a step this environment cannot run. This is NOT
# a pass: the script exits 2 if any step lands here, so a caller cannot read a
# partial run as a full one.
skip() {
	printf '\n>>> SKIP %s -- %s\n' "$1" "$2"
	printf '    CI runs it anyway; a green run here does not clear it.\n'
	UNRUNNABLE+=("$1")
}

# ---------------------------------------------------------------------------
# job: root -- .github/workflows/tests.yml, job `lint`
# ---------------------------------------------------------------------------

job_root() {
	# Vendored skills unchanged in merge range.
	#
	# CI supplies RANGE_BASE (the PR base sha, or `github.event.before` on a
	# push). There is no such thing locally, so compare against where this
	# branch left main.
	local range_base="${RANGE_BASE:-}" head="${GITHUB_SHA:-HEAD}"
	if [ -z "$range_base" ]; then
		range_base=$(git merge-base origin/main HEAD 2>/dev/null || true)
	fi
	if [ -n "$range_base" ]; then
		step vendored-skills \
			python3 scripts/check-vendored-skill-diff.py "$range_base" "$head"
	else
		skip vendored-skills "no merge range (fetch origin/main, or set RANGE_BASE)"
	fi

	step zero-deps scripts/check-zero-runtime-deps.sh
	step black uv run black --check culture_nodes tests
	step isort uv run isort --check-only culture_nodes tests
	step flake8 uv run flake8 culture_nodes tests
	step bandit uv run bandit -c pyproject.toml -r culture_nodes
	step markdownlint markdownlint_step
	step afi-rubric uv run teken cli doctor . --strict

	# Open-issue triage is complete and current. Needs a reachable GitHub.
	if [ -n "${GH_TOKEN:-}" ] || gh auth status >/dev/null 2>&1; then
		step triage python3 scripts/triage-report.py --check
	else
		skip triage "gh is not authenticated"
	fi
}

markdownlint_step() {
	# CI's runner has no markdownlint-cli2 and installs the pin. Installing
	# unconditionally would rewrite a developer's global npm on every run, so
	# install only when the pinned version is not already there -- and install
	# rather than proceed with a DIFFERENT version, because a version skew
	# between here and CI is the exact failure this script exists to prevent.
	if ! markdownlint-cli2 --version 2>/dev/null | head -1 | grep -q "v$MARKDOWNLINT_VERSION"; then
		npm install -g "markdownlint-cli2@$MARKDOWNLINT_VERSION" || return 1
	fi
	markdownlint-cli2 "**/*.md" "#node_modules" "#.local" "#.claude/skills" "#.teken"
}

# ---------------------------------------------------------------------------
# job: adapter-codex -- .github/workflows/adapter-codex.yml, job `lint`
#
# Run from the REPO ROOT against adapter paths. adapters/codex has no lint
# config of its own, so this deliberately reuses the ROOT black/isort/flake8
# config and dev group. Running the same four linters from inside
# adapters/codex would pick up different configuration and is precisely the
# locally-green/CI-red form.
# ---------------------------------------------------------------------------

job_adapter_codex() {
	step codex-black uv run black --check adapters/codex/src adapters/codex/tests
	step codex-isort uv run isort --check-only adapters/codex/src adapters/codex/tests
	step codex-flake8 uv run flake8 adapters/codex/src adapters/codex/tests
	step codex-bandit uv run bandit -c pyproject.toml -r adapters/codex/src
}

# ---------------------------------------------------------------------------
# job: adapter-claude-code -- .github/workflows/adapter-claude-code.yml, job
# `lint`
#
# Run from the ADAPTER DIRECTORY (the workflow sets
# defaults.run.working-directory), so adapters/claude-code's own pyproject
# supplies the config and its own uv project supplies the toolchain. The cd is
# the load-bearing part; the arguments are `src tests`, not adapter-prefixed
# paths.
# ---------------------------------------------------------------------------

job_adapter_claude_code() {
	local dir="$ROOT/adapters/claude-code"
	step cc-black in_dir "$dir" uv run black --check src tests
	step cc-isort in_dir "$dir" uv run isort --check-only src tests
	step cc-flake8 in_dir "$dir" uv run flake8 src tests
}

in_dir() {
	local dir=$1
	shift
	(cd "$dir" && "$@")
}

# ---------------------------------------------------------------------------

case "${1:-}" in
--list)
	printf '%s\n' "${JOBS[@]}"
	exit 0
	;;
-h | --help)
	usage
	;;
esac

requested=("$@")
[ ${#requested[@]} -gt 0 ] || requested=("${JOBS[@]}")

for job in "${requested[@]}"; do
	printf '\n=== job: %s ===\n' "$job"
	case "$job" in
	root) job_root ;;
	adapter-codex) job_adapter_codex ;;
	adapter-claude-code) job_adapter_claude_code ;;
	*)
		printf 'error: unknown job %s\n' "$job" >&2
		printf 'hint: one of: %s\n' "${JOBS[*]}" >&2
		exit 2
		;;
	esac
done

printf '\n=== summary ===\n'
if [ ${#WAIVED[@]} -gt 0 ]; then
	printf 'waived:     %s (LINT_ALL_SKIP)\n' "${WAIVED[*]}"
fi
if [ ${#UNRUNNABLE[@]} -gt 0 ]; then
	printf 'UNRUNNABLE: %s\n' "${UNRUNNABLE[*]}" >&2
	printf '            this environment could not run these; CI runs them anyway.\n' >&2
fi
# A failure outranks an unrunnable step: a defect you measured is more
# actionable than one you could not look for.
if [ ${#FAILED[@]} -gt 0 ]; then
	printf 'FAILED:     %s\n' "${FAILED[*]}" >&2
	exit 1
fi
if [ ${#UNRUNNABLE[@]} -gt 0 ]; then
	printf 'every step this environment could run passed, but %d could not run -- exiting 2\n' "${#UNRUNNABLE[@]}" >&2
	exit 2
fi
printf 'all lint steps passed\n'
