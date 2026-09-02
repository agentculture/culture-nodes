"""The board-driven /think leg (task t13, issue #199 / #230; frame decisions
q1 and q4 = B of jira-flow-spec-read-related-bugs).

Three things are pinned here, matching the task's first two acceptance
items (the third — a live run on prod — is the operator's t14):

1. THE LANE WORKFLOW DOCUMENT. examples/spec-chain-lane/workflow.yaml mints
   from a ticket fact (the sweep's transition event), asks the developer
   lane for the devague moves with `devague_write: true` (the custody
   request the bridge answers from its own declaration), posts every frame
   decision as a MARKED question through the jira-comment actor, and lets
   the ENGINE — a decision node evaluating the same three clauses
   `question_correlation.correlates` evaluates — decide whether a wake is
   this run's answer before any session is billed.
2. THE STATED MOVE. A human answer — a sweep comment fact or a ticket-page
   reply fact, one schema — transacts exactly `devague question --resolve
   <qid> --decision <reply> --frame <slug>` and nothing else; a decoy
   (another question's answer, a self-originated echo) transacts nothing.
3. THE FRAME POST. post_frame.py sends devague's own frame file byte-equal
   as `frame`, with the decision token from the environment only.

The trigger half ("a fixture ticket fact mints the lane workflow") runs
through the real engine in internal/engine/specchainlane_test.go, the way
the comment consumer's own trigger test does.
"""

from __future__ import annotations

import http.server
import importlib.util
import json
import subprocess
import sys
import threading
from pathlib import Path

import jsonschema
import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]
LANE = ROOT / "examples" / "spec-chain-lane"
WORKFLOW = LANE / "workflow.yaml"
SCHEMA = ROOT / "schemas" / "events" / "jira_comment.schema.json"
DEVELOPER = "actor://company/developer@sha256:"
JIRA_COMMENT = "actor://company/jira-comment@sha256:"


def _load(name: str):
    spec = importlib.util.spec_from_file_location(name, LANE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


frame_moves = _load("frame_moves")
post_frame = _load("post_frame")


@pytest.fixture(scope="module")
def document():
    return yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))


def _nodes(document):
    return document["spec"]["nodes"]


def _bindings(node):
    return ((node.get("input") or {}).get("bindings")) or {}


# ---------------------------------------------------------------------------
# 1. the lane workflow document
# ---------------------------------------------------------------------------


def test_lane_mints_from_the_sweeps_ticket_fact_only(document):
    triggers = document["spec"]["triggers"]
    assert [t["onEvent"] for t in triggers] == ["pr-upkeep.jira.transitioned.in-progress"]
    assert 'event.payload.source == "jira"' in triggers[0]["when"]
    # The intake contract, verbatim: the trigger-created run's input IS the
    # sweep's transition fact.
    assert document["spec"]["contract"]["input"]["schema"]["required"] == [
        "source",
        "id",
        "title",
        "status",
    ]


def test_lane_contract_and_opening_actor_receive_the_ticket_description(document):
    schema = document["spec"]["contract"]["input"]["schema"]
    fact = {
        "source": "jira",
        "id": "SCRUM-6",
        "title": "Ticket description reaches agents",
        "status": "In Progress",
        "description": "Use the full ticket ask while framing the work.",
        "description_truncated": False,
    }

    jsonschema.Draft202012Validator(schema).validate(fact)
    bindings = _bindings(_nodes(document)["think"])
    assert bindings["description"] == "/run/input/description"
    assert "description" in bindings["instruction"]["literal"]


def test_every_devague_move_runs_on_the_developer_lane_with_the_custody_request(document):
    movers = {
        node_id: node
        for node_id, node in _nodes(document).items()
        if node["kind"] == "agent" and node["uses"].startswith(DEVELOPER)
    }
    assert set(movers) == {"think", "transact", "transact-2", "transact-3"}
    for node_id, node in movers.items():
        bindings = _bindings(node)
        assert bindings["devague_write"] == {"literal": True}, node_id
        assert node["ledger"]["propose"] == ["claim"], node_id
        outcomes = set(node["contract"]["outcomes"])
        assert outcomes == {"question_raised", "converged", "needs_confirmation"}, node_id
        raised = node["contract"]["outcomes"]["question_raised"]["schema"]
        required = set(raised["required"])
        assert required >= {"question_id", "question", "frame_slug", "frame_version"}
        instruction = bindings["instruction"]["literal"]
        assert "frame_moves.py" in instruction
        assert "post_frame.py" in instruction
        assert "devague confirm" in instruction  # named as the move a session never makes
        assert "NODES_ACTOR_TOKEN" in instruction


