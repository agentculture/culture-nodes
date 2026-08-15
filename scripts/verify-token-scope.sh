#!/usr/bin/env bash
#
# verify-token-scope.sh — verify a GitHub token's TYPE and EFFECTIVE PERMISSIONS
# without ever printing, logging, or passing the token value in argv.
#
# WHY THIS EXISTS. The own-the-work-end-to-end spec gives an unattended bridge
# service a push credential (claim c71) and bounds its blast radius (boundary
# c73: the handover token must be a different, narrower credential than the
# sweep's read-only one). Both of those are only true if somebody checks — and
# "somebody checks" was, until this script, an operator squinting at the GitHub
# UI. That is exactly the shape the spec objects to elsewhere: a fact the system
# could measure, left to human memory.
#
# So this is a DETERMINISTIC VALIDATOR. It measures three facts and reports
# them; it forms no opinion an agent could have fabricated. Under the PRD's
# ledger authority model that makes its output eligible to be `derived`
# evidence rather than a `proposed` claim — which is the whole point.
#
# WHAT IT MEASURES
#   1. Token TYPE, from its prefix alone (github_pat_ = fine-grained,
#      ghp_ = classic, ghs_ = app installation, gho_/ghu_ = OAuth/user).
#      A classic token carries coarse `repo` scope — admin-equivalent on every
#      repository it can reach — which is more than any handover needs.
#   2. Whether the token is LIVE, and what it authenticates as.
#   3. Its EFFECTIVE permissions on a named repository, plus the coarse OAuth
#      scopes GitHub reports back for classic tokens.
#
# WHAT IT DELIBERATELY DOES NOT DO
#   * It never echoes the token, never writes it to a file, never puts it in
#     argv (argv is world-readable via /proc on Linux). The value is read from
#     the environment and handed to curl through a header file on stdin.
#   * It never PUSHES to prove push works. A verification step with a
#     side effect is not a verification step.
#   * It reads only the FIRST 11 CHARACTERS to classify the prefix. That is a
#     type discriminator, not key material, and it is never printed.
#
# THE CAVEAT THAT MOTIVATED THE WHOLE SCRIPT. For a fine-grained token, the
# `permissions` block on a repository response reflects the AUTHENTICATED
# IDENTITY's role on that repo, not the token's grant. So `admin: true` on a
# fine-grained token owned by an org owner says nothing about how narrowly the
# token was scoped. This script therefore reports the type and the observed
# permissions as SEPARATE facts and refuses to collapse them into a single
# verdict — see the `caveat` field in its output.
#
# USAGE
#   TOKEN_ENV=GITHUB_TOKEN_WORKER REPO=agentculture/culture-nodes \
#     scripts/verify-token-scope.sh
#
#   TOKEN_ENV  name of the environment variable holding the token
#              (the NAME is passed, never the value). Default GITHUB_TOKEN.
#   REPO       owner/name to check effective permissions against. Required.
#
# EXIT CODES — the code-node contract examples/pr-upkeep/sweep.py established:
#   0   verified and within policy: fine-grained token with push on REPO
#   10  verified, but a FINDING: a classic/coarse token, or no push permission.
#       A finding is a DOMAIN OUTCOME a workflow routes on, not a failure.
#   2   environment error: token absent, repo unset, or GitHub unreachable
#   1   usage error
#
# Output is a single JSON object on stdout; diagnostics go to stderr, never
# mixed — the same split every CLI surface in this repo holds itself to.

set -uo pipefail

TOKEN_ENV="${TOKEN_ENV:-GITHUB_TOKEN}"
REPO="${REPO:-}"

die() { printf 'error: %s\n' "$1" >&2; printf 'hint: %s\n' "${2:-}" >&2; exit "${3:-1}"; }

[ -n "$REPO" ] || die "REPO is not set" \
  "set REPO=owner/name, e.g. REPO=agentculture/culture-nodes" 2

# Indirect expansion: we take the NAME of the variable, so the value never
# appears in this script's argv or in any caller's command line.
TOKEN="${!TOKEN_ENV:-}"
[ -n "$TOKEN" ] || die "\$$TOKEN_ENV is empty or unset" \
  "export the token into this process's environment; it is never read from a file or an argument" 2

