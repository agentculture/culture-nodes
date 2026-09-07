"""The sweep emits ``pr.closed`` for a PR closed WITHOUT merge (plan
loop-closure-claude-codex t12; spec c6 / c37, honesty h24).

The human-inbox tracker already observes a close-without-merge
(``github_pr_closed``, adapters/human-inbox tracker.py CLOSED_OBSERVATION_KIND)
but nothing in examples/ or internal/ ever produced a fact from it, so a
closed PR's parked upkeep runs stayed running forever. The sweep now mirrors
``pr.merged``: one ``pr.closed`` fact per closed-unmerged PR, keyed
``github:{repo}:pr:{n}:closed`` and watermarked by the immutable
``closed_at``, carrying the work item as both payload field and subject. A
merged PR never emits ``pr.closed``; a closed-unmerged PR never emits
``pr.merged``. The consumer (the cleanup node) is task t13.

Split from test_pr_upkeep_sweep.py to keep that file under the 1000-line
hard limit (tests/lint filelength guard).
"""

import importlib
import json
import urllib.error

import pytest

from tests.test_pr_upkeep_sweep import (  # noqa: F401
    EXAMPLE_DIR,
    FIXTURES,
    REPOSITORY_GRANT,
    _stub_sweep,
    sweep,
)

emit = importlib.import_module("pr_upkeep_emit")

REPOSITORY = "agentculture/culture-nodes"

CLOSED_UNMERGED = {
    "number": 55,
    "state": "closed",
    "merged_at": None,
    "closed_at": "2026-09-06T14:00:00Z",
    "updated_at": "2026-09-06T14:00:00Z",
    "html_url": "https://github.com/agentculture/culture-nodes/pull/55",
    "head": {"ref": "SCRUM-55/declined", "sha": "sha55"},
    "body": "",
}

MERGED = {
    "number": 56,
    "state": "closed",
    "merged_at": "2026-09-06T15:00:00Z",
    "closed_at": "2026-09-06T15:00:00Z",
    "updated_at": "2026-09-06T15:00:00Z",
    "html_url": "https://github.com/agentculture/culture-nodes/pull/56",
    "head": {"ref": "SCRUM-56/landed", "sha": "sha56"},
    "body": "",
}


@pytest.fixture(autouse=True)
def repository_grant(monkeypatch):
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", REPOSITORY_GRANT)


def _run_twice_with_cursor_dedup(monkeypatch, closed_pulls):
    """Drive main() twice over the same closed listing behind a control-plane
    style watermark-equality dedup; return the facts that were NOT duplicates."""
    calls = _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
    monkeypatch.setattr(sweep, "fetch_closed_pulls", lambda *_: [dict(p) for p in closed_pulls])
    cursors, appended = {}, []

    def dedup(name, payload, source_key, watermark, **kwargs):
        encoded = json.dumps(watermark, sort_keys=True)
        duplicate = cursors.get(source_key) == encoded
        cursors[source_key] = encoded
        if not duplicate:
            appended.append((name, payload, source_key, watermark, kwargs))
        return {"duplicate": duplicate}

    monkeypatch.setattr(sweep, "raise_event", dedup)
    assert sweep.main() == 0
    assert sweep.main() == 0
    assert calls["events"] == []  # every emission went through `dedup`
    return appended


class TestClosedPrFact:
    def test_closed_unmerged_pr_builds_the_fact_shape(self):
        fact = emit.closed_pr_fact(CLOSED_UNMERGED, REPOSITORY)
        assert fact == {
            "source": "github_pr",
            "repository": REPOSITORY,
            "number": 55,
            "head_sha": "sha55",
            "closed_at": "2026-09-06T14:00:00Z",
            "work_item": "SCRUM-55",
            "url": "https://github.com/agentculture/culture-nodes/pull/55",
        }

    def test_merged_pr_is_never_a_closed_fact(self):
        assert emit.closed_pr_fact(MERGED, REPOSITORY) is None

    def test_open_pr_is_never_a_closed_fact(self):
        pull = {**CLOSED_UNMERGED, "state": "open", "closed_at": None}
        assert emit.closed_pr_fact(pull, REPOSITORY) is None

    def test_closed_pr_without_a_closed_at_is_skipped_not_raised(self):
        """closed_at is the watermark; a fact without one has no cursor."""
        assert emit.closed_pr_fact({**CLOSED_UNMERGED, "closed_at": None}, REPOSITORY) is None

    def test_work_item_falls_to_the_transient_gh_form_without_a_ticket(self):
        pull = {**CLOSED_UNMERGED, "head": {"ref": "loop/t12", "sha": "sha55"}}
        fact = emit.closed_pr_fact(pull, REPOSITORY)
        assert fact["work_item"] == f"gh:{REPOSITORY}#55"

    def test_work_item_is_narrowed_to_the_configured_jira_project(self):
        pull = {**CLOSED_UNMERGED, "head": {"ref": "adr/0002", "sha": "sha55"}, "body": "ADR-0002"}
        assert emit.closed_pr_fact(pull, REPOSITORY, "SCRUM")["work_item"] == f"gh:{REPOSITORY}#55"

    def test_url_is_omitted_when_github_sends_none(self):
        pull = {k: v for k, v in CLOSED_UNMERGED.items() if k != "html_url"}
        assert "url" not in emit.closed_pr_fact(pull, REPOSITORY)

    def test_head_sha_accepts_the_normalised_open_pull_shape_too(self):
        pull = {**CLOSED_UNMERGED, "head": {"ref": "SCRUM-55/declined"}, "head_sha": "shaN"}
        assert emit.closed_pr_fact(pull, REPOSITORY)["head_sha"] == "shaN"


