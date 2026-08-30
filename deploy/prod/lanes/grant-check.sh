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
# The three declines that remain are about not being able to ASK at all, not
# about failing to understand an answer: a host with no runner.env yet (first
# deploy), no python3 on the operator's machine, and a control plane that does
# not answer. Each says UNVERIFIED out loud and none of them reaches the line
# that claims the grants were checked.
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
GRANT_CHECK_DEPLOY_GRANTS='NODES_RUNNER_LISTEN NODES_RUNNER_SECRET_FILE NODES_RUNNER_STATE_DIR NODES_RUNNER_HEADSPACE_PROFILES NODES_RUNNER_HEADSPACE_BIN NODES_API_URL PR_UPKEEP_SWEEP_SOURCE_URL PR_UPKEEP_SWEEP_SOURCE_SHA256 PR_UPKEEP_SWEEP_JIRA_SOURCE_URL PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256 PR_UPKEEP_REPOSITORIES'

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

grant_check_host() { # host
  local host=$1 url source_of_url workspace granted report read_status

  if [ "$(ssh "$host" 'if [ -f ~/.culture-nodes/runner.env ]; then echo yes; else echo no; fi')" = no ]; then
    say "grant check: no runner.env on $host yet (first deploy) — the runner holds no grants to diff; skipped"
    return 0
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    say "WARNING: grant check skipped — no python3 on this machine to read the published workflows with; this deploy's grants are UNVERIFIED"
    return 0
  fi

  # The control plane to ask. The operator's own NODES_API_URL wins; otherwise
  # the URL the runner on this host was granted, which is by definition the one
  # whose workflows that runner serves.
  url=${NODES_API_URL:-}
  source_of_url="the NODES_API_URL set in this shell"
  if [ -z "$url" ]; then
    # shellcheck disable=SC2016 # the expansion is deliberately remote
    url=$(ssh "$host" 'sed -n "s/^NODES_API_URL=//p" "$HOME/.culture-nodes/runner.env" | tail -n 1')
    source_of_url="the NODES_API_URL granted to the runner on $host"
  fi
  if [ -z "$url" ]; then
    say "WARNING: grant check skipped — no control-plane URL in this shell or in runner.env on $host; this deploy's grants are UNVERIFIED"
    return 0
  fi
  url=${url%/}

  # Both answers land in FILES. prod's workflow list is megabytes -- every
  # published version carries its whole source -- and handing that to python3
  # as an environment value exceeds the exec argument limit, which the shell
  # reports as `Argument list too long` and this lane used to swallow as one
  # more "UNVERIFIED, proceeding". A gate that only fails on the payload size
  # the production control plane actually returns is not a gate.
  workspace=$(mktemp -d "${TMPDIR:-/tmp}/nodes-grant-check.XXXXXX")

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
  granted=$(grant_check_names_on_host "$host")

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
