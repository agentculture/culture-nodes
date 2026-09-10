"""The analysis node: judge a PR's findings BEFORE a fix session is bought.

Plan loop-closure-claude-codex task t14 (spec claim c5, honesty condition
h14). Two halves, both here because they are two ends of one contract:

* the **graph** half -- `examples/pr-upkeep/workflow.yaml` 2.5.0 grows an
  `analyse` agent node between `route`/`stamp-pr` and `fix`, whose output
  carries a verdict per finding and the packages `fix` is to work, and whose
  pushbacks reach the run's own output so the tick summary can name them;
* the **sweep** half -- `examples/pr-upkeep/pr_upkeep_emit.py` emits one
  fact per PR per tick carrying every undispatched finding on the
  highest-priority finding's FILE, and its dedupe holds or releases that set
  as one thing.

Why the fixture cannot be an assertion about what an LLM says: `analyse` is
an agent node, so its verdicts are judgment, not a function. What is gated
here is everything around the judgment -- that every finding gets exactly one
verdict with a one-line reason, that the same-rule findings in a file arrive
as ONE package, that a document missing a reason is refused by the node's own
declared contract, and that the fixture's verdicts are the ones its recorded
review threads actually justify. The fixture output is what a correct session
returns; the schema in the graph is what makes a wrong one a
`contract_rejected` instead of a bad fix.

The fixture PR (`tests/fixtures/pr-upkeep-analysis/`) is the shape honesty
condition h14 names: one file carrying a resolved thread, a pushed-back
thread, a cross-rule duplicate, and 13 same-rule SonarCloud nits.
"""

from __future__ import annotations

import importlib
import json
from pathlib import Path

import jsonschema
import pytest
import yaml

from tests.test_pr_upkeep_sweep import _stub_sweep, sweep  # noqa: F401

emit = importlib.import_module("pr_upkeep_emit")

ROOT = Path(__file__).resolve().parents[1]
EXAMPLE = ROOT / "examples" / "pr-upkeep"
FIXTURES = ROOT / "tests" / "fixtures" / "pr-upkeep-analysis"
LANE_DOC = ROOT / "docs" / "operations" / "pr-upkeep-lane.md"
WORKFLOW_SCHEMA = ROOT / "schemas" / "workflow" / "workflow.schema.json"

#: The node this task adds, and the two it must not displace.
ANALYSIS_NODE = "analyse"
FIX_NODE = "fix"
END_NODE = "finish"

#: Every node 2.4.0 shipped. The task is an insertion, not a rewrite: a
#: version that loses one of these has changed something it was not asked to.
NODES_2_4_0 = {
    "route",
    "intake-orphan",
    "stamp-pr",
    "stage-dispatch",
    "fix",
    "stage-pr-open",
    "human-merges-pr",
    "finish",
}

VERDICTS = {"FIX", "PUSHBACK", "DUPLICATE", "SKIP"}


@pytest.fixture(scope="module")
def document() -> dict:
    return yaml.safe_load((EXAMPLE / "workflow.yaml").read_text(encoding="utf-8"))


@pytest.fixture(scope="module")
def nodes(document) -> dict:
    return document["spec"]["nodes"]


@pytest.fixture(scope="module")
def edges(document) -> list:
    return document["spec"]["edges"]


@pytest.fixture(scope="module")
def analysis_outcomes(nodes) -> dict:
    return nodes[ANALYSIS_NODE]["contract"]["outcomes"]


@pytest.fixture(scope="module")
def fixture_findings() -> list[dict]:
    return json.loads((FIXTURES / "findings.json").read_text(encoding="utf-8"))["findings"]


@pytest.fixture(scope="module")
def fixture_threads() -> dict:
    return json.loads((FIXTURES / "review-threads.json").read_text(encoding="utf-8"))


@pytest.fixture(scope="module")
def fixture_analysis() -> dict:
    return json.loads((FIXTURES / "analysis-output.json").read_text(encoding="utf-8"))


def _targets(edges: list, source: str) -> dict:
    """`from` -> {target: guard}. A guard of "" is an unguarded edge."""
    return {edge["to"]: edge.get("when", "") for edge in edges if edge["from"] == source}


def _bindings(node: dict) -> dict:
    return ((node.get("input") or {}).get("bindings")) or {}


# --------------------------------------------------------------------------
# The graph
# --------------------------------------------------------------------------


