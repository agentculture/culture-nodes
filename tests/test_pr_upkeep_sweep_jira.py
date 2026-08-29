"""Jira-source tests for examples/pr-upkeep/sweep.py (split from
test_pr_upkeep_sweep.py to stay under the 1000-line hard limit; see
tests/lint filelength gate). Shares fixtures via test helpers below."""

import importlib
import json
import re
import urllib.parse

import pytest

from tests.test_pr_upkeep_sweep import EXAMPLE_DIR, FIXTURES, _stub_sweep, sweep

jira = importlib.import_module("pr_upkeep_jira")


@pytest.fixture(scope="module")
def jira_payload():
    return json.loads((FIXTURES / "jira-search.json").read_text())


@pytest.fixture(scope="module")
def jira_round_trip():
    return json.loads((FIXTURES / "jira-history-round-trip.json").read_text())


@pytest.fixture(scope="module")
def jira_round_trip_complete():
    return json.loads((FIXTURES / "jira-history-round-trip-complete.json").read_text())


@pytest.fixture(scope="module")
def jira_scrum_3_self_echo():
    return json.loads((FIXTURES / "jira-scrum-3-transition-self-echo.json").read_text())


class TestJiraHistoryReplay:
    @staticmethod
    def _fresh_issue():
        return {
            "key": "SCRUM-5",
            "fields": {
                "created": "2026-08-29T09:45:00.000+0000",
                "status": {"name": "To Do"},
                "comment": {"comments": []},
            },
            "changelog": {"histories": []},
        }

    def test_brand_new_issue_emits_creation_transition_once_across_sweep_passes(self):
        issue = self._fresh_issue()
        cursors = {}
        appended = []

        for _pass in range(2):
            for fact in jira.jira_history_facts(issue, "bot-1"):
                name, payload, watermark, position_kind, position_id = fact
                source_key = f"jira:team.example.com:SCRUM-5:history:{position_kind}:{position_id}"
                encoded = json.dumps(watermark, sort_keys=True)
                if cursors.get(source_key) == encoded:
                    continue
                cursors[source_key] = encoded
                appended.append(fact)

            assert len(appended) == 1

        [(name, payload, watermark, position_kind, position_id)] = appended
        assert name == "pr-upkeep.jira.transitioned.to-do"
        assert payload["status"] == "To Do"
        assert payload["changelog_id"] == "0"
        assert watermark == {"changelog_id": "0", "comment_id": ""}
        assert (position_kind, position_id) == ("changelog", "0")

    def test_adopted_cutover_watermark_suppresses_creation_position(self):
        adopted = {"changelog_id": "10180", "comment_id": "10118"}
        facts = jira.jira_history_facts(self._fresh_issue(), "bot-1")

        emitted = [
            fact
            for fact in facts
            if not (
                int(fact[2]["changelog_id"] or 0) <= int(adopted["changelog_id"])
                and int(fact[2]["comment_id"] or 0) <= int(adopted["comment_id"])
            )
        ]

        assert emitted == []

    def test_scrum_3_entry_10180_bridge_transition_does_not_refire_but_human_transition_emits(
        self, jira_scrum_3_self_echo
    ):
        facts = jira.jira_history_facts(
            jira_scrum_3_self_echo["issues"][0], "712020:bridge-account"
        )

        transition_ids = [fact[1]["changelog_id"] for fact in facts]
        assert "10180" not in transition_ids
        assert transition_ids == ["0", "10179", "10181"]

    def test_transition_self_echo_uses_exact_author_id_not_marker_substrings(
        self, jira_scrum_3_self_echo
    ):
        facts = jira.jira_history_facts(
            jira_scrum_3_self_echo["issues"][0], "712020:bridge-account"
        )

        assert any(fact[1]["changelog_id"] == "10179" for fact in facts)

    def test_to_do_round_trip_between_polls_replays_both_transitions_in_order(
        self, jira_round_trip, jira_round_trip_complete
    ):
        before = jira.jira_history_facts(jira_round_trip["issues"][0], "bot-1")
        after = jira.jira_history_facts(jira_round_trip_complete["issues"][0], "bot-1")

        assert [fact[0] for fact in before][-1:] == ["pr-upkeep.jira.transitioned.to-do"]
        assert [fact[0] for fact in after][-5:] == [
            "pr-upkeep.jira.transitioned.to-do",
            "pr-upkeep.jira.transitioned.in-progress",
            jira.JIRA_COMMENT_EVENT_NAME,
            jira.JIRA_COMMENT_EVENT_NAME,
            "pr-upkeep.jira.transitioned.done",
        ]
        transitions = [fact for fact in after if fact[0].startswith("pr-upkeep.jira.transitioned.")]
        assert [fact[1]["status"] for fact in transitions][-3:] == [
            "To Do",
            "In Progress",
            "Done",
        ]

    def test_two_comment_reply_replays_two_facts_with_cumulative_history_watermarks(
        self, jira_round_trip_complete
    ):
        facts = jira.jira_history_facts(jira_round_trip_complete["issues"][0], "bot-1")
        comments = [fact for fact in facts if fact[0] == jira.JIRA_COMMENT_EVENT_NAME]

        assert [fact[1]["answer"]["comment_id"] for fact in comments] == ["30000", "30001"]
        assert [fact[2] for fact in comments] == [
            {"changelog_id": "20001", "comment_id": "30000"},
            {"changelog_id": "20001", "comment_id": "30001"},
        ]

    def test_history_ids_are_monotonic_even_when_jira_returns_entries_out_of_order(
        self, jira_round_trip_complete
    ):
        issue = json.loads(json.dumps(jira_round_trip_complete["issues"][0]))
        issue["changelog"]["histories"].reverse()
        facts = jira.jira_history_facts(issue, "bot-1")
        transitions = [fact for fact in facts if fact[0].startswith("pr-upkeep.jira.transitioned.")]
        assert [fact[1].get("changelog_id") for fact in transitions] == [
            "0",
            "20000",
            "20001",
            "20002",
        ]


