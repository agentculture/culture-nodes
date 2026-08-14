#!/usr/bin/env bash
# scripts/validate-examples.sh
#
# Compile every workflow under examples/ and fail if any of them does not.
#
# Issue #73's recurrence half. `examples/pr-upkeep/workflow.yaml` shipped an
# authoring convention that had never compiled -- the mechanism worked, the
# documented shape did not, and nothing noticed because nothing ever compiled
# the examples. Fixing the example closes that instance; this script is what
# stops the next one.
#
# It runs `nodes validate` -- the same verb an author runs by hand -- once per
# file, and needs NO control plane: validate reads a file and compiles it
# through internal/compiler, so there is no database, no API server, and no
# network in this path. NODES_DATABASE_URL is scrubbed from each invocation's
# environment so that stays true by construction rather than by assertion.
#
# Usage:
#   scripts/validate-examples.sh
#
# Env:
#   NODES_BIN   path to an already-built `nodes` binary. When unset, the
#               script builds one into a temporary directory.
#
# Exit codes (matching the CLI's own policy, docs in cmd/nodes/validate.go):
#   0   every example compiles
#   1   at least one example does not compile (a domain outcome)
#   2   the check could not run (build failed, or no examples were found)

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

nodes_bin="${NODES_BIN:-}"
build_dir=""
cleanup() {
	if [[ -n $build_dir ]]; then
		rm -rf "$build_dir"
	fi
}
trap cleanup EXIT

if [[ -z $nodes_bin ]]; then
	build_dir="$(mktemp -d)"
	nodes_bin="$build_dir/nodes"
	echo "building nodes into $nodes_bin"
	if ! go build -o "$nodes_bin" ./cmd/nodes; then
		echo "error: could not build ./cmd/nodes" >&2
		echo "hint: run 'go build ./...' to see the compile failure" >&2
		exit 2
	fi
fi

# Both spellings are collected: the compiler reads anything that is not .json
# as YAML, so an example spelled either way is one an author could copy.
workflows=()
while IFS= read -r path; do
	workflows+=("$path")
done < <(find examples -type f \( -name '*.yaml' -o -name '*.yml' \) | sort)

# A gate that matched nothing would report a clean sweep over zero files --
# the exact way this check would rot back into the state issue #73 describes.
if [[ ${#workflows[@]} -eq 0 ]]; then
	echo "error: no workflow files found under examples/" >&2
	echo "hint: this gate must never pass vacuously; check the find expression and that examples/ still exists" >&2
	exit 2
fi

echo "compiling ${#workflows[@]} workflow(s) under examples/ (no control plane)"

failed=()
for workflow in "${workflows[@]}"; do
	# validate prints its verdict to stdout and CliErrors to stderr; both are
	# captured so a failure report carries whichever one explains it.
	if output="$(env -u NODES_DATABASE_URL "$nodes_bin" validate "$workflow" 2>&1)"; then
		echo "ok   $workflow"
	else
		failed+=("$workflow")
		echo "FAIL $workflow"
		while IFS= read -r line; do
			printf '       %s\n' "$line"
		done <<<"$output"
	fi
done

if [[ ${#failed[@]} -gt 0 ]]; then
	echo >&2
	echo "error: ${#failed[@]} of ${#workflows[@]} example workflow(s) do not compile:" >&2
	printf '  %s\n' "${failed[@]}" >&2
	echo "hint: run 'go run ./cmd/nodes validate <file>' for the full diagnostics of one file" >&2
	exit 1
fi

echo "all ${#workflows[@]} example workflow(s) compile"