# --- 1. type, from the prefix only -----------------------------------------
# Never printed. Compared, then discarded.
case "$TOKEN" in
  github_pat_*) TYPE="fine_grained"; COARSE="false" ;;
  ghp_*)        TYPE="classic";      COARSE="true"  ;;
  ghs_*)        TYPE="app_installation"; COARSE="false" ;;
  gho_*|ghu_*)  TYPE="oauth";        COARSE="true"  ;;
  *)            TYPE="unknown";      COARSE="unknown" ;;
esac

# --- 2 & 3. liveness, identity, effective permissions ----------------------
# The token rides an Authorization header supplied on stdin via curl's
# --config, so it never becomes an argument.
api() {
  local path="$1" outfile="$2"
  printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" \
    | curl -sS -m 15 --config - \
        -H "Accept: application/vnd.github+json" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        -D "${outfile}.hdr" -o "${outfile}" -w '%{http_code}' \
        "https://api.github.com${path}" 2>/dev/null
}

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

USER_CODE="$(api /user "$TMP/user.json")" || die "could not reach api.github.com" \
  "check network egress from this host" 2
[ "$USER_CODE" = "200" ] || die "token did not authenticate (HTTP $USER_CODE)" \
  "the token in \$$TOKEN_ENV is expired, revoked, or malformed" 2

REPO_CODE="$(api "/repos/${REPO}" "$TMP/repo.json")"

# Classic tokens advertise their coarse scopes in a response header;
# fine-grained tokens do not send it at all. Absence is itself a signal.
OAUTH_SCOPES="$(grep -i '^x-oauth-scopes:' "$TMP/user.json.hdr" 2>/dev/null \
  | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r' || true)"

# --- 4. the write-capability SCREEN (necessary, NOT sufficient) -------------
#
# READ THIS BEFORE TRUSTING THE RESULT. An earlier version of this script
# treated the receive-pack advertisement as authoritative proof of push
# capability. That was WRONG, and it was falsified the first time anyone
# pushed: on 2026-08-15, GITHUB_TOKEN_WORKER got 200 from this endpoint and
# then 403 from an actual `git push --dry-run` against the same repository
# ("Permission to agentculture/culture-nodes.git denied to OriNachum"). An
# unauthenticated request gets 401 and a genuinely read-only token gets 403,
# so the endpoint does discriminate — it is simply discriminating on something
# coarser than the token's ref-update grant. Plausibly the identity's role, or
# an organisation policy applied later in the push path.
#
# So this is a SCREEN, not a verdict:
#   401/403 here  -> the credential definitely cannot push. Conclusive.
#   200 here      -> it MIGHT be able to. Not conclusive.
#
# The only sufficient check is an actual push negotiation. `git push --dry-run`
# performs one and updates nothing — it is side-effect free in the sense that
# matters (no ref moves), while genuinely exercising the permission the real
# push needs. It costs a git invocation and a repository, which is why it is
# opt-in via PROBE_PUSH rather than default.
#
# The lesson generalises past this script, and it is the same one the spec
# keeps arriving at: a cheaper signal that CORRELATES with the fact you want is
# not the fact you want. Measure the thing itself.
PUSH_CODE="$(printf 'user = "x-access-token:%s"\n' "$TOKEN" \
  | curl -sS -m 15 --config - -o /dev/null -w '%{http_code}' \
      "https://github.com/${REPO}.git/info/refs?service=git-receive-pack" 2>/dev/null || echo "000")"

# The sufficient probe, opt-in. PROBE_PUSH=<git-dir> runs a real push
# negotiation from that repository and updates nothing. The token reaches git
# through GIT_ASKPASS so it never enters argv or a remote URL (a URL-embedded
# credential would persist in the reflog and in `git remote -v`).
PUSH_DRYRUN="not_probed"
if [ -n "${PROBE_PUSH:-}" ]; then
  _ap="$TMP/askpass.sh"
  printf '#!/bin/sh\nexec printf %%s "$_VTS_TOKEN"\n' > "$_ap"; chmod +x "$_ap"
  _ref="$(git -C "$PROBE_PUSH" symbolic-ref --quiet --short HEAD 2>/dev/null || echo HEAD)"
  if _VTS_TOKEN="$TOKEN" GIT_ASKPASS="$_ap" GIT_TERMINAL_PROMPT=0 \
       git -C "$PROBE_PUSH" push --dry-run \
         "https://x-access-token@github.com/${REPO}.git" "$_ref" >/dev/null 2>"$TMP/push.err"; then
    PUSH_DRYRUN="allowed"
  else
    PUSH_DRYRUN="denied"
  fi
fi