class TestJiraWorkItems:
    """Issue #76 acceptance is recorded-fixture-only; the live backlog is empty."""

    def test_recorded_backlog_item_enters_the_priority_list(self, jira_payload):
        items = jira.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert len(items) == 1
        assert items[0]["source"] == "jira"
        assert items[0]["id"] == "EX-17"
        assert items[0]["severity"] == "High"
        assert sweep.prioritise(items)[0]["title"] == ("Make the recorded backlog item actionable")

    def test_jira_provenance_uses_only_reserved_example_configuration(self, jira_payload):
        (item,) = jira.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert item["project"] == "EX"
        assert item["details_url"] == "https://team.example.com/browse/EX-17"

    def test_basic_auth_is_built_in_the_request_not_argv_or_output(self, monkeypatch):
        seen = {}

        def fake_get(url, token=None, *, basic=None):
            seen.update(url=url, token=token, basic=basic)
            return {"issues": []}

        monkeypatch.setattr(jira, "_get_json", fake_get)
        assert jira.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token"
        ) == {"issues": [], "isLast": True}
        assert seen["basic"] == ("robot@example.com", "fixture-token")
        assert seen["token"] is None
        assert "robot@example.com" not in seen["url"]
        assert "fixture-token" not in seen["url"]
        assert "maxResults=100" in seen["url"]
        assert "expand=changelog" in seen["url"]
        assert 100 < jira.JIRA_RATE_LIMIT_PER_WINDOW == 350

    def test_search_changelog_and_comment_pages_are_collected_in_monotonic_id_order(
        self, monkeypatch
    ):
        issue = {
            "id": "10002",
            "key": "SCRUM-2",
            "fields": {
                "comment": {
                    "startAt": 0,
                    "maxResults": 1,
                    "total": 2,
                    "comments": [{"id": "30001", "created": "2026-08-19T08:05:00Z"}],
                }
            },
            "changelog": {
                "startAt": 0,
                "maxResults": 1,
                "total": 2,
                "histories": [{"id": "20001", "created": "2026-08-19T08:03:00Z"}],
            },
        }
        seen = []

        def fake_get(url, token=None, *, basic=None):
            seen.append(url)
            if "/changelog?" in url:
                return {"startAt": 1, "maxResults": 100, "total": 2, "values": [{"id": "20000"}]}
            if "/comment?" in url:
                return {"startAt": 1, "maxResults": 100, "total": 2, "comments": [{"id": "30000"}]}
            return {"issues": [issue], "isLast": True}

        monkeypatch.setattr(jira, "_get_json", fake_get)
        payload = jira.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token"
        )

        fetched = payload["issues"][0]
        assert [entry["id"] for entry in fetched["changelog"]["histories"]] == [
            "20000",
            "20001",
        ]
        assert [entry["id"] for entry in fetched["fields"]["comment"]["comments"]] == [
            "30000",
            "30001",
        ]
        assert any("startAt=1" in url and "/changelog?" in url for url in seen)
        assert any("startAt=1" in url and "/comment?" in url for url in seen)

    def test_discovery_jql_keeps_recently_resolved_issues_inside_the_bounded_lookback(
        self, monkeypatch
    ):
        seen = []
        monkeypatch.setattr(
            jira,
            "_get_json",
            lambda url, token=None, **kwargs: seen.append(url) or {"issues": [], "isLast": True},
        )

        jira.fetch_jira_issues("team.example.com", "EX", "robot@example.com", "token")

        query = urllib.parse.parse_qs(urllib.parse.urlparse(seen[0]).query)
        assert query["jql"] == [
            'project = "EX" AND '
            f"(resolution IS EMPTY OR resolved >= -{jira.JIRA_RESOLVED_LOOKBACK_DAYS}d) "
            "ORDER BY priority ASC"
        ]
        assert jira.JIRA_RESOLVED_LOOKBACK_DAYS >= 2

    def test_scrum_2_resolved_between_polls_emits_terminal_transition_once(
        self, monkeypatch, capsys, jira_round_trip, jira_round_trip_complete
    ):
        grant = {
            "cycle": 0,
            "repositories": [
                {
                    "github_repo": "owner.example/repo",
                    "sonar_component": "owner_repo",
                    "jira_site": "team.example.com",
                    "jira_project": "SCRUM",
                }
            ],
        }
        monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", json.dumps(grant))
        monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
        monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
        _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
        polls = iter([jira_round_trip, jira_round_trip_complete, jira_round_trip_complete])
        monkeypatch.setattr(sweep, "fetch_jira_issues", lambda *_args: next(polls))

        cursors = {}
        appended = []

        def equality_dedup(name, payload, source_key, watermark, **_kw):
            encoded = json.dumps(watermark, sort_keys=True)
            if cursors.get(source_key) == encoded:
                return {"duplicate": True}
            cursors[source_key] = encoded
            appended.append((name, payload, source_key, watermark))
            return {"duplicate": False}

        monkeypatch.setattr(sweep, "raise_event", equality_dedup)

        assert sweep.main() == 0
        first_count = len(appended)
        assert sweep.main() == 0
        second_count = len(appended)
        assert [event[0] for event in appended if event[0].endswith(".done")] == [
            "pr-upkeep.jira.transitioned.done"
        ]
        assert second_count > first_count

        assert sweep.main() == 0
        assert len(appended) == second_count
        capsys.readouterr()

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
        assert jira.jira_transition_event_name("To Do") == "pr-upkeep.jira.transitioned.to-do"
        assert jira.jira_transition_event_name("Ready for Dev") == (
            "pr-upkeep.jira.transitioned.ready-for-dev"
        )

    def test_transition_slug_normalises_punctuation_and_case(self):
        assert jira.jira_transition_event_name("  In Progress!! ") == (
            "pr-upkeep.jira.transitioned.in-progress"
        )

    def test_an_empty_status_still_yields_a_stable_name(self):
        # Defensive: a malformed Jira payload must not crash the sweep, and
        # must not collide with a real status's event name.
        assert jira.jira_transition_event_name("") == "pr-upkeep.jira.transitioned.unspecified"

    def test_transition_and_comment_event_names_never_collide(self):
        for status in ("To Do", "In Progress", "Ready for Dev", "Done", ""):
            name = jira.jira_transition_event_name(status)
            assert name != jira.JIRA_COMMENT_EVENT_NAME
            assert not name.startswith(jira.JIRA_COMMENT_EVENT_NAME)
        assert not jira.JIRA_COMMENT_EVENT_NAME.startswith("pr-upkeep.jira.transitioned.")

    def test_comment_consumer_trigger_exactly_matches_sweep_comment_event_name(self):
        workflow = (EXAMPLE_DIR.parent / "jira-comment-consumer" / "workflow.yaml").read_text()
        declared = re.findall(r"^\s*- onEvent:\s*(\S+)\s*$", workflow, re.MULTILINE)

        assert declared == [jira.JIRA_COMMENT_EVENT_NAME]


