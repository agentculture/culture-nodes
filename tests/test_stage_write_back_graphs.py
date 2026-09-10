"""The stage write-back is a GRAPH property (plan loop-closure-claude-codex
task t17; spec c9/c16, honesty h18).

`tests/test_pr_upkeep_stage.py` covers the reading half -- how the sweep parses
a stage comment into a watermark. This file covers the writing half: the loop
graphs post the stage comments, through the jira actor's `post_comment` verb,
and the sweep gains no Jira write. Everything asserted here is read out of the
example workflow documents themselves, because those documents ARE the
contract: a stage comment nobody's graph posts is a vocabulary entry, not a
behaviour.

Why a structural test and not a compiler test: `nodes workflow validate` needs
a running Go control plane, so the parts of publication a laptop can still
check -- the JSON Schema, reachability from the entry, a terminal path, and the
guard exhaustiveness the engine's NORMALIZED edge order makes load-bearing --
are checked here directly. The end-to-end proof (real engine, real trigger,
fake jira bridge) is `tests/e2e/stagewriteback_test.go`.
"""

from __future__ import annotations

import json
from pathlib import Path

import jsonschema
import pytest
import yaml

from tests.test_pr_upkeep_sweep import jira

ROOT = Path(__file__).resolve().parents[1]
EXAMPLES = ROOT / "examples"
WORKFLOW_SCHEMA = ROOT / "schemas" / "workflow" / "workflow.schema.json"

#: The jira actor every stage comment goes through. `uses:` pins a digest; the
#: registry key is the part before it (internal/worker/registry.go strips it).
JIRA_ACTOR = "actor://company/jira-comment"

#: Which graph posts which stage, and from which node. This mapping is the
#: deliverable: it says where in the loop each stage of the enum is written.
#:
#: `spec` is deliberately absent. Its writer is the spec-chain lane
#: (examples/spec-chain-lane), which is not touched by this task -- the stage
#: is in the vocabulary and documented as reserved, and no graph emits it yet.
STAGE_NODES = {
    "jira-intake": {"stage-intake": "intake"},
    "pr-upkeep": {"stage-dispatch": "dispatch", "stage-pr-open": "pr-open"},
    "cleanup": {
        "stage-merged": "merged",
        "stage-cleanup": "cleanup",
        "stage-cleanup-declined": "cleanup",
    },
}

STAGES_WITHOUT_A_WRITER = {"spec"}


def _document(graph: str) -> dict:
    return yaml.safe_load((EXAMPLES / graph / "workflow.yaml").read_text(encoding="utf-8"))


@pytest.fixture(scope="module")
def documents() -> dict[str, dict]:
    return {graph: _document(graph) for graph in STAGE_NODES}


def _nodes(document: dict) -> dict[str, dict]:
    return document["spec"]["nodes"]


def _bindings(node: dict) -> dict:
    return ((node.get("input") or {}).get("bindings")) or {}


def _stage_of(node: dict) -> str | None:
    """The stage a node's `comment` literal declares, or None.

    Read the way the SWEEP reads it -- ``jira_stage_record`` on the comment as
    the bridge will have posted it, i.e. with the jira-actor marker the bridge
    appends and this graph must not write itself. That keeps the two halves
    honest about one another: a literal this parser cannot read is a stage
    comment the watermark would never see.
    """
    comment = _bindings(node).get("comment")
    if not isinstance(comment, dict) or "literal" not in comment:
        return None
    as_posted = f"{comment['literal']}\n\n[{jira.JIRA_ACTOR_MARKER}]"
    record = jira.jira_stage_record(
        {"id": "1", "created": "2026-09-01T00:00:00.000+0000", "body": as_posted}, ""
    )
    return record["stage"] if record else None


def _found_stage_nodes(document: dict) -> dict[str, str]:
    found = {}
    for node_id, node in _nodes(document).items():
        stage = _stage_of(node)
        if stage is not None:
            found[node_id] = stage
    return found


def _edges(document: dict) -> list[dict]:
    return document["spec"]["edges"]