def test_the_workflow_declares_the_analysis_version(document):
    assert document["metadata"]["version"] == "2.5.0"


def test_every_node_the_previous_version_shipped_is_still_here(nodes):
    assert NODES_2_4_0 <= set(nodes)


def test_analysis_is_an_agent_node_that_proposes_a_claim(nodes):
    node = nodes[ANALYSIS_NODE]
    assert node["kind"] == "agent"
    assert node["uses"].startswith("actor://company/developer@sha256:")
    # A bridge completion proposes a claim (PRD 10.4); a node declaring no
    # propose type rejects every honest completion as contract_rejected.
    assert node["ledger"]["propose"] == ["claim"]


def test_both_routes_reach_the_analysis_before_the_fix(edges):
    """`route`/`stamp-pr` hand to `analyse`, and nothing hands to `fix` early.

    The keyed route used to go straight to `stage-dispatch`, and the orphan
    route straight to `fix`. Both now pass the analysis first, so the stage
    comment that says a developer session was dispatched is only posted when
    one actually will be.
    """
    assert _targets(edges, "route.keyed") == {ANALYSIS_NODE: ""}
    assert _targets(edges, "stamp-pr.stamped") == {ANALYSIS_NODE: ""}
    assert set(_targets(edges, "stage-dispatch.comment_posted")) == {FIX_NODE}


def test_a_packaged_analysis_reaches_the_fix_on_both_shapes(edges):
    """Keyed items take the stage node; `gh:` items go straight to `fix`.

    Same shape, and same reason, as the two guarded `fix.completed` edges:
    the engine picks the first eligible edge in the COMPILER's normalized
    order, so an unguarded sibling would win before the stage guard was ever
    evaluated.
    """
    packaged = _targets(edges, f"{ANALYSIS_NODE}.packaged")
    assert set(packaged) == {"stage-dispatch", FIX_NODE}
    assert packaged["stage-dispatch"] == '!input.work_item.startsWith("gh:")'
    assert packaged[FIX_NODE] == 'input.work_item.startsWith("gh:")'


def test_an_analysis_that_packages_nothing_ends_the_run(edges):
    """Every finding pushed back, skipped or duplicate buys no fix session."""
    assert _targets(edges, f"{ANALYSIS_NODE}.no_fix") == {END_NODE: ""}


def test_no_guarded_analysis_outcome_has_an_unguarded_sibling(edges):
    """The normalized-edge-order trap, pinned for the outcomes t14 adds."""
    by_source: dict[str, list[str]] = {}
    for edge in edges:
        if edge["from"].startswith(f"{ANALYSIS_NODE}."):
            by_source.setdefault(edge["from"], []).append(edge.get("when", ""))
    for source, guards in by_source.items():
        if len(guards) > 1:
            assert all(guards), f"{source} mixes guarded and unguarded edges"


def test_the_fix_binds_the_analysis_package_not_the_raw_findings(nodes):
    """`fix` works what the analysis packaged, not the event payload's list."""
    bindings = _bindings(nodes[FIX_NODE])
    assert bindings.get("package") == f"/nodes/{ANALYSIS_NODE}/output/packages"
    # The PR context stays -- an actor still has to know where to push -- but
    # the raw findings list is no longer the thing it selects work from.
    assert "finding" not in bindings
    assert bindings.get("pr") == "/run/input"


def test_the_fix_instruction_works_the_first_package_as_one_change(nodes):
    instruction = _bindings(nodes[FIX_NODE])["instruction"]["literal"]
    assert "package" in instruction.lower()
    assert "one change" in instruction.lower()


def test_the_analysis_instruction_states_the_deferral_rule(nodes):
    """Deferral is a stated rule, not a second dedupe implementation.

    The sweep already enforces it at head_sha level -- a finding worked at a
    commit is not re-asked until the PR moves -- so the analysis node's job
    is to say so, and to leave the rest of the file's findings alone.
    """
    instruction = _bindings(nodes[ANALYSIS_NODE])["instruction"]["literal"].lower()
    assert "re-scan" in instruction or "rescan" in instruction
    for word in ("resolved", "pushback", "duplicate", "skip"):
        assert word in instruction


def test_the_analysis_reads_the_pull_request_it_is_judging(nodes):
    assert _bindings(nodes[ANALYSIS_NODE]).get("pr") == "/run/input"


