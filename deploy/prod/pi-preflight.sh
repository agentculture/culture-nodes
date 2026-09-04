#!/usr/bin/env bash
# pi-preflight.sh — non-billable readiness check for a pi actor bridge
# deployment (adapters/pi), run at deploy time and again as the unit's
# ExecStartPre before pointing a production host at it.
#
# "Non-billable" is the whole contract: this script invokes the configured
# pi binary with ONLY `--version` — a read-only, free CLI call — and never
# a prompt, a `-p` run, or anything else that would consume a turn or touch
# a repo. Every other check below is a local file-system, subprocess-exit,
# or read-only HTTP probe; nothing here dispatches actual work.
#
# Usage:
#   pi-preflight.sh <bridge-config.json>
#   PI_BRIDGE_CONFIG=<bridge-config.json> pi-preflight.sh
#
# The config file is the same JSON document adapters/pi's own bridge reads
# (pi-developer.json). This script reads only the subset of fields it needs
# directly out of the file: pi_bin, provider, model, model_endpoint,
# repo_allowlist and state_dir.
#
# Checks, in order. Each names its own failure condition on a distinct
# one-line "preflight: ..." message on stderr and exits non-zero — the whole
# point being that an operator can tell the failure classes apart from a CI
# log without re-running anything interactively. Check 3 is the one
# exception: it is skippable (see its own section) for bootstrap ordering.
#   1. pi_bin is set, is an executable file (never a PATH lookup — an
#      operator who left pi_bin as the bare, PATH-resolvable name "pi" is
#      exactly the mistake this check exists to catch), and
#      `<pi_bin> --version` equals the version pinned in
#      lanes/unix-user.sh (UNIX_USER_PI_VERSION), read from that file rather
#      than baked in here so the pin has one home.
#   2. ~/.pi/agent/models.json exists, parses, and names the configured
#      provider under "providers" with a models[] entry whose id equals the
#      configured model — a pi account without a matching provider has no
#      way to serve the first request.
#   3. GET <model_endpoint>/v1/models returns 200 and lists the configured
#      model. Skippable with SKIP_PI_ENDPOINT_CHECK=1 (downgraded to a
#      warning), since at first bootstrap the endpoint may not be reachable
#      yet in the order deploy steps run.
#   4. every repo_allowlist entry exists and is owned (`stat -c %u`) by the
#      running uid (`id -u`) — a checkout owned by a different account is
#      either unusable in practice or shared with a principal this process
#      should not act as.
#   5. the account this process runs as is a dedicated, unprivileged one —
#      refuses if `id -nG` names the "sudo" or "docker" group (either is
#      root-equivalent). pi has no built-in sandbox, so the account is the
#      confinement; this is the same intent as codex-preflight's check 8.
#   6. state_dir exists (created at mode 700 if absent) and is a writable
#      directory.
#
# On success, prints exactly one line to stdout: the measured pi version,
# e.g. "preflight: ok pi 0.85.0".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONFIG_PATH="${1:-${PI_BRIDGE_CONFIG:-}}"
if [[ -z "$CONFIG_PATH" ]]; then
  echo "preflight: no config path given (pass one as \$1 or set PI_BRIDGE_CONFIG)" >&2
  exit 1
fi
if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "preflight: config file not found: $CONFIG_PATH" >&2
  exit 1
fi

# The pinned pi version lives in lanes/unix-user.sh (UNIX_USER_PI_VERSION) —
# the same file the install lane pins the account's pi from. Read it out of
# that file rather than baking a literal here, so the pin has exactly one
# home and check 1 can never drift from what was installed.
UNIX_USER_LANE="$SCRIPT_DIR/lanes/unix-user.sh"
if [[ ! -f "$UNIX_USER_LANE" ]]; then
  echo "preflight: cannot find lanes/unix-user.sh next to this script to read the pinned pi version: $UNIX_USER_LANE" >&2
  exit 1
fi
PINNED_VERSION=$(sed -n 's/^UNIX_USER_PI_VERSION=\(.*\)$/\1/p' "$UNIX_USER_LANE" | head -n1 | tr -d '[:space:]')
if [[ -z "$PINNED_VERSION" ]]; then
  echo "preflight: could not read UNIX_USER_PI_VERSION from $UNIX_USER_LANE" >&2
  exit 1
fi