def test_no_agent_node_holds_a_stronger_authority_than_proposed(document):
    for node_id, node in _nodes(document).items():
        if node["kind"] != "agent":
            continue
        ledger = node.get("ledger") or {}
        assert ledger.get("propose") == ["claim"], node_id
        assert "observe" not in ledger, node_id
        assert "derive" not in ledger, node_id


def test_every_marked_question_is_posted_through_the_jira_actor_with_its_id(document):
    askers = {
        node_id: node
        for node_id, node in _nodes(document).items()
        if node["kind"] == "agent" and node["uses"].startswith(JIRA_COMMENT)
    }
    assert set(askers) == {"post-question", "post-question-2", "post-question-3"}
    sources = {
        "post-question": "think",
        "post-question-2": "transact",
        "post-question-3": "transact-2",
    }
    for node_id, node in askers.items():
        bindings = _bindings(node)
        assert bindings["verb"] == {"literal": "post_comment"}
        assert bindings["issue"] == "/run/input/id"
        assert bindings["question_id"] == f"/nodes/{sources[node_id]}/output/question_id"
        assert bindings["comment"] == f"/nodes/{sources[node_id]}/output/question"
    # The marker is the actor's; the document never writes one itself.
    assert "[culture-nodes:jira-actor" not in WORKFLOW.read_text(encoding="utf-8")


def test_the_engine_correlates_the_answer_before_any_session_is_billed(document):
    """The decision nodes evaluate the SAME three clauses
    question_correlation.correlates does — same question id, an answer
    present, not self-originated — over the wait node's folded event, so a
    cross-ticket wake (#239: a signal park matches (namespace, event_name)
    only) re-parks at engine cost, never at session cost."""
    nodes = _nodes(document)
    rounds = {
        "correlate": ("await-answer", "think"),
        "correlate-2": ("await-answer-2", "transact"),
        "correlate-3": ("await-answer-3", "transact-2"),
    }
    for node_id, (waiter, asker) in rounds.items():
        node = nodes[node_id]
        assert node["kind"] == "decision"
        bindings = _bindings(node)
        assert bindings["event"] == f"/nodes/{waiter}/output/event/payload"
        assert bindings["question_id"] == f"/nodes/{asker}/output/question_id"
        assert nodes[waiter] == {
            "kind": "wait",
            "ownerRef": nodes[waiter]["ownerRef"],
            "until": {"signal": "pr-upkeep.jira.comment"},
        }
        select = {port["outcome"]: port["when"] for port in node["select"]}
        assert list(select) == ["answered", "not_my_answer"]
        answered = select["answered"]
        for clause in (
            "has(output.event.originating_question_id)",
            "output.event.originating_question_id == output.question_id",
            "has(output.event.answer)",
            "!(has(output.event.self_originated) && output.event.self_originated == true)",
        ):
            assert clause in answered, (node_id, clause)
        assert select["not_my_answer"].strip() == "true"


def test_edges_route_answers_to_the_transact_and_wakes_back_to_the_park(document):
    edges = {(e["from"], e["to"]) for e in document["spec"]["edges"]}
    for n, suffix in ((1, ""), (2, "-2"), (3, "-3")):
        assert (f"correlate{suffix}.answered", f"transact{suffix}") in edges
        assert (f"correlate{suffix}.not_my_answer", f"await-answer{suffix}") in edges
        assert (f"post-question{suffix}.comment_posted", f"await-answer{suffix}") in edges
        assert (f"await-answer{suffix}.completed", f"correlate{suffix}") in edges
    # transact is reachable from its round's correlate.answered and nowhere
    # else: an uncorrelated wake cannot reach a devague move.
    for target in ("transact", "transact-2", "transact-3"):
        sources = {src for src, dst in edges if dst == target}
        assert sources == {f"correlate{target[len('transact'):]}.answered"}, target
    # Every mover's three outcomes are routed; the fourth decision reaches a
    # person, and converged/needs_confirmation end or park the run.
    assert ("think.question_raised", "post-question") in edges
    assert ("transact.question_raised", "post-question-2") in edges
    assert ("transact-2.question_raised", "post-question-3") in edges
    assert ("transact-3.question_raised", "needs-human") in edges
    for mover in ("think", "transact", "transact-2", "transact-3"):
        assert (f"{mover}.converged", "frame-converged") in edges
        assert (f"{mover}.needs_confirmation", "needs-human") in edges
    assert _nodes(document)["needs-human"]["kind"] == "approval"


