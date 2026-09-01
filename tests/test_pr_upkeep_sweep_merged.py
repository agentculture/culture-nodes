"""pr.merged emission tests, split from test_pr_upkeep_sweep.py to keep it under the
1000-line hard limit (tests/lint filelength guard)."""

import json

from tests.test_pr_upkeep_sweep import EXAMPLE_DIR, FIXTURES, _stub_sweep, sweep  # noqa: F401


def test_open_pr_fact_uses_branch_then_body_ticket_key_and_project_prefix():
    branch = sweep.opened_pr_fact(
        {
            "number": 41,
            "state": "open",
            "created_at": "2026-09-02T08:00:00Z",
            "html_url": "https://github.com/o/r/pull/41",
            "head": {"ref": "SCRUM-41-review"},
            "body": "ADR-0002 must not win",
        },
        "o/r",
        "SCRUM",
    )
    body = sweep.opened_pr_fact(
        {
            "number": 42,
            "state": "open",
            "created_at": "2026-09-02T09:00:00Z",
            "html_url": "https://github.com/o/r/pull/42",
            "head": {"ref": "plain-branch"},
            "body": "Delivers SCRUM-42",
        },
        "o/r",
        "SCRUM",
    )

    assert branch == {
        "source": "github_pr",
        "repository": "o/r",
        "number": 41,
        "url": "https://github.com/o/r/pull/41",
        "opened_at": "2026-09-02T08:00:00Z",
        "issue_key": "SCRUM-41",
    }
    assert body["issue_key"] == "SCRUM-42"
    assert sweep.opened_pr_fact({**branch, "state": "closed"}, "o/r", "SCRUM") is None


def test_open_pr_fact_is_emitted_once_per_pr_by_its_durable_watermark(monkeypatch, capsys):
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {
                "repositories": [
                    {
                        "github_repo": "owner/repo",
                        "sonar_component": "owner_repo",
                        "jira_site": "team.example.com",
                        "jira_project": "SCRUM",
                    }
                ]
            }
        ),
    )
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
    monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
    pull = {
        "number": 41,
        "state": "open",
        "created_at": "2026-09-02T08:00:00Z",
        "html_url": "https://github.com/owner/repo/pull/41",
        "head": {"ref": "SCRUM-41-review"},
        "head_sha": "sha41",
        "body": "",
    }
    _stub_sweep(monkeypatch, pulls=[pull], sonar_main={"issues": []})
    monkeypatch.setattr(sweep, "fetch_jira_issues", lambda *_args: {"issues": []})
    cursors, events = {}, []

    def dedup(name, payload, source_key, watermark, **kwargs):
        encoded = json.dumps(watermark, sort_keys=True)
        duplicate = cursors.get(source_key) == encoded
        cursors[source_key] = encoded
        if not duplicate:
            events.append((name, payload, source_key, watermark, kwargs))
        return {"duplicate": duplicate}

    monkeypatch.setattr(sweep, "raise_event", dedup)
    assert sweep.main() == 0
    assert sweep.main() == 0
    opened = [event for event in events if event[0] == "pr.opened"]
    assert len(opened) == 1
    assert opened[0][2] == "github:owner/repo:pr:41:opened"
    assert opened[0][3] == {"opened_at": "2026-09-02T08:00:00Z"}
    assert opened[0][4]["subject"] == "SCRUM-41"
    capsys.readouterr()


def test_merged_pr_fact_uses_branch_then_body_ticket_key():
    branch = sweep.merged_pr_fact(
        {
            "number": 7,
            "merged_at": "2026-08-29T10:00:00Z",
            "head": {"ref": "SCRUM-230/fix"},
            "body": "SCRUM-999",
        },
        "agentculture/culture-nodes",
    )
    body = sweep.merged_pr_fact(
        {
            "number": 8,
            "merged_at": "2026-08-29T11:00:00Z",
            "head": {"ref": "fix"},
            "body": "Closes SCRUM-231",
        },
        "agentculture/culture-nodes",
    )
    assert branch["issue_key"] == "SCRUM-230"
    assert body["issue_key"] == "SCRUM-231"