def test_the_run_output_carries_the_pushbacks(nodes):
    """The end node returns the analysis, so a pushback survives the run.

    An end node's output is a single pointer (`#/$defs/pointer`, `from`), so
    "surface the pushbacks" means the run's own output IS the analysis
    document -- verdicts, reasons and all -- which is what the tick summary
    reads back off the run listing.
    """
    assert nodes[END_NODE]["output"]["from"] == f"/nodes/{ANALYSIS_NODE}/output"


def test_every_path_to_the_end_passes_through_the_analysis(document, edges):
    """`finish` reads `/nodes/analyse/output`, so `analyse` must dominate it.

    An end node whose output pointer names a node some paths skip resolves to
    nothing on those paths. Every route into `fix` and every route into
    `finish` therefore has to cross `analyse` -- which is true because both
    of the entry decision's outcomes reach it and nothing else reaches `fix`.
    Asserted by enumeration rather than by reading the picture, because the
    picture is what stops being true when a fourteenth edge is added.
    """
    outgoing: dict[str, set[str]] = {}
    for edge in edges:
        outgoing.setdefault(edge["from"].split(".", 1)[0], set()).add(edge["to"])

    def paths(node: str, seen: frozenset) -> list[list[str]]:
        nexts = outgoing.get(node, set()) - seen
        if not nexts:
            return [[node]]
        return [[node, *rest] for n in sorted(nexts) for rest in paths(n, seen | {node, n})]

    entry = document["spec"]["entry"]
    reaching_end = [path for path in paths(entry, frozenset({entry})) if END_NODE in path]
    assert reaching_end
    for path in reaching_end:
        assert ANALYSIS_NODE in path, path
        assert path.index(ANALYSIS_NODE) < path.index(END_NODE), path


def test_every_edge_names_a_node_that_exists(nodes, edges):
    for edge in edges:
        assert edge["from"].split(".", 1)[0] in nodes, edge
        assert edge["to"] in nodes, edge


def test_every_analysis_outcome_has_an_edge(analysis_outcomes, edges):
    """An outcome with no edge fails the run with "no edge matched"."""
    routed = {e["from"].split(".", 1)[1] for e in edges if e["from"].startswith("analyse.")}
    assert routed == set(analysis_outcomes)


def test_the_document_still_validates_against_the_workflow_schema(document):
    schema = json.loads(WORKFLOW_SCHEMA.read_text(encoding="utf-8"))
    jsonschema.validate(document, schema)


# --------------------------------------------------------------------------
# The verdict contract, against the fixture PR
# --------------------------------------------------------------------------


def _packaged_schema(analysis_outcomes: dict) -> dict:
    return analysis_outcomes["packaged"]["schema"]


def test_the_declared_outcomes_are_packaged_and_no_fix(analysis_outcomes):
    assert set(analysis_outcomes) == {"packaged", "no_fix"}


def test_the_fixture_analysis_satisfies_the_declared_contract(analysis_outcomes, fixture_analysis):
    jsonschema.validate(fixture_analysis, _packaged_schema(analysis_outcomes))


def test_every_fixture_finding_gets_exactly_one_verdict(fixture_findings, fixture_analysis):
    judged = [verdict["id"] for verdict in fixture_analysis["verdicts"]]
    assert len(judged) == len(set(judged))
    assert set(judged) == {finding["id"] for finding in fixture_findings}


def test_every_verdict_is_from_the_vocabulary_with_a_one_line_reason(fixture_analysis):
    for verdict in fixture_analysis["verdicts"]:
        assert verdict["verdict"] in VERDICTS
        assert verdict["reason"].strip()
        assert "\n" not in verdict["reason"]


def test_the_resolved_thread_is_skipped(fixture_analysis, fixture_threads):
    resolved = [t["finding_id"] for t in fixture_threads["reviewThreads"] if t["isResolved"]]
    assert resolved, "the fixture must carry a resolved thread"
    by_id = {v["id"]: v for v in fixture_analysis["verdicts"]}
    for finding_id in resolved:
        assert by_id[finding_id]["verdict"] == "SKIP"


def test_the_pushed_back_thread_is_a_pushback_naming_the_owner_reply(
    fixture_analysis, fixture_threads
):
    owner = fixture_threads["owner"]
    pushed_back = [
        thread["finding_id"]
        for thread in fixture_threads["reviewThreads"]
        if not thread["isResolved"]
        and any(comment["author"] == owner for comment in thread["comments"])
    ]
    assert pushed_back, "the fixture must carry a thread the owner replied to"
    by_id = {v["id"]: v for v in fixture_analysis["verdicts"]}
    for finding_id in pushed_back:
        assert by_id[finding_id]["verdict"] == "PUSHBACK"


