#!/usr/bin/env bash
# Issue ONE bridge's dial-in credential and deliver it, in one command
# (plan task t13; issue #111's dial-in half, decided for issue #136).
#
#   issue-dialin-credential.sh <actor-key> [bridge-ssh-host]
#   issue-dialin-credential.sh --revoke <actor-key> [bridge-ssh-host]
#
# WHY THIS IS ITS OWN SCRIPT AND NOT A LANE IN install-secrets.sh.
#
# install-secrets.sh installs the CONTROL PLANE's environment file, and
# carries two open hazards this credential must not inherit:
#
#   #133  a FORCE_PROD rotation can update one copy of a value and leave
#         another stale, silently. For a dial-in credential that failure is
#         worse than for a database password: a bridge whose token no longer
#         matches the stored digest simply stops being dispatchable, and
#         looks exactly like a bridge that is idle.
#   #134  probes of install-secrets.sh run from an operator environment relay
#         whatever credentials that environment holds into the files they
#         write. That lane relays six values from its own environment; this
#         one relays none, by construction (see "no relay" below).
#
# THE STRUCTURE THAT MAKES A STALE COPY IMPOSSIBLE RATHER THAN UNLIKELY.
#
# A dial-in credential is the first bridge credential in this deployment with
# exactly ONE plaintext custody point. Every other one has two: the bridge
# holds the token and the control plane holds the same token to PRESENT when
# it dispatches outbound (NODES_ACTOR_*_TOKEN in prod.env, mirrored from
# codex-bridge.env / notify.env / a bridge JSON config). Two copies of one
# value is the shape #133 is about, and install-secrets.sh has a whole lane
# per credential devoted to keeping each pair in step.
#
# Dial-in inverts the direction, so the control plane never presents the
# value -- it only VERIFIES one. It therefore needs no plaintext at all, and
# keeps a SHA-256 verifier (migration 0031, 0037's issued_at/issuance_count).
# That leaves:
#
#   one plaintext   the bridge's own per-bridge file, written only here
#   one digest      inbound_authentication.verifier_sha256, written only by
#                   POST /v1alpha1/inbound/credentials
#
# and no lane anywhere that can write either one alone. Minting REPLACES the
# verifier and reveals the plaintext exactly once, in the same request; this
# script has no mode that mints without delivering, and no mode that writes a
# credential value an operator supplies (there is no such value: the control
# plane chooses it with crypto/rand and nothing can read it back). So a
# rotation is one command that replaces both copies or neither.
#
# The one thing two machines cannot do is commit atomically. What is
# guaranteed instead is that a half-completed rotation is IMPOSSIBLE TO MISS:
#
#   - the delivery is prepare-then-replace, so the bridge's previous
#     credential survives byte-intact if the write fails;
#   - the deliverer recomputes the SHA-256 of what it received and refuses to
#     write unless it equals the digest the control plane stored, so a value
#     damaged in transit never reaches a bridge;
#   - a delivery that fails after a successful mint exits non-zero and names
#     the party whose copies now disagree, which way round, and the repair
#     (re-run: re-issuing replaces the verifier again);
#   - deploy/prod/audit-credentials.sh fails by name if a dial-in credential
#     ever appears in prod.env, because for a single-copy credential the only
#     inconsistency prod.env can express is holding one at all.
#
# NO RELAY, NO ARGV, NO LOCAL COPY (#134, and the repo's argv discipline).
#
# The issuance bearer is read from the control plane host's OWN prod.env by a
# command running on that host -- never exported by the operator, never
# passed to ssh as an argument, never returned. The credential itself travels
# from `curl` on the control plane host directly into the delivering command
# on the bridge host, through a shell pipeline: it is never assigned to a
# variable in this script, never written to a local file, and never printed.
# The only things this script's own process handles are an actor key, a host
# name, a URL and a digest.
#
# Exit codes follow the repo's policy: 0 ok, 1 a user error (an actor this
# deployment runs no bridge for, a malformed key), 2 the issuance or the
# delivery could not be completed.
set -euo pipefail