def test_merged_pr_is_emitted_once_across_two_watermarked_passes(monkeypatch):
    monkeypatch.setenv(
        "PR_UPKEEP_REPOSITORIES",
        json.dumps(
            {
                "cycle": 0,
                "repositories": [{"github_repo": "owner/repo", "sonar_component": "owner_repo"}],
            }
        ),
    )
    calls = _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
    merged = {
        "number": 9,
        "merged_at": "2026-08-29T12:00:00Z",
        "head": {"ref": "SCRUM-232/done"},
        "body": "",
    }
    monkeypatch.setattr(sweep, "fetch_merged_pulls", lambda *_: [merged])
    cursors = set()
    appended = []

    def dedup(name, payload, source_key, watermark, **_kw):
        key = (source_key, json.dumps(watermark, sort_keys=True))
        if key not in cursors:
            cursors.add(key)
            appended.append((name, payload))
        return {"duplicate": key in cursors}

    monkeypatch.setattr(sweep, "raise_event", dedup)
    assert sweep.main() == 0
    assert sweep.main() == 0
    assert [(name, payload["issue_key"]) for name, payload in appended] == [
        ("pr.merged", "SCRUM-232")
    ]
    assert calls["events"] == []


def test_merged_pr_fact_only_correlates_the_configured_jira_project():
    """PR #70's body mentioned ADR-0002 and froze a phantom ticket on prod (#230)."""
    pull = {
        "number": 70,
        "merged_at": "2026-08-29T10:00:00Z",
        "head": {"ref": "adr/0002"},
        "body": "See ADR-0002",
    }
    assert sweep.merged_pr_fact(pull, "agentculture/culture-nodes", "SCRUM") is None
    pull["body"] = "See ADR-0002 and SCRUM-42"
    assert (
        sweep.merged_pr_fact(pull, "agentculture/culture-nodes", "SCRUM")["issue_key"] == "SCRUM-42"
    )


def test_jira_facts_carry_the_issue_key_as_subject(monkeypatch):
    """runs.subject was NULL on every prod run until t8 (#230): raise_event
    never sent a subject, so the ticket projection listed no runs."""
    seen = []
    monkeypatch.setattr(sweep, "raise_event", lambda *a, **kw: seen.append(kw.get("subject")) or {})
    fact = {
        "number": 9,
        "merged_at": "2026-08-29T10:00:00Z",
        "head": {"ref": "SCRUM-5/x"},
        "body": "",
    }
    sweep.raise_event(
        "pr.merged",
        sweep.merged_pr_fact(fact, "r", "SCRUM"),
        "k",
        {"merged_at": "x"},
        subject="SCRUM-5",
    )
    assert seen == ["SCRUM-5"]


def test_fetch_merged_pulls_follows_pages_inside_the_lookback_window(monkeypatch):
    """One 50-item page dropped every merge past it (Qodo 6 on #244)."""
    from datetime import datetime, timedelta, timezone

    fresh = (datetime.now(timezone.utc) - timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    stale = (datetime.now(timezone.utc) - timedelta(days=90)).strftime("%Y-%m-%dT%H:%M:%SZ")
    pages = {
        1: [{"number": n, "merged_at": fresh, "updated_at": fresh} for n in range(1, 51)],
        2: [{"number": 51, "merged_at": fresh, "updated_at": fresh}]
        + [{"number": n, "merged_at": None, "updated_at": stale} for n in range(52, 60)],
        3: [{"number": 99, "merged_at": stale, "updated_at": stale}],
    }
    seen = []

    def fake_get(url, token=None, **_kw):
        page = int(url.rsplit("page=", 1)[1])
        seen.append(page)
        return pages.get(page, [])

    monkeypatch.setattr(sweep, "_get_json", fake_get)
    merged = sweep.fetch_merged_pulls(None, "agentculture/culture-nodes")
    assert [p["number"] for p in merged] == list(range(1, 52))
    assert seen == [1, 2], "page 2 ended before the window; page 3 must not be read"