class TestTheStageVocabularyHasWriters:
    def test_each_graph_posts_exactly_the_stages_it_owns(self, documents):
        for graph, want in STAGE_NODES.items():
            assert _found_stage_nodes(documents[graph]) == want, graph

    def test_every_stage_in_the_enum_is_written_by_a_graph_or_named_as_reserved(self, documents):
        written = {
            stage
            for graph in STAGE_NODES
            for stage in _found_stage_nodes(documents[graph]).values()
        }
        assert written | STAGES_WITHOUT_A_WRITER == set(jira.STAGES)
        # The gap is pinned, not hidden: a stage nobody writes is a claim the
        # board reader cannot check, so it is listed here and in the docs.
        assert written.isdisjoint(STAGES_WITHOUT_A_WRITER)

    def test_the_stages_the_sweep_gates_on_are_all_written(self, documents):
        written = {
            stage
            for graph in STAGE_NODES
            for stage in _found_stage_nodes(documents[graph]).values()
        }
        # Whatever the sweep closes a transition on, some graph must be able
        # to record -- otherwise the entry can never fire.
        assert set(jira.STAGE_DRIVEN_BY.values()) <= written


class TestEveryStageNodeIsTheJiraActorsNarrowWrite:
    def test_the_node_shape_is_the_bridges_exact_key_post_comment(self, documents):
        for graph, stages in STAGE_NODES.items():
            nodes = _nodes(documents[graph])
            for node_id in stages:
                node = nodes[node_id]
                assert node["kind"] == "agent", node_id
                assert node["uses"].startswith(JIRA_ACTOR + "@sha256:"), node_id
                # PRD 10.4: a bridge completion proposes a claim; a node that
                # declares no propose type rejects every honest completion.
                assert node["ledger"] == {"propose": ["claim"]}, node_id
                bindings = _bindings(node)
                # adapters/jira post_comment.parse admits exactly these keys
                # (question_id optional, and a stage comment answers nothing).
                assert set(bindings) == {"verb", "issue", "comment"}, node_id
                assert bindings["verb"] == {"literal": "post_comment"}, node_id
                assert isinstance(bindings["issue"], str), node_id
                assert bindings["issue"].startswith("/run/input/"), node_id
                assert node["contract"]["outcomes"]["comment_posted"]["schema"]["required"] == [
                    "issue",
                    "comment_id",
                ], node_id

    def test_a_retried_stage_comment_is_allowed_but_a_failed_one_never_stalls_the_loop(
        self, documents
    ):
        # Unlike intake-orphan (maxAttempts 1, because a retried create is a
        # second ticket), a retried comment is at worst a duplicate record and
        # the newest one wins.
        for graph, stages in STAGE_NODES.items():
            for node_id in stages:
                policy = _nodes(documents[graph])[node_id]["policy"]
                assert policy["retry"]["maxAttempts"] == 2, node_id
                assert policy["timeout"] == "10m", node_id

    def test_the_comment_is_a_literal_whose_first_line_is_the_machine_readable_one(self, documents):
        for graph, stages in STAGE_NODES.items():
            for node_id, stage in stages.items():
                literal = _bindings(_nodes(documents[graph])[node_id])["comment"]["literal"]
                first, _, rest = literal.partition("\n")
                assert first == f"{jira.STAGE_LINE_PREFIX}{stage}", node_id
                # A person reads the board too: the machine line is followed by
                # at least one sentence of prose.
                assert rest.strip(), node_id

    def test_no_graph_writes_the_actors_own_marker(self):
        # The bridge stamps [culture-nodes:jira-actor] itself; a graph that
        # wrote one would be forging the identity the self-echo filter trusts.
        for graph in STAGE_NODES:
            text = (EXAMPLES / graph / "workflow.yaml").read_text(encoding="utf-8")
            assert "[culture-nodes:jira-actor" not in text, graph

    def test_one_comment_per_stage_per_run_is_a_declared_limit(self, documents):
        for graph in STAGE_NODES:
            assert documents[graph]["spec"]["limits"]["maxVisitsPerNode"] == 1, graph