# The control plane's HTTP surface, as reached FROM the control plane host --
# that is where the mint request is made, so that it can read its own bearer
# without the secret crossing a wire.
NODES_API_URL=${NODES_API_URL:-http://thor:18080}
# The ssh target that runs the control plane (and therefore holds the
# issuance bearer in ~/.culture-nodes/prod.env).
NODES_CONTROL_HOST=${NODES_CONTROL_HOST:-thor}
# The URL the BRIDGE will dial. Usually the same one; separable because the
# bridge may reach the control plane by a different name than the control
# plane host reaches itself.
DIALIN_CONTROL_PLANE_URL=${DIALIN_CONTROL_PLANE_URL:-$NODES_API_URL}

usage() {
  echo "usage: issue-dialin-credential.sh [--revoke] <actor-key> [bridge-ssh-host]" >&2
  echo "hint: <actor-key> is a registered actor (company/codex-thor, company/developer, ...); one bridge, one credential" >&2
}

REVOKE=0
case "${1:-}" in
  --revoke) REVOKE=1; shift ;;
  --help|-h) usage; exit 1 ;;
  -*) echo "error: unknown option: $1" >&2; usage; exit 1 ;;
esac

ACTOR_KEY=${1:-}
if [ -z "$ACTOR_KEY" ]; then
  echo "error: no actor key given" >&2
  usage
  exit 1
fi