class TestJiraSelfEcho:
    """Task t9, requirement 2: the sweep must skip comments authored by the
    system's own Jira account when deciding to emit a comment/resume event —
    otherwise a posted question would answer itself. The account id is
    configuration (run input), the same pattern as jira_site/jira_project,
    never a module constant."""

    @staticmethod
    def _comments(*pairs):
        return [
            {"id": str(30000 + index), "author": {"accountId": account_id}, "created": created}
            for index, (account_id, created) in enumerate(pairs)
        ]

    def test_newest_comment_by_the_bot_is_self_echo(self):
        comments = self._comments(
            ("human-1", "2026-08-15T00:00:00Z"),
            ("bot-1", "2026-08-16T00:00:00Z"),
        )
        assert jira.jira_comment_is_self_echo(comments, "bot-1") is True

    def test_newest_comment_by_a_human_after_the_bots_is_not_self_echo(self):
        comments = self._comments(
            ("bot-1", "2026-08-15T00:00:00Z"),
            ("human-1", "2026-08-16T00:00:00Z"),
        )
        assert jira.jira_comment_is_self_echo(comments, "bot-1") is False

    def test_no_comments_is_not_self_echo(self):
        assert jira.jira_comment_is_self_echo([], "bot-1") is False

    def test_an_unconfigured_bot_account_id_never_filters(self):
        # Without configuration there is nothing to compare against — this
        # is a documented limitation (see selected_repository), not a
        # silent failure.
        comments = self._comments(("bot-1", "2026-08-16T00:00:00Z"))
        assert jira.jira_comment_is_self_echo(comments, "") is False
        assert jira.jira_comment_is_self_echo(comments, None) is False

    def test_actor_marker_filters_self_echo_when_bot_identity_is_unconfigured(self):
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
        assert jira.jira_comment_is_self_echo(comments, None) is True

    def test_human_quoting_actor_marker_is_not_silenced_when_bot_identity_is_known(self):
        issue = {
            "key": "SCRUM-3",
            "fields": {
                "comment": {
                    "comments": [
                        {
                            "id": "10117",
                            "author": {"accountId": "human-operator"},
                            "created": "2026-08-19T16:16:00Z",
                            "body": (
                                "The prior reply contained "
                                "[culture-nodes:jira-actor question_id=q-17]; continue now."
                            ),
                        }
                    ]
                }
            },
            "changelog": {"histories": []},
        }

        facts = jira.jira_history_facts(issue, "bridge-bot")

        comment_facts = [fact for fact in facts if fact[0] == jira.JIRA_COMMENT_EVENT_NAME]
        assert [(fact[0], fact[1]["answer"]["comment_id"]) for fact in comment_facts] == [
            (jira.JIRA_COMMENT_EVENT_NAME, "10117")
        ]

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
        assert jira.jira_question_id_for_answer(comments) == "q-17"

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
                    "jira_bot_account_id": "personal-operator",
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
            event for event in calls["events"] if event[0] == jira.JIRA_COMMENT_EVENT_NAME
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
        assert jira.jira_watermark(issue)["comment_id"] == "30001"
