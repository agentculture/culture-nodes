"""The Jira stage watermark (plan loop-closure-claude-codex task t17; spec c9,
c16, c42; honesty h18/h10).

A stage comment is posted on the ticket BY A GRAPH NODE through the jira
actor's ``post_comment`` verb (never by the sweep), with a machine-readable
first line ``culture-nodes:stage=<stage>``. The sweep READS the latest such
comment as the ticket's stage watermark and emits nothing for a transition
that watermark already records. ``pr_upkeep_jira.py`` stays GET-only.
"""

import json

import pytest

from tests.test_pr_upkeep_sweep import _stub_sweep, jira, sweep

BOT = "bot-account-1"
HUMAN = "human-account-7"
MARKER = "[culture-nodes:jira-actor]"


def _comment(comment_id, created, text, account=BOT, adf=True):
    body = (
        {
            "type": "doc",
            "version": 1,
            "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}],
        }
        if adf
        else text
    )
    return {"id": comment_id, "created": created, "author": {"accountId": account}, "body": body}


def _stage_text(stage, sentence="One human sentence about this transition.", attrs=""):
    # Exactly what the bridge posts: the node's literal, then the actor marker
    # adapters/jira mapping.Comment.marked_text appends.
    first = f"culture-nodes:stage={stage}" + (f" {attrs}" if attrs else "")
    return f"{first}\n{sentence}\n\n{MARKER}"


@pytest.fixture
def stage_comments():
    """A ticket's comment list as Jira returns it: a human comment, a stage
    comment, an ordinary self-echo, a human quoting the prefix, then the
    newest stage comment."""
    return [
        _comment("1001", "2026-09-01T09:00:00.000+0000", "please pick this up", account=HUMAN),
        _comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("intake")),
        _comment("1003", "2026-09-01T09:06:00.000+0000", f"Acknowledged.\n\n{MARKER}"),
        _comment(
            "1004",
            "2026-09-01T10:00:00.000+0000",
            "culture-nodes:stage=merged is what I would like to see",
            account=HUMAN,
        ),
        _comment(
            "1005",
            "2026-09-02T08:00:00.000+0000",
            _stage_text("pr-open", attrs="work_item=SCRUM-9 ref=01ARZ3NDEKTSV4RRFFQ69G5FAV"),
        ),
    ]


class TestStageRecordParsing:
    def test_stage_vocabulary_is_the_six_stages_in_order(self):
        assert jira.STAGES == ("intake", "spec", "dispatch", "pr-open", "merged", "cleanup")

    def test_a_stage_comment_parses_its_first_line(self, stage_comments):
        record = jira.jira_stage_record(stage_comments[4], BOT)
        assert record == {
            "stage": "pr-open",
            "comment_id": "1005",
            "recorded_at": "2026-09-02T08:00:00.000+0000",
            "work_item": "SCRUM-9",
            "ref": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
        }

    def test_the_optional_tokens_are_optional(self, stage_comments):
        record = jira.jira_stage_record(stage_comments[1], BOT)
        assert record == {
            "stage": "intake",
            "comment_id": "1002",
            "recorded_at": "2026-09-01T09:05:00.000+0000",
        }

    def test_plain_string_bodies_parse_too(self):
        comment = _comment("1", "2026-09-01T00:00:00Z", _stage_text("dispatch"), adf=False)
        assert jira.jira_stage_record(comment, BOT)["stage"] == "dispatch"

    def test_an_ordinary_self_echo_is_not_a_stage_record(self, stage_comments):
        assert jira.jira_stage_record(stage_comments[2], BOT) is None

    def test_a_human_quoting_the_prefix_is_a_persons_comment_not_a_stage_record(
        self, stage_comments
    ):
        # s14's lesson: a configured account id is authoritative, so a person
        # cannot move the watermark by typing the prefix.
        assert jira.jira_stage_record(stage_comments[3], BOT) is None

    def test_an_unknown_stage_word_is_ignored(self):
        comment = _comment("1", "2026-09-01T00:00:00Z", _stage_text("shipped"))
        assert jira.jira_stage_record(comment, BOT) is None

    def test_without_an_account_id_the_actor_marker_is_the_fallback(self):
        stage = _comment("1", "2026-09-01T00:00:00Z", _stage_text("dispatch"), account="")
        unmarked = _comment("2", "2026-09-01T00:00:00Z", "culture-nodes:stage=dispatch", account="")
        assert jira.jira_stage_record(stage, "")["stage"] == "dispatch"
        assert jira.jira_stage_record(unmarked, "") is None