class TestClosedPullEvent:
    def test_merged_pull_routes_to_pr_merged(self):
        name, payload, source_key, watermark, subject = emit.closed_pull_event(MERGED, REPOSITORY)
        assert name == "pr.merged"
        assert source_key == f"github:{REPOSITORY}:pr:56:merged"
        assert watermark == {"merged_at": "2026-09-06T15:00:00Z"}
        assert subject == payload["issue_key"] == "SCRUM-56"

    def test_closed_unmerged_pull_routes_to_pr_closed(self):
        name, payload, source_key, watermark, subject = emit.closed_pull_event(
            CLOSED_UNMERGED, REPOSITORY
        )
        assert name == "pr.closed"
        assert source_key == f"github:{REPOSITORY}:pr:55:closed"
        assert watermark == {"closed_at": "2026-09-06T14:00:00Z"}
        assert subject == payload["work_item"] == "SCRUM-55"

    def test_merged_pull_without_a_ticket_emits_nothing_at_all(self):
        """The pre-t12 rule for pr.merged, kept: an uncorrelatable merge is
        silent, and it must not fall through to pr.closed either."""
        pull = {**MERGED, "head": {"ref": "plain", "sha": "sha56"}}
        assert emit.closed_pull_event(pull, REPOSITORY) is None


class TestSweepEmission:
    def test_closed_unmerged_pr_emits_exactly_one_pr_closed_and_never_pr_merged(self, monkeypatch):
        appended = _run_twice_with_cursor_dedup(monkeypatch, [CLOSED_UNMERGED])
        assert [name for name, *_ in appended] == ["pr.closed"]
        [(_, payload, source_key, watermark, kwargs)] = appended
        assert source_key == f"github:{REPOSITORY}:pr:55:closed"
        assert watermark == {"closed_at": "2026-09-06T14:00:00Z"}
        assert payload == {
            "source": "github_pr",
            "repository": REPOSITORY,
            "number": 55,
            "head_sha": "sha55",
            "closed_at": "2026-09-06T14:00:00Z",
            "work_item": "SCRUM-55",
            "url": "https://github.com/agentculture/culture-nodes/pull/55",
        }
        assert kwargs["subject"] == payload["work_item"]

    def test_merged_pr_emits_pr_merged_and_no_pr_closed(self, monkeypatch):
        appended = _run_twice_with_cursor_dedup(monkeypatch, [MERGED])
        assert [(name, source_key) for name, _, source_key, *_ in appended] == [
            ("pr.merged", f"github:{REPOSITORY}:pr:56:merged")
        ]
        assert appended[0][4]["subject"] == "SCRUM-56"

    def test_mixed_listing_emits_one_fact_per_pr_of_the_right_kind(self, monkeypatch):
        appended = _run_twice_with_cursor_dedup(monkeypatch, [CLOSED_UNMERGED, MERGED])
        assert sorted((name, payload["number"]) for name, payload, *_ in appended) == [
            ("pr.closed", 55),
            ("pr.merged", 56),
        ]

    def test_replaying_the_same_listing_emits_nothing_new(self, monkeypatch):
        """Two passes, one fact: closed_at is immutable, so the watermark the
        control plane compares for equality never moves."""
        appended = _run_twice_with_cursor_dedup(monkeypatch, [CLOSED_UNMERGED])
        assert len(appended) == 1

    def test_pr_closed_fact_subject_equals_its_work_item(self, monkeypatch):
        seen = []
        _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
        monkeypatch.setattr(sweep, "fetch_closed_pulls", lambda *_: [dict(CLOSED_UNMERGED)])
        monkeypatch.setattr(
            sweep,
            "raise_event",
            lambda name, payload, *_a, **kw: seen.append((name, payload, kw)) or {},
        )
        assert sweep.main() == 0
        [(name, payload, kw)] = seen
        assert name == "pr.closed"
        assert kw["subject"] == payload["work_item"] == "SCRUM-55"

    def test_failure_stage_names_pr_closed(self, monkeypatch, capsys):
        _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
        monkeypatch.setattr(sweep, "fetch_closed_pulls", lambda *_: [dict(CLOSED_UNMERGED)])

        def boom(*_a, **_kw):
            raise urllib.error.URLError("control plane away")

        monkeypatch.setattr(sweep, "raise_event", boom)
        assert sweep.main() == 1
        assert "emitting pr.closed for #55 (control plane)" in capsys.readouterr().err


def test_fetch_closed_pulls_keeps_unmerged_closures_inside_the_window(monkeypatch):
    """The closed listing is shared: pr.merged filters it on merged_at and
    pr.closed on its absence, so the read itself must keep both."""
    from datetime import datetime, timedelta, timezone

    fresh = (datetime.now(timezone.utc) - timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    stale = (datetime.now(timezone.utc) - timedelta(days=90)).strftime("%Y-%m-%dT%H:%M:%SZ")
    pages = {
        1: [
            {"number": 1, "merged_at": fresh, "closed_at": fresh, "updated_at": fresh},
            {"number": 2, "merged_at": None, "closed_at": fresh, "updated_at": fresh},
            {"number": 3, "merged_at": None, "closed_at": stale, "updated_at": stale},
        ],
    }
    monkeypatch.setattr(
        sweep, "_get_json", lambda url, token=None, **_kw: pages.get(int(url[-1]), [])
    )
    closed = sweep.fetch_closed_pulls(None, REPOSITORY)
    assert [p["number"] for p in closed] == [1, 2, 3]
    assert [p["number"] for p in sweep.fetch_merged_pulls(None, REPOSITORY)] == [1]
