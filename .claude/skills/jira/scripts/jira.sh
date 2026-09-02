#!/usr/bin/env bash
# jira — operator-lane Jira comment / create / show.
#
# Custody is copied verbatim from jira-status: the credential pair lives on
# THOR (~/.culture-nodes/runner-secrets.env, the sweep's granted read/write
# pair). This script sshes there and runs the REST calls on that host, so
# neither the email nor the token ever reaches this machine, an argv, or a
# log line. Everything it posts lands under the SYSTEM's Jira account, which
# the sweep filters as self-echo by account id (see SKILL.md).
set -euo pipefail

HOST=${JIRA_STATUS_HOST:-thor}
SITE=${JIRA_STATUS_SITE:-agentculture.atlassian.net}

usage() {
  cat >&2 <<'EOF'
usage: jira.sh show <ISSUE>
       jira.sh comment <ISSUE> "<text>"
       jira.sh create --project KEY --summary "<text>" [--type Task] [--description "<text>"]
EOF
}

refuse() {
  # error/hint to stderr, never mixed with stdout; exit 1 = user error.
  echo "error: $1" >&2
  echo "hint: $2" >&2
  exit 1
}

# Validated LOCALLY, before any ssh: a malformed key never becomes a request.
valid_issue() { [[ $1 =~ ^[A-Z][A-Z0-9_]*-[1-9][0-9]*$ ]]; }
valid_project() { [[ $1 =~ ^[A-Z][A-Z0-9_]*$ ]]; }

verb=${1:-}
[ -n "$verb" ] || { usage; refuse "missing verb" "use show, comment, or create"; }
shift

project="" summary="" itype="Task" description="" issue="" text=""
case "$verb" in
  show)
    issue=${1:-}
    [ -n "$issue" ] || refuse "show needs an issue key" "jira.sh show SCRUM-5"
    ;;
  comment)
    issue=${1:-}; text=${2:-}
    [ -n "$issue" ] || refuse "comment needs an issue key" 'jira.sh comment SCRUM-5 "text"'
    [ -n "$text" ] || refuse "comment needs a non-empty text" 'jira.sh comment SCRUM-5 "text"'
    ;;
  create)
    while [ $# -gt 0 ]; do
      case "$1" in
        --project)     project=${2:-}; shift 2 ;;
        --summary)     summary=${2:-}; shift 2 ;;
        --type)        itype=${2:-}; shift 2 ;;
        --description) description=${2:-}; shift 2 ;;
        *) refuse "unknown create option '$1'" "options: --project KEY --summary TEXT [--type Task] [--description TEXT]" ;;
      esac
    done
    [ -n "$project" ] || refuse "create needs --project" "jira.sh create --project SCRUM --summary \"...\""
    [ -n "$summary" ] || refuse "create needs --summary" "jira.sh create --project SCRUM --summary \"...\""
    valid_project "$project" || refuse "'$project' is not a Jira project key (like SCRUM)" "project keys are upper-case: ^[A-Z][A-Z0-9_]*$"
    [ -n "$itype" ] || refuse "--type cannot be empty" "omit it for Task, or name an issue type like Bug"
    ;;
  *)
    usage; refuse "unknown verb '$verb'" "use show, comment, or create"
    ;;
esac

if [ -n "$issue" ] && ! valid_issue "$issue"; then
  refuse "'$issue' is not a Jira issue key (like SCRUM-5)" "issue keys are upper-case PROJECT-N: ^[A-Z][A-Z0-9_]*-[1-9][0-9]*$"
fi

# Arguments ride argv into the REMOTE python (never into a shell string);
# the heredoc is quoted so nothing local interpolates into it. ssh joins its
# arguments with spaces for the remote shell, which would eat an empty
# argument and split multi-word text — printf %q quotes each one for that
# trip. The credential is READ on the remote host and is never an argument.
remote_argv=$(printf '%q ' "$SITE" "$verb" "$issue" "$text" "$project" "$summary" "$itype" "$description")
exec ssh "$HOST" "python3 - $remote_argv" <<'PYEOF'
import base64, json, os, sys, urllib.error, urllib.request

site, verb, issue, text, project, summary, itype, description = sys.argv[1:9]


