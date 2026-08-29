#!/usr/bin/env bash
# codex-preflight.sh — non-billable readiness check for a codex bridge
# deployment (adapters/codex), run before pointing a production host at it.
#
# "Non-billable" is the whole contract: this script invokes the configured
# codex binary with ONLY `--version` and `login status` — both read-only,
# free CLI calls — and never `codex exec` or anything else that would
# consume a turn or touch a repo. Every check below is a local file-system
# or subprocess-exit-code probe; nothing here dispatches actual work.
#
# Usage:
#   codex-preflight.sh <bridge-config.json>
#   CODEX_BRIDGE_CONFIG=<bridge-config.json> codex-preflight.sh
#
# The config file is the same JSON document adapters/codex's own
# Config.load() reads (see adapters/codex/src/codex_bridge/config.py) —
# this script only reads the subset of fields it needs directly out of the
# file; it does not apply that module's CODEX_BRIDGE_* env-var overrides,
# since preflight is meant to validate the file an operator is about to
# ship, not the merged runtime config a running bridge process would use.
#
# Checks, in order. Checks 1-6 and 8 each have their own failure class with
# a distinct one-line "preflight: ..." message on stderr and a non-zero
# exit. Check 7 is the one exception — see its own section below:
#   1. codex_bin is set and is an executable file (never a PATH lookup —
#      an operator who left codex_bin as the bare, PATH-resolvable name
#      "codex" is exactly the mistake this check exists to catch)
#   2. `<codex_bin> --version` runs and its output parses to a version
#   3. `<codex_bin> login status` reports an authenticated session
#      (codex_env.CODEX_HOME, when set, is passed through to this call —
#      it selects which auth profile codex reads)
#   4. every repo_allowlist entry is a real git checkout
#   5. state_dir exists (created if absent) and is writable
#   6. a non-loopback host requires auth_token to be set
#   7. WARNING ONLY, does not fail the deploy: can the host create an
#      unprivileged user namespace? This used to be the safety line (issue
#      #63) back when codex sandboxed every shell command inside bwrap. As
#      of issue #243 the safety line is the account this process runs as
#      (check 8), not the sandbox, so a restricted userns is no longer a
#      deploy blocker — it is printed for visibility only.
#   8. the account this process runs as is a dedicated, unprivileged one —
#      refuses if `id -nG` names the "sudo" or "docker" group (either is
#      root-equivalent) or if any repo_allowlist checkout is not owned by
#      the running uid (issue #243)
#
# On success, prints exactly one line to stdout: the measured codex
# version, e.g. "preflight: ok codex-cli 0.147.0".
set -euo pipefail

CONFIG_PATH="${1:-${CODEX_BRIDGE_CONFIG:-}}"
if [[ -z "$CONFIG_PATH" ]]; then
  echo "preflight: no config path given (pass one as \$1 or set CODEX_BRIDGE_CONFIG)" >&2
  exit 1
fi
if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "preflight: config file not found: $CONFIG_PATH" >&2
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
    # Values are file paths / hostnames / tokens, never multi-line, so a
    # bare KEY=VALUE line is unambiguous for the shell side to split on
    # the first '=' only.
    print(f"{key}={value}")


emit("CODEX_BIN", cfg.get("codex_bin") or "")
emit("STATE_DIR", cfg.get("state_dir") or "")
emit("HOST", cfg.get("host") or "")
emit("AUTH_TOKEN", cfg.get("auth_token") or "")
codex_env = cfg.get("codex_env") or {}
if not isinstance(codex_env, dict):
    print(f"preflight: config file {path!r} field 'codex_env' must be an object", file=sys.stderr)
    sys.exit(1)
emit("CODEX_HOME", codex_env.get("CODEX_HOME") or "")

repo_allowlist = cfg.get("repo_allowlist") or []
if not isinstance(repo_allowlist, list):
    print(f"preflight: config file {path!r} field 'repo_allowlist' must be an array", file=sys.stderr)
    sys.exit(1)
for repo in repo_allowlist:
    print(f"REPO={repo}")
PYEOF
) || exit 1

# Split CONFIG_FIELDS into shell variables. Parameter expansion (not `read
# -r key value` with IFS='=') so a value that itself contains '=' (a token,
# say) survives intact instead of being mangled by field-splitting.
CODEX_BIN=""
STATE_DIR=""
HOST=""
AUTH_TOKEN=""
CODEX_HOME=""
REPOS=()
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  key="${line%%=*}"
  value="${line#*=}"
  case "$key" in
    CODEX_BIN) CODEX_BIN="$value" ;;
    STATE_DIR) STATE_DIR="$value" ;;
    HOST) HOST="$value" ;;
    AUTH_TOKEN) AUTH_TOKEN="$value" ;;
    CODEX_HOME) CODEX_HOME="$value" ;;
    REPO) REPOS+=("$value") ;;
  esac
