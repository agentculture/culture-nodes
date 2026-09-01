# Deploy-time grant check (task t5, issue #253) — sourced by deploy.sh's
# preflight, before anything on the host is shipped, built or stopped.
#
# What it answers: does the runner on this host hold every environment grant
# that a workflow the control plane can start TODAY declares? On 2026-08-29 the
# answer became "no" — a deploy truncated runner-secrets.env — and nothing
# asked the question until 183 sweep runs had been refused over sixteen hours.
# The runner boundary's own refusal is correct and immediate
# (`rejected_input: environment_refs names GITHUB_TOKEN, SONAR_TOKEN,
# NODES_EVENT_TOKEN, not set in this worker process's own environment`), but it
# happens per attempt, in a run nobody is watching, hours after the deploy that
# caused it. This moves the same comparison to the one moment an operator is
# looking at the screen.
#
# NAMES ONLY, on every path. The diff is between two sets of KEY NAMES: the
# names a workflow declares in environmentRefs, and the names present in
# runner.env + runner-secrets.env. No grant VALUE is read off the host — the
# remote command emits names and nothing else — so the refusal is safe to paste
# into an issue, which is what an operator does with it.
#
# SCOPE: the LATEST version of each workflow_key that has a trigger, or that an
# enabled schedule can start. prod carries ~104 published workflow versions,
# most of them superseded; diffing all of them would flag grants nothing can
# ask for, and a gate that cries wolf is a gate people learn to skip. A
# workflow started only by hand is out of scope for the same reason: whoever
# starts it is present to read the refusal.
#
# IT FAILS CLOSED ON WHAT IT CANNOT READ. A safety gate has exactly two honest
# answers — "I diffed everything and it is granted", or "I could not diff it".
# It must never reach the first by quietly discarding the part it could not
# parse: an unreadable declaration is not an absent one, and coercing a
# malformed answer to an empty list would print the green line over precisely
# the workflow whose grant is missing. So anything in scope that this check
# cannot read — a body that is not the JSON object it expects, an `items` that
# is not a list, a current version whose `normalized_ir` will not parse — is
# reported as `unreadable:` and refuses the deploy alongside `missing:`.
#
# EXACTLY TWO PATHS DO NOT REFUSE, and neither of them is an error.
#
#   1. A host with no runner.env yet (a first deploy). There are no grants to
#      diff, so there is nothing this check could have found.
#   2. A control plane that does not answer. That is a state a deploy is often
#      the fix for, so it prints WARNING ... UNVERIFIED and proceeds. It is the
#      one deliberate hole, and it is documented in deploy/prod/README.md.
#
# Everything else refuses and names the step: an ssh call that failed, a
# runner.env that names no control plane, a temporary directory that could not
# be created, no python3 to read the answer with, and every unreadable payload
# above. The `no python3` case in particular used to print WARNING and proceed;
# it does not any more, because it is the purest form of the thing this gate
# exists to prevent -- a deploy in which nothing compared the grants, reported
# in a line nobody reads at 03:00.
#
# NOT A ROLLBACK GATE. This lane writes nothing, on any path — it is the one
# lane that touches these files and needs no backup, because it never opens
# them for writing. lanes/env-backup.sh covers the two that do.

# GRANT_CHECK_START -- tests/deploy/grantsafety_test.go sources this whole file
# against a fake host and a fake control plane.

# The keys THIS DEPLOY grants itself, later in the same run
# (lanes/runner-env-write.sh rewrites runner.env after preflight). Without
# this, the deploy that first introduces a new deploy-managed key would be
# refused by its own preflight for not having granted it yet.
# tests/deploy/grantsafety_test.go derives the same list from the lane and
# fails if this one falls behind it.
GRANT_CHECK_DEPLOY_GRANTS='NODES_RUNNER_LISTEN NODES_RUNNER_SECRET_FILE NODES_RUNNER_STATE_DIR NODES_RUNNER_HEADSPACE_PROFILES NODES_RUNNER_HEADSPACE_BIN NODES_API_URL PR_UPKEEP_SWEEP_SOURCE_URL PR_UPKEEP_SWEEP_SOURCE_SHA256 PR_UPKEEP_SWEEP_JIRA_SOURCE_URL PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256 PR_UPKEEP_SWEEP_EMIT_SOURCE_URL PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256 PR_UPKEEP_REPOSITORIES JIRA_TRANSITION_TARGETS JIRA_TRANSITION_PROJECT_PREFIX'

