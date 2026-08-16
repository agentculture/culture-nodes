#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# WHAT THIS GATES, and why it grew (task t18).
#
# The zero-dependency property was never the root package's alone. Every
# bridge in adapters/ declares `dependencies = []` for a sharper reason than
# the CLI does: a bridge is `uv tool install`ed beside somebody else's
# toolchain (a codex CLI, a claude CLI, a colleague venv) on a host this repo
# does not control, and a PyPI dependency graph is one more thing that host
# has to reconcile before an actor can dial in.
#
# Until t18 this script loaded exactly ONE file -- the root pyproject -- so
# the adapters' half of the property was convention, checked by nobody. That
# mattered right now: the dial-in transport was a candidate for a websocket
# client, and "it would take a dependency" was an argument held in memory
# rather than a test. The manifest list below is a glob, so a sixth adapter
# is covered the day it is added rather than the day someone remembers it.
#
# Usage:
#   check-zero-runtime-deps.sh              # the CI gate: every manifest, then
#                                           # the bare-environment smoke test
#   check-zero-runtime-deps.sh --list       # print the manifests it would check
#   check-zero-runtime-deps.sh <toml>...    # check exactly these manifests
#
# The explicit-path form is what tests/test_adapter_zero_dependencies.py uses
# to prove the gate REJECTS a manifest that gained a dependency -- a guard
# nobody has watched fail is a guard nobody knows works.

manifests=("$ROOT/pyproject.toml" "$ROOT"/adapters/*/pyproject.toml)
check_runtime=1

case "${1:-}" in
    --list)
        printf '%s\n' "${manifests[@]}"
        exit 0
        ;;
    "") ;;
    *)
        manifests=("$@")
        check_runtime=0
        ;;
esac

python3 - "${manifests[@]}" <<'PY'
import sys
import tomllib

failures = []
for path in sys.argv[1:]:
    with open(path, "rb") as handle:
        dependencies = tomllib.load(handle)["project"]["dependencies"]
    if dependencies:
        failures.append(f"{path}: project.dependencies must remain empty, found: {dependencies!r}")
    else:
        print(f"ok: {path} declares no runtime dependencies")
if failures:
    raise SystemExit("\n".join(failures))
PY

if [[ "$check_runtime" == "1" ]]; then
    # --isolated gives this invocation a fresh environment. --no-project prevents
    # uv from importing this checkout's dev dependency group; only the editable
    # package (whose runtime dependency list was checked above) is installed.
    uv run --isolated --no-project --with-editable "$ROOT" nodes whoami
fi
