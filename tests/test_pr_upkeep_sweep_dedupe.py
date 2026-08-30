"""Finding-id emission dedupe (task t12, spec c7/h6).

The sweep's watermark says *the PR moved*; it does not say *this finding is
already being worked*. A push therefore re-emitted every still-open finding,
and each emission minted a fresh pr-upkeep run and a fresh human-merges-pr
approval — ``pr236-qodo-1`` had four running runs on prod. These tests pin
the second key: a finding id a still-running pr-upkeep run already carries
is not emitted again.

Split from test_pr_upkeep_sweep.py to keep that file under the 1000-line
hard limit (tests/lint filelength guard).
"""

import json
import urllib.error

import pytest

from tests.test_pr_upkeep_sweep import (  # noqa: F401
    EXAMPLE_DIR,
    FIXTURES,
    _reopen,
    _stub_sweep,
    sweep,
)

GRANT = json.dumps(
    {"cycle": 0, "repositories": [{"github_repo": "owner/repo", "sonar_component": "owner_repo"}]}
)

#: The three open findings the reopened PR #35 review body yields when it is
#: read as PR 236's review — the prod PR whose findings piled up four runs.
PR = 236
ALL_FINDINGS = ["pr236-qodo-1", "pr236-qodo-2", "pr236-qodo-3"]


@pytest.fixture(autouse=True)
def repository_grant(monkeypatch):
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", GRANT)


def _qodo_comment():
    body = _reopen((FIXTURES / "qodo-pr35-code-review.body.txt").read_text())
    return {"user": {"login": "qodo-code-review[bot]"}, "body": body}


def _pass(monkeypatch, *, head_sha, running_findings=()):
    """Stub one sweep cycle over PR 236 at `head_sha`; return its call log."""
    return _stub_sweep(
        monkeypatch,
        pulls=[{"number": PR, "head_sha": head_sha}],
        sonar_main={"issues": []},
        comments={PR: [_qodo_comment()]},
        running_findings=running_findings,
    )


def _emitted_finding_ids(calls):
    return [[f["id"] for f in payload["findings"]] for _n, payload, _k, _w in calls["events"]]


def test_a_moved_watermark_alone_re_emits_every_open_finding(monkeypatch):
    """The behaviour the dedupe is layered onto: two head SHAs, two events."""
    first = _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    second = _pass(monkeypatch, head_sha="sha-b")
    assert sweep.main() == 0
    assert _emitted_finding_ids(first) == [ALL_FINDINGS]
    assert _emitted_finding_ids(second) == [ALL_FINDINGS], "watermark logic is unchanged"


def test_findings_a_running_run_carries_are_not_re_emitted_on_a_new_head_sha(monkeypatch):
    first = _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    assert _emitted_finding_ids(first) == [ALL_FINDINGS]

    # That emission minted a run; it is still running when the PR is pushed.
    second = _pass(monkeypatch, head_sha="sha-b", running_findings=ALL_FINDINGS)
    assert sweep.main() == 0
    assert second["events"] == [], "one open finding, one running run, one event"


def test_a_finding_no_running_run_carries_still_emits(monkeypatch):
    calls = _pass(monkeypatch, head_sha="sha-a", running_findings=["pr999-qodo-1"])
    assert sweep.main() == 0
    assert _emitted_finding_ids(calls) == [ALL_FINDINGS]


def test_a_new_finding_beside_an_in_flight_one_emits_only_the_new_one(monkeypatch):
    calls = _pass(monkeypatch, head_sha="sha-b", running_findings=["pr236-qodo-1"])
    assert sweep.main() == 0
    assert _emitted_finding_ids(calls) == [["pr236-qodo-2", "pr236-qodo-3"]]


def test_the_stdout_summary_names_every_skipped_finding(monkeypatch, capsys):
    _pass(monkeypatch, head_sha="sha-b", running_findings=ALL_FINDINGS)
    assert sweep.main() == 0
    report = json.loads(capsys.readouterr().out)
    assert report["skipped_findings"] == ALL_FINDINGS
    assert report["emitted"] == 0


def test_a_pr_with_no_findings_at_all_is_unaffected(monkeypatch):
    """An empty finding list is not a skip: nothing was deduped away, so the
    fact still goes out exactly as before (the trigger declines it anyway)."""
    calls = _stub_sweep(
        monkeypatch,
        pulls=[{"number": PR, "head_sha": "sha-a"}],
        sonar_main={"issues": []},
        running_findings=ALL_FINDINGS,
    )
    assert sweep.main() == 0
    assert _emitted_finding_ids(calls) == [[]]


class TestUndispatchedFindings:
    def test_splits_by_id_preserving_order(self):
        findings = [{"id": "a"}, {"id": "b"}, {"id": "c"}]
        kept, skipped = sweep.undispatched_findings(findings, {"b"})
        assert kept == [{"id": "a"}, {"id": "c"}]
        assert skipped == ["b"]

    def test_an_empty_running_set_keeps_everything(self):
        findings = [{"id": "a"}]
        assert sweep.undispatched_findings(findings, set()) == (findings, [])


class TestFetchRunningFindingIds:
    def test_asks_only_for_running_pr_upkeep_runs(self, monkeypatch):
        monkeypatch.setenv("NODES_API_URL", "https://nodes.example/")
        monkeypatch.setenv("NODES_EVENT_TOKEN", "event-token")
        seen = {}

        def fake_get(url, token=None, **_kw):
            seen["url"], seen["token"] = url, token
            return {
                "items": [
                    {"input": {"findings": [{"id": "a"}, {"id": "b"}]}},
                    {"input": {"findings": [{"id": "b"}]}},
                    {"input": {"findings": []}},
                    {"input": {}},
                    {},
                ]
            }

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        assert sweep.fetch_running_finding_ids() == {"a", "b"}
        assert seen["url"].startswith("https://nodes.example/v1alpha1/runs?")
        assert "workflow_key=pr-upkeep" in seen["url"]
        assert "state=running" in seen["url"]
        assert seen["token"] == "event-token"

    def test_a_non_object_input_is_ignored_rather_than_crashing(self, monkeypatch):
        monkeypatch.setenv("NODES_API_URL", "https://nodes.example")
        monkeypatch.setenv("NODES_EVENT_TOKEN", "t")
        monkeypatch.setattr(
            sweep,
            "_get_json",
            lambda *_a, **_kw: {
                "items": [{"input": ["not an object"]}, {"input": {"findings": {}}}]
            },
        )
        assert sweep.fetch_running_finding_ids() == set()

    def test_requires_the_same_grant_raise_event_requires(self, monkeypatch):
        monkeypatch.delenv("NODES_API_URL", raising=False)
        monkeypatch.delenv("NODES_EVENT_TOKEN", raising=False)
        with pytest.raises(ValueError):
            sweep.fetch_running_finding_ids()


def test_an_unreadable_runs_list_names_its_stage_and_emits_nothing(monkeypatch, capsys):
    """Degrading to "emit everything" would restore the duplicate-approval bug
    silently. The sweep fails loudly instead, like every other read surface."""
    calls = _pass(monkeypatch, head_sha="sha-a")

    def boom():
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(sweep, "fetch_running_finding_ids", boom)
    assert sweep.main() == 1
    assert "running pr-upkeep runs" in capsys.readouterr().err
    assert calls["events"] == []