class TestStageWatermark:
    def test_latest_stage_comment_wins_and_non_stage_comments_are_ignored(self, stage_comments):
        mark = jira.jira_stage_watermark(stage_comments, BOT)
        assert mark["stage"] == "pr-open"
        assert mark["comment_id"] == "1005"

    def test_order_of_arrival_does_not_matter(self, stage_comments):
        mark = jira.jira_stage_watermark(list(reversed(stage_comments)), BOT)
        assert mark["comment_id"] == "1005"

    def test_a_ticket_with_no_stage_comment_has_no_watermark(self, stage_comments):
        assert jira.jira_stage_watermark(stage_comments[:1], BOT) is None
        assert jira.jira_stage_watermark([], BOT) is None

    def test_watermarks_are_read_per_ticket_from_the_search_payload(self, stage_comments):
        payload = {
            "issues": [
                {"key": "SCRUM-9", "fields": {"comment": {"comments": stage_comments}}},
                {"key": "SCRUM-10", "fields": {"comment": {"comments": stage_comments[:1]}}},
            ]
        }
        marks = jira.jira_stage_watermarks(payload, BOT)
        assert set(marks) == {"SCRUM-9"}
        assert marks["SCRUM-9"]["stage"] == "pr-open"


class TestStageAlreadyRecorded:
    """Which sweep fact drives which stage, and when the record closes it."""

    def test_the_driving_facts_are_the_ticket_lifecycle_facts(self):
        assert jira.STAGE_DRIVEN_BY == {
            "pr-upkeep.jira.transitioned.to-do": "intake",
            "pr.merged": "merged",
            "pr.closed": "cleanup",
        }
        # Finding dispatch is deliberately NOT stage-gated: a stage comment
        # cannot name a head or a finding, and the lane promises a finding is
        # not blocked by the run before it (docs/operations/pr-upkeep-lane.md).
        assert "pr-upkeep.pr" not in jira.STAGE_DRIVEN_BY

    @staticmethod
    def _mark(stage, at="2026-09-02T08:00:00.000+0000"):
        return {"stage": stage, "comment_id": "1", "recorded_at": at}

    def test_a_stage_at_or_beyond_the_driven_one_recorded_after_the_fact_closes_it(self):
        assert jira.stage_already_recorded(
            "pr.merged", self._mark("merged"), "2026-09-01T00:00:00Z"
        )
        assert jira.stage_already_recorded(
            "pr.merged", self._mark("cleanup"), "2026-09-01T00:00:00Z"
        )
        assert jira.stage_already_recorded(
            "pr-upkeep.jira.transitioned.to-do", self._mark("pr-open"), "2026-09-01T00:00:00Z"
        )

    def test_an_earlier_stage_does_not_close_a_later_transition(self):
        assert not jira.stage_already_recorded(
            "pr.merged", self._mark("pr-open"), "2026-09-01T00:00:00Z"
        )

    def test_a_record_older_than_the_fact_does_not_close_it(self):
        # A second PR for the ticket merges after the first one's cleanup was
        # recorded: the new merge is a new transition.
        assert not jira.stage_already_recorded(
            "pr.merged",
            self._mark("cleanup", at="2026-09-01T00:00:00.000+0000"),
            "2026-09-03T00:00:00Z",
        )

    def test_mixed_timestamp_formats_compare_as_instants_not_strings(self):
        # Jira renders +0000, GitHub renders Z; same instant either way.
        assert jira.stage_already_recorded(
            "pr.merged",
            self._mark("merged", at="2026-09-02T08:00:00.000+0000"),
            "2026-09-02T08:00:00Z",
        )
        assert jira.stage_already_recorded(
            "pr.merged",
            self._mark("merged", at="2026-09-02T11:00:00.000+0300"),
            "2026-09-02T08:00:00Z",
        )

    def test_no_watermark_or_an_ungated_fact_is_never_closed(self):
        assert not jira.stage_already_recorded("pr.merged", None, "2026-09-01T00:00:00Z")
        assert not jira.stage_already_recorded("pr-upkeep.pr", self._mark("pr-open"), "")
        assert not jira.stage_already_recorded("pr.opened", self._mark("cleanup"), "")

    def test_an_unreadable_fact_time_falls_back_to_the_stage_alone(self):
        assert jira.stage_already_recorded("pr.merged", self._mark("merged"), "")