# grant_check_names_on_host <host> -- the key names in both grant files, one
# per line. `sed` prints capture group 1 and discards the rest of the line, so
# a value cannot leave the host even by accident.
grant_check_names_on_host() { # host
  # shellcheck disable=SC2016 # the expansions are deliberately remote
  ssh "$1" 'for f in "$HOME/.culture-nodes/runner.env" "$HOME/.culture-nodes/runner-secrets.env"; do
  [ -f "$f" ] || continue
  sed -n "s/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p" "$f"
done'
}

# The reader, kept in one string so the quoting is visible in one place. It
# receives three environment values (two JSON documents and the granted name
# list) and prints a report; it never sees, and cannot print, a grant value.
read -r -d '' GRANT_CHECK_PY <<'PYTHON' || true
import json, os, sys


def refuse(reason):
    """Stop without a verdict. stderr, not the report, because the report is
    what the caller greps for `missing:` -- a reader that died must not be
    mistaken for a reader that found nothing."""
    sys.exit("grant check: %s" % reason)


def document(name):
    """One control-plane answer, as the JSON object this check expects.

    Read from a FILE whose path is in the environment, never from the
    environment itself: prod publishes ~104 workflow versions and each one
    carries its whole source, so the answer is megabytes and passing it as an
    environment value hits the exec argument limit -- which the shell reports
    as a failure of this reader, on the one payload size that matters.

    Every failure here refuses rather than returning an empty document: the
    body arrived (curl already insisted on a 2xx), so "I cannot read it" is a
    statement about this check, and answering it with {} would let the diff
    below report a clean scope of zero workflows."""
    path = os.environ.get(name, "")
    try:
        with open(path, "rb") as handle:
            raw = handle.read().decode("utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        refuse("the body %s names could not be read: %s" % (name, exc))
    if not raw.strip():
        refuse("%s is empty; the control plane answered with no body to read" % name)
    try:
        parsed = json.loads(raw)
    except ValueError as exc:
        refuse("%s is not the JSON this check knows how to read: %s" % (name, exc))
    if not isinstance(parsed, dict):
        refuse("%s is a %s, not the JSON object this check knows how to read"
               % (name, type(parsed).__name__))
    return parsed


def collection(name):
    """The `items` of one answer. Absent or null is a genuinely empty list --
    Go marshals an empty slice as null -- but any other shape is unreadable."""
    items = document(name).get("items")
    if items is None:
        return []
    if not isinstance(items, list):
        refuse("%s names an `items` that is a %s, not a list" % (name, type(items).__name__))
    return items


def environment_refs(node):
    """Every environmentRefs entry anywhere under `node`.

    Walked rather than read at a fixed path because a node operation, a
    pre-run hook and a post-run hook all carry one, and a check that knew
    only about node operations would pass a workflow whose hook names the
    missing grant."""
    found = []
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "environmentRefs" and isinstance(value, list):
                found.extend(ref for ref in value if isinstance(ref, str))
            else:
                found.extend(environment_refs(value))
    elif isinstance(node, list):
        for item in node:
            found.extend(environment_refs(item))
    return found


granted = set(os.environ.get("GRANT_CHECK_GRANTED", "").split())
granted |= set(os.environ.get("GRANT_CHECK_DEPLOY_GRANTS", "").split())
schedules = collection("GRANT_CHECK_SCHEDULES")
workflows = collection("GRANT_CHECK_WORKFLOWS")

# Anything in scope this check could not parse. It refuses the deploy exactly
# like a missing grant does, because it is the same fact: nobody has confirmed
# the runner holds what that workflow asks for.
unreadable = []


def cannot_read(subject, reason):
    unreadable.append((subject, reason))


# An unreadable schedule is not a harmless one: it decides which workflows are
# reachable, so dropping it can move a workflow OUT of scope and hide the grant
# it needs.
enabled_events = set()
for index, schedule in enumerate(schedules):
    if not isinstance(schedule, dict):
        cannot_read("schedule #%d" % index,
                    "it is a %s, not an object" % type(schedule).__name__)
        continue
    if not schedule.get("enabled"):
        continue
    event = schedule.get("event_name")
    if not isinstance(event, str) or not event:
        cannot_read("the enabled schedule %s" % (schedule.get("id") or "#%d" % index),
                    "it names no event this check can match a trigger against")
        continue
    enabled_events.add(event)

# One version per workflow_key: the highest `version`. The list endpoint
# returns every published version, newest first, but the ordering is the
# API's business and this comparison is not going to depend on it.
latest = {}
for index, version in enumerate(workflows):
    if not isinstance(version, dict):
        cannot_read("workflow version #%d" % index,
                    "it is a %s, not an object" % type(version).__name__)
        continue
    key = version.get("workflow_key")
    if not isinstance(key, str) or not key:
        cannot_read("workflow version #%d" % index, "it names no workflow_key")
        continue
    number = version.get("version")
    if isinstance(number, bool) or not isinstance(number, int):
        cannot_read(key, "one of its versions is numbered %r, so this check cannot tell "
                         "which version is the current one" % (number,))
        continue
    if key not in latest or number > latest[key][0]:
        latest[key] = (number, version)

missing = {}
in_scope = []
for key in sorted(latest):
    number, version = latest[key]
    subject = "%s@%d" % (key, number)
    ir = version.get("normalized_ir")
    if isinstance(ir, (str, bytes)):
        try:
            ir = json.loads(ir)
        except ValueError as exc:
            cannot_read(subject, "its normalized_ir is not JSON: %s" % exc)
            continue
    if not isinstance(ir, dict):
        cannot_read(subject, "its normalized_ir is a %s, not an object" % type(ir).__name__)
        continue
    spec = ir.get("spec")
    if not isinstance(spec, dict):
        cannot_read(subject, "its normalized_ir carries a %s where the spec belongs"
                    % type(spec).__name__)
        continue
    triggers = spec.get("triggers") or []
    if not isinstance(triggers, list):
        cannot_read(subject, "its triggers are a %s, not a list, so this check cannot tell "
                             "whether anything can start it" % type(triggers).__name__)
        continue
    events = {t.get("onEvent") for t in triggers if isinstance(t, dict)}
    scheduled = bool(events & enabled_events)
    if not triggers and not scheduled:
        continue
    nodes = spec.get("nodes") or {}
    if not isinstance(nodes, dict):
        cannot_read(subject, "its nodes are a %s, not an object, so its grants cannot be read"
                    % type(nodes).__name__)
        continue
    declared = {}
    for node_name in sorted(nodes):
        for ref in environment_refs(nodes[node_name]):
            declared.setdefault(ref, "node %s" % node_name)
    outside = {name: value for name, value in spec.items() if name != "nodes"}
    for ref in environment_refs(outside):
        declared.setdefault(ref, "the workflow itself")
    in_scope.append((key, number, "an enabled schedule" if scheduled else "a trigger", len(declared)))
    for ref in sorted(declared):
        if ref in granted:
            continue
        missing.setdefault(ref, []).append("%s@%d (%s)" % (key, number, declared[ref]))

print("scope: %d workflow version(s) startable without a human" % len(in_scope))
for key, number, reason, count in in_scope:
    print("  %s@%d — reachable by %s, %d environment ref(s)" % (key, number, reason, count))
for subject, reason in unreadable:
    print("unreadable: %s — %s" % (subject, reason))
for ref in sorted(missing):
    print("missing: %s — declared by %s" % (ref, ", ".join(missing[ref])))
PYTHON

# grant_check_refuse <host> <what could not be done> [<remediation line>...]
#
# Every path that cannot COMPLETE the diff ends here, and ends the deploy. The
# wording is fixed on purpose: an operator greps a deploy log for
# "preflight failed", and a step that failed for a reason nobody named is a
# step that gets skipped by hand on the next run.
grant_check_refuse() { # host, step, remediation lines
  local host=$1 step=$2 line
  shift 2
  {
    echo "preflight failed on $host: $step, so not one workflow's grants were diffed. Nothing on $host was changed."
    for line in "$@"; do echo "$line"; done
    echo "See deploy/prod/README.md, 'Runner grants'."
  } >&2
  exit 1
}

grant_check_host() { # host
  local host=$1 url source_of_url workspace granted report read_status runner_env_state

  # THE SSH CALLS ARE CHECKED, all three of them. Each answers a question this
  # diff is built out of, and each used to answer the empty string when it
  # failed -- which is a valid-looking answer to every one of them. An ssh
  # failure here read as "the file is not absent" (so: carry on), as "the host
  # names no control plane" (so: UNVERIFIED, carry on) and as "the host holds
  # no grants at all" (so: refuse, naming keys the host may well have). Only
  # the third of those even stopped the deploy, and it stopped it with a
  # diagnosis that was not true.
  runner_env_state=$(ssh "$host" 'if [ -f ~/.culture-nodes/runner.env ]; then echo yes; else echo no; fi') ||
    grant_check_refuse "$host" "the ssh probe for ~/.culture-nodes/runner.env on $host failed" \
      "Restore ssh access to $host and re-run this deploy. A host this check cannot reach is a host whose grants it cannot read, and shipping to it is the 2026-08-29 shape exactly: sixteen hours of refused runs caused by an environment nobody had compared."
  case "$runner_env_state" in
    no)
      say "grant check: no runner.env on $host yet (first deploy) — the runner holds no grants to diff; skipped"
      return 0
      ;;
    yes) ;;
    *)
      grant_check_refuse "$host" "the ssh probe for ~/.culture-nodes/runner.env on $host answered '$runner_env_state' rather than yes or no" \
        "Something between this script and $host is rewriting command output (a login banner on a non-interactive shell is the usual one). Until that is fixed this check cannot tell a first deploy from a host it failed to read."
      ;;
  esac
  if ! command -v python3 >/dev/null 2>&1; then
    grant_check_refuse "$host" "there is no python3 on the machine running this deploy to read the published workflows with" \
      "Install python3 here and re-run. This step used to print a WARNING and proceed; it does not any more, because 'nothing compared the grants' and 'the grants are fine' are the two states this gate exists to tell apart, and only one of them may ship."
  fi

  # The control plane to ask. The operator's own NODES_API_URL wins; otherwise
  # the URL the runner on this host was granted, which is by definition the one
  # whose workflows that runner serves.
  url=${NODES_API_URL:-}
  source_of_url="the NODES_API_URL set in this shell"
  if [ -z "$url" ]; then
    # shellcheck disable=SC2016 # the expansion is deliberately remote
    url=$(ssh "$host" 'sed -n "s/^NODES_API_URL=//p" "$HOME/.culture-nodes/runner.env" | tail -n 1') ||
      grant_check_refuse "$host" "reading NODES_API_URL out of runner.env on $host over ssh failed" \
        "Restore ssh access to $host and re-run, or export NODES_API_URL in this shell to name the control plane directly."
    source_of_url="the NODES_API_URL granted to the runner on $host"
  fi
  if [ -z "$url" ]; then
    # NOT the documented unreachable-control-plane decline. Nothing has been
    # asked yet: this is a runner.env with no NODES_API_URL in it, which is the
    # same host state lanes/runner-env-write.sh refuses this deploy over two
    # lanes further on. Saying so here says it while the operator is still
    # watching.
    grant_check_refuse "$host" "no control-plane URL is set in this shell and runner.env on $host names none" \
      "export NODES_API_URL=http://<thor-address>:18080 and re-run. runner.env on $host is missing the key lanes/runner-env-write.sh refuses a deploy for, so this is a host to correct rather than a check to skip."
  fi
  url=${url%/}

  # Both answers land in FILES. prod's workflow list is megabytes -- every
  # published version carries its whole source -- and handing that to python3
  # as an environment value exceeds the exec argument limit, which the shell
  # reports as `Argument list too long` and this lane used to swallow as one
  # more "UNVERIFIED, proceeding". A gate that only fails on the payload size
  # the production control plane actually returns is not a gate.
  # A workspace that could not be created is not an unreachable control plane,
  # though it arrives looking like one: curl would have nowhere to write, and
  # curl failing is the ONE decline this lane is allowed to proceed on.
  workspace=$(mktemp -d "${TMPDIR:-/tmp}/nodes-grant-check.XXXXXX") ||
    grant_check_refuse "$host" "a temporary directory for the control plane's answers could not be created under ${TMPDIR:-/tmp}" \
      "Make ${TMPDIR:-/tmp} writable on the machine running this deploy (or set TMPDIR to somewhere that is) and re-run."

  say "grant check: reading published workflows and schedules from $source_of_url (read-only)"
  curl -fsS --max-time "${NODES_API_TIMEOUT_SECONDS:-10}" -o "$workspace/workflows.json" "$url/v1alpha1/workflows?limit=500" || {
    rm -rf "$workspace"
    say "WARNING: grant check skipped — the control plane did not answer GET /v1alpha1/workflows; this deploy's grants are UNVERIFIED"
    return 0
  }
  curl -fsS --max-time "${NODES_API_TIMEOUT_SECONDS:-10}" -o "$workspace/schedules.json" "$url/v1alpha1/schedules" || {
    rm -rf "$workspace"
    say "WARNING: grant check skipped — the control plane did not answer GET /v1alpha1/schedules; this deploy's grants are UNVERIFIED"
    return 0
  }
  granted=$(grant_check_names_on_host "$host") || {
    rm -rf "$workspace"
    grant_check_refuse "$host" "reading the granted key NAMES off $host over ssh failed" \
      "Restore ssh access to $host and re-run. Continuing here would diff every declared ref against an empty set and name keys as missing that $host may hold — a refusal an operator would act on by re-granting what was never gone."
  }

  # The reader dying is a refusal, not a skip. It only runs once curl has an
  # answer in hand, so "I could not read it" means the answer itself was not
  # what this check reads -- and continuing would ship a deploy whose grants
  # nothing compared.
  read_status=0
  report=$(GRANT_CHECK_WORKFLOWS=$workspace/workflows.json \
    GRANT_CHECK_SCHEDULES=$workspace/schedules.json \
    GRANT_CHECK_GRANTED=$granted \
    GRANT_CHECK_DEPLOY_GRANTS=$GRANT_CHECK_DEPLOY_GRANTS \
    python3 -c "$GRANT_CHECK_PY") || read_status=$?
  rm -rf "$workspace"
  if [ "$read_status" -ne 0 ]; then
    {
      echo "preflight failed on $host: the control plane answered, but this check could not read the answer (reason above), so not one workflow's grants were diffed. Nothing on $host was changed."
      echo "Confirm the control plane at $url is serving v1alpha1 and re-run this deploy; if the published shape has genuinely changed, deploy/prod/lanes/grant-check.sh is what has to learn it. See deploy/prod/README.md, 'Runner grants'."
    } >&2
    exit 1
  fi
  printf '%s\n' "$report" | sed -e '/^missing: /d' -e '/^unreadable: /d'

  if printf '%s\n' "$report" | grep -q -e '^missing: ' -e '^unreadable: '; then
    {
      if printf '%s\n' "$report" | grep -q '^unreadable: '; then
        echo "preflight failed on $host: this check could not read part of what the control plane published, so it cannot say the runner on $host holds every grant — an unreadable declaration is not an absent one. Nothing on $host was changed."
        printf '%s\n' "$report" | grep '^unreadable: '
      fi
      if printf '%s\n' "$report" | grep -q '^missing: '; then
        echo "preflight failed on $host: a workflow the control plane can start today declares an environment grant the runner on $host does not have. Nothing on $host was changed."
        printf '%s\n' "$report" | grep '^missing: '
        echo "Grant each named key on $host — secrets in ~/.culture-nodes/runner-secrets.env, non-secrets in ~/.culture-nodes/runner.env — then re-run this deploy. See deploy/prod/README.md, 'Runner grants'."
      fi
      echo "Only key NAMES are printed here; this check never reads a value off the host."
    } >&2
    exit 1
  fi
  say "grant check: every environment ref a startable workflow declares is granted on $host"
}

grant_check_host "$HOST"
# GRANT_CHECK_END