# The key is interpolated into a curl config file on the far side, so it is
# validated here rather than escaped there. The pattern is the one migration
# 0031 and internal/actors.ValidateInboundParty already enforce; anything
# else is refused before a byte leaves this machine.
case "$ACTOR_KEY" in
  */*) : ;;
  *) echo "error: $ACTOR_KEY is not an actor key (expected namespace/name)" >&2
     usage
     exit 1 ;;
esac
case "$ACTOR_KEY" in
  *[!A-Za-z0-9._/-]*)
    echo "error: $ACTOR_KEY carries a character an actor key may not contain" >&2
    echo "hint: actor keys are namespace/name over [A-Za-z0-9._-]" >&2
    exit 1 ;;
esac

# --- which bridge holds which credential ----------------------------------
#
# One row per PARTY, not per backend: issuance is per party, and spark runs
# four claude-code bridges that share one prefix and one systemd
# EnvironmentFile (issue #147). A per-backend destination would give all four
# the same identity, which is exactly the gap #147 records.
#
# Columns: actor key, the env prefix that bridge's dialin.py reads, the ssh
# host it runs on, and the destination.
#
# A destination is `<kind>:<path relative to the bridge user's home>`:
#
#   env:   a single-purpose mode-0600 EnvironmentFile carrying the three
#          settings dialin.configured() requires together. This is what the
#          SHIPPED bridge code reads today (os.environ only), and it is
#          per-bridge, which the shared bridge-push.env is not.
#   json:  the per-bridge JSON config, where every other per-bridge setting
#          already lives. Issue #147 leans this way and task t8 decides it;
#          this lane can already write it, so that decision does not need
#          this script rewritten -- pass DIALIN_DESTINATION or edit the row.
#
# The ssh host is where the OPERATOR reaches the bridge from. It is not an
# address the control plane stores, learns or depends on (#136): the control
# plane still never knows where any bridge is, because the bridge dials it.
dialin_bridges() {
  cat <<'EOF'
company/codex-thor	CODEX_BRIDGE	thor	env:.culture-nodes/dialin/codex-thor.env
company/codex-orin	CODEX_BRIDGE	orin	env:.culture-nodes/dialin/codex-orin.env
company/developer	CLAUDE_CODE_BRIDGE	spark	env:.culture-nodes/dialin/developer.env
company/planner	CLAUDE_CODE_BRIDGE	spark	env:.culture-nodes/dialin/planner.env
company/verifier	CLAUDE_CODE_BRIDGE	spark	env:.culture-nodes/dialin/verifier.env
company/intake	CLAUDE_CODE_BRIDGE	spark	env:.culture-nodes/dialin/intake.env
company/human-ops	HUMAN_INBOX_BRIDGE	spark	env:.culture-nodes/dialin/human-ops.env
company/notify-discord	NOTIFY_BRIDGE	thor	env:.culture-nodes/dialin/notify-discord.env
EOF
}

ROW=$(dialin_bridges | awk -F'\t' -v key="$ACTOR_KEY" '$1 == key { print; exit }')

PREFIX=${DIALIN_PREFIX:-$(printf '%s' "$ROW" | cut -f2)}
HOST=${DIALIN_HOST:-$(printf '%s' "$ROW" | cut -f3)}
DESTINATION=${DIALIN_DESTINATION:-$(printf '%s' "$ROW" | cut -f4)}
# A second positional argument overrides the host, the way install-secrets.sh
# and deploy.sh take their hosts on argv.
if [ -n "${2:-}" ]; then HOST=$2; fi

if [ -z "$PREFIX" ] || [ -z "$HOST" ] || [ -z "$DESTINATION" ]; then
  echo "error: $ACTOR_KEY is not a bridge this deployment issues dial-in credentials for" >&2
  echo "hint: add a row to dialin_bridges() in $(basename "$0"), or set DIALIN_PREFIX, DIALIN_HOST and DIALIN_DESTINATION for a one-off. Known parties: $(dialin_bridges | cut -f1 | tr '\n' ' ')" >&2
  exit 1
fi

DEST_KIND=${DESTINATION%%:*}
DEST_REL=${DESTINATION#*:}
case "$DEST_KIND" in
  env|json) : ;;
  *) echo "error: destination kind $DEST_KIND is neither env nor json" >&2
     echo "hint: a destination is env:<path> (a per-bridge EnvironmentFile) or json:<path> (the per-bridge JSON config), relative to the bridge user's home" >&2
     exit 1 ;;
esac

# --- the two remote halves -------------------------------------------------

# MINT_REMOTE runs on the CONTROL PLANE host. It reads the issuance bearer
# out of the prod.env already on that host and POSTs the issuance request,
# emitting the response on stdout. The bearer never enters an argv: curl
# takes its whole configuration -- url, method, headers, body -- on stdin
# (`curl -K -`), so nothing in /proc on that host shows it.
#
# Single-quoted below: every expansion in it is for the remote shell.
# shellcheck disable=SC2016
MINT_REMOTE='set -eu
f=$HOME/.culture-nodes/prod.env
if [ ! -r "$f" ]; then
  echo "REFUSED: no readable ~/.culture-nodes/prod.env on the control plane host, so the dial-in issuance bearer cannot be read" >&2
  exit 2
fi
sec=
while IFS= read -r cur || [ -n "$cur" ]; do
  case "$cur" in "NODES_INBOUND_ISSUANCE_TOKEN_SECRET="?*) sec=${cur#*=} ;; esac
done < "$f"
if [ -z "$sec" ]; then
  echo "REFUSED: prod.env on the control plane host carries no NODES_INBOUND_ISSUANCE_TOKEN_SECRET, so POST /v1alpha1/inbound/credentials answers 401. Run deploy/prod/install-secrets.sh to install it, then re-run." >&2
  exit 2
fi
cat <<CURLRC | curl -fsS -K -
url = "$API_URL/v1alpha1/inbound/credentials$ROUTE_SUFFIX"
request = "POST"
header = "Authorization: Bearer $sec"
header = "Content-Type: application/json"
data = "{\"party_kind\":\"actor\",\"party_key\":\"$PARTY\"}"
CURLRC
'

# DELIVER_REMOTE runs on the BRIDGE host with the issuance response on its
# stdin. It verifies the pair before it commits anything, then prepares a
# mode-0600 temp file and replaces the destination with it, so the previous
# credential survives a failed write byte-intact.
#
# The python program is built by a here-document into a variable and passed
# with `-c`, because stdin is already carrying the response. It contains no
# secret, so an argv is the right place for it -- the same shape
# actor-placement.sh uses.
#
# shellcheck disable=SC2016
DELIVER_REMOTE='set -eu
umask 077
prog=$(cat <<"DELIVERPY"
import hashlib, json, os, sys

expected = os.environ["PARTY"]


def refuse(message):
    sys.stderr.write("REFUSED on the bridge host: " + message + "; nothing was written\n")
    raise SystemExit(2)


raw = sys.stdin.read()
if not raw.strip():
    refuse("the control plane returned no issuance response for " + expected)
try:
    payload = json.loads(raw)
except ValueError:
    refuse("the issuance response for " + expected + " was not JSON")
if not isinstance(payload, dict):
    refuse("the issuance response for " + expected + " was not an object")

credential = payload.get("credential") or ""
digest = payload.get("digest_sha256") or ""
party = payload.get("party_key") or ""

if party != expected:
    refuse("the control plane issued for " + repr(party) + ", not " + expected)
if not credential or not digest:
    refuse("the issuance response for " + expected + " carried no credential")
# The end-to-end check: what the bridge is about to hold must be the value
# whose digest the control plane just stored. A response that lost or altered
# it in transit cannot install a credential the control plane will not admit.
if hashlib.sha256(credential.encode("utf-8")).hexdigest() != digest:
    refuse("the delivered credential for " + expected + " does not match the digest_sha256 the control plane stored")

dest = os.path.join(os.path.expanduser("~"), os.environ["DEST_REL"])
prefix = os.environ["PREFIX"]
url = os.environ["CP_URL"]
try:
    parent = os.path.dirname(dest)
    if parent:
        os.makedirs(parent, mode=0o700, exist_ok=True)
    if os.environ["DEST_KIND"] == "json":
        try:
            with open(dest) as handle:
                config = json.load(handle)
        except (IOError, OSError, ValueError):
            config = {}
        if not isinstance(config, dict):
            config = {}
        config["dial_control_plane_url"] = url
        config["dial_actor_key"] = expected
        config["dial_token"] = credential
        body = json.dumps(config, indent=2, sort_keys=True) + "\n"
    else:
        body = "".join([
            "# Written by deploy/prod/issue-dialin-credential.sh. Do not edit.\n",
            "# One bridge, one credential, one custody point: this file.\n",
            "# party: " + expected + "\n",
            "# digest_sha256: " + digest + "\n",
            "# issued_at: " + str(payload.get("issued_at") or "") + "\n",
            prefix + "_CONTROL_PLANE_URL=" + url + "\n",
            prefix + "_ACTOR_KEY=" + expected + "\n",
            prefix + "_DIAL_TOKEN=" + credential + "\n",
        ])
    tmp = dest + ".new"
    handle = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(handle, "w") as out:
        out.write(body)
        out.flush()
        os.fsync(out.fileno())
    os.chmod(tmp, 0o600)
    os.replace(tmp, dest)
except (IOError, OSError) as exc:
    refuse("writing " + dest + " for " + expected + " failed: " + str(exc))

sys.stdout.write("delivered " + expected + " to " + dest + " digest=" + digest + "\n")
DELIVERPY
)
python3 -c "$prog"
'

# REMOVE_REMOTE runs on the bridge host after a revocation: a revoked
# credential admits nothing, and leaving its plaintext in a file is how a
# dead value later gets mistaken for a working one.
# shellcheck disable=SC2016
REMOVE_REMOTE='set -eu
f=$HOME/$DEST_REL
if [ -e "$f" ]; then
  rm -f "$f"
  echo "removed the revoked credential file $f"
else
  echo "no credential file at $f to remove"
fi
'

# Non-secret values only on either argv: an actor key, a URL, an env prefix,
# a relative path. ssh forwards no environment, so they are prefixed into the
# remote command exactly the way install-secrets.sh prefixes FORCE.
mint_prefix() { # route-suffix
  printf "API_URL='%s'; ROUTE_SUFFIX='%s'; PARTY='%s'; " "$NODES_API_URL" "$1" "$ACTOR_KEY"
}
deliver_prefix() {
  printf "PARTY='%s'; PREFIX='%s'; CP_URL='%s'; DEST_REL='%s'; DEST_KIND='%s'; export PARTY PREFIX CP_URL DEST_REL DEST_KIND; " \
    "$ACTOR_KEY" "$PREFIX" "$DIALIN_CONTROL_PLANE_URL" "$DEST_REL" "$DEST_KIND"
}

if [ "$REVOKE" = "1" ]; then
  # shellcheck disable=SC2029 # the remote body is deliberately remote
  if ! ssh "$NODES_CONTROL_HOST" "$(mint_prefix /revoke)$MINT_REMOTE" > /dev/null; then
    echo "error: revoking $ACTOR_KEY's dial-in credential failed at the control plane" >&2
    echo "hint: the credential is still live. Check $NODES_API_URL from $NODES_CONTROL_HOST and re-run; nothing on the bridge was touched." >&2
    exit 2
  fi
  echo "revoked $ACTOR_KEY at the control plane — its next dial is refused"
  # shellcheck disable=SC2029 # the remote body is deliberately remote
  if ! ssh "$HOST" "DEST_REL='$DEST_REL'; $REMOVE_REMOTE"; then
    echo "error: $ACTOR_KEY is revoked but its dead credential file on $HOST could not be removed" >&2
    echo "hint: the credential admits nothing either way; remove ~/$DEST_REL on $HOST by hand, then re-run to confirm" >&2
    exit 2
  fi
  exit 0
fi

# The credential travels from curl on the control plane host straight into
# the deliverer on the bridge host. It is never a variable here, never a file
# here, never printed here: this process passes two file descriptors and
# reads two exit statuses.
set +e
# shellcheck disable=SC2029 # both remote bodies are deliberately remote
ssh "$NODES_CONTROL_HOST" "$(mint_prefix '')$MINT_REMOTE" | ssh "$HOST" "$(deliver_prefix)$DELIVER_REMOTE"
STATUS=("${PIPESTATUS[@]}")
set -e

MINT_STATUS=${STATUS[0]}
DELIVER_STATUS=${STATUS[1]}

if [ "$MINT_STATUS" -ne 0 ]; then
  echo "error: the control plane did not issue a dial-in credential for $ACTOR_KEY" >&2
  echo "hint: nothing changed on either side — no verifier was replaced and the bridge still holds whatever it held. Check $NODES_API_URL from $NODES_CONTROL_HOST (POST /v1alpha1/inbound/credentials needs NODES_INBOUND_ISSUANCE_TOKEN_SECRET in that host's prod.env), then re-run." >&2
  exit 2
fi

if [ "$DELIVER_STATUS" -ne 0 ]; then
  echo "error: the control plane issued a NEW dial-in credential for $ACTOR_KEY and the bridge on $HOST did not receive it" >&2
  echo "       the two copies now disagree: the control plane holds the new verifier, the bridge holds the previous credential (unchanged — the write prepares then replaces)." >&2
  echo "hint: $ACTOR_KEY cannot dial in until this succeeds, and nothing else will report that. Fix the destination on $HOST and re-run the same command: re-issuing replaces the verifier again, so there is no half-updated state to repair by hand and no value to recover — the plaintext is revealed once and this one is gone." >&2
  exit 2
fi

echo "next: $HOST must read ~/$DEST_REL — for an env destination add"
echo "      EnvironmentFile=-%h/$DEST_REL"
echo "      to that bridge's systemd unit and restart it (issue #147 / task t8 decides where a bridge reads this from)."
