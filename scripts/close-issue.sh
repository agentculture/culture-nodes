#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ISSUE DISPOSITION REASON (--run-id RUN_ID | --test-path PATH --test-command COMMAND) [--repo OWNER/REPO]" >&2
  exit 2
}

[[ $# -ge 5 ]] || usage
issue=$1
disposition=$2
reason=$3
shift 3
repo=agentculture/culture-nodes
run_id=
test_path=
test_command=

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) [[ $# -ge 2 ]] || usage; repo=$2; shift 2 ;;
    --run-id) [[ $# -ge 2 ]] || usage; run_id=$2; shift 2 ;;
    --test-path) [[ $# -ge 2 ]] || usage; test_path=$2; shift 2 ;;
    --test-command) [[ $# -ge 2 ]] || usage; test_command=$2; shift 2 ;;
    *) usage ;;
  esac
done

[[ $issue =~ ^[0-9]+$ && -n $disposition && -n $reason ]] || usage
if [[ -n $run_id && ( -n $test_path || -n $test_command ) ]]; then
  echo "close-issue: choose a run id or test evidence, not both" >&2
  exit 2
fi
if [[ -z $run_id && ( -z $test_path || -z $test_command ) ]]; then
  echo "close-issue: a run id or both --test-path and --test-command are mandatory" >&2
  exit 2
fi

comment=$(printf 'Disposition: %s\n\nReason: %s\n' "$disposition" "$reason")
if [[ -n $run_id ]]; then
  comment+=$(printf '\nCulture Nodes run id: `%s`\n' "$run_id")
else
  comment+=$(printf '\nTest path: `%s`\n\nCommand: `%s`\n' "$test_path" "$test_command")
fi

# One command carries both the mandatory comment and closure. Do not replace
# this with a bare `gh issue close` call.
gh issue close "$issue" --repo "$repo" --reason completed --comment "$comment"