class TestAGhKeyedRunPostsNoStage:
    """The guard: only a Jira-shaped work item can carry a stage.

    A `gh:<owner>/<repo>#<n>` work item has no ticket to comment on, so every
    path into a stage node is gated on the item's shape -- either by the
    edge's own CEL `when`, or by the decision node whose outcome the edge
    leaves, which tested the same thing.
    """

    @staticmethod
    def _decision_tests(document: dict) -> dict[str, str]:
        """Node id -> every `when` its select ports declare, joined.

        A decision node's ports are read together, not one at a time: the
        `keyed` port that reaches pr-upkeep's stage node is unconditional
        precisely BECAUSE its sibling `orphan` port took the gh: shape first,
        and first-match-wins over a declared port order is what makes that
        sound (the compiler enforces the ordering).
        """
        tests = {}
        for node_id, node in _nodes(document).items():
            if node.get("kind") != "decision":
                continue
            tests[node_id] = " ".join(port.get("when", "") for port in node.get("select", []))
        return tests

    def test_every_path_into_a_stage_node_tests_the_work_items_shape(self, documents):
        """Every path in names the field the node binds, and establishes that
        it holds a Jira key -- one of exactly two ways:

        * `startsWith("gh:")`, the shape test proper, for a field that may
          carry the transient GitHub form (`work_item`, and jira-intake's
          `id`); or
        * `has(... issue_key)`, for the one field that cannot carry it: a
          pr.merged fact's `issue_key` is the CORRELATED Jira key and a merged
          PR that correlates to no ticket produces no fact at all
          (pr_upkeep_emit.merged_pr_fact), so its presence IS the shape.
        """
        for graph, stages in STAGE_NODES.items():
            document = documents[graph]
            decisions = self._decision_tests(document)
            incoming = [edge for edge in _edges(document) if edge["to"] in stages]
            assert incoming, graph
            for edge in incoming:
                where = f"{graph}: {edge['from']} -> {edge['to']}"
                field = _bindings(_nodes(document)[edge["to"]])["issue"].rsplit("/", 1)[-1]
                guard = edge.get("when", "") or decisions.get(edge["from"].split(".", 1)[0], "")
                assert field in guard, where
                established = 'startsWith("gh:")' in guard or "has(" in guard
                assert established, where

    def test_a_guarded_outcome_has_no_unguarded_sibling(self, documents):
        # The engine selects the first eligible edge in the COMPILER's
        # normalized order (source, outcome, target, guard text), not in the
        # order the document lists them (internal/compiler/normalize.go
        # normalizeEdges). So an unguarded sibling edge whose target happens
        # to sort first wins before any guard is evaluated -- which is exactly
        # how a stage node would be silently skipped. Mutually exclusive
        # guards on every sibling is the only shape that does not depend on
        # how the targets happen to be named.
        for graph in STAGE_NODES:
            grouped: dict[str, list[dict]] = {}
            for edge in _edges(documents[graph]):
                grouped.setdefault(edge.get("from", edge.get("onEvent", "")), []).append(edge)
            for source, edges in grouped.items():
                if len(edges) < 2:
                    continue
                unguarded = [edge for edge in edges if not edge.get("when")]
                assert not unguarded, f"{graph}: {source} has {len(edges)} edges, one unguarded"