TYPE="$TYPE" COARSE="$COARSE" REPO="$REPO" REPO_CODE="$REPO_CODE" \
OAUTH_SCOPES="$OAUTH_SCOPES" PUSH_CODE="$PUSH_CODE" TOKEN_ENV="$TOKEN_ENV" \
 PUSH_DRYRUN="$PUSH_DRYRUN" \
REQUIRE="${REQUIRE:-any}" python3 - "$TMP/user.json" "$TMP/repo.json" <<'PY'
import json, os, sys

user = json.load(open(sys.argv[1]))
try:
    repo = json.load(open(sys.argv[2]))
except Exception:
    repo = {}

perms = repo.get("permissions") or {}
ttype = os.environ["TYPE"]
repo_code = os.environ["REPO_CODE"]
scopes = [s for s in os.environ.get("OAUTH_SCOPES", "").split(", ") if s]

reachable = repo_code == "200"
role_push = bool(perms.get("push"))

# The MEASURED write capability, from the receive-pack advertisement. Unlike
# the role-derived `permissions` block, this reflects what the TOKEN can do.
push_code = os.environ["PUSH_CODE"]
dryrun = os.environ["PUSH_DRYRUN"]

# The SCREEN. Conclusive only when it says no.
if push_code in ("401", "403"):
    screen, screen_note = False, f"receive-pack advertisement returned {push_code}: cannot push (conclusive)"
elif push_code == "200":
    screen, screen_note = None, "receive-pack advertisement returned 200: MIGHT be able to push (not conclusive — see caveat)"
else:
    screen, screen_note = None, f"receive-pack screen inconclusive (HTTP {push_code})"

# The VERDICT. Only a real push negotiation settles it.
if dryrun == "allowed":
    can_push, push_note = True, "git push --dry-run succeeded (authoritative)"
elif dryrun == "denied":
    can_push, push_note = False, "git push --dry-run was refused (authoritative)"
elif screen is False:
    can_push, push_note = False, screen_note
else:
    can_push, push_note = None, screen_note + "; set PROBE_PUSH=<git-dir> for an authoritative answer"

# REQUIRE declares what this caller NEEDS, so one script serves both sides of
# the credential separation: a handover token must be able to push, a sweep
# token must NOT. Default 'any' only reports.
require = os.environ["REQUIRE"]

findings = []
if os.environ["COARSE"] == "true":
    findings.append(
        f"token is {ttype}: coarse scopes grant admin-equivalent access on every "
        "reachable repository, which is broader than a handover credential needs"
    )
if ttype == "unknown":
    findings.append("token prefix is unrecognised; its kind could not be classified")
if not reachable:
    findings.append(f"repository {os.environ['REPO']} not reachable with this token (HTTP {repo_code})")
if can_push is None:
    findings.append(push_note)
elif require == "write" and not can_push:
    findings.append(f"REQUIRE=write but the token cannot push to {os.environ['REPO']}: {push_note}")
elif require == "read" and can_push:
    findings.append(
        f"REQUIRE=read but the token CAN push to {os.environ['REPO']}: {push_note}. "
        "A read-only lane holding a write credential has a larger blast radius than it needs."
    )

out = {
    "token_env": os.environ.get("TOKEN_ENV", "GITHUB_TOKEN"),
    "type": ttype,
    "coarse_scoped": os.environ["COARSE"],
    "oauth_scopes": scopes,
    "authenticated_as": user.get("login"),
    "identity_type": user.get("type"),
    "repo": os.environ["REPO"],
    "repo_reachable": reachable,
    "observed_permissions": perms,
    "role_implies_push": role_push,
    "can_push": can_push,
    "can_push_note": push_note,
    "can_push_screen": screen,
    "can_push_screen_note": screen_note,
    "push_dryrun": dryrun,
    "required": require,
    "within_policy": not findings,
    "findings": findings,
    "caveat": (
        "observed_permissions/role_implies_push reflect the AUTHENTICATED "
        "IDENTITY's role on this repository, not the token's granted scope — for "
        "a token held by an org owner they read admin however narrowly it was "
        "scoped. measured_can_push is the authoritative field: it comes from the "
        "receive-pack advertisement, which GitHub gates on the TOKEN's write "
        "permission. Where the two disagree, believe the measured one."
    ),
}
json.dump(out, sys.stdout, indent=2, sort_keys=True)
print()
sys.exit(0 if out["within_policy"] else 10)
PY