done <<< "$CONFIG_FIELDS"

# --- 1. codex_bin: explicit file, never an ambient PATH lookup -------------
# `[[ -x "$CODEX_BIN" ]]` tests CODEX_BIN as a literal path relative to the
# current directory (or absolute) — it does NOT consult PATH the way
# `command -v` or a bare invocation would, so a config left at the
# PATH-resolvable bare name "codex" fails here rather than silently
# succeeding against whatever binary happens to be on this host's PATH.
if [[ -z "$CODEX_BIN" || ! -x "$CODEX_BIN" || ! -f "$CODEX_BIN" ]]; then
  echo "preflight: codex_bin is not an executable file: ${CODEX_BIN:-<unset>}" >&2
  exit 1
fi

# --- 2. `<codex_bin> --version` runs and parses -----------------------------
if ! VERSION_OUTPUT=$("$CODEX_BIN" --version 2>&1); then
  echo "preflight: codex_bin --version failed to run: $CODEX_BIN" >&2
  exit 1
fi
VERSION_LINE=$(printf '%s\n' "$VERSION_OUTPUT" | head -n1 | tr -d '\r')
if [[ ! "$VERSION_LINE" =~ [0-9]+\.[0-9]+\.[0-9]+ ]]; then
  echo "preflight: --version output does not contain a parseable version: $VERSION_LINE" >&2
  exit 1
fi

# --- 3. `<codex_bin> login status` reports an authenticated session --------
# CODEX_HOME, when the config sets one, selects which auth profile codex
# reads — it must ride along on this call specifically, since that is the
# only codex invocation this script makes that consults stored auth.
if [[ -n "$CODEX_HOME" ]]; then
  LOGIN_STATUS=0
  LOGIN_OUTPUT=$(CODEX_HOME="$CODEX_HOME" "$CODEX_BIN" login status 2>&1) || LOGIN_STATUS=$?
else
  LOGIN_STATUS=0
  LOGIN_OUTPUT=$("$CODEX_BIN" login status 2>&1) || LOGIN_STATUS=$?
fi
LOGIN_OUTPUT_LOWER=$(printf '%s' "$LOGIN_OUTPUT" | tr '[:upper:]' '[:lower:]')
if [[ "$LOGIN_STATUS" -ne 0 || "$LOGIN_OUTPUT_LOWER" != *"logged in"* ]]; then
  echo "preflight: codex login status did not report an authenticated session" >&2
  exit 1
fi

