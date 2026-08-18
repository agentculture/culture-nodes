"""Jira-source tests for examples/pr-upkeep/sweep.py (split from
test_pr_upkeep_sweep.py to stay under the 1000-line hard limit; see
tests/lint filelength gate). Shares fixtures via test helpers below."""

import json

import pytest

from tests.test_pr_upkeep_sweep import EXAMPLE_DIR, FIXTURES, _stub_sweep, sweep


@pytest.fixture(scope="module")
def jira_payload():
    return json.loads((FIXTURES / "jira-search.json").read_text())


class TestJiraWorkItems:
    """Issue #76 acceptance is recorded-fixture-only; the live backlog is empty."""

    def test_recorded_backlog_item_enters_the_priority_list(self, jira_payload):
        items = sweep.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert len(items) == 1
        assert items[0]["source"] == "jira"
        assert items[0]["id"] == "EX-17"
        assert items[0]["severity"] == "High"
        assert sweep.prioritise(items)[0]["title"] == ("Make the recorded backlog item actionable")

    def test_jira_provenance_uses_only_reserved_example_configuration(self, jira_payload):
        (item,) = sweep.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert item["project"] == "EX"
        assert item["details_url"] == "https://team.example.com/browse/EX-17"

    def test_basic_auth_is_built_in_the_request_not_argv_or_output(self, monkeypatch):
        seen = {}

        def fake_get(url, token=None, *, basic=None):
            seen.update(url=url, token=token, basic=basic)
            return {"issues": []}

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        assert sweep.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token"
        ) == {"issues": []}
        assert seen["basic"] == ("robot@example.com", "fixture-token")
        assert seen["token"] is None
        assert "robot@example.com" not in seen["url"]
        assert "fixture-token" not in seen["url"]
        assert "maxResults=100" in seen["url"]
        assert 100 < sweep.JIRA_RATE_LIMIT_PER_WINDOW == 350

    def test_jira_configuration_requires_both_host_and_project(self):
        raw = json.dumps(
            {
                "repositories": [
                    {
                        "github_repo": "owner.example/repo",
                        "sonar_component": "owner_repo",
                        "jira_site": "team.example.com",
                    }
                ]
            }
        )
        with pytest.raises(ValueError, match="configured together"):
            sweep.selected_repository(raw)

    def test_jira_bot_account_id_is_run_input_not_a_module_constant(self):
        # Same pattern as jira_site/jira_project: optional per-repository
        # configuration, never a literal baked into this file.
        source = (EXAMPLE_DIR / "sweep.py").read_text()
        assert "JIRA_BOT_ACCOUNT_ID =" not in source
        raw = json.dumps(
            {
                "repositories": [
                    {
                        "github_repo": "owner.example/repo",
                        "sonar_component": "owner_repo",
                        "jira_site": "team.example.com",
                        "jira_project": "EX",
                        "jira_bot_account_id": "example-bot-account-id",
                    }
                ]
            }
        )
        selected = sweep.selected_repository(raw)
        assert selected["jira_bot_account_id"] == "example-bot-account-id"

    def test_jira_bot_account_id_must_be_a_string_when_present(self):
        raw = json.dumps(
            {
                "repositories": [
                    {
                        "github_repo": "owner.example/repo",
                        "sonar_component": "owner_repo",
                        "jira_site": "team.example.com",
                        "jira_project": "EX",
                        "jira_bot_account_id": 12345,
                    }
                ]
            }
        )
        with pytest.raises(ValueError, match="jira_bot_account_id"):
            sweep.selected_repository(raw)


class TestJiraEventNames:
    """Task t9, requirement 1: a Jira issue entering a named state raises a
    distinct event name from "a comment appeared" — the only structural gap
    #118 step 1 named in the sweep. A workflow trigger subscribed to one
    must never structurally be able to receive the other."""

    def test_transition_event_name_is_derived_from_the_current_status(self):
        assert sweep.jira_transition_event_name("To Do") == "pr-upkeep.jira.transitioned.to-do"
        assert sweep.jira_transition_event_name("Ready for Dev") == (
            "pr-upkeep.jira.transitioned.ready-for-dev"
        )

    def test_transition_slug_normalises_punctuation_and_case(self):
        assert sweep.jira_transition_event_name("  In Progress!! ") == (
            "pr-upkeep.jira.transitioned.in-progress"
        )

    def test_an_empty_status_still_yields_a_stable_name(self):
        # Defensive: a malformed Jira payload must not crash the sweep, and
        # must not collide with a real status's event name.
        assert sweep.jira_transition_event_name("") == "pr-upkeep.jira.transitioned.unspecified"

    def test_transition_and_comment_event_names_never_collide(self):
        for status in ("To Do", "In Progress", "Ready for Dev", "Done", ""):
            name = sweep.jira_transition_event_name(status)
            assert name != sweep.JIRA_COMMENT_EVENT_NAME
            assert not name.startswith(sweep.JIRA_COMMENT_EVENT_NAME)
        assert not sweep.JIRA_COMMENT_EVENT_NAME.startswith("pr-upkeep.jira.transitioned.")