class TestTheDocumentsStillPublish:
    """What a laptop can check of publication without the Go control plane."""

    def test_each_document_validates_against_the_workflow_schema(self, documents):
        schema = json.loads(WORKFLOW_SCHEMA.read_text(encoding="utf-8"))
        for graph in STAGE_NODES:
            jsonschema.validate(documents[graph], schema)

    @staticmethod
    def _adjacency(document: dict) -> dict[str, set[str]]:
        adjacency: dict[str, set[str]] = {node: set() for node in _nodes(document)}
        for edge in _edges(document):
            source = edge.get("from", "").split(".", 1)[0]
            if source in adjacency:
                adjacency[source].add(edge["to"])
        return adjacency

    def test_every_node_is_reachable_from_the_entry_and_reaches_an_end(self, documents):
        for graph in STAGE_NODES:
            document = documents[graph]
            nodes, adjacency = _nodes(document), self._adjacency(document)
            seen, frontier = set(), [document["spec"]["entry"]]
            while frontier:
                node = frontier.pop()
                if node in seen:
                    continue
                seen.add(node)
                frontier.extend(adjacency.get(node, ()))
            assert seen == set(nodes), f"{graph}: unreachable {set(nodes) - seen}"
            ends = {node for node, body in nodes.items() if body["kind"] == "end"}
            for node in nodes:
                walked, walking = set(), [node]
                while walking:
                    at = walking.pop()
                    if at in walked:
                        continue
                    walked.add(at)
                    walking.extend(adjacency.get(at, ()))
                assert walked & ends, f"{graph}: {node} reaches no end node"

    def test_every_declared_outcome_of_a_stage_node_is_routed(self, documents):
        for graph, stages in STAGE_NODES.items():
            document = documents[graph]
            routed = {edge["from"] for edge in _edges(document)}
            for node_id in stages:
                for outcome in _nodes(document)[node_id]["contract"]["outcomes"]:
                    assert f"{node_id}.{outcome}" in routed, f"{graph}: {node_id}.{outcome}"

    def test_the_longest_path_fits_the_declared_transition_bound(self, documents):
        for graph in STAGE_NODES:
            document = documents[graph]
            adjacency = self._adjacency(document)

            # No cycles in these three graphs, so the longest simple path is
            # the depth the transition bound has to cover.
            def depth(node: str, seen: frozenset[str]) -> int:
                nexts = [n for n in adjacency.get(node, ()) if n not in seen]
                if not nexts:
                    return 0
                return 1 + max(depth(n, seen | {n}) for n in nexts)

            entry = document["spec"]["entry"]
            longest = depth(entry, frozenset({entry}))
            bound = document["spec"]["limits"]["maxTransitions"]
            assert longest <= bound, f"{graph}: longest path {longest} > maxTransitions {bound}"


class TestTheGraphChangeIsPublishable:
    def test_each_touched_graph_declares_a_bumped_version(self, documents):
        # A published workflow is immutable and pinned by digest: a node added
        # to a graph that keeps its version number is a definition nobody can
        # roll forward to. These are the versions this task publishes -- and
        # pr-upkeep moved on twice more in the same wave (2.5.0, the `analyse`
        # node of task t14; 2.6.0, the `readiness` collector of task t18),
        # which is the rule holding, not breaking.
        assert documents["pr-upkeep"]["metadata"]["version"] == "2.6.0"
        assert documents["cleanup"]["metadata"]["version"] == "1.1.0"
        assert documents["jira-intake"]["metadata"]["version"] == "1.8.0"

    def test_the_pr_upkeep_graph_keeps_its_existing_nodes(self, documents):
        nodes = set(_nodes(documents["pr-upkeep"]))
        assert {"route", "intake-orphan", "stamp-pr", "fix", "human-merges-pr", "finish"} <= nodes

    def test_the_cleanup_graph_keeps_its_code_node_and_both_endings(self, documents):
        nodes = _nodes(documents["cleanup"])
        assert nodes["cleanup"]["kind"] == "code"
        assert nodes["cleaned"]["kind"] == "end" and nodes["cleanup-failed"]["kind"] == "end"
        assert {"pr.merged", "pr.closed"} == {
            trigger["onEvent"] for trigger in documents["cleanup"]["spec"]["triggers"]
        }


class TestTheStageVocabularyIsDocumented:
    """The acceptance criterion: the vocabulary is written down where the two
    audiences read -- the board reader (drive-from-jira) and the operator
    (the lane recipe)."""

    DOCS = (
        ROOT / "docs" / "drive-from-jira.md",
        ROOT / "docs" / "operations" / "pr-upkeep-lane.md",
    )

    @pytest.mark.parametrize("doc", DOCS, ids=lambda path: path.name)
    def test_the_doc_names_every_stage_and_the_machine_readable_line(self, doc):
        text = doc.read_text(encoding="utf-8")
        assert jira.STAGE_LINE_PREFIX in text, doc.name
        for stage in jira.STAGES:
            assert f"{jira.STAGE_LINE_PREFIX}{stage}" in text or f"`{stage}`" in text, stage

    @pytest.mark.parametrize("doc", DOCS, ids=lambda path: path.name)
    def test_the_doc_says_a_stage_with_no_writer_has_none(self, doc):
        text = doc.read_text(encoding="utf-8")
        for stage in STAGES_WITHOUT_A_WRITER:
            assert stage in text, stage
        assert "watermark" in text, doc.name