def test_document_names_no_deployment_and_carries_the_deployment_heading():
    source = WORKFLOW.read_text(encoding="utf-8")
    assert "Deployment configuration" in source
    for forbidden in ("/home/", "192.168.", "http://", "https://"):
        assert forbidden not in source, forbidden


# ---------------------------------------------------------------------------
# 2. the stated move
# ---------------------------------------------------------------------------

SWEEP_FACT = {
    "source": "jira",
    "id": "SCRUM-9",
    "title": "Read related bugs before speccing",
    "status": "In Progress",
    "originating_question_id": "scrum-9.q1",
    "answer": {
        "comment_id": "10142",
        "body": "Postgres, same instance as the control plane.",
    },
}
PAGE_REPLY_FACT = {
    "id": "SCRUM-9",
    "origin": {"kind": "human", "replier": "ori"},
    "replier": "ori",
    "originating_question_id": "scrum-9.q1",
    "answer": {
        "comment_id": "ticket-page",
        "body": "Postgres, same instance as the control plane.",
    },
}
EXPECTED_MOVE = [
    "devague",
    "question",
    "--resolve",
    "q1",
    "--decision",
    "Postgres, same instance as the control plane.",
    "--frame",
    "scrum-9",
]


def test_both_answer_facts_validate_against_the_one_comment_schema():
    schema = json.loads(SCHEMA.read_text(encoding="utf-8"))
    for fact in (SWEEP_FACT, PAGE_REPLY_FACT):
        jsonschema.Draft202012Validator(schema).validate(fact)


@pytest.mark.parametrize("fact", [SWEEP_FACT, PAGE_REPLY_FACT], ids=["sweep", "page-reply"])
def test_an_answer_transacts_exactly_the_stated_move(fact):
    assert frame_moves.transact(fact, "scrum-9.q1") == EXPECTED_MOVE


@pytest.mark.parametrize(
    "decoy",
    [
        {**SWEEP_FACT, "originating_question_id": "scrum-9.q2"},
        {**SWEEP_FACT, "originating_question_id": "scrum-12.q1"},
        {**SWEEP_FACT, "self_originated": True},
        {k: v for k, v in SWEEP_FACT.items() if k != "answer"},
        {k: v for k, v in SWEEP_FACT.items() if k != "originating_question_id"},
    ],
    ids=["other-question", "other-ticket", "self-echo", "no-answer", "bare-comment"],
)
def test_a_decoy_transacts_nothing(decoy):
    assert frame_moves.transact(decoy, "scrum-9.q1") is None


def test_the_stated_move_is_a_resolve_and_never_another_verb():
    move = frame_moves.stated_move("scrum-9.q3", "  option B  ")
    assert move[:3] == ["devague", "question", "--resolve"]
    assert "confirm" not in move
    assert "capture" not in move
    assert "export" not in move
    assert move == [
        "devague",
        "question",
        "--resolve",
        "q3",
        "--decision",
        "option B",
        "--frame",
        "scrum-9",
    ]
    with pytest.raises(ValueError):
        frame_moves.stated_move("scrum-9.q3", "   ")
    with pytest.raises(ValueError):
        frame_moves.stated_move("not-a-lane-id", "x")


def test_question_ids_name_the_frame_and_the_question_and_pass_the_marker_charset():
    assert frame_moves.frame_slug("SCRUM-9") == "scrum-9"
    marker = frame_moves.question_id("scrum-9", "q1")
    assert marker == "scrum-9.q1"
    assert frame_moves.correlation.QUESTION_ID_RE.fullmatch(marker)
    assert frame_moves.split_question_id(marker) == ("scrum-9", "q1")
    with pytest.raises(ValueError):
        frame_moves.question_id("scrum-9", "c1")


def test_next_envelope_reads_devagues_own_json():
    questions = {
        "slug": "scrum-9",
        "questions": [{"id": "q1", "text": "Which store?", "resolved": False}],
    }
    blocked = {
        "ready_for_spec": False,
        "blockers": ["claim c1 has no confirmed honesty condition"],
    }
    raised = frame_moves.next_envelope("SCRUM-9", questions, blocked, 3)
    assert raised["outcome"] == "question_raised"
    assert raised["output"]["question_id"] == "scrum-9.q1"
    assert raised["output"]["frame_version"] == 3
    assert "devague question --resolve q1" in raised["output"]["question"]
    assert "[culture-nodes:jira-actor" not in raised["output"]["question"]

    resolved = {"slug": "scrum-9", "questions": [{"id": "q1", "text": "?", "resolved": True}]}
    needs = frame_moves.next_envelope("SCRUM-9", resolved, blocked, 4)
    assert needs["outcome"] == "needs_confirmation"
    assert needs["output"]["blockers"] == blocked["blockers"]

    converged = frame_moves.next_envelope("SCRUM-9", resolved, {"ready_for_spec": True}, 5)
    assert converged["outcome"] == "converged"