def refuse(msg, hint):
    print(f"error: {msg}", file=sys.stderr)
    print(f"hint: {hint}", file=sys.stderr)
    sys.exit(1)


# The pair is read from the granted env file on THIS host and never echoed.
secrets = {}
with open(os.path.expanduser("~/.culture-nodes/runner-secrets.env")) as fh:
    for line in fh:
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            secrets[k.strip()] = v.strip().strip('"').strip("'")
email, token = secrets.get("JIRA_ACCOUNT_EMAIL"), secrets.get("JIRA_API_TOKEN")
if not email or not token:
    refuse("the Jira credential pair is not granted on this host",
           "the sweep's pair lives in ~/.culture-nodes/runner-secrets.env on thor")
auth = base64.b64encode(f"{email}:{token}".encode()).decode()

# The REST base, granted beside the pair. A SCOPED Jira Cloud service-account
# token is accepted only at the Atlassian gateway and the site URL answers 401
# for it, so this is part of how the credential authenticates -- it is read
# from the same granted file rather than passed from the operator's machine.
# Empty means the site URL, which is what an unscoped token wants.
api_base = (secrets.get("JIRA_API_BASE") or "").strip().rstrip("/")
if api_base and not api_base.startswith("https://"):
    refuse(f"JIRA_API_BASE on this host is not an https URL: {api_base!r}",
           "it is the Atlassian gateway base for this site's cloud id, or empty")
root = api_base or f"https://{site}"


def call(path, payload=None):
    # Browse links are NOT built here: `jira.sh` prints issue keys, and the
    # gateway serves the API, never the board.
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
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            body = r.read()
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")[:400]
        refuse(f"Jira answered {e.code} for {path}: {detail}",
               "check the key/project exists and the granted account may act on it")
    return json.loads(body) if body.strip() else {}


def adf(s):
    paragraphs = [p for p in s.replace("\r\n", "\n").split("\n\n")] or [s]
    content = []
    for p in paragraphs:
        lines = p.split("\n")
        inner = []
        for i, ln in enumerate(lines):
            if i:
                inner.append({"type": "hardBreak"})
            if ln:
                inner.append({"type": "text", "text": ln})
        content.append({"type": "paragraph", "content": inner or [{"type": "text", "text": " "}]})
    return {"type": "doc", "version": 1, "content": content}


def adf_text(node):
    if not isinstance(node, dict):
        return ""
    if node.get("type") == "text":
        return node.get("text", "")
    if node.get("type") == "hardBreak":
        return "\n"
    parts = [adf_text(c) for c in node.get("content") or []]
    sep = "\n" if node.get("type") == "doc" else ""
    return sep.join(parts)


if verb == "comment":
    made = call(f"issue/{issue}/comment", {"body": adf(text)})
    print(made.get("id", "?"))
    sys.exit(0)

if verb == "create":
    fields = {"project": {"key": project}, "summary": summary, "issuetype": {"name": itype}}
    if description:
        fields["description"] = adf(description)
    made = call("issue", {"fields": fields})
    print(made.get("key", "?"))
    sys.exit(0)

# show: what the sweep will see, including which comments it discards as
# its own echo (same account id as the credential in use here).
me = call("myself").get("accountId", "")
cur = call(f"issue/{issue}?fields=summary,status,issuetype,comment")
f = cur.get("fields", {})
print(f"{issue}: {f.get('summary', '')}")
print(f"status: {(f.get('status') or {}).get('name', '?')}  "
      f"type: {(f.get('issuetype') or {}).get('name', '?')}")
print(f"system account (self-echo id): {me or '?'}")
comments = (f.get("comment") or {}).get("comments") or []
print(f"comments: {len(comments)} total, last {min(5, len(comments))}:")
for c in comments[-5:]:
    a = c.get("author") or {}
    aid = a.get("accountId", "?")
    tag = "  [self-echo]" if me and aid == me else ""
    print(f"- id={c.get('id', '?')} author={aid} ({a.get('displayName', '?')}) "
          f"created={c.get('created', '?')}{tag}")
    body = adf_text(c.get("body") or {}).strip()
    for ln in body.splitlines() or [""]:
        print(f"    {ln}")
PYEOF
