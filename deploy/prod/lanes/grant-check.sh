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


def document(name):
    raw = os.environ.get(name, "")
    if not raw.strip():
        return {}
    try:
        return json.loads(raw)
    except ValueError as exc:
        sys.exit("grant check: %s is not the JSON this check knows how to read: %s" % (name, exc))


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
schedules = document("GRANT_CHECK_SCHEDULES").get("items") or []
workflows = document("GRANT_CHECK_WORKFLOWS").get("items") or []

enabled_events = {
    schedule.get("event_name")
    for schedule in schedules
    if isinstance(schedule, dict) and schedule.get("enabled")
}
enabled_events.discard(None)

# One version per workflow_key: the highest `version`. The list endpoint
# returns every published version, newest first, but the ordering is the
# API's business and this comparison is not going to depend on it.
latest = {}
for version in workflows:
    if not isinstance(version, dict) or not version.get("workflow_key"):
        continue
    try:
        number = int(version.get("version") or 0)
    except (TypeError, ValueError):
        number = 0
    key = version["workflow_key"]
    if key not in latest or number > latest[key][0]:
        latest[key] = (number, version)

missing = {}
in_scope = []
for key in sorted(latest):
    number, version = latest[key]
    ir = version.get("normalized_ir")
    if isinstance(ir, (str, bytes)):
        try:
            ir = json.loads(ir)
        except ValueError:
            ir = {}
    spec = ir.get("spec") if isinstance(ir, dict) and isinstance(ir.get("spec"), dict) else {}
    triggers = spec.get("triggers") if isinstance(spec.get("triggers"), list) else []
    events = {t.get("onEvent") for t in triggers if isinstance(t, dict)}
    scheduled = bool(events & enabled_events)
    if not triggers and not scheduled:
        continue
    nodes = spec.get("nodes") if isinstance(spec.get("nodes"), dict) else {}
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
for ref in sorted(missing):
    print("missing: %s — declared by %s" % (ref, ", ".join(missing[ref])))
PYTHON

grant_check_host() { # host
  local host=$1 url source_of_url workflows schedules granted report

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

  say "grant check: reading published workflows and schedules from $source_of_url (read-only)"
  workflows=$(curl -fsS --max-time "${NODES_API_TIMEOUT_SECONDS:-10}" "$url/v1alpha1/workflows?limit=500") || {
    say "WARNING: grant check skipped — the control plane did not answer GET /v1alpha1/workflows; this deploy's grants are UNVERIFIED"
    return 0
  }
  schedules=$(curl -fsS --max-time "${NODES_API_TIMEOUT_SECONDS:-10}" "$url/v1alpha1/schedules") || {
    say "WARNING: grant check skipped — the control plane did not answer GET /v1alpha1/schedules; this deploy's grants are UNVERIFIED"
    return 0
  }
  granted=$(grant_check_names_on_host "$host")

  report=$(GRANT_CHECK_WORKFLOWS=$workflows \
    GRANT_CHECK_SCHEDULES=$schedules \
    GRANT_CHECK_GRANTED=$granted \
    GRANT_CHECK_DEPLOY_GRANTS=$GRANT_CHECK_DEPLOY_GRANTS \
    python3 -c "$GRANT_CHECK_PY") || {
    say "WARNING: grant check skipped — the control plane's answer could not be read (reason above); this deploy's grants are UNVERIFIED"
    return 0
  }
  printf '%s\n' "$report" | sed -n '/^missing: /!p'

  if printf '%s\n' "$report" | grep -q '^missing: '; then
    {
      echo "preflight failed on $host: a workflow the control plane can start today declares an environment grant the runner on $host does not have. Nothing on $host was changed."
      printf '%s\n' "$report" | grep '^missing: '
      echo "Grant each named key on $host — secrets in ~/.culture-nodes/runner-secrets.env, non-secrets in ~/.culture-nodes/runner.env — then re-run this deploy. See deploy/prod/README.md, 'Runner grants'."
      echo "Only key NAMES are printed here; this check never reads a value off the host."
    } >&2
    exit 1
  fi
  say "grant check: every environment ref a startable workflow declares is granted on $host"
}

grant_check_host "$HOST"
# GRANT_CHECK_END
