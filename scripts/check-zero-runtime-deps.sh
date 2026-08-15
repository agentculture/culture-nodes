#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

python3 - "$ROOT/pyproject.toml" <<'PY'
import sys
import tomllib

with open(sys.argv[1], "rb") as handle:
    dependencies = tomllib.load(handle)["project"]["dependencies"]
if dependencies:
    raise SystemExit(f"project.dependencies must remain empty, found: {dependencies!r}")
print("ok: project.dependencies is empty")
PY

# --isolated gives this invocation a fresh environment. --no-project prevents
# uv from importing this checkout's dev dependency group; only the editable
# package (whose runtime dependency list was checked above) is installed.
uv run --isolated --no-project --with-editable "$ROOT" nodes whoami