# --- 4. every repo_allowlist entry is a real git checkout -------------------
if [[ ${#REPOS[@]} -gt 0 ]]; then
  for repo in "${REPOS[@]}"; do
    if ! git -C "$repo" rev-parse --git-dir >/dev/null 2>&1; then
      echo "preflight: repo_allowlist entry is not a git checkout: $repo" >&2
      exit 1
    fi
  done
fi

# --- 5. state_dir exists (create if absent) and is writable ----------------
if [[ -z "$STATE_DIR" ]]; then
  echo "preflight: config has no state_dir set" >&2
  exit 1
fi
if [[ ! -d "$STATE_DIR" ]]; then
  if ! mkdir -p "$STATE_DIR" 2>/dev/null; then
    echo "preflight: state_dir does not exist and could not be created: $STATE_DIR" >&2
    exit 1
  fi
fi
if [[ ! -w "$STATE_DIR" ]]; then
  echo "preflight: state_dir is not writable: $STATE_DIR" >&2
  exit 1
fi

# --- 6. non-loopback host requires auth_token -------------------------------
# The bridge's config loader applies CODEX_BRIDGE_AUTH_TOKEN as an env
# override on top of the file (adapters/codex config.py precedence), and the
# unit's EnvironmentFile delivers exactly that variable to every Exec* step —
# so a token present in the environment satisfies this check the same way a
# config-file token would. The committed config template deliberately carries
# no auth_token key; the env var is the expected path.
case "$HOST" in
  127.*|localhost|::1|"") ;; # loopback (or unset, which defaults to loopback) — no token required
  *)
    if [[ -z "$AUTH_TOKEN" && -z "${CODEX_BRIDGE_AUTH_TOKEN:-}" ]]; then
      echo "preflight: host is non-loopback ($HOST) but auth_token is not set (config or CODEX_BRIDGE_AUTH_TOKEN)" >&2
      exit 1
    fi
    ;;
esac

# --- 7. the host can create an unprivileged user namespace — WARNING ONLY,
# does not fail the deploy (issue #63, downgraded by issue #243) ----------
# The only check here about the MACHINE rather than the config file. It
# used to be the safety line: codex sandboxed every shell command it ran
# inside a user namespace, so a host that could not create one got an actor
# that registered, accepted dispatched work, and then failed each command
# it tried — after the turn was spent, surfacing as a bridge or runner
# problem that was neither. As of issue #243 the bridges run as a dedicated
# Unix account instead of relying on that sandbox — check 8 below is the
# safety line now — so a restricted userns is no longer a reason to refuse
# the deploy; it stays worth printing because it still means codex's own
# sandboxing (if the operator relies on it for anything else) will not
# work.
#
# Ubuntu 24.04 ships kernel.apparmor_restrict_unprivileged_userns=1, which
# is exactly that state, so every fresh host in this fleet starts in this
# condition until it is provisioned otherwise. See deploy/prod/README.md,
# "Unprivileged user namespaces".
#
# Probed by capability, never by reading the sysctl back: the sysctl is one
# of several things that can block a namespace (a seccomp filter, a
# container's own restrictions, a missing bwrap), and its value says what
# was configured rather than what works. bwrap is preferred because it is
# the exact mechanism codex uses; `unshare` is the fallback that probes the
# same kernel capability one layer down.
USERNS_PROBE=""
if command -v bwrap >/dev/null 2>&1; then
  USERNS_PROBE="bwrap"
elif command -v unshare >/dev/null 2>&1; then
  USERNS_PROBE="unshare"
fi
case "$USERNS_PROBE" in
  bwrap)
    if ! bwrap --unshare-user --unshare-net --ro-bind / / /bin/true >/dev/null 2>&1; then
      echo "preflight: bwrap cannot create a user namespace — codex would register, accept work, then fail every shell command it runs (issue #63)" >&2
      echo "preflight: on Ubuntu 24.04 this is kernel.apparmor_restrict_unprivileged_userns=1; see deploy/prod/README.md 'Unprivileged user namespaces'" >&2
    fi
    ;;
  unshare)
    if ! unshare --user --map-root-user true >/dev/null 2>&1; then
      echo "preflight: this host cannot create a user namespace — codex would register, accept work, then fail every shell command it runs (issue #63)" >&2
      echo "preflight: on Ubuntu 24.04 this is kernel.apparmor_restrict_unprivileged_userns=1; see deploy/prod/README.md 'Unprivileged user namespaces'" >&2
    fi
    ;;
  *)
    # Neither probe tool is installed, so the capability was not measured.
    # Saying so is the honest outcome: passing silently would report a
    # readiness this script never established, and failing would invent a
    # provisioning policy for a host shape this fleet does not have.
    echo "preflight: note — neither bwrap nor unshare is installed, so user-namespace creation was NOT probed (issue #63)" >&2
    ;;
esac

# --- 8. the process runs as a dedicated, unprivileged account (issue #243) -
# This is the safety line now, replacing the bwrap sandbox check 7 used to
# be: the bridges run as a dedicated Unix account (`culture-codex` on
# thor/orin) rather than depending on codex's own sandboxing, so this check
# refuses a deploy under an account that would defeat the isolation that
# account is supposed to provide.
#
# Two conditions, both about the running account rather than the config
# file, each its own message:
#   - group membership: "sudo" or "docker" in `id -nG` is root-equivalent
#     (sudo directly; docker via bind-mounting the host root into a
#     container), so either grant defeats the whole point of a dedicated
#     account.
#   - checkout ownership: every repo_allowlist entry must be owned
#     (`stat -c %u`) by the running uid (`id -u`) — a checkout owned by a
#     different account is either unreadable/unwritable in practice, or
#     (worse) shared with a principal this process should not be able to
#     act as.
RUNNING_GROUPS=$(id -nG)
for restricted_group in sudo docker; do
  for have_group in $RUNNING_GROUPS; do
    if [[ "$have_group" == "$restricted_group" ]]; then
      echo "preflight: running user is a member of group '$restricted_group' — a codex bridge account must be dedicated and unprivileged, not one with $restricted_group access (issue #243)" >&2
      exit 1
    fi
  done
done

RUNNING_UID=$(id -u)
if [[ ${#REPOS[@]} -gt 0 ]]; then
  for repo in "${REPOS[@]}"; do
    if ! REPO_UID=$(stat -c %u "$repo" 2>/dev/null); then
      echo "preflight: cannot stat repo_allowlist entry to check ownership: $repo" >&2
      exit 1
    fi
    if [[ "$REPO_UID" != "$RUNNING_UID" ]]; then
      echo "preflight: repo_allowlist entry is not owned by the running user (uid $RUNNING_UID): $repo (owned by uid $REPO_UID) (issue #243)" >&2
      exit 1
    fi
  done
fi

printf 'preflight: ok %s\n' "$VERSION_LINE"
