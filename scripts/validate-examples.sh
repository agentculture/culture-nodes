#!/usr/bin/env bash
# scripts/validate-examples.sh
#
# Compile every workflow under examples/ and fail if any of them does not, and
# check every gate matrix they carry against the vocabulary the gate program
# actually reads.
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
# The second half is issue #148's. A workflow that COMPILES can still carry a
# gate matrix nobody reads: the matrix is a JSON string in a code node's argv,
# so `nodes validate` sees a string and is right to. #148 found an entry
# carrying "tools" and "threshold" -- neither a key scripts/merge-gate.py has
# ever read -- so the tools were never required of the host and the threshold
# was never compared against anything, while the entry looked exactly like a
# declared measurement. The gate reported not_applicable over an empty set and
# nothing complained. merge-gate.py now refuses an unknown key by name; this is
# where that refusal is exercised, so a malformed matrix fails HERE rather than
# on a live dispatch, and where the matrices this repo actually ships are held
# to the same check.
#
# Usage:
#   scripts/validate-examples.sh
#
# Env:
#   NODES_BIN   path to an already-built `nodes` binary. When unset, the
#               script builds one into a temporary directory.
#
# Exit codes (matching the CLI's own policy, docs in cmd/nodes/validate.go):
#   0   every example compiles and every gate matrix it carries is readable
#   1   at least one example does not compile, or a gate matrix is malformed,
#       or the unknown-key refusal did not fire (all domain outcomes)
#   2   the check could not run (build failed, no examples were found, or no
#       gate matrix was found to check)

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

nodes_bin="${NODES_BIN:-}"
build_dir=""
matrix_dir=""
cleanup() {
	if [[ -n $build_dir ]]; then
		rm -rf "$build_dir"
	fi
	if [[ -n $matrix_dir ]]; then
		rm -rf "$matrix_dir"
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

# ---------------------------------------------------------------------------
# The gate matrices those workflows carry (issue #148). See the header.

gate_program="$repo_root/scripts/merge-gate.py"
matrix_dir="$(mktemp -d)"

# Every JSON object with a `gates` key that appears as a block scalar in an
# example, written out one file per matrix. Deliberately NOT a YAML parse: this
# script has no YAML dependency and `nodes validate --json` reports a verdict
# and a digest rather than the compiled graph, so the matrices are read out of
# the source text the same way an author reads them.
echo
echo "extracting gate matrices from ${#workflows[@]} example(s)"
python3 - "$matrix_dir" "${workflows[@]}" <<'PY'
import json
import pathlib
import sys

destination = pathlib.Path(sys.argv[1])
found = 0
for name in sys.argv[2:]:
    lines = pathlib.Path(name).read_text(encoding="utf-8").splitlines()
    for index, line in enumerate(lines):
        if line.strip() != "{":
            continue
        margin = len(line) - len(line.lstrip())
        block = []
        for candidate in lines[index:]:
            if candidate.strip() and len(candidate) - len(candidate.lstrip()) < margin:
                break
            block.append(candidate[margin:])
        try:
            matrix = json.loads("\n".join(block))
        except json.JSONDecodeError:
            continue
        if not isinstance(matrix, dict) or "gates" not in matrix:
            continue
        found += 1
        # The temp file is numbered, and the workflow it came from is recorded
        # BESIDE it rather than encoded into its name, so a refusal below names
        # the workflow an author has to go and fix rather than a temporary file.
        # Encoding the path into the filename (replacing '/' with '_') was
        # lossy: decoding it back turned every underscore into a slash, so any
        # workflow path containing one would be misreported. No example carries
        # an underscore today, which is exactly why a name-mangling scheme
        # survives until the day it silently mislabels a failure.
        target = destination / f"{found:02d}.json"
        target.write_text(json.dumps(matrix), encoding="utf-8")
        target.with_suffix(".origin").write_text(name, encoding="utf-8")
        print(f"  {len(matrix['gates'])} gate(s) at line {index + 1} of {name}")
PY

matrices=()
while IFS= read -r path; do
	matrices+=("$path")
done < <(find "$matrix_dir" -type f -name '*.json' | sort)

# The same rule the compile loop holds itself to: a sweep over zero files is
# how this check rots back into the state #148 describes.
if [[ ${#matrices[@]} -eq 0 ]]; then
	echo "error: no gate matrix found in any example workflow" >&2
	echo "hint: examples/merge-gate/workflow.yaml pins one; if it moved or changed shape, this extraction must follow it" >&2
	exit 2
fi

bad_matrices=()
for matrix in "${matrices[@]}"; do
	# Read the origin the extractor recorded rather than decoding it out of
	# the filename; the mapping is exact for any path, underscores included.
	origin="$(cat "${matrix%.json}.origin")"
	if output="$(python3 "$gate_program" --gates "@$matrix" --check-matrix 2>&1)"; then
		echo "ok   $origin -- $(head -n 1 <<<"$output")"
	else
		bad_matrices+=("$origin")
		echo "FAIL $origin"
		while IFS= read -r line; do
			printf '       %s\n' "$line"
		done <<<"$output"
	fi
done

if [[ ${#bad_matrices[@]} -gt 0 ]]; then
	echo >&2
	echo "error: ${#bad_matrices[@]} of ${#matrices[@]} pinned gate matrix/matrices declare something the gate program cannot read:" >&2
	printf '  %s\n' "${bad_matrices[@]}" >&2
	echo "hint: a key nobody reads declares nothing while looking like it does; fix the key the refusal names, in the example workflow it came from" >&2
	exit 1
fi

# The refusal itself, exercised. Everything above is the accepting direction,
# and a guard that had stopped refusing would pass all of it.
malformed='{"base":"origin/main","gates":[{"gate":"go-test","reaches":["**/*.go"],"command":["go","test","./..."],"tools":["go"],"threshold":0}]}'
if refusal="$(python3 "$gate_program" --gates "$malformed" --check-matrix 2>&1)"; then
	echo "error: the gate program accepted a matrix carrying 'tools' and 'threshold', which it never reads" >&2
	echo "hint: scripts/merge-gate.py's KNOWN_GATE_KEYS check is not firing; issue #148 is open again" >&2
	exit 1
fi
for key in tools threshold; do
	if [[ $refusal != *"'$key'"* ]]; then
		echo "error: the refusal did not name the offending key '$key':" >&2
		printf '  %s\n' "$refusal" >&2
		echo "hint: the refusal must name what is wrong; an author cannot fix a key the message does not identify" >&2
		exit 1
	fi
done
echo "ok   an unknown gate-matrix key is refused by name"

echo "all ${#workflows[@]} example workflow(s) compile; all ${#matrices[@]} pinned gate matrix/matrices are readable"
