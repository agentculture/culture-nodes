"""pr.merged emission tests, split from test_pr_upkeep_sweep.py to keep it under the
1000-line hard limit (tests/lint filelength guard)."""

import json

from tests.test_pr_upkeep_sweep import EXAMPLE_DIR, FIXTURES, _stub_sweep, sweep  # noqa: F401


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