def test_cli_transact_exits_three_on_a_non_answer(tmp_path):
    event = tmp_path / "event.json"
    event.write_text(json.dumps(SWEEP_FACT), encoding="utf-8")
    ok = subprocess.run(
        [sys.executable, str(LANE / "frame_moves.py"), "transact"]
        + ["--question-id", "scrum-9.q1", "--event-file", str(event)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert ok.returncode == 0, ok.stderr
    assert json.loads(ok.stdout)["move"] == EXPECTED_MOVE
    decoy = subprocess.run(
        [sys.executable, str(LANE / "frame_moves.py"), "transact"]
        + ["--question-id", "scrum-9.q2", "--event-file", str(event)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert decoy.returncode == 3
    assert json.loads(decoy.stdout)["move"] is None


# ---------------------------------------------------------------------------
# 3. the frame post
# ---------------------------------------------------------------------------


class _FrameServer(http.server.BaseHTTPRequestHandler):
    seen: list[dict] = []
    status = 201

    def do_POST(self):  # noqa: N802 — http.server's own naming
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        type(self).seen.append(
            {"path": self.path, "auth": self.headers.get("Authorization"), "body": body}
        )
        payload = json.dumps(
            {
                "ticket_id": self.path.split("/")[-2],
                "version": 7,
                "frame": json.loads(body)["frame"],
            }
        ).encode("utf-8")
        self.send_response(type(self).status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        return


@pytest.fixture
def frame_server():
    _FrameServer.seen = []
    _FrameServer.status = 201
    server = http.server.HTTPServer(("127.0.0.1", 0), _FrameServer)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_address[1]}", _FrameServer
    server.shutdown()
    server.server_close()


def test_post_frame_sends_devagues_frame_file_byte_equal_with_the_granted_token(
    tmp_path, frame_server, capsys
):
    base, handler = frame_server
    frame = {
        "slug": "scrum-9",
        "claims": [{"id": "c1", "status": "confirmed"}],
        "open_vagueness": [],
    }
    frame_dir = tmp_path / ".devague" / "frames"
    frame_dir.mkdir(parents=True)
    (frame_dir / "scrum-9.json").write_text(json.dumps(frame), encoding="utf-8")

    rc = post_frame.main(
        ["--ticket", "SCRUM-9", "--repo", str(tmp_path)],
        env={"NODES_API_URL": base, "NODES_ACTOR_TOKEN": "tok-123"},
    )
    assert rc == 0
    assert len(handler.seen) == 1
    call = handler.seen[0]
    assert call["path"] == "/v1alpha1/tickets/SCRUM-9/frame"
    assert call["auth"] == "Bearer tok-123"
    sent = json.loads(call["body"])
    assert sent["frame"] == frame
    assert sent["posted_by"] == "actor://company/developer"
    out = capsys.readouterr()
    assert json.loads(out.out) == {"ticket_id": "SCRUM-9", "version": 7}
    assert "tok-123" not in out.out
    assert "tok-123" not in out.err


def test_post_frame_refuses_without_the_granted_token_and_never_calls(
    tmp_path, frame_server, capsys
):
    base, handler = frame_server
    rc = post_frame.main(
        ["--ticket", "SCRUM-9", "--repo", str(tmp_path)], env={"NODES_API_URL": base}
    )
    assert rc == 2
    assert handler.seen == []
    assert "NODES_ACTOR_TOKEN" in capsys.readouterr().err


def test_post_frame_reports_a_rejected_grant_as_a_failure(tmp_path, frame_server, capsys):
    base, handler = frame_server
    handler.status = 401
    frame_dir = tmp_path / ".devague" / "frames"
    frame_dir.mkdir(parents=True)
    (frame_dir / "scrum-9.json").write_text("{}", encoding="utf-8")
    rc = post_frame.main(
        ["--ticket", "SCRUM-9", "--repo", str(tmp_path)],
        env={"NODES_API_URL": base, "NODES_ACTOR_TOKEN": "wrong"},
    )
    assert rc == 1
    assert "401" in capsys.readouterr().err