# Parse the fields this script needs out of the JSON config with python3
# (present on every host this deploys to — no jq dependency assumed). One
# call emits KEY=VALUE lines on stdout (one REPO= line per allowlist
# entry); any parse problem is reported directly to stderr, already
# "preflight:"-prefixed, and the process exits non-zero so the `||` below
# short-circuits without a second error message layered on top.
CONFIG_FIELDS=$(python3 - "$CONFIG_PATH" <<'PYEOF'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
except OSError as exc:
    print(f"preflight: cannot read config file {path!r}: {exc}", file=sys.stderr)
    sys.exit(1)

try:
    cfg = json.loads(raw)
except ValueError as exc:  # json.JSONDecodeError is a ValueError
    print(f"preflight: config file {path!r} is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)

if not isinstance(cfg, dict):
    print(f"preflight: config file {path!r} must contain a JSON object", file=sys.stderr)
    sys.exit(1)


def emit(key, value):
    # Values are file paths / hostnames / model ids, never multi-line, so a
    # bare KEY=VALUE line is unambiguous for the shell side to split on the
    # first '=' only.
    print(f"{key}={value}")


emit("PI_BIN", cfg.get("pi_bin") or "")
emit("PROVIDER", cfg.get("provider") or "")
emit("MODEL", cfg.get("model") or "")
emit("MODEL_ENDPOINT", cfg.get("model_endpoint") or "")
emit("STATE_DIR", cfg.get("state_dir") or "")

repo_allowlist = cfg.get("repo_allowlist") or []
if not isinstance(repo_allowlist, list):
    print(f"preflight: config file {path!r} field 'repo_allowlist' must be an array", file=sys.stderr)
    sys.exit(1)
for repo in repo_allowlist:
    print(f"REPO={repo}")
PYEOF
) || exit 1

# Split CONFIG_FIELDS into shell variables. Parameter expansion (not `read
# -r key value` with IFS='=') so a value that itself contains '=' survives
# intact instead of being mangled by field-splitting.
PI_BIN=""
PROVIDER=""
MODEL=""
MODEL_ENDPOINT=""
STATE_DIR=""
REPOS=()
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  key="${line%%=*}"
  value="${line#*=}"
  case "$key" in
    PI_BIN) PI_BIN="$value" ;;
    PROVIDER) PROVIDER="$value" ;;
    MODEL) MODEL="$value" ;;
    MODEL_ENDPOINT) MODEL_ENDPOINT="$value" ;;
    STATE_DIR) STATE_DIR="$value" ;;
    REPO) REPOS+=("$value") ;;
  esac
done <<< "$CONFIG_FIELDS"

# --- 1. pi_bin: explicit executable file, --version equals the pin ---------
# `[[ -x "$PI_BIN" ]]` tests PI_BIN as a literal path — it does NOT consult
# PATH the way `command -v` or a bare invocation would, so a config left at
# the PATH-resolvable bare name "pi" fails here rather than silently
# succeeding against whatever binary happens to be on this host's PATH.
if [[ -z "$PI_BIN" || ! -x "$PI_BIN" || ! -f "$PI_BIN" ]]; then
  echo "preflight: pi_bin is not an executable file: ${PI_BIN:-<unset>}" >&2
  exit 1
fi
if ! VERSION_OUTPUT=$("$PI_BIN" --version 2>&1); then
  echo "preflight: pi --version failed to run: $PI_BIN" >&2
  exit 1
fi
# Extract the first semver-shaped token from the output; pi may print a bare
# "0.85.0" or a "pi 0.85.0"-style line, so match the version rather than the
# whole line.
MEASURED_VERSION=$(printf '%s\n' "$VERSION_OUTPUT" | head -n1 | tr -d '\r' | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)
if [[ -z "$MEASURED_VERSION" ]]; then
  echo "preflight: pi --version output does not contain a parseable version: $VERSION_OUTPUT" >&2
  exit 1
fi
if [[ "$MEASURED_VERSION" != "$PINNED_VERSION" ]]; then
  echo "preflight: pi --version ($MEASURED_VERSION) does not equal the pinned version ($PINNED_VERSION) from lanes/unix-user.sh" >&2
  exit 1
fi

# --- 2. ~/.pi/agent/models.json names the provider + model -----------------
MODELS_JSON="$HOME/.pi/agent/models.json"
if [[ ! -f "$MODELS_JSON" ]]; then
  echo "preflight: pi provider config not found: $MODELS_JSON — a pi account without it has no provider, so every session would fail on its first request" >&2
  exit 1
fi
if ! python3 - "$MODELS_JSON" "$PROVIDER" "$MODEL" <<'PYEOF'
import json
import sys

path, provider, model = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with open(path, "r", encoding="utf-8") as f:
        doc = json.load(f)