class TestJiraSelfEcho:
    """Task t9, requirement 2: the sweep must skip comments authored by the
    system's own Jira account when deciding to emit a comment/resume event —
    otherwise a posted question would answer itself. The account id is
    configuration (run input), the same pattern as jira_site/jira_project,
    never a module constant."""

    @staticmethod
    def _comments(*pairs):
        return [
            {"author": {"accountId": account_id}, "created": created}
            for account_id, created in pairs
        ]

    def test_newest_comment_by_the_bot_is_self_echo(self):
        comments = self._comments(
            ("human-1", "2026-08-15T00:00:00Z"),
            ("bot-1", "2026-08-16T00:00:00Z"),
        )
        assert sweep.jira_comment_is_self_echo(comments, "bot-1") is True

    def test_newest_comment_by_a_human_after_the_bots_is_not_self_echo(self):
        comments = self._comments(
            ("bot-1", "2026-08-15T00:00:00Z"),
            ("human-1", "2026-08-16T00:00:00Z"),
        )
        assert sweep.jira_comment_is_self_echo(comments, "bot-1") is False

    def test_no_comments_is_not_self_echo(self):
        assert sweep.jira_comment_is_self_echo([], "bot-1") is False

    def test_an_unconfigured_bot_account_id_never_filters(self):
        # Without configuration there is nothing to compare against — this
        # is a documented limitation (see selected_repository), not a
        # silent failure.
        comments = self._comments(("bot-1", "2026-08-16T00:00:00Z"))
        assert sweep.jira_comment_is_self_echo(comments, "") is False
        assert sweep.jira_comment_is_self_echo(comments, None) is False

    def test_actor_marker_filters_self_echo_when_personal_account_identity_cannot(self):
        comments = [
            {
                "author": {"accountId": "operators-personal-account"},
                "created": "2026-08-16T00:00:00Z",
                "body": {
                    "type": "doc",
                    "content": [
                        {
                            "type": "paragraph",
                            "content": [
                                {
                                    "type": "text",
                                    "text": "Question\n\n"
                                    "[culture-nodes:jira-actor question_id=q-17]",
                                }
                            ],
                        }
                    ],
                },
            }
        ]
        assert sweep.jira_comment_is_self_echo(comments, "different-bot-id") is True

    def test_human_answer_names_the_nearest_originating_question(self):
        comments = [
            {"created": "2026-08-15T00:00:00Z", "body": "old answer"},
            {
                "created": "2026-08-16T00:00:00Z",
                "body": "Question\n\n[culture-nodes:jira-actor question_id=q-17]",
            },
            {
                "id": "answer-9",
                "created": "2026-08-17T00:00:00Z",
                "body": "Use the bounded option.",
            },
        ]
        assert sweep.jira_question_id_for_answer(comments) == "q-17"

    def test_answer_event_payload_carries_question_id_for_continuation(
        self, monkeypatch, capsys, jira_payload
    ):
        payload = json.loads(json.dumps(jira_payload))
        payload["issues"][0]["fields"]["comment"] = {
            "comments": [
                {
                    "id": "1008",
                    "author": {"accountId": "personal-operator"},
                    "created": "2026-08-16T00:00:00Z",
                    "body": "Which bound?\n\n[culture-nodes:jira-actor question_id=q-17]",
                },
                {
                    "id": "1009",
                    "author": {"accountId": "human-1"},
                    "created": "2026-08-17T00:00:00Z",
                    "body": "Use three attempts.",
                },
            ]
        }
        grant = {
            "cycle": 0,
            "repositories": [
                {
                    "github_repo": "owner.example/repo",
                    "sonar_component": "owner_repo",
                    "jira_site": "team.example.com",
                    "jira_project": "EX",
                    "jira_bot_account_id": "a-different-machine-account",
                }
            ],
        }
        monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", json.dumps(grant))
        monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
        monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
        calls = _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
        monkeypatch.setattr(sweep, "fetch_jira_issues", lambda *_args: payload)

        assert sweep.main() == 0
        capsys.readouterr()
        comment = next(
            event for event in calls["events"] if event[0] == sweep.JIRA_COMMENT_EVENT_NAME
        )
        assert comment[1]["originating_question_id"] == "q-17"
        assert comment[1]["answer"] == {"comment_id": "1009", "body": "Use three attempts."}

    def test_the_watermark_position_advances_past_the_bots_own_comment(self):
        # jira_watermark is unfiltered by design: it must keep advancing to
        # the newest comment regardless of authorship, or a later real reply
        # would be compared against a stale, pre-echo position.
        issue = {
            "fields": {
                "updated": "2026-08-16T00:00:00Z",
                "comment": {
                    "comments": self._comments(
                        ("human-1", "2026-08-15T00:00:00Z"),
                        ("bot-1", "2026-08-16T00:00:00Z"),
                    )
                },
            }
        }
        assert sweep.jira_watermark(issue)["newest_comment_at"] == "2026-08-16T00:00:00Z"
