"""The Jira stage record (plan loop-closure-claude-codex task t17; spec c9,
c16, c42; honesty h18/h10).

A stage comment is posted on the ticket BY A GRAPH NODE through the jira
actor's ``post_comment`` verb (never by the sweep), with a machine-readable
first line ``culture-nodes:stage=<stage>``. The sweep READS those comments as
stage RECORDS: a record later on the ticket's timeline closes the To Do
transition it records, so pickup does not re-fire every tick.

It closes nothing else. A stage comment names a TICKET and cannot name the
pull request it was posted for, and two pull requests citing one ticket is an
admitted case, so ``pr.merged`` / ``pr.closed`` are emitted unconditionally
and deduplicated by the control plane on their own source key.
``pr_upkeep_jira.py`` stays GET-only.
"""

import json
import urllib.error

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


class TestStageDrivenBy:
    """Which sweep fact a stage record may close -- and which it may not."""

    def test_the_only_driven_fact_is_the_pickup_transition(self):
        assert jira.STAGE_DRIVEN_BY == {"pr-upkeep.jira.transitioned.to-do": "intake"}

    def test_no_pull_request_keyed_fact_is_stage_gated(self):
        # A stage comment names a TICKET; the jira actor's post_comment takes
        # exactly {verb, issue, comment, question_id} and a graph binding is a
        # pointer OR a literal, so no node can write `pr=<number>` into the
        # first line. Two PRs on one ticket is admitted, so a record about PR
        # A must never answer for PR B's merge.
        for name in ("pr.merged", "pr.closed", "pr.opened", "pr-upkeep.pr"):
            assert name not in jira.STAGE_DRIVEN_BY, name

    def test_the_gate_helpers_the_lifecycle_facts_used_are_gone(self):
        # Not merely unused: absent, so no caller can re-introduce the
        # suppression the two-PR case measured.
        for gone in ("stage_already_recorded", "jira_stage_watermark", "jira_stage_watermarks"):
            assert not hasattr(jira, gone), gone


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
    def test_a_merged_pr_whose_ticket_already_records_cleanup_still_emits_pr_merged(
        self, monkeypatch, capsys
    ):
        """A recorded `cleanup` says something about A ticket, not about THIS
        pull request, so it cannot answer for the merge. The fact goes out on
        its own source_key and the control plane deduplicates it."""
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
        assert [event[0] for event in calls["events"]] == ["pr.merged"]

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
        assert len(upkeep) == 1
        assert upkeep[0][1]["work_item"] == "SCRUM-9"

    def test_a_replayed_tick_re_emits_the_same_source_key_and_watermark(self, monkeypatch, capsys):
        """What makes a replay a no-op is the control plane, not the sweep.

        Both lifecycle facts are re-emitted every tick by design: identical
        `source_key` and an immutable timestamp watermark, which the signal
        watermark row answers with `duplicate=true`. The ticket's Jira facts
        stay silent -- there the stage record IS a position on the same
        timeline, so it can close the transition it records."""
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
        assert [event[0] for event in calls["events"]] == ["pr.merged", "pr.merged"]
        first, second = calls["events"]
        assert first[2] == second[2] == "github:owner.example/repo:pr:41:merged"
        assert first[3] == second[3] == {"merged_at": "2026-09-04T11:00:00Z"}


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


class TestLifecycleFactsAreNeverStageGated:
    """A stage comment names a TICKET, never a pull request.

    The jira bridge's ``post_comment`` takes exactly
    ``{verb, issue, comment, question_id}`` and a graph binding is a pointer
    OR a literal — never a composition — so no node can write ``pr=<number>``
    into the structured first line. Two pull requests citing one ticket is an
    admitted case (docs/operations/pr-upkeep-lane.md), so a stage record can
    never be allowed to answer for a ``pr.merged`` / ``pr.closed`` fact it may
    not be about. Both facts carry their own ``source_key`` plus an immutable
    timestamp watermark and are deduplicated by the control plane.
    """

    def test_a_pr_merged_before_another_prs_stage_comment_still_emits(self, monkeypatch, capsys):
        # PR #41 merged 11:00 and the cleanup node posted `merged` at 12:00.
        # PR #42 merged 11:30 on the SAME ticket but was only listed later.
        # Its merge is a fact about a different pull request; the watermark
        # left by #41 must not suppress it for the whole closed lookback.
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1010", "2026-09-04T12:00:00.000+0000", _stage_text("merged"))],
        )
        calls = _tick(
            monkeypatch,
            pulls=[],
            closed=[_merged_pull(42, "SCRUM-9", "2026-09-04T11:30:00Z")],
            issues=[issue],
        )
        assert sweep.main() == 0
        capsys.readouterr()
        merged = [event for event in calls["events"] if event[0] == "pr.merged"]
        assert [event[1]["number"] for event in merged] == [42]
        assert merged[0][2] == "github:owner.example/repo:pr:42:merged"

    def test_a_declined_pr_is_not_suppressed_by_another_prs_cleanup_stage(
        self, monkeypatch, capsys
    ):
        issue = _issue(
            "SCRUM-9",
            "2026-09-01T09:00:00.000+0000",
            [_comment("1011", "2026-09-04T12:01:00.000+0000", _stage_text("cleanup"))],
        )
        closed = dict(_merged_pull(43, "SCRUM-9", "2026-09-04T11:30:00Z"), merged_at=None)
        calls = _tick(monkeypatch, pulls=[], closed=[closed], issues=[issue])
        assert sweep.main() == 0
        capsys.readouterr()
        assert [event[0] for event in calls["events"]] == ["pr.closed"]

    def test_a_failing_sonar_surface_does_not_hold_back_a_merge_fact(self, monkeypatch, capsys):
        # The lifecycle facts are read from their own listing and emitted
        # before the per-PR finding loop: a broken finding surface fails the
        # tick, but the merge that already happened still reaches the loop.
        calls = _tick(
            monkeypatch,
            pulls=[
                {
                    "number": 7,
                    "head_sha": "a" * 40,
                    "head": {"ref": "SCRUM-9/fix"},
                    "body": "",
                    "created_at": "2026-09-03T09:00:00Z",
                }
            ],
            closed=[_merged_pull(41, "SCRUM-9", "2026-09-04T11:00:00Z")],
            issues=[],
        )

        def unreachable_sonar(component, pr=None):
            raise urllib.error.URLError("sonarcloud.io unreachable")

        monkeypatch.setattr(sweep, "fetch_sonar_issues", unreachable_sonar)
        assert sweep.main() == 1
        assert "sweep failed while" in capsys.readouterr().err
        assert [event[0] for event in calls["events"]] == ["pr.opened", "pr.merged"]
