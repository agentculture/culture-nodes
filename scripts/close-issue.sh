#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 ISSUE DISPOSITION REASON (--run-id RUN_ID | --test-path PATH --test-command COMMAND | --artifact PATH) [--repo OWNER/REPO]" >&2
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
artifact=

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) [[ $# -ge 2 ]] || usage; repo=$2; shift 2 ;;
    --run-id) [[ $# -ge 2 ]] || usage; run_id=$2; shift 2 ;;
    --test-path) [[ $# -ge 2 ]] || usage; test_path=$2; shift 2 ;;
    --test-command) [[ $# -ge 2 ]] || usage; test_command=$2; shift 2 ;;
    --artifact) [[ $# -ge 2 ]] || usage; artifact=$2; shift 2 ;;
    *) usage ;;
  esac
done

[[ $issue =~ ^[0-9]+$ && -n $disposition && -n $reason ]] || usage

# An empty $root must not be allowed to reach the artifact checks: `$root/$path`
# would silently become `/path`, and the operator would be told their artifact
# "does not exist" when the real fault is that this is not a git checkout. The
# nick lookup below tolerates an empty root by design (it falls back), so the
# refusal is scoped to the check that actually needs a repository.
root=$(git rev-parse --show-toplevel 2>/dev/null || true)

# Exactly one evidence shape. Counted rather than pairwise-compared so adding
# a fourth shape later stays a one-line change and cannot reopen a hole.
# `if` blocks, not `[[ ... ]] && n=$((n+1))`: under `set -e` a trailing false
# AND-list would exit the script instead of skipping the increment.
shapes=0
if [[ -n $run_id ]]; then shapes=$((shapes + 1)); fi
if [[ -n $test_path || -n $test_command ]]; then shapes=$((shapes + 1)); fi
if [[ -n $artifact ]]; then shapes=$((shapes + 1)); fi

if [[ $shapes -gt 1 ]]; then
  echo "close-issue: choose one of --run-id, --test-path/--test-command or --artifact, not several" >&2
  exit 2
fi
if [[ $shapes -eq 0 ]]; then
  echo "close-issue: evidence is mandatory: --run-id, or --test-path with --test-command, or --artifact" >&2
  exit 2
fi
if [[ -n $test_path || -n $test_command ]] && [[ -z $test_path || -z $test_command ]]; then
  echo "close-issue: --test-path and --test-command must be given together" >&2
  exit 2
fi

# A Record is complete when it is written, so it has no run and no test — the
# evidence is the committed artifact it points at. Two checks, because the
# cheap one is not the load-bearing one: a record drafted on the author's disk
# and never committed passes an existence test and is still not evidence.
if [[ -n $artifact ]]; then
  if [[ -z $root ]]; then
    echo "close-issue: --artifact needs a git checkout, and this is not one" >&2
    echo "hint: the artifact must be a repo-relative path to a COMMITTED record;" >&2
    echo "      run this from inside the repository that tracks it" >&2
    exit 2
  fi
  if [[ ! -e $root/$artifact ]]; then
    echo "close-issue: artifact path does not exist: $artifact" >&2
    exit 2
  fi
  if ! git -C "$root" ls-files --error-unmatch -- "$artifact" >/dev/null 2>&1; then
    echo "close-issue: artifact path is not tracked by git: $artifact" >&2
    exit 2
  fi
fi

comment=$(printf 'Disposition: %s\n\nReason: %s\n' "$disposition" "$reason")
if [[ -n $run_id ]]; then
  comment+=$(printf '\nCulture Nodes run id: `%s`\n' "$run_id")
elif [[ -n $artifact ]]; then
  comment+=$(printf '\nArtifact: `%s`\n' "$artifact")
else
  comment+=$(printf '\nTest path: `%s`\n\nCommand: `%s`\n' "$test_path" "$test_command")
fi

# Every post made on the user's behalf is signed so a reader can tell it was
# written by an AI assistant. The nick resolves from culture.yaml, matching
# what the cicd skill's pr-reply.sh does for PR comments.
nick=$(sed -n 's/^[[:space:]-]*suffix:[[:space:]]*//p' "$root/culture.yaml" 2>/dev/null | head -1)
comment+=$(printf '\n\n- %s (Claude)\n' "${nick:-culture-nodes}")

# One command carries both the mandatory comment and closure. Do not replace
# this with a bare `gh issue close` call.
gh issue close "$issue" --repo "$repo" --reason completed --comment "$comment"