def test_the_thirteen_same_rule_nits_are_one_package(fixture_findings, fixture_analysis):
    nits = [f for f in fixture_findings if f.get("rule") == "python:S1192"]
    assert len(nits) == 13
    assert len({f["file"] for f in nits}) == 1
    packages = fixture_analysis["packages"]
    assert len(packages) == 1
    assert packages[0]["rule"] == "python:S1192"
    assert packages[0]["file"] == nits[0]["file"]
    assert sorted(packages[0]["finding_ids"]) == sorted(f["id"] for f in nits)


def test_only_fix_verdicts_are_packaged(fixture_analysis):
    packaged = {i for package in fixture_analysis["packages"] for i in package["finding_ids"]}
    fixed = {v["id"] for v in fixture_analysis["verdicts"] if v["verdict"] == "FIX"}
    assert packaged == fixed


@pytest.mark.parametrize(
    "mutation",
    [
        pytest.param(lambda d: d["verdicts"][0].pop("reason"), id="reason-missing"),
        pytest.param(lambda d: d["verdicts"][0].update({"reason": ""}), id="reason-empty"),
        pytest.param(
            lambda d: d["verdicts"][0].update({"reason": "no\nbecause"}), id="reason-two-lines"
        ),
        pytest.param(
            lambda d: d["verdicts"][0].update({"verdict": "MAYBE"}), id="verdict-invented"
        ),
        pytest.param(lambda d: d["verdicts"][0].pop("id"), id="id-missing"),
        pytest.param(lambda d: d.pop("packages"), id="packages-missing"),
        pytest.param(lambda d: d.update({"packages": []}), id="packages-empty"),
        pytest.param(lambda d: d["packages"][0].update({"finding_ids": []}), id="package-empty"),
    ],
)
def test_a_malformed_analysis_is_refused_by_the_node_contract(
    analysis_outcomes, fixture_analysis, mutation
):
    document = json.loads(json.dumps(fixture_analysis))
    mutation(document)
    with pytest.raises(jsonschema.ValidationError):
        jsonschema.validate(document, _packaged_schema(analysis_outcomes))


def test_the_no_fix_outcome_refuses_a_package(analysis_outcomes, fixture_analysis):
    schema = analysis_outcomes["no_fix"]["schema"]
    with pytest.raises(jsonschema.ValidationError):
        jsonschema.validate(fixture_analysis, schema)


def test_the_no_fix_outcome_refuses_a_fix_verdict(analysis_outcomes, fixture_analysis):
    """ "Nothing to fix" and "this one earns FIX" cannot both be true.

    `no_fix` is the outcome that buys no session, so a verdict list naming a
    FIX on that outcome is a run about to drop work on the floor silently.
    The enum on the outcome is what makes it `contract_rejected` instead.
    """
    schema = analysis_outcomes["no_fix"]["schema"]
    unfixable = [v for v in fixture_analysis["verdicts"] if v["verdict"] != "FIX"]
    jsonschema.validate({"verdicts": unfixable, "packages": []}, schema)
    with pytest.raises(jsonschema.ValidationError):
        jsonschema.validate({"verdicts": fixture_analysis["verdicts"], "packages": []}, schema)


# --------------------------------------------------------------------------
# The sweep half: one fact per file, held and released as one set
# --------------------------------------------------------------------------


def test_the_fact_carries_the_top_findings_whole_file(fixture_findings):
    """A bundle can only be analysed if it is dispatched together.

    Before t14 a fact carried exactly one finding, so no run ever held the
    13 nits at once and nothing could bundle them. The unit is now the FILE
    of the highest-priority finding -- still one dispatch per PR per tick,
    still not the whole PR.
    """
    package = emit.finding_package(sweep.prioritise(fixture_findings))
    assert [f["id"] for f in package] == [f["id"] for f in fixture_findings]


def test_a_finding_with_no_file_is_a_package_of_one():
    """A failed CI check names no file, so it cannot be bundled with one."""
    findings = [{"id": "check-1", "file": ""}, {"id": "check-2", "file": ""}]
    assert emit.finding_package(findings) == [findings[0]]