except ValueError as exc:
    print(f"preflight: pi provider config {path!r} is not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)
except OSError as exc:
    print(f"preflight: cannot read pi provider config {path!r}: {exc}", file=sys.stderr)
    sys.exit(1)

providers = doc.get("providers") if isinstance(doc, dict) else None
if not isinstance(providers, dict) or provider not in providers:
    print(f"preflight: pi provider config {path} does not name provider {provider!r} under 'providers'", file=sys.stderr)
    sys.exit(1)

entry = providers[provider] or {}
models = entry.get("models") or []
ids = {m.get("id") for m in models if isinstance(m, dict)}
if model not in ids:
    print(f"preflight: pi provider {provider!r} in {path} does not list model {model!r}", file=sys.stderr)
    sys.exit(1)
PYEOF
then
  exit 1
fi

# --- 3. GET <model_endpoint>/v1/models lists the model (skippable) ---------
# At first bootstrap the endpoint may not be reachable yet in the order the
# deploy steps run, so SKIP_PI_ENDPOINT_CHECK=1 downgrades this to a
# warning rather than a refusal. Every other check stays a hard gate.
if [[ "${SKIP_PI_ENDPOINT_CHECK:-0}" == "1" ]]; then
  echo "preflight: WARNING — SKIP_PI_ENDPOINT_CHECK=1, so the model endpoint was NOT probed (${MODEL_ENDPOINT:-<unset>}); the first request will discover an unreachable provider if it is wrong" >&2
elif [[ -z "$MODEL_ENDPOINT" ]]; then
  echo "preflight: config has no model_endpoint set" >&2
  exit 1
else
  ENDPOINT_BODY=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$ENDPOINT_BODY'" EXIT
  HTTP_CODE=$(curl -sS -m 10 -o "$ENDPOINT_BODY" -w '%{http_code}' "${MODEL_ENDPOINT%/}/v1/models" 2>/dev/null || echo "000")
  if [[ "$HTTP_CODE" != "200" ]]; then
    echo "preflight: model endpoint ${MODEL_ENDPOINT%/}/v1/models did not return 200 (got $HTTP_CODE)" >&2
    exit 1
  fi
  if ! python3 - "$ENDPOINT_BODY" "$MODEL" <<'PYEOF'
import json
import sys

path, model = sys.argv[1], sys.argv[2]
try:
    with open(path, "r", encoding="utf-8") as f:
        doc = json.load(f)
except ValueError:
    # Not JSON: fall back to a plain substring test so a non-standard but
    # correct listing still passes.
    with open(path, "r", encoding="utf-8") as f:
        if model in f.read():
            sys.exit(0)
    print(f"preflight: model endpoint response does not list model {model!r}", file=sys.stderr)
    sys.exit(1)

ids = set()
data = doc.get("data") if isinstance(doc, dict) else None
if isinstance(data, list):
    ids = {m.get("id") for m in data if isinstance(m, dict)}
if model not in ids:
    print(f"preflight: model endpoint does not list model {model!r}", file=sys.stderr)
    sys.exit(1)
PYEOF
  then
    exit 1
  fi
fi

# --- 4. every repo_allowlist entry exists and is owned by the running uid --
RUNNING_UID=$(id -u)
if [[ ${#REPOS[@]} -gt 0 ]]; then
  for repo in "${REPOS[@]}"; do
    if [[ ! -e "$repo" ]]; then
      echo "preflight: repo_allowlist entry does not exist: $repo" >&2
      exit 1
    fi
    if ! REPO_UID=$(stat -c %u "$repo" 2>/dev/null); then
      echo "preflight: cannot stat repo_allowlist entry to check ownership: $repo" >&2
      exit 1
    fi
    if [[ "$REPO_UID" != "$RUNNING_UID" ]]; then
      echo "preflight: repo_allowlist entry is not owned by the running user (uid $RUNNING_UID): $repo (owned by uid $REPO_UID)" >&2
      exit 1
    fi
  done
fi

# --- 5. the process runs as a dedicated, unprivileged account --------------
# pi has no built-in sandbox, so the account IS the confinement: "sudo" or
# "docker" in `id -nG` is root-equivalent (sudo directly; docker via
# bind-mounting the host root), so either grant defeats the whole point of a
# dedicated account. Same intent as codex-preflight's check 8.
RUNNING_GROUPS=$(id -nG)
for restricted_group in sudo docker; do
  for have_group in $RUNNING_GROUPS; do
    if [[ "$have_group" == "$restricted_group" ]]; then
      echo "preflight: running user is a member of group '$restricted_group' — a pi bridge account must be dedicated and unprivileged, not one with $restricted_group access" >&2
      exit 1
    fi
  done
done

# --- 6. state_dir exists (create at mode 700 if absent) and is writable ----
if [[ -z "$STATE_DIR" ]]; then
  echo "preflight: config has no state_dir set" >&2
  exit 1
fi
if [[ ! -d "$STATE_DIR" ]]; then
  if ! (umask 077 && mkdir -p "$STATE_DIR") 2>/dev/null; then
    echo "preflight: state_dir does not exist and could not be created: $STATE_DIR" >&2
    exit 1
  fi
  chmod 700 "$STATE_DIR"
fi
if [[ ! -w "$STATE_DIR" ]]; then
  echo "preflight: state_dir is not writable: $STATE_DIR" >&2
  exit 1
fi

printf 'preflight: ok pi %s\n' "$MEASURED_VERSION"
