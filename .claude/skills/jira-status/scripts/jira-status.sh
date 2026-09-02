#!/usr/bin/env bash
# jira-status — operator-lane Jira status reads and moves.
#
# The credential pair lives on THOR (~/.culture-nodes/runner-secrets.env,
# the sweep's granted read/write pair); this script sshes there and runs the
# REST calls on that host, so neither the email nor the token ever reaches
# this machine, an argv, or a log line. See SKILL.md for custody notes.
set -euo pipefail

HOST=${JIRA_STATUS_HOST:-thor}
SITE=${JIRA_STATUS_SITE:-agentculture.atlassian.net}

verb=${1:?usage: jira-status.sh <status|move> <ISSUE> [<target status>]}
issue=${2:?usage: jira-status.sh <status|move> <ISSUE> [<target status>]}
case "$verb" in
  status) target="" ;;
  move)   target=${3:?usage: jira-status.sh move <ISSUE> "<target status>"} ;;
  *) echo "jira-status: unknown verb '$verb' (status|move)" >&2; exit 1 ;;
esac

# Arguments ride argv into the REMOTE python (never into a shell string);
# the heredoc is quoted so nothing local interpolates into it. ssh joins its
# arguments with spaces for the remote shell, which would eat an empty
# target and split "To Do" — printf %q quotes each argument for that trip.
exec ssh "$HOST" "python3 - $(printf '%q %q %q %q' "$SITE" "$verb" "$issue" "$target")" <<'PYEOF'
import base64, json, os, re, sys, urllib.request

site, verb, issue, target = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
if not re.fullmatch(r"[A-Z][A-Z0-9_]*-[1-9][0-9]*", issue):
    sys.exit(f"jira-status: '{issue}' is not a Jira issue key (like SCRUM-2)")

sec = open(os.path.expanduser("~/.culture-nodes/runner-secrets.env")).read()
email = re.search(r"^JIRA_ACCOUNT_EMAIL=(.*)$", sec, re.M)
token = re.search(r"^JIRA_API_TOKEN=(.*)$", sec, re.M)
if not email or not token or not email.group(1) or not token.group(1):
    sys.exit("jira-status: the Jira credential pair is not granted on this host")
auth = base64.b64encode(f"{email.group(1)}:{token.group(1)}".encode()).decode()

# The REST base, granted beside the pair: a SCOPED service-account token is
# accepted only at the Atlassian gateway, and the site URL answers 401 for it.
# Empty (or absent) means the site URL, which is what an unscoped token wants.
base = re.search(r"^JIRA_API_BASE=(.*)$", sec, re.M)
api_base = (base.group(1).strip().strip('"').strip("'").rstrip("/") if base else "")
if api_base and not api_base.startswith("https://"):
    sys.exit(f"jira-status: JIRA_API_BASE on this host is not an https URL: {api_base!r}")
root = api_base or f"https://{site}"

def call(path, payload=None):
    req = urllib.request.Request(
        f"{root}/rest/api/3/{path}",
        data=None if payload is None else json.dumps(payload).encode(),
        headers={
            "Authorization": "Basic " + auth,
            "Accept": "application/json",
            **({"Content-Type": "application/json"} if payload is not None else {}),
        },
        method="GET" if payload is None else "POST",
    )
    with urllib.request.urlopen(req, timeout=20) as r:
        body = r.read()
        return json.loads(body) if body.strip() else {}

cur = call(f"issue/{issue}?fields=status,summary")
status = (cur.get("fields", {}).get("status") or {}).get("name", "?")
summary = cur.get("fields", {}).get("summary", "")
transitions = call(f"issue/{issue}/transitions").get("transitions", [])
names = [(t.get("to") or {}).get("name", t.get("name", "?")) for t in transitions]

if verb == "status":
    print(f"{issue}: {status}  ({summary})")
    print("transitions available:", ", ".join(sorted(set(names))) or "none")
    sys.exit(0)

match = [t for t in transitions
         if ((t.get("to") or {}).get("name", "") or t.get("name", "")).casefold() == target.casefold()]
if not match:
    sys.exit(f"jira-status: no transition from '{status}' to '{target}' on {issue}; "
             f"available: {', '.join(sorted(set(names))) or 'none'}")
call(f"issue/{issue}/transitions", {"transition": {"id": match[0]["id"]}})
after = call(f"issue/{issue}?fields=status")
print(f"{issue}: {status} -> " + ((after.get("fields", {}).get("status") or {}).get("name", "?")))
PYEOF