def test_findings_on_another_file_are_left_for_the_next_tick(fixture_findings):
    other = {"id": "other-1", "file": "internal/api/runs.go", "severity": "BLOCKER"}
    package = emit.finding_package(sweep.prioritise([*fixture_findings, other]))
    assert [f["id"] for f in package] == ["other-1"]


def test_a_dispatched_finding_is_never_also_reported_deferred(monkeypatch, capsys):
    """`deferred_findings` is a membership question, not a slice by count.

    A package's members are the findings on one file, and they are NOT
    contiguous in a priority-ordered list: a higher-severity finding on
    another file sits between them. Reporting "everything after the first N"
    as deferred therefore names findings the tick actually dispatched, and
    hides ones it did not — the summary reading backwards for exactly the
    PRs with findings across several files.
    """
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {
                "cycle": 0,
                "repositories": [{"github_repo": "owner/repo", "sonar_component": "owner_repo"}],
            }
        ),
    )
    # BLOCKER and MINOR on one file, MAJOR on another: prioritise interleaves
    # them, so the package [top, third] is not a prefix of the list.
    issues = [
        {"key": "top", "status": "OPEN", "component": "owner_repo:a.py", "severity": "BLOCKER"},
        {"key": "other", "status": "OPEN", "component": "owner_repo:b.py", "severity": "MAJOR"},
        {"key": "third", "status": "OPEN", "component": "owner_repo:a.py", "severity": "MINOR"},
    ]
    calls = _stub_sweep(
        monkeypatch,
        pulls=[{"number": 9, "head_sha": "sha9"}],
        sonar_main={"issues": []},
        sonar_pr={"issues": issues},
    )

    assert sweep.main() == 0

    upkeep = [event for event in calls["events"] if event[0] == "pr-upkeep.pr"]
    dispatched = {finding["id"] for finding in upkeep[0][1]["findings"]}
    assert dispatched == {"top", "third"}
    report = json.loads(capsys.readouterr().out)
    assert report["deferred_findings"] == ["other"]


def test_the_dedupe_holds_the_whole_bundle_when_one_member_is_in_flight(fixture_findings):
    """The bundle is one finding SET, so a partial release is not an option.

    Releasing the other twelve would mint a second run pushing the same
    change to the same file -- exactly the collision #268's cadence was
    shaped to avoid, arriving through a different door.
    """
    kept, skipped, worked = emit.undispatched_findings(fixture_findings, {"AZm412-nit07"})
    assert kept == []
    assert sorted(skipped) == sorted(f["id"] for f in fixture_findings)
    assert worked == []


def test_the_dedupe_holds_the_whole_bundle_when_one_member_was_worked(fixture_findings):
    kept, skipped, worked = emit.undispatched_findings(fixture_findings, set(), {"AZm412-nit07"})
    assert kept == []
    assert skipped == []
    assert sorted(worked) == sorted(f["id"] for f in fixture_findings)


def test_an_untouched_bundle_is_released_whole(fixture_findings):
    kept, skipped, worked = emit.undispatched_findings(fixture_findings, set(), set())
    assert kept == fixture_findings
    assert (skipped, worked) == ([], [])


def test_a_held_bundle_does_not_hold_another_file(fixture_findings):
    other = {"id": "other-1", "file": "internal/api/runs.go"}
    kept, skipped, _ = emit.undispatched_findings([*fixture_findings, other], {"AZm412-nit07"})
    assert kept == [other]
    assert "other-1" not in skipped


def test_pushbacks_are_read_off_the_run_listing(fixture_analysis):
    """The tick summary's pushback list comes from the walk it already does.

    `fetch_dispatched_findings` already follows the whole `pr-upkeep` run
    listing for the dedupe, and a run's listing row carries its `output`
    (`RunOut.Output`) -- which, since 2.5.0, IS the analysis document.
    """
    listed = {
        "items": [
            {
                "id": "01M-run-a",
                "state": "completed",
                "input": {"repository": "agentculture/culture-nodes", "head_sha": "f412aaa1"},
                "output": fixture_analysis,
            }
        ]
    }
    pushbacks = emit.pushback_findings(listed, "agentculture/culture-nodes")
    assert pushbacks == [
        {
            "id": "pr412-qodo-1",
            "reason": (
                "the PR owner replied on the thread that omitting hint from --json is "
                "deliberate machine-contract behaviour"
            ),
            "run_id": "01M-run-a",
        }
    ]


