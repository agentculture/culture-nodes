"""Jira emitters pass the stamped `origin` (issue #328, task t38f).

A bridge embeds the cn1 marker the engine minted in the artifact it creates
(stamping.stamp_text) and reports the provider id it got back. The Jira
emitter reads the marker back out of the artifact and passes
``origin: {marker, artifact_kind, artifact_id, author, bridge_account}`` on
the neutral reaction fact, so internal/declengine/marker.go verify can
continue the firing's lineage. ``bridge_account`` comes from the emitter's
own configuration (``jira_bot_account_id``), never from the artifact, and
without one no origin is attached at all (fail closed).

The legacy ``pr-upkeep.jira.*`` facts are untouched: the system's own
comments are still self-echo there.
"""

import importlib.util
from pathlib import Path

from tests.test_pr_upkeep_stage import BOT, HUMAN, MARKER, _comment, _issue, _stage_text
from tests.test_pr_upkeep_sweep import jira

REPO = Path(__file__).resolve().parent.parent
FIRING = "01K6TCA38F0000000000000001"
HEX48, HEX64 = "a" * 48, "b" * 64
COMMENT_MARKER = f"cn1:{FIRING}:jira.comment:{HEX48}:{HEX64}"
ISSUE_MARKER = f"cn1:{FIRING}:jira.issue:{HEX48}:{HEX64}"
SITE = "team.example.com"


def _emit(issue, bot=BOT):
    return jira.jira_emissions({"issues": [issue]}, site=SITE, project="SCRUM", bot_account_id=bot)


def _named(facts, name):
    return [fact for fact in facts if fact["name"] == name]


def _stamped(text, marker):
    # What the jira bridge posts: its actor marker, then stamp_text's marker.
    return f"{text}\n\n{MARKER}\n\n{marker}"


def test_the_marker_pattern_is_stamping_pys():
    spec = importlib.util.spec_from_file_location(
        "jira_stamping", REPO / "adapters/jira/src/jira_bridge/stamping.py"
    )
    stamping = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(stamping)
    assert jira._CN1_MARKER.pattern == stamping._MARKER.pattern


def test_a_bot_comment_with_a_marker_raises_the_neutral_fact_with_origin():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [_comment("1002", "2026-09-01T09:05:00.000+0000", _stamped("Fixed.", COMMENT_MARKER))],
    )
    facts = _emit(issue)
    assert _named(facts, "pr-upkeep.jira.comment") == [], "the legacy fact stays self-echo"
    (neutral,) = _named(facts, "jira.comment")
    assert neutral["payload"]["origin"] == {
        "marker": COMMENT_MARKER,
        "artifact_kind": "jira.comment",
        "artifact_id": "1002",
        "author": BOT,
        "bridge_account": BOT,
    }
    assert neutral["source_key"] == "jira:team.example.com:SCRUM-9:comment:1002"
    assert neutral["watermark"] == {"comment_id": "1002"}
    assert neutral["subject"] == "SCRUM-9"
    assert neutral["payload"]["issue"] == "SCRUM-9"
    assert neutral["payload"]["comment_id"] == "1002"


def test_a_marked_stage_comment_is_still_a_stage_record():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [
            _comment(
                "1002",
                "2026-09-01T09:05:00.000+0000",
                f"{_stage_text('intake')}\n\n{COMMENT_MARKER}",
            )
        ],
    )
    facts = _emit(issue)
    # The intake stage record still closes the To Do creation transition.
    assert not [f for f in facts if f["name"].startswith("pr-upkeep.jira.transitioned.")]
    (neutral,) = _named(facts, "jira.comment")
    assert neutral["payload"]["origin"]["artifact_id"] == "1002"


def test_a_bot_comment_without_a_marker_raises_nothing():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [_comment("1003", "2026-09-01T09:06:00.000+0000", f"Acknowledged.\n\n{MARKER}")],
    )
    facts = _emit(issue)
    assert _named(facts, "jira.comment") == []
    assert _named(facts, "pr-upkeep.jira.comment") == []


def test_a_humans_copied_marker_names_the_human_as_author():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [
            _comment(
                "1005",
                "2026-09-01T10:00:00.000+0000",
                f"quoting the bot:\n{COMMENT_MARKER}",
                account=HUMAN,
            )
        ],
    )
    facts = _emit(issue)
    (legacy,) = _named(facts, "pr-upkeep.jira.comment")
    assert "origin" not in legacy["payload"]
    (neutral,) = _named(facts, "jira.comment")
    assert neutral["payload"]["origin"] == {
        "marker": COMMENT_MARKER,
        "artifact_kind": "jira.comment",
        "artifact_id": "1005",
        "author": HUMAN,
        "bridge_account": BOT,
    }


def test_a_human_comment_without_a_marker_raises_the_neutral_fact_without_origin():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [_comment("1001", "2026-09-01T09:00:00.000+0000", "please pick this up", account=HUMAN)],
    )
    (neutral,) = _named(_emit(issue), "jira.comment")
    assert "origin" not in neutral["payload"]
    assert neutral["payload"]["author"] == HUMAN
    assert neutral["payload"]["body"] == "please pick this up"