def _issue(key, created, comments, histories=()):
    return {
        "key": key,
        "fields": {
            "created": created,
            "status": {"name": "To Do"},
            "comment": {"comments": list(comments)},
        },
        "changelog": {"histories": list(histories)},
    }


def _to_do_history(history_id, created, account=HUMAN):
    return {
        "id": history_id,
        "created": created,
        "author": {"accountId": account},
        "items": [{"field": "status", "fromString": "In Progress", "toString": "To Do"}],
    }


class TestHistoryFactsHonourTheStageWatermark:
    def test_a_to_do_transition_recorded_by_a_later_intake_stage_is_not_replayed(self):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("intake"))],
        )
        names = [fact[0] for fact in jira.jira_history_facts(issue, BOT)]
        assert names == []

    def test_a_stage_comment_is_a_record_not_a_comment_fact(self):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("dispatch"))],
        )
        names = [fact[0] for fact in jira.jira_history_facts(issue, BOT)]
        assert jira.JIRA_COMMENT_EVENT_NAME not in names
        # dispatch is beyond intake, so the creation transition is closed too.
        assert names == []

    def test_a_re_triage_after_the_stage_comment_fires_again(self):
        # Moving a ticket back to To Do is a human's re-triage signal
        # (jira-intake decision c24); a stage recorded BEFORE it cannot close it.
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("pr-open"))],
            histories=[_to_do_history("501", "2026-09-03T09:00:00.000+0000")],
        )
        facts = jira.jira_history_facts(issue, BOT)
        assert [fact[0] for fact in facts] == ["pr-upkeep.jira.transitioned.to-do"]
        assert facts[0][1]["changelog_id"] == "501"

    def test_a_humans_comment_with_the_prefix_stays_a_humans_fact(self):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [
                _comment(
                    "1004",
                    "2026-09-01T10:00:00.000+0000",
                    "culture-nodes:stage=merged?",
                    account=HUMAN,
                )
            ],
        )
        names = [fact[0] for fact in jira.jira_history_facts(issue, BOT)]
        assert names == ["pr-upkeep.jira.transitioned.to-do", jira.JIRA_COMMENT_EVENT_NAME]

    def test_the_position_watermark_still_advances_past_a_stage_comment(self):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("intake"))],
        )
        assert jira.jira_watermark(issue)["comment_id"] == "1002"


GRANT = {
    "cycle": 0,
    "repositories": [
        {
            "github_repo": "owner.example/repo",
            "sonar_component": "owner_repo",
            "jira_site": "team.example.com",
            "jira_project": "SCRUM",
            "jira_bot_account_id": BOT,
        }
    ],
}


def _merged_pull(number, key, merged_at):
    return {
        "number": number,
        "state": "closed",
        "merged_at": merged_at,
        "closed_at": merged_at,
        "html_url": f"https://github.example/owner/repo/pull/{number}",
        "head": {"ref": f"{key}/fix", "sha": "f" * 40},
    }


def _tick(monkeypatch, *, pulls, closed, issues, sonar_pr=None):
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", json.dumps(GRANT))
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
    monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
    calls = _stub_sweep(monkeypatch, pulls=pulls, sonar_main={"issues": []}, sonar_pr=sonar_pr)
    monkeypatch.setattr(sweep, "fetch_closed_pulls", lambda token, repository: list(closed))
    monkeypatch.setattr(sweep, "fetch_jira_issues", lambda *_args: {"issues": list(issues)})
    return calls