def test_pushbacks_from_another_repository_are_not_this_tick_s(fixture_analysis):
    listed = {
        "items": [
            {
                "id": "01M-run-b",
                "state": "completed",
                "input": {"repository": "someone/else"},
                "output": fixture_analysis,
            }
        ]
    }
    assert emit.pushback_findings(listed, "agentculture/culture-nodes") == []


def test_an_unreadable_run_row_does_not_stop_the_tick():
    assert emit.pushback_findings({"items": [None, {}, {"output": "not a document"}]}, "") == []


# --------------------------------------------------------------------------
# One whole tick
# --------------------------------------------------------------------------


def _sonar_payload(findings: list[dict]) -> dict:
    return {
        "issues": [
            {
                "key": finding["id"],
                "status": "OPEN",
                "component": f"agentculture_culture-nodes:{finding['file']}",
                "severity": finding["severity"],
                "type": finding["kind"],
                "rule": finding["rule"],
                "line": finding["line"],
                "message": finding["title"],
                "effort": finding["effort"],
            }
            for finding in findings
            if finding["source"] == "sonarcloud"
        ]
    }


def test_one_tick_emits_the_file_bundle_as_a_single_fact(monkeypatch, fixture_findings):
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {
                "cycle": 0,
                "repositories": [
                    {
                        "github_repo": "agentculture/culture-nodes",
                        "sonar_component": "agentculture_culture-nodes",
                    }
                ],
            }
        ),
    )
    sonar = _sonar_payload(fixture_findings)
    calls = _stub_sweep(
        monkeypatch,
        pulls=[{"number": 412, "head_sha": "f412aaa1"}],
        sonar_main={"issues": []},
        sonar_pr=sonar,
    )

    assert sweep.main() == 0

    upkeep = [event for event in calls["events"] if event[0] == "pr-upkeep.pr"]
    assert len(upkeep) == 1
    emitted = [finding["id"] for finding in upkeep[0][1]["findings"]]
    assert sorted(emitted) == sorted(
        finding["id"] for finding in fixture_findings if finding["source"] == "sonarcloud"
    )


def test_the_tick_summary_lists_pushbacks(monkeypatch, capsys, fixture_analysis):
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {
                "cycle": 0,
                "repositories": [
                    {
                        "github_repo": "agentculture/culture-nodes",
                        "sonar_component": "agentculture_culture-nodes",
                    }
                ],
            }
        ),
    )
    pushbacks = emit.pushback_findings(
        {
            "items": [
                {
                    "id": "01M-run-a",
                    "state": "completed",
                    "input": {"repository": "agentculture/culture-nodes"},
                    "output": fixture_analysis,
                }
            ]
        },
        "agentculture/culture-nodes",
    )
    _stub_sweep(
        monkeypatch,
        pulls=[{"number": 412, "head_sha": "f412aaa1"}],
        sonar_main={"issues": []},
        pushbacks=pushbacks,
    )

    assert sweep.main() == 0

    report = json.loads(capsys.readouterr().out)
    assert report["pushbacks"] == pushbacks


# --------------------------------------------------------------------------
# The documents that go stale in the same change
# --------------------------------------------------------------------------


def test_the_lane_doc_no_longer_claims_n_findings_take_n_ticks():
    assert "N findings takes N ticks" not in LANE_DOC.read_text(encoding="utf-8")


def test_the_lane_doc_states_the_bundled_cadence():
    text = LANE_DOC.read_text(encoding="utf-8")
    assert "one tick per file" in text
    assert "analyse" in text


def test_the_v1_driver_is_gone():
    """`driver.sh` posted a run whose input the 2.x contract refuses.

    It described the v1 graph (`sweep` -> `fix` -> `review` -> ... inside one
    looping run) and carried `repo`/`fix_instruction`/`review_instruction`
    keys that `additionalProperties: false` rejects. Deleted rather than
    rewritten: the loop is started by a schedule, not by a person running a
    script (spec s6).
    """
    assert not (EXAMPLE / "driver.sh").exists()
    # Naming it is fine and is the point -- the README says what was deleted
    # and why. Pointing a reader AT it is the drift: a link resolves to a 404
    # and a command line invites a shell error.
    for doc in (EXAMPLE / "README.md", LANE_DOC, EXAMPLE / "sweep-cycle.workflow.yaml"):
        text = doc.read_text(encoding="utf-8")
        assert "](driver.sh)" not in text, doc
        assert "driver.sh\n" not in text, doc
        assert "./driver.sh" not in text, doc
