#!/usr/bin/env bash
# scripts/open-issue.sh — open an issue with a rendered body AND a type,
# in one command (issue #157, task t6).
#
# # Why this exists
#
# A one-time backfill of issue types decays the moment the next issue is
# opened. Nothing in this repo sets a type at creation: there are no GitHub
# issue templates, and `agtag issue post` has no `--type` flag. The installed
# gh (2.45.0) has no `issueType` JSON field and no verb to set one, so the
# type is reachable only through GraphQL.
#
# # Why it is THIN, and must stay that way
#
# agentculture/agtag#19 asks agtag to absorb exactly this (template rendering
# + type at creation). When it lands, deleting this file is the whole
# migration — so nothing else may accrete here. In particular this script
# does NOT post, sign or authenticate: `agtag issue post` does all three, and
# resolves the signing nick from culture.yaml itself. The single delegation
# line below is the entire posting story.
#
# The vendored .claude/skills/communicate/scripts/post-issue.sh is NOT edited
# for this — that tree is cited verbatim. This is a first-party script beside
# it. tests/test_open_issue.py pins both properties.
#
# # Why the type name is validated first
#
# GitHub's `type:` search qualifier FAILS OPEN: `type:NotARealType` returns 0
# results rather than an error, so a misspelled type reads as a clean backlog
# forever after. A name is only trustworthy if it was checked against
# organization.issueTypes — and the check has to happen BEFORE the post, or a
# typo leaves a real, untyped issue behind for someone to clean up.
#
# Usage:
#   scripts/open-issue.sh --type Record --title "..." --template record \
#       --set ARTIFACT_PATH=docs/deviations/x.md --set SUMMARY="..."
#
#   --template takes a path, or a bare name resolved inside
#   docs/triage/issue-templates/<name>.md.
set -euo pipefail

usage() {
  echo "usage: $0 --type NAME --title TITLE --template PATH|NAME [--set KEY=VALUE]... [--repo OWNER/REPO]" >&2
  exit 2
}

repo=agentculture/culture-nodes
type_name=
title=
template=
subs=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) [[ $# -ge 2 ]] || usage; repo=$2; shift 2 ;;
    --type) [[ $# -ge 2 ]] || usage; type_name=$2; shift 2 ;;
    --title) [[ $# -ge 2 ]] || usage; title=$2; shift 2 ;;
    --template) [[ $# -ge 2 ]] || usage; template=$2; shift 2 ;;
    --set) [[ $# -ge 2 ]] || usage; subs+=("$2"); shift 2 ;;
    -h|--help) usage ;;
    *) echo "open-issue: unknown flag: $1" >&2; usage ;;
  esac
done

[[ -n $type_name && -n $title && -n $template && $repo == */* ]] || usage

org=${repo%%/*}
name=${repo##*/}
root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)

if ! command -v agtag >/dev/null 2>&1; then
  echo "open-issue: agtag not found on PATH — install agtag (>=0.1); it owns posting and signing" >&2
  exit 2
fi

# A bare name resolves inside the template directory; a path is used as given.
if [[ ! -f $template ]]; then
  candidate=$root/docs/triage/issue-templates/$template.md
  if [[ ! -f $candidate ]]; then
    echo "open-issue: no such template: $template (looked in docs/triage/issue-templates/)" >&2
    exit 2
  fi
  template=$candidate
fi

body_file=$(mktemp -t open-issue-body.XXXXXX)
trap 'rm -f "$body_file"' EXIT

# `{{PLACEHOLDER}}` substitution, matching the convention the vendored
# communicate templates already use. A placeholder that survives rendering is
# a refusal, not a warning: posting a literal `{{ARTIFACT_PATH}}` produces a
# Record that points at nothing.
python3 - "$template" "$body_file" "${subs[@]}" <<'PY'
import re
import sys

src, dest, *pairs = sys.argv[1:]
text = open(src, encoding="utf-8").read()
for pair in pairs:
    if "=" not in pair:
        sys.stderr.write("open-issue: --set expects KEY=VALUE, got %r\n" % pair)
        raise SystemExit(2)
    key, value = pair.split("=", 1)
    text = text.replace("{{%s}}" % key, value)
left = sorted(set(re.findall(r"\{\{([A-Z0-9_]+)\}\}", text)))
if left:
    sys.stderr.write("open-issue: unsubstituted placeholder(s): %s\n" % ", ".join(left))
    raise SystemExit(2)
open(dest, "w", encoding="utf-8").write(text)
PY

# Resolve the type name against the org's own menu. Unknown or disabled is a
# hard stop, before anything is created.
type_id=$(gh api graphql -f org="$org" \
  -f query='query($org:String!){organization(login:$org){issueTypes(first:20){nodes{id name isEnabled}}}}' \
  | python3 -c 'import json,sys
want = sys.argv[1]
nodes = json.load(sys.stdin)["data"]["organization"]["issueTypes"]["nodes"] or []
print(next((n["id"] for n in nodes if n["name"] == want and n["isEnabled"]), ""))' "$type_name")

if [[ -z $type_id ]]; then
  echo "open-issue: no enabled issue type named '$type_name' in the $org org" >&2
  echo "hint: list them with: gh api graphql -f query='{organization(login:\"$org\"){issueTypes(first:20){nodes{name isEnabled}}}}'" >&2
  exit 2
fi

# --- delegation ------------------------------------------------------------
# agtag owns posting, signing and auth. This script adds exactly one thing
# agtag cannot do yet: the type. Everything between here and the mutation is
# the whole of that addition.
issue_json=$(agtag issue post --repo "$repo" --title "$title" --body-file "$body_file" --json)
read -r number url < <(printf '%s' "$issue_json" |
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["number"], d["url"])')
node_id=$(gh api graphql -f owner="$org" -f name="$name" -F number="$number" \
  -f query='query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){id}}}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["repository"]["issue"]["id"])')
gh api graphql -f id="$node_id" -f typeId="$type_id" \
  -f query='mutation($id:ID!,$typeId:ID!){updateIssue(input:{id:$id,issueTypeId:$typeId}){issue{number}}}' >/dev/null
# --- end delegation --------------------------------------------------------

printf '%s\n' "$url"