def test_no_configured_bot_attaches_no_origin_and_says_so(capsys):
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [
            _comment("1002", "2026-09-01T09:05:00.000+0000", _stamped("Fixed.", COMMENT_MARKER)),
            _comment(
                "1005",
                "2026-09-01T10:00:00.000+0000",
                f"copied {COMMENT_MARKER}",
                account=HUMAN,
            ),
        ],
    )
    facts = _emit(issue, bot="")
    assert all("origin" not in fact["payload"] for fact in facts)
    # The bridge's comment is still self-echo by its actor marker; the
    # person's comment is still their fact, only without an origin.
    assert [f["payload"]["comment_id"] for f in _named(facts, "jira.comment")] == ["1005"]
    assert "origin withheld" in capsys.readouterr().err


def test_the_last_valid_marker_wins():
    other = f"cn1:{FIRING}:jira.comment:{'c' * 48}:{'d' * 64}"
    text = f"upstream said {other}\ncn1:not:a:marker\n\n{MARKER}\n\n{COMMENT_MARKER}\ncn1:x:y:zz:zz"
    assert jira.last_cn1_marker(text) == COMMENT_MARKER
    assert jira.last_cn1_marker("no marker here") == ""


def test_a_transitions_marker_comment_raises_the_neutral_transition_with_origin():
    bot_move = {
        "id": "501",
        "created": "2026-09-01T09:10:00.000+0000",
        "author": {"accountId": BOT},
        "items": [{"field": "status", "fromString": "To Do", "toString": "In Progress"}],
    }
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [_comment("1006", "2026-09-01T09:10:01.000+0000", ISSUE_MARKER)],
        histories=[bot_move],
    )
    facts = _emit(issue)
    # The bot's own status change is still self-echo for both legacy and the
    # changelog-driven neutral fact; the marker comment raises the reaction.
    assert not [f for f in facts if f["name"].startswith("pr-upkeep.jira.transitioned.in")]
    stamped = [f for f in _named(facts, "jira.issue.transitioned") if "origin" in f["payload"]]
    (neutral,) = stamped
    assert neutral["payload"] == {
        "source": "jira",
        "issue": "SCRUM-9",
        "from_status": "To Do",
        "to_status": "In Progress",
        "site": SITE,
        "summary": "",  # t48 display field; this fixture issue has none
        "origin": {
            "marker": ISSUE_MARKER,
            "artifact_kind": "jira.issue",
            "artifact_id": "1006",
            "author": BOT,
            "bridge_account": BOT,
        },
    }
    assert neutral["source_key"] == (
        "jira:team.example.com:SCRUM-9:transitioned:In Progress:comment:1006"
    )
    assert _named(facts, "jira.comment") == []


def test_a_created_issue_carries_its_description_marker():
    issue = _issue("SCRUM-9", "2026-09-01T09:00:00.000+0000", [])
    issue["id"] = "10042"
    issue["fields"]["creator"] = {"accountId": BOT}
    issue["fields"]["description"] = {
        "type": "doc",
        "version": 1,
        "content": [
            {"type": "paragraph", "content": [{"type": "text", "text": "Orphan PR #7."}]},
            {"type": "paragraph", "content": [{"type": "text", "text": ISSUE_MARKER}]},
        ],
    }
    (created,) = _named(_emit(issue), "jira.issue.created")
    assert created["payload"]["origin"] == {
        "marker": ISSUE_MARKER,
        "artifact_kind": "jira.issue",
        "artifact_id": "10042",
        "author": BOT,
        "bridge_account": BOT,
    }
    del issue["fields"]["creator"]
    (created,) = _named(_emit(issue), "jira.issue.created")
    assert "origin" not in created["payload"], "no author, no origin: verify would skip it"


def test_reaction_facts_are_idempotent_on_repoll():
    issue = _issue(
        "SCRUM-9",
        "2026-09-01T09:00:00.000+0000",
        [
            _comment("1002", "2026-09-01T09:05:00.000+0000", _stamped("Fixed.", COMMENT_MARKER)),
            _comment("1001", "2026-09-01T09:00:00.000+0000", "hi", account=HUMAN),
        ],
    )
    first = [(f["source_key"], f["watermark"]) for f in _named(_emit(issue), "jira.comment")]
    again = [(f["source_key"], f["watermark"]) for f in _named(_emit(issue), "jira.comment")]
    assert first == again
    assert [key for key, _ in first] == [
        "jira:team.example.com:SCRUM-9:comment:1001",
        "jira:team.example.com:SCRUM-9:comment:1002",
    ]


def test_the_poller_fetches_the_creator():
    source = (REPO / "examples/pr-upkeep/pr_upkeep_jira.py").read_text()
    assert '"summary,description,priority,status,issuetype,created,updated,comment,creator"' in (
        source
    )