class TestATickAgainstARecordedStage:
    def test_a_merged_pr_whose_ticket_already_records_cleanup_emits_no_pr_merged(
        self, monkeypatch, capsys
    ):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [
                _comment("1002", "2026-09-01T09:05:00.000+0000", _stage_text("intake")),
                _comment("1010", "2026-09-04T12:00:00.000+0000", _stage_text("merged")),
                _comment("1011", "2026-09-04T12:01:00.000+0000", _stage_text("cleanup")),
            ],
        )
        calls = _tick(
            monkeypatch,
            pulls=[],
            closed=[_merged_pull(41, "SCRUM-9", "2026-09-04T11:00:00Z")],
            issues=[issue],
        )
        assert sweep.main() == 0
        capsys.readouterr()
        assert [event[0] for event in calls["events"]] == []

    def test_a_ticket_at_pr_open_emits_nothing_for_its_intake_transition(self, monkeypatch, capsys):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1005", "2026-09-02T08:00:00.000+0000", _stage_text("pr-open"))],
        )
        calls = _tick(monkeypatch, pulls=[], closed=[], issues=[issue])
        assert sweep.main() == 0
        capsys.readouterr()
        assert calls["events"] == []

    def test_a_merge_after_the_recorded_stage_is_a_new_transition_and_is_emitted(
        self, monkeypatch, capsys
    ):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1005", "2026-09-02T08:00:00.000+0000", _stage_text("pr-open"))],
        )
        calls = _tick(
            monkeypatch,
            pulls=[],
            closed=[_merged_pull(41, "SCRUM-9", "2026-09-04T11:00:00Z")],
            issues=[issue],
        )
        assert sweep.main() == 0
        capsys.readouterr()
        assert [event[0] for event in calls["events"]] == ["pr.merged"]

    def test_finding_dispatch_is_not_held_back_by_the_ticket_stage(self, monkeypatch, capsys):
        # The lane's promise: a finding is not blocked by the run before it.
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1005", "2026-09-02T08:00:00.000+0000", _stage_text("pr-open"))],
        )
        sonar_pr = {
            "issues": [
                {
                    "key": "AZ1",
                    "severity": "MAJOR",
                    "type": "BUG",
                    "component": "owner_repo:internal/x.go",
                    "line": 3,
                    "message": "a finding",
                    "status": "OPEN",
                }
            ]
        }
        calls = _tick(
            monkeypatch,
            pulls=[{"number": 7, "head_sha": "a" * 40, "head": {"ref": "SCRUM-9/fix"}, "body": ""}],
            closed=[],
            issues=[issue],
            sonar_pr=sonar_pr,
        )
        assert sweep.main() == 0
        capsys.readouterr()
        upkeep = [event for event in calls["events"] if event[0] == "pr-upkeep.pr"]
        assert len(upkeep) == 1 and upkeep[0][1]["work_item"] == "SCRUM-9"

    def test_a_replayed_tick_emits_nothing_new(self, monkeypatch, capsys):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [
                _comment("1010", "2026-09-04T12:00:00.000+0000", _stage_text("merged")),
                _comment("1011", "2026-09-04T12:01:00.000+0000", _stage_text("cleanup")),
            ],
        )
        calls = _tick(
            monkeypatch,
            pulls=[],
            closed=[_merged_pull(41, "SCRUM-9", "2026-09-04T11:00:00Z")],
            issues=[issue],
        )
        assert sweep.main() == 0
        assert sweep.main() == 0
        capsys.readouterr()
        assert calls["events"] == []


class TestTheSweepGainsNoJiraWrite:
    def test_pr_upkeep_jira_is_still_get_only(self):
        from tests.test_pr_upkeep_sweep import EXAMPLE_DIR

        source = (EXAMPLE_DIR / "pr_upkeep_jira.py").read_text()
        assert "stage" in source
        for forbidden in ['method="POST"', '/comment"', "post_comment(", "v1alpha1/events"]:
            assert forbidden not in source, forbidden

    def test_stage_comments_are_posted_by_graph_nodes_not_the_sweep(self):
        from tests.test_pr_upkeep_sweep import EXAMPLE_DIR

        assert "culture-nodes:stage=" not in (EXAMPLE_DIR / "sweep.py").read_text()
        for graph in ("pr-upkeep", "cleanup", "jira-intake"):
            text = (EXAMPLE_DIR.parent / graph / "workflow.yaml").read_text()
            assert "culture-nodes:stage=" in text, graph
