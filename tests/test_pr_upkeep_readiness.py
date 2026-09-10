"""The merge-gate readiness collector (plan loop-closure-claude-codex t18).

Spec claim c10, honesty condition h19, decision c14
(`docs/specs/2026-09-07-loop-closure-claude-codex.md`).

The thing this task had to get right is *where* a readiness block can live.
`presentation` metadata is lifted out of the executable spec by the compiler
and never reaches a human task, and `internal/engine/humantask.go` writes
`context_refs` as POINTERS -- the binding exactly as authored -- not as
resolved values. So "the approval presents readiness" cannot be a presentation
field. It is a deterministic CODE node whose output the approval binds, and
whose completion is the only way into the approval at all.

Two halves, both gated here because they are two ends of one contract:

* the **graph** half -- `examples/pr-upkeep/workflow.yaml` 2.6.0 grows a
  `readiness` code node between `stage-pr-open`/`fix` and `human-merges-pr`,
  the approval binds `readiness: /nodes/readiness/output`, and NO other edge
  reaches the approval, so a task cannot be presented before the collector
  ran;
* the **program** half -- `examples/pr-upkeep/readiness.py` assembles the five
  fields (CI check states, SonarCloud open-issue count, unresolved thread
  count, proposed devague records, failing evidence) from real HTTP against a
  fake GitHub and a fake SonarCloud plus a temp `.devague` tree, and never
  reports a measurement it did not take.

The fakes are real loopback HTTP servers and the `.devague` tree is a real
directory, for the reason `tests/test_cleanup_node.py` records for its git
fixtures: the whole claim this program makes is that it READ these surfaces,
and a test that stubbed the read would assert nothing about that claim.

Never touches a real GitHub, a real SonarCloud, or the real `.devague`.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

sys.path.insert(0, str(Path(__file__).parent))
from fake_api import FakeNodesAPI  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]
EXAMPLE = ROOT / "examples" / "pr-upkeep"
WORKFLOW = EXAMPLE / "workflow.yaml"
SCRIPT = EXAMPLE / "readiness.py"
README = EXAMPLE / "README.md"
LANE_DOC = ROOT / "docs" / "operations" / "pr-upkeep-lane.md"
ISSUE_DRAFT = ROOT / "docs" / "triage" / "devague-bulk-confirm-issue.md"
PR_STATUS = ROOT / ".claude" / "skills" / "cicd" / "scripts" / "pr-status.sh"
RUNNER_ENV_LANE = ROOT / "deploy" / "prod" / "lanes" / "runner-env-write.sh"
GRANT_CHECK_LANE = ROOT / "deploy" / "prod" / "lanes" / "grant-check.sh"
DEPLOY_README = ROOT / "deploy" / "prod" / "README.md"

READINESS_NODE = "readiness"
APPROVAL_NODE = "human-merges-pr"

#: Every node 2.5.0 shipped. This task is an INSERTION: a version that loses
#: one of these changed something it was not asked to.
NODES_2_5_0 = {
    "route",
    "intake-orphan",
    "stamp-pr",
    "analyse",
    "stage-dispatch",
    "fix",
    "stage-pr-open",
    "human-merges-pr",
    "finish",
}

#: The five fields honesty condition h19 names, and the block's contract.
READINESS_FIELDS = ("ci", "sonar", "threads", "devague", "evidence")

REPOSITORY = "agentculture/culture-nodes"
PR_NUMBER = 307
HEAD_SHA = "f" * 40
WORK_ITEM = "SCRUM-42"


# ---------------------------------------------------------------------------
# graph fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module")
def document() -> dict:
    return yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))


@pytest.fixture(scope="module")
def nodes(document) -> dict:
    return document["spec"]["nodes"]


@pytest.fixture(scope="module")
def edges(document) -> list:
    return document["spec"]["edges"]


def _bindings(node: dict) -> dict:
    return ((node.get("input") or {}).get("bindings")) or {}


def _incoming(edges: list, target: str) -> list[dict]:
    return [edge for edge in edges if edge["to"] == target]


def _outgoing(edges: list, source_node: str) -> list[dict]:
    return [edge for edge in edges if edge["from"].split(".", 1)[0] == source_node]


# ---------------------------------------------------------------------------
# The graph
# ---------------------------------------------------------------------------


def test_the_workflow_declares_the_readiness_version(document):
    assert document["metadata"]["version"] == "2.6.0"


def test_the_readiness_node_is_an_insertion_not_a_rewrite(nodes):
    missing = NODES_2_5_0 - set(nodes)
    assert not missing, f"2.6.0 dropped nodes 2.5.0 shipped: {sorted(missing)}"
    assert READINESS_NODE in nodes


def test_readiness_is_a_deterministic_code_node(nodes):
    """Not an agent, and not `presentation`. A readiness block a human reads
    before a merge is a measurement, and a measurement an LLM composes is a
    claim; the compiler drops presentation metadata before a run exists."""
    node = nodes[READINESS_NODE]
    assert node["kind"] == "code"
    assert node["uses"].startswith("runner://")
    assert "presentation" not in node


def test_readiness_fetches_its_program_by_granted_url_and_digest(nodes):
    """The same rule the sweep and the cleanup node carry (task t16): the
    origin of the bytes that execute is a DEPLOYMENT's choice, and it is
    verifiable. A URL in the graph is a third party running our code."""
    operation = nodes[READINESS_NODE]["operation"]
    refs = operation["environmentRefs"]
    argv = "\n".join(operation["argv"])
    for want in ("PR_UPKEEP_READINESS_SOURCE_URL", "PR_UPKEEP_READINESS_SOURCE_SHA256"):
        assert want in refs, f"{want} is not granted to the readiness operation"
        assert want in argv, f"granting {want} changes nothing: argv never reads it"
    assert "http://" not in argv
    assert "https://" not in argv


def test_every_readiness_grant_is_stamped_by_the_deploy_lane(nodes):
    """A readiness node nothing granted is a merge decision with no block.

    Qodo read the 2.6.0 graph on PR #326 and named the consequence exactly:
    the collector sits on the ONLY path into `human-merges-pr`, so a ref this
    deployment does not grant is not a node that quietly does not run — the
    runner refuses the operation by name and every approver gets the failed
    path instead of the block. The five endpoint refs matter as much as the
    two source ones: `resolveEnv` asks `os.LookupEnv`, so *unset* is refused
    where *empty* resolves, and "optional to readiness.py" is not "optional
    to the runner". Two grant files, because that is where they live.
    """
    payload = RUNNER_ENV_LANE.read_text()
    payload = payload[payload.index("{ printf '%s\\n' \\") : payload.index("\n\t} | ssh")]
    stamped = {
        line.strip().strip("\\ \t\"'").split("=", 1)[0]
        for line in payload.splitlines()
        if "=" in line
    }
    by_hand = DEPLOY_README.read_text()
    for ref in nodes[READINESS_NODE]["operation"]["environmentRefs"]:
        if ref.startswith("PR_UPKEEP_READINESS_"):
            assert ref in stamped, (
                f"{ref} is declared by the readiness node and written by no deploy lane; "
                "the runner refuses the operation by name and the approval is presented "
                "without its block"
            )
            assert ref in GRANT_CHECK_LANE.read_text(), (
                f"{ref} is written by lanes/runner-env-write.sh but missing from "
                "grant-check.sh's GRANT_CHECK_DEPLOY_GRANTS; the deploy that first "
                "grants it would be refused by its own preflight"
            )
        else:
            assert f"`{ref}`" in by_hand, (
                f"{ref} is granted by no lane, so deploy/prod/README.md has to say who "
                "puts it on the host"
            )


def test_readiness_declares_the_two_conventional_code_outcomes(nodes):
    """`passed`/`failed` are the names internal/worker/code.go's conventional
    resolver recognises; a node whose ports it cannot map is refused before
    the runner is invoked."""
    outcomes = nodes[READINESS_NODE]["contract"]["outcomes"]
    assert set(outcomes) == {"passed", "failed"}


def test_the_approval_binds_the_collector_output(nodes):
    """h19's first half. The binding is a POINTER -- humantask.go carries
    context_refs as the author wrote them -- so this line is the whole
    mechanism by which a readiness block reaches a person."""
    bindings = _bindings(nodes[APPROVAL_NODE])
    assert bindings.get("readiness") == "/nodes/readiness/output"
    # and the two 2.5.0 bindings are still there
    assert bindings.get("finding") == "/run/input"
    assert bindings.get("fix") == "/nodes/fix/output"


def test_the_approval_is_reachable_only_from_the_collector(edges):
    """h19's second half, and the reason it is an EDGE property rather than a
    habit: the task is not created until the collector completed, because
    there is nowhere else to reach it from."""
    sources = {edge["from"] for edge in _incoming(edges, APPROVAL_NODE)}
    assert sources, "no edge reaches the approval at all"
    assert {source.split(".", 1)[0] for source in sources} == {READINESS_NODE}


def test_both_paths_reach_the_collector(edges):
    """The keyed path arrives through the stage comment, the orphan path
    directly from the fix -- the same two mutually exclusive guards 2.5.0
    used to reach the approval, moved one node earlier."""
    incoming = {edge["from"]: edge.get("when", "") for edge in _incoming(edges, READINESS_NODE)}
    assert "stage-pr-open.comment_posted" in incoming
    assert incoming["stage-pr-open.comment_posted"] == ""
    assert incoming.get("fix.completed") == 'input.work_item.startsWith("gh:")'
    # The stage edge keeps its own, mutually exclusive guard: the engine picks
    # the first eligible edge in the COMPILER's normalized order, not this
    # document's order.
    stage = {edge["to"]: edge.get("when", "") for edge in edges if edge["from"] == "fix.completed"}
    assert stage.get("stage-pr-open") == '!input.work_item.startsWith("gh:")'


def test_a_collector_that_could_not_run_still_reaches_the_person(edges):
    """A merge decision is a human authority (PRD 10.4). A collector that
    produced no block is exactly when a person most needs to be asked, so
    `failed` reaches the approval too -- and the node run's OUTCOME is what
    says which of the two happened."""
    out = {edge["from"]: edge["to"] for edge in _outgoing(edges, READINESS_NODE)}
    assert out.get("readiness.passed") == APPROVAL_NODE
    assert out.get("readiness.failed") == APPROVAL_NODE


def test_the_limits_admit_the_longer_path(document):
    """route -> analyse -> stage-dispatch -> fix -> stage-pr-open -> readiness
    -> human-merges-pr -> finish is seven transitions."""
    assert document["spec"]["limits"]["maxTransitions"] >= 7


def test_the_deployment_configuration_block_names_what_resolves_outside(document):
    """The Go lint guard (tests/lint/exampleportability_test.go) enforces this
    for every example; it is repeated here because Go is not on every lane
    that edits this graph."""
    prose = "\n".join(
        line
        for line in WORKFLOW.read_text(encoding="utf-8").splitlines()
        if line.lstrip().startswith("#")
    )
    node = document["spec"]["nodes"][READINESS_NODE]
    assert node["uses"].split("@", 1)[0] in prose
    for ref in node["operation"]["environmentRefs"]:
        assert (
            ref in prose
        ), f"{ref} is granted but never named in the deployment-configuration block"


# ---------------------------------------------------------------------------
# The program: fakes
# ---------------------------------------------------------------------------


CHECK_RUNS = {
    "check_runs": [
        {"name": "tests", "status": "completed", "conclusion": "success"},
        {"name": "lint", "status": "completed", "conclusion": "failure"},
        {"name": "webglass", "status": "in_progress", "conclusion": None},
        {"name": "codeql", "status": "completed", "conclusion": "skipped"},
    ]
}

COMMIT_STATUS = {
    "statuses": [
        {"context": "SonarCloud Code Analysis", "state": "success"},
        {"context": "cloudflare/pages", "state": "pending"},
    ]
}

REVIEW_THREADS = {
    "data": {
        "repository": {
            "pullRequest": {
                "reviewThreads": {
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                    "nodes": [
                        {"id": "t1", "isResolved": True},
                        {"id": "t2", "isResolved": False},
                        {"id": "t3", "isResolved": False},
                        {"id": "t4", "isResolved": True},
                        {"id": "t5", "isResolved": True},
                    ],
                }
            }
        }
    }
}


@pytest.fixture
def github():
    fake = FakeNodesAPI()
    fake.graphql_queries = []  # type: ignore[attr-defined]
    # Mutable so a test can hand one endpoint an error-shaped 200 without
    # re-registering a route (the fake matches routes in registration order,
    # so the body is the only overridable half).
    fake.bodies = {  # type: ignore[attr-defined]
        "check-runs": CHECK_RUNS,
        "status": COMMIT_STATUS,
        "graphql": REVIEW_THREADS,
    }

    def check_runs(handler, _match, _query, _body):
        handler.send_json(200, fake.bodies["check-runs"])  # type: ignore[attr-defined]

    def commit_status(handler, _match, _query, _body):
        handler.send_json(200, fake.bodies["status"])  # type: ignore[attr-defined]

    def graphql(handler, _match, _query, body):
        fake.graphql_queries.append(json.loads(body))  # type: ignore[attr-defined]
        handler.send_json(200, fake.bodies["graphql"])  # type: ignore[attr-defined]

    fake.route("GET", r"^/repos/[^/]+/[^/]+/commits/[^/]+/check-runs$", check_runs)
    fake.route("GET", r"^/repos/[^/]+/[^/]+/commits/[^/]+/status$", commit_status)
    fake.route("POST", r"^/graphql$", graphql)
    fake.start()
    yield fake
    fake.stop()


@pytest.fixture
def sonar():
    fake = FakeNodesAPI()
    fake.seen = []  # type: ignore[attr-defined]
    fake.bodies = {  # type: ignore[attr-defined]
        "gate": {"projectStatus": {"status": "ERROR"}},
        "issues": {"total": 4, "issues": []},
        "hotspots": {"paging": {"total": 2}},
    }

    def gate(handler, _match, query, _body):
        fake.seen.append(("gate", query))  # type: ignore[attr-defined]
        handler.send_json(200, fake.bodies["gate"])  # type: ignore[attr-defined]

    def issues(handler, _match, query, _body):
        fake.seen.append(("issues", query))  # type: ignore[attr-defined]
        handler.send_json(200, fake.bodies["issues"])  # type: ignore[attr-defined]

    def hotspots(handler, _match, query, _body):
        fake.seen.append(("hotspots", query))  # type: ignore[attr-defined]
        handler.send_json(200, fake.bodies["hotspots"])  # type: ignore[attr-defined]

    fake.route("GET", r"^/api/qualitygates/project_status$", gate)
    fake.route("GET", r"^/api/issues/search$", issues)
    fake.route("GET", r"^/api/hotspots/search$", hotspots)
    fake.start()
    yield fake
    fake.stop()


@pytest.fixture
def devague(tmp_path: Path) -> Path:
    """A `.devague` tree the shape the real one has: a frame with claims and
    nested honesty conditions, and a delivery with evidence, deviations and
    obligations -- some proposed, some confirmed, one evidence record failing."""
    root = tmp_path / ".devague"
    (root / "frames").mkdir(parents=True)
    (root / "deliveries").mkdir(parents=True)
    (root / "frames" / "loop-closure.json").write_text(
        json.dumps(
            {
                "slug": "loop-closure",
                "claims": [
                    {
                        "id": "c1",
                        "status": "confirmed",
                        "honesty_conditions": [
                            {"id": "h1", "status": "confirmed"},
                            {"id": "h19", "status": "proposed"},
                        ],
                    },
                    {"id": "c2", "status": "proposed"},
                ],
            }
        ),
        encoding="utf-8",
    )
    (root / "deliveries" / "loop-closure.json").write_text(
        json.dumps(
            {
                "plan_slug": "loop-closure",
                "evidence": [
                    {"id": "e1", "status": "proposed", "outcome": "pass"},
                    {"id": "e2", "status": "approved", "outcome": "fail"},
                    {"id": "e3", "status": "proposed", "outcome": "fail"},
                ],
                "deviations": [{"id": "d1", "status": "proposed"}],
                "obligations": [{"id": "o1", "status": "approved"}],
            }
        ),
        encoding="utf-8",
    )
    return root


def fact(**extra) -> dict:
    return {
        "source": "github_pr",
        "repository": REPOSITORY,
        "number": PR_NUMBER,
        "head_sha": HEAD_SHA,
        "findings": [{"id": "pr307-qodo-1"}],
        "work_item": WORK_ITEM,
        **extra,
    }


def run_readiness(
    github: FakeNodesAPI, sonar: FakeNodesAPI, devague: Path, payload: dict, **env_overrides: str
) -> tuple[subprocess.CompletedProcess[str], dict | None]:
    env = {
        **os.environ,
        "NODES_INPUT_JSON": json.dumps(payload),
        "PR_UPKEEP_READINESS_GITHUB_API": github.base_url,
        "PR_UPKEEP_READINESS_SONAR_API": sonar.base_url,
        "PR_UPKEEP_READINESS_DEVAGUE_ROOT": str(devague),
        "GITHUB_TOKEN": "x-test-token",
        "SONAR_TOKEN": "",
        **env_overrides,
    }
    proc = subprocess.run(
        [sys.executable, str(SCRIPT)], capture_output=True, text=True, env=env, check=False
    )
    block = None
    lines = [line for line in proc.stdout.splitlines() if line.strip()]
    if lines:
        block = json.loads(lines[-1])
    return proc, block


# ---------------------------------------------------------------------------
# The program: the block
# ---------------------------------------------------------------------------


def test_the_block_carries_the_five_fields(github, sonar, devague):
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr
    assert block is not None, proc.stdout
    for field in READINESS_FIELDS:
        assert field in block, f"the readiness block has no {field!r} field"
    assert block["failures"] == [], block["failures"]


def test_ci_states_carry_check_runs_and_commit_statuses(github, sonar, devague):
    """`gh pr checks` (what pr-status.sh reads) merges both surfaces, so a
    collector that read only check-runs would report a green PR whose
    SonarCloud commit status is red."""
    _, block = run_readiness(github, sonar, devague, fact())
    assert {entry["name"]: entry["state"] for entry in block["ci"]} == {
        "tests": "pass",
        "lint": "fail",
        "webglass": "pending",
        "codeql": "skipping",
        "SonarCloud Code Analysis": "pass",
        "cloudflare/pages": "pending",
    }


def test_sonar_and_threads_have_pr_status_sh_s_shape(github, sonar, devague):
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["sonar"] == {"gate": "ERROR", "open_issues": 4, "hotspots": 2}
    assert block["threads"] == {"unresolved": 2, "total": 5}


def test_the_sonar_queries_are_the_ones_pr_status_sh_issues(github, sonar, devague):
    """The reuse claim, gated rather than asserted in prose: the same three
    endpoints with the same filters. A different `statuses=` filter would
    count a different thing under the same name."""
    run_readiness(github, sonar, devague, fact())
    seen = {name: query for name, query in sonar.seen}
    shell = PR_STATUS.read_text(encoding="utf-8")

    assert seen["gate"]["pullRequest"] == [str(PR_NUMBER)]
    assert "qualitygates/project_status?projectKey=" in shell

    assert seen["issues"]["statuses"] == ["OPEN,CONFIRMED"]
    assert "statuses=OPEN,CONFIRMED" in shell

    assert seen["hotspots"]["status"] == ["TO_REVIEW"]
    assert "status=TO_REVIEW" in shell


def test_the_sonar_component_defaults_to_the_repository_convention(github, sonar, devague):
    run_readiness(github, sonar, devague, fact())
    seen = dict(sonar.seen)
    assert seen["issues"]["componentKeys"] == ["agentculture_culture-nodes"]
    assert seen["gate"]["projectKey"] == ["agentculture_culture-nodes"]


def test_a_granted_component_overrides_the_derivation(github, sonar, devague):
    run_readiness(
        github, sonar, devague, fact(), PR_UPKEEP_READINESS_SONAR_COMPONENT="other_project"
    )
    seen = dict(sonar.seen)
    assert seen["gate"]["projectKey"] == ["other_project"]


def test_the_thread_query_reads_resolution_the_way_pr_status_sh_does(github, sonar, devague):
    run_readiness(github, sonar, devague, fact())
    assert len(github.graphql_queries) == 1
    body = github.graphql_queries[0]
    assert "reviewThreads" in body["query"] and "isResolved" in body["query"]
    # The PR is a query VARIABLE, not string-interpolated into the document --
    # pr-status.sh interpolates because it is a shell heredoc; a program with a
    # JSON encoder has no excuse to.
    assert body["variables"]["number"] == PR_NUMBER
    assert body["variables"]["owner"] == REPOSITORY.split("/")[0]
    assert body["variables"]["name"] == REPOSITORY.split("/")[1]


def test_proposed_devague_records_are_collected_across_collections(github, sonar, devague):
    """Claims, nested honesty conditions, evidence, deviations and obligations
    all carry a status, and a bulk confirm is exactly the reason to count them
    together (decision c14)."""
    _, block = run_readiness(github, sonar, devague, fact())
    assert sorted(block["devague"]["proposed"]) == [
        "loop-closure/claims/c2",
        "loop-closure/deviations/d1",
        "loop-closure/evidence/e1",
        "loop-closure/evidence/e3",
        "loop-closure/honesty_conditions/h19",
    ]


def test_failing_evidence_is_reported_regardless_of_its_status(github, sonar, devague):
    """A confirmed failing evidence record is the sharpest one there is: it is
    a measured failure somebody already agreed to."""
    _, block = run_readiness(github, sonar, devague, fact())
    assert sorted(block["evidence"]["failing"]) == [
        "loop-closure/evidence/e2",
        "loop-closure/evidence/e3",
    ]


def test_a_granted_slug_scopes_the_devague_read(github, sonar, devague, tmp_path):
    (devague / "frames" / "other.json").write_text(
        json.dumps({"slug": "other", "claims": [{"id": "cX", "status": "proposed"}]}),
        encoding="utf-8",
    )
    _, block = run_readiness(
        github, sonar, devague, fact(), PR_UPKEEP_READINESS_DEVAGUE_SLUG="loop-closure"
    )
    assert block["devague"]["scope"] == ["loop-closure"]
    assert "other/claims/cX" not in block["devague"]["proposed"]


def test_a_missing_devague_tree_is_reported_never_counted_as_zero(github, sonar, devague, tmp_path):
    _, block = run_readiness(
        github, sonar, devague, fact(), PR_UPKEEP_READINESS_DEVAGUE_ROOT=str(tmp_path / "absent")
    )
    assert block["devague"] is None
    assert block["evidence"] is None
    assert [f["source"] for f in block["failures"]] == ["devague"]


# ---------------------------------------------------------------------------
# The program: what it refuses to fabricate
# ---------------------------------------------------------------------------


def test_an_unreadable_source_is_null_with_a_named_failure(github, sonar, devague):
    """The merge gate's doctrine at the collector: an unmeasured field folded
    into a number is the false green this block exists to prevent. Zero open
    Sonar issues and "we could not ask SonarCloud" must not read the same."""
    proc, block = run_readiness(
        github, sonar, devague, fact(), PR_UPKEEP_READINESS_SONAR_API="http://127.0.0.1:1"
    )
    assert proc.returncode == 0, proc.stderr
    assert block["sonar"] is None
    assert [f["source"] for f in block["failures"]] == ["sonar"]
    # ... and the other four fields were still collected.
    assert block["ci"] and block["threads"] and block["devague"] is not None


def test_an_unreadable_github_leaves_ci_and_threads_null(github, sonar, devague):
    proc, block = run_readiness(
        github, sonar, devague, fact(), PR_UPKEEP_READINESS_GITHUB_API="http://127.0.0.1:1"
    )
    assert proc.returncode == 0, proc.stderr
    assert block["ci"] is None and block["threads"] is None
    assert sorted(f["source"] for f in block["failures"]) == ["github-checks", "github-threads"]
    assert block["sonar"] == {"gate": "ERROR", "open_issues": 4, "hotspots": 2}


# --- an error-shaped 200 is an unread source, not a clean measurement -------
#
# Qodo read this file on PR #326 and named the half the tests above missed:
# every "could not read" case they cover is a TRANSPORT failure -- a port that
# refuses the connection, a directory that is not there. A source that answers
# 200 with a body missing the field the question was about took a different
# path entirely, through `body.get(field, default)`, and arrived at the
# approval as a number. Those are the sharpest false greens this block can
# produce: `open_issues: 0` and `unresolved: 0` are what a mergeable PR looks
# like.


def test_an_error_shaped_check_runs_response_is_not_a_commit_with_no_ci(github, sonar, devague):
    """GitHub answering `{"message": "Not Found"}` is not a green commit.

    Read as `check_runs: []` the block says the head commit has no CI at all,
    which reads to an approver as nothing failing.
    """
    github.bodies["check-runs"] = {"message": "Not Found", "status": "404"}
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr
    assert block["ci"] is None
    assert [f["source"] for f in block["failures"]] == ["github-checks"]
    assert "check_runs" in block["failures"][0]["detail"]
    # ... and the sources that DID answer are still measured.
    assert block["sonar"] == {"gate": "ERROR", "open_issues": 4, "hotspots": 2}
    assert block["threads"] == {"unresolved": 2, "total": 5}


def test_a_commit_status_body_without_statuses_leaves_ci_null(github, sonar, devague):
    """The check-runs half answering does not license reporting the block as
    complete: SonarCloud and Cloudflare report through this surface, so half a
    CI read is not a CI read."""
    github.bodies["status"] = {"state": "success"}
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["ci"] is None
    assert [f["source"] for f in block["failures"]] == ["github-checks"]


def test_a_sonar_issue_search_with_no_total_is_never_zero_open_issues(github, sonar, devague):
    """The exact false green the module docstring promises not to produce."""
    sonar.bodies["issues"] = {"errors": [{"msg": "Component key not found"}]}
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr
    assert block["sonar"] is None, "an unanswered issue search was reported as a count"
    assert [f["source"] for f in block["failures"]] == ["sonar"]


def test_a_sonar_gate_body_without_a_status_leaves_the_whole_block_null(github, sonar, devague):
    sonar.bodies["gate"] = {"errors": [{"msg": "Project not found"}]}
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["sonar"] is None
    assert [f["source"] for f in block["failures"]] == ["sonar"]


def test_the_hotspot_total_is_read_from_either_place_sonar_puts_it(github, sonar, devague):
    """`issues/search` carries `total` at the top level and `hotspots/search`
    under `paging`; the collector accepts either and refuses neither-present.
    Pinned so the strictness added above cannot become brittleness."""
    sonar.bodies["hotspots"] = {"total": 7}
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["sonar"]["hotspots"] == 7


def test_a_graphql_data_null_is_not_a_pr_with_no_unresolved_threads(github, sonar, devague):
    """GitHub answers a partial GraphQL failure with HTTP 200 and `data: null`
    -- and no `errors` key when the failure is an empty result rather than a
    query error. Walked with `or {}` that is `unresolved: 0`, which is the
    single field an approver is most likely to merge on."""
    github.bodies["graphql"] = {"data": None}
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr
    assert block["threads"] is None
    assert [f["source"] for f in block["failures"]] == ["github-threads"]


def test_a_next_page_with_no_cursor_is_refused_rather_than_counted_twice(github, sonar, devague):
    """`hasNextPage` with a null `endCursor` says another page exists and does
    not say where it starts. Following it means re-sending `after: null` and
    counting page one again -- a tally, but not the tally."""
    github.bodies["graphql"] = {
        "data": {
            "repository": {
                "pullRequest": {
                    "reviewThreads": {
                        "pageInfo": {"hasNextPage": True, "endCursor": None},
                        "nodes": [{"id": "t1", "isResolved": False}],
                    }
                }
            }
        }
    }
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["threads"] is None
    assert [f["source"] for f in block["failures"]] == ["github-threads"]
    assert len(github.graphql_queries) == 1, "the cursorless page was re-read"


def test_a_thread_without_isresolved_is_not_counted_as_resolved(github, sonar, devague):
    """`not node.get("isResolved")` would have called a missing field
    unresolved; a truthiness read of an absent field is a guess either way."""
    github.bodies["graphql"] = {
        "data": {
            "repository": {
                "pullRequest": {
                    "reviewThreads": {
                        "pageInfo": {"hasNextPage": False, "endCursor": None},
                        "nodes": [{"id": "t1"}],
                    }
                }
            }
        }
    }
    _, block = run_readiness(github, sonar, devague, fact())
    assert block["threads"] is None
    assert [f["source"] for f in block["failures"]] == ["github-threads"]


def test_a_source_answering_a_json_array_is_a_failure_not_a_traceback(github, sonar, devague):
    """A non-object body used to reach `.get` and raise AttributeError, which
    `attempt()` did not catch -- so ONE malformed source cost the whole block
    and every approver got the failed path. It is a named failure now."""
    github.bodies["check-runs"] = ["not", "an", "object"]
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr
    assert block is not None, proc.stdout + proc.stderr
    assert block["ci"] is None
    assert [f["source"] for f in block["failures"]] == ["github-checks"]
    assert "Traceback" not in proc.stderr


def test_a_refused_input_exits_nonzero_and_writes_no_block(github, sonar, devague):
    proc, block = run_readiness(github, sonar, devague, {"source": "github_pr"})
    assert proc.returncode != 0
    assert block is None
    assert "repository" in proc.stderr


def test_the_node_spawns_nothing_so_it_can_call_neither_gh_nor_devex():
    """`gh` and `devex` are operator tools on an operator's PATH. This runs in
    a runner image, so the reuse of pr-status.sh is of its SHAPE, never of its
    shell: the program spawns no process at all, which is a stronger and more
    checkable property than the absence of two command names from the text."""
    import ast

    tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
    spawners = {"subprocess", "shutil", "pty", "multiprocessing"}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                assert alias.name.split(".")[0] not in spawners, alias.name
        elif isinstance(node, ast.ImportFrom):
            assert (node.module or "").split(".")[0] not in spawners, node.module
        elif isinstance(node, ast.Attribute):
            assert node.attr not in {"system", "popen", "execv", "fork", "spawnv"}, node.attr


def test_the_program_imports_only_the_standard_library():
    """The runtime constraint every example script honours."""
    import ast

    tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
    stdlib = set(sys.stdlib_module_names)
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names = [alias.name.split(".")[0] for alias in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0:
            names = [(node.module or "").split(".")[0]]
        else:
            continue
        for name in names:
            assert name in stdlib, f"readiness.py imports third-party module {name!r}"


# ---------------------------------------------------------------------------
# The whole chain: a human task's context resolves to the block
# ---------------------------------------------------------------------------


#: The code-node output surface (internal/runners/dispatch.go CodeNodeOutput +
#: artifactMap): `/nodes/<id>/output` is this document, and the program's
#: stdout travels as the artifact it references. Pinned against the Go source
#: so a rename there fails this test rather than silently making the chain
#: below fiction.
CODE_NODE_OUTPUT_KEYS = ("operation_id", "state", "exit_code", "artifacts")
STDOUT_ARTIFACT_KEY = "stdout_ref"


def test_the_code_node_output_surface_is_what_this_test_models():
    dispatch = (ROOT / "internal" / "runners" / "dispatch.go").read_text(encoding="utf-8")
    for key in CODE_NODE_OUTPUT_KEYS:
        assert f'json:"{key}' in dispatch
    assert f'"{STDOUT_ARTIFACT_KEY}":' in dispatch


def _resolve(pointer: str, run_input: dict, node_outputs: dict, artifacts: dict):
    """The resolution a presenting surface performs on a `context_refs`
    pointer: `/run/input/...` and `/nodes/<id>/output/...` walked over the
    run, then an artifact reference dereferenced against the store."""
    parts = [p for p in pointer.split("/") if p]
    if parts[:2] == ["run", "input"]:
        value = run_input
        rest = parts[2:]
    elif parts[0] == "nodes" and parts[2] == "output":
        value = node_outputs[parts[1]]
        rest = parts[3:]
    else:
        raise AssertionError(f"unresolvable pointer {pointer}")
    for key in rest:
        value = value[key]
    return value


def test_the_human_task_context_resolves_to_the_readiness_block(
    github, sonar, devague, nodes, tmp_path
):
    """h19 end to end, in the one place a Python test can hold it: the graph's
    binding, the code node's output surface, and the program's real stdout.

    The Go half -- that the engine writes these bindings onto the human task
    as `context_refs` and that the task does not exist before the collector
    completed -- is `tests/e2e/stagewriteback_test.go`.
    """
    proc, block = run_readiness(github, sonar, devague, fact())
    assert proc.returncode == 0, proc.stderr

    # What the runner stored, and what the engine wrote as the node's output.
    stdout_ref = "artifact://sha256:" + "a" * 64
    artifacts = {stdout_ref: proc.stdout}
    node_outputs = {
        READINESS_NODE: {
            "operation_id": "op_readiness",
            "state": "completed",
            "exit_code": 0,
            "artifacts": {STDOUT_ARTIFACT_KEY: stdout_ref},
        },
        "fix": {"summary": "opened a pull request for the finding"},
    }

    # What the engine wrote as the task's context_refs: the bindings verbatim.
    context_refs = _bindings(nodes[APPROVAL_NODE])
    assert set(context_refs) >= {"readiness", "finding", "fix"}

    pointed_at = _resolve(context_refs["readiness"], fact(), node_outputs, artifacts)
    stored = artifacts[pointed_at["artifacts"][STDOUT_ARTIFACT_KEY]]
    resolved = json.loads([line for line in stored.splitlines() if line.strip()][-1])

    assert resolved == block
    for field in READINESS_FIELDS:
        assert field in resolved
    assert resolved["repository"] == REPOSITORY
    assert resolved["pull_request"] == PR_NUMBER
    assert resolved["work_item"] == WORK_ITEM


# ---------------------------------------------------------------------------
# The documents that have to stay true
# ---------------------------------------------------------------------------


def test_the_readme_and_lane_doc_describe_the_collector():
    readme = README.read_text(encoding="utf-8")
    lane = LANE_DOC.read_text(encoding="utf-8")
    assert "readiness" in readme
    assert "/nodes/readiness/output" in readme
    assert "readiness" in lane


def test_the_upstream_bulk_confirm_ask_is_written_down_and_cited():
    """Decision c14: the one-transaction devague confirm is an upstream
    agentculture/devague change, filed as a sibling-repo issue and NOT built
    here. The issue text is a committed artifact so the operator posts the
    same words this task reasoned about."""
    assert ISSUE_DRAFT.exists(), f"{ISSUE_DRAFT} is missing"
    draft = ISSUE_DRAFT.read_text(encoding="utf-8")
    assert "agentculture/devague" in draft
    lane = LANE_DOC.read_text(encoding="utf-8")
    assert "devague-bulk-confirm-issue.md" in lane
