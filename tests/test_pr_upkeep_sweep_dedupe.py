"""Finding-id emission dedupe (task t12, spec c7/h6) and one-finding-per-fact
dispatch (issue #268).

The sweep's watermark says *the PR moved*; it does not say *this finding is
already being worked*. A push therefore re-emitted every still-open finding,
and each emission minted a fresh pr-upkeep run and a fresh human-merges-pr
approval — ``pr236-qodo-1`` had four running runs on prod. These tests pin
the second key: a finding id a still-running pr-upkeep run already carries
is not emitted again.

They also pin what #268 fixed on top of it. The dedupe reads the WHOLE
``input.findings`` list off a running run, and a fact used to carry every
surviving finding while the fix node worked exactly one of them — so one run
parked on ``human-merges-pr`` suppressed findings it had never touched, and a
PR could only get one fix per merge. A fact now carries the single finding
that will be worked, and names it in the watermark so the next one is not
answered ``duplicate=true`` at an unchanged head SHA.

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
    _reopen,
    _stub_sweep,
    sweep,
)

emit = importlib.import_module("pr_upkeep_emit")

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


def _pass(monkeypatch, *, head_sha, running_findings=(), worked_by_head=None):
    """Stub one sweep cycle over PR 236 at `head_sha`; return its call log."""
    return _stub_sweep(
        monkeypatch,
        pulls=[{"number": PR, "head_sha": head_sha}],
        sonar_main={"issues": []},
        comments={PR: [_qodo_comment()]},
        running_findings=running_findings,
        worked_by_head=worked_by_head,
    )


def _emitted_finding_ids(calls):
    return [[f["id"] for f in payload["findings"]] for _n, payload, _k, _w in calls["events"]]


def test_a_fact_carries_exactly_the_one_finding_it_dispatches(monkeypatch):
    """Issue #268. Three open findings, one fact, one finding on it — the one
    the fix node's instruction says it will work. Carrying all three is what
    let a single parked run suppress the two it never touched."""
    first = _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    second = _pass(monkeypatch, head_sha="sha-b")
    assert sweep.main() == 0
    assert _emitted_finding_ids(first) == [[ALL_FINDINGS[0]]]
    assert _emitted_finding_ids(second) == [[ALL_FINDINGS[0]]], "watermark logic is unchanged"


def test_the_second_finding_is_dispatched_while_the_first_run_is_still_parked(monkeypatch):
    """The whole of #268: the first run parks on the human approval node and
    stays running until a person merges. The second finding must not wait for
    that — one tick later it is the finding the next fact carries."""
    first = _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    assert _emitted_finding_ids(first) == [["pr236-qodo-1"]]

    second = _pass(monkeypatch, head_sha="sha-a", running_findings=["pr236-qodo-1"])
    assert sweep.main() == 0
    assert _emitted_finding_ids(second) == [["pr236-qodo-2"]]

    # ...and its watermark differs from the first one's, or the control plane
    # would answer this identical (source_key, watermark) with duplicate=true
    # and never mint the run (internal/store/postgres/signal.go).
    assert first["events"][0][3] != second["events"][0][3]
    assert second["events"][0][3]["finding"] == "pr236-qodo-2"


def test_findings_a_running_run_carries_are_not_re_emitted_on_a_new_head_sha(monkeypatch):
    first = _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    assert _emitted_finding_ids(first) == [["pr236-qodo-1"]]

    # That emission minted a run; it is still running when the PR is pushed,
    # and every remaining finding is in flight too.
    second = _pass(monkeypatch, head_sha="sha-b", running_findings=ALL_FINDINGS)
    assert sweep.main() == 0
    assert second["events"] == [], "nothing left to dispatch, nothing emitted"


def test_a_finding_no_running_run_carries_still_emits(monkeypatch):
    calls = _pass(monkeypatch, head_sha="sha-a", running_findings=["pr999-qodo-1"])
    assert sweep.main() == 0
    assert _emitted_finding_ids(calls) == [[ALL_FINDINGS[0]]]


def test_a_new_finding_beside_an_in_flight_one_emits_only_the_new_one(monkeypatch):
    calls = _pass(monkeypatch, head_sha="sha-b", running_findings=["pr236-qodo-1"])
    assert sweep.main() == 0
    assert _emitted_finding_ids(calls) == [["pr236-qodo-2"]]


def test_the_stdout_summary_names_every_skipped_finding(monkeypatch, capsys):
    _pass(monkeypatch, head_sha="sha-b", running_findings=ALL_FINDINGS)
    assert sweep.main() == 0
    report = json.loads(capsys.readouterr().out)
    assert report["skipped_findings"] == ALL_FINDINGS
    assert report["emitted"] == 0


def test_the_summary_separates_deferred_findings_from_suppressed_ones(monkeypatch, capsys):
    """A finding that lost the priority ordering is emittable next cycle; a
    finding a running run holds is not. Reporting both as `skipped` would say
    the sweep is waiting on work when it is only taking its turn."""
    _pass(monkeypatch, head_sha="sha-a", running_findings=["pr236-qodo-2"])
    assert sweep.main() == 0
    report = json.loads(capsys.readouterr().out)
    assert report["skipped_findings"] == ["pr236-qodo-2"]
    assert report["deferred_findings"] == ["pr236-qodo-3"]
    assert report["emitted"] == 1


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
    # ...and with the two-key watermark it has always had, so the rollout of
    # #268 does not re-deliver every clean PR's current head as a new fact.
    assert calls["events"][0][3] == {"head_sha": "sha-a", "newest_comment_at": ""}


class TestEmissionWatermark:
    """Issue #268: the cursor has to distinguish two findings on one unmoved
    PR, because the control plane's duplicate check is an equality test of the
    whole watermark against the row stored for this source key."""

    def test_names_the_finding_being_dispatched(self):
        assert sweep.emission_watermark("sha", "2026-08-30T20:00:00Z", [{"id": "pr1-qodo-2"}]) == {
            "head_sha": "sha",
            "newest_comment_at": "2026-08-30T20:00:00Z",
            "finding": "pr1-qodo-2",
        }

    def test_two_findings_at_one_head_sha_get_different_watermarks(self):
        first = sweep.emission_watermark("sha", "t", [{"id": "a"}])
        second = sweep.emission_watermark("sha", "t", [{"id": "b"}])
        assert first != second

    def test_a_findingless_pr_keeps_the_watermark_it_always_had(self):
        assert sweep.emission_watermark("sha", "t", []) == {
            "head_sha": "sha",
            "newest_comment_at": "t",
        }


class TestUndispatchedFindings:
    def test_splits_by_id_preserving_order(self):
        findings = [{"id": "a"}, {"id": "b"}, {"id": "c"}]
        kept, skipped, worked = emit.undispatched_findings(findings, {"b"})
        assert kept == [{"id": "a"}, {"id": "c"}]
        assert skipped == ["b"]
        assert worked == []

    def test_an_empty_running_set_keeps_everything(self):
        findings = [{"id": "a"}]
        assert emit.undispatched_findings(findings, set()) == (findings, [], [])

    def test_a_finding_worked_at_this_head_is_refused_separately(self):
        findings = [{"id": "a"}, {"id": "b"}]
        kept, skipped, worked = emit.undispatched_findings(findings, {"a"}, {"b"})
        assert kept == []
        assert (skipped, worked) == (["a"], ["b"])

    def test_in_flight_wins_when_a_finding_is_both(self):
        """The more actionable state: it may be waiting on a human right now."""
        kept, skipped, worked = emit.undispatched_findings([{"id": "a"}], {"a"}, {"a"})
        assert (kept, skipped, worked) == ([], ["a"], [])


class TestDispatchedFindingIds:
    """The listing -> the two dedupe clauses. Pure: no network in this module."""

    LISTED = {
        "items": [
            # working it now, at the current head
            {"state": "running", "input": {"head_sha": "sha-a", "findings": [{"id": "a"}]}},
            # ENDED at the current head — clause 2's whole population
            {"state": "completed", "input": {"head_sha": "sha-a", "findings": [{"id": "b"}]}},
            # ended at an OLDER head: says nothing about sha-a
            {"state": "failed", "input": {"head_sha": "sha-old", "findings": [{"id": "c"}]}},
            # running at an older head: still in flight, so still refused
            {"state": "running", "input": {"head_sha": "sha-old", "findings": [{"id": "d"}]}},
            {"input": {"findings": []}},
            {"input": {}},
            {"input": ["not an object"]},
            {},
        ]
    }

    def test_in_flight_is_every_running_run_regardless_of_head(self):
        in_flight, _ = emit.dispatched_finding_ids(self.LISTED)
        assert in_flight == {"a", "d"}

    def test_worked_is_keyed_by_the_head_it_was_dispatched_against(self):
        _, by_head = emit.dispatched_finding_ids(self.LISTED)
        assert by_head == {"sha-a": {"a", "b"}, "sha-old": {"c", "d"}}

    def test_a_malformed_or_absent_listing_is_empty_rather_than_raising(self):
        for listing in (None, {}, {"items": None}, {"items": []}):
            assert emit.dispatched_finding_ids(listing) == (set(), {})

    def test_the_query_does_not_filter_to_running_runs(self):
        """Clause 2 asks about runs that have ENDED; a state=running listing
        cannot see them, which is exactly how the loop got in."""
        query = emit.runs_query()
        assert "workflow_key=pr-upkeep" in query
        assert "state" not in query
        assert "limit=500" in query
        assert "cursor" not in query

    def test_a_cursor_is_handed_back_url_encoded_and_unparsed(self):
        """`next_cursor` is opaque and the control plane refuses anything it
        did not mint, so the only safe handling is pass-through."""
        query = emit.runs_query(cursor="b3Bh/cXVl+1")
        assert "cursor=b3Bh%2FcXVl%2B1" in query
        assert "workflow_key=pr-upkeep" in query

    def test_the_pages_of_one_listing_are_read_as_one_population(self):
        """A page boundary is an artefact of the read, not a fact about a
        finding: the run that already worked one may sit on any page."""
        pages = [
            {
                "items": [
                    {"state": "running", "input": {"head_sha": "sha-a", "findings": [{"id": "a"}]}}
                ]
            },
            {
                "items": [
                    {
                        "state": "completed",
                        "input": {"head_sha": "sha-a", "findings": [{"id": "b"}]},
                    }
                ]
            },
        ]
        assert emit.dispatched_finding_ids(pages) == ({"a"}, {"sha-a": {"a", "b"}})

    def test_a_malformed_page_is_skipped_rather_than_raising(self):
        assert emit.dispatched_finding_ids([None, "not a page", {}]) == (set(), {})


class TestNextRunCursor:
    """Where the next page is, or that there is none."""

    def test_a_listing_with_a_cursor_asks_for_the_next_page(self):
        assert emit.next_run_cursor({"items": [], "next_cursor": "cur-2"}) == "cur-2"

    def test_an_exhausted_or_unreadable_listing_ends_the_walk(self):
        for listing in (None, {}, {"next_cursor": ""}, {"next_cursor": 7}, []):
            assert emit.next_run_cursor(listing) == ""


class TestFetchDispatchedFindings:
    def test_reads_the_run_listing_with_the_event_grant(self, monkeypatch):
        monkeypatch.setenv("NODES_API_URL", "https://nodes.example/")
        monkeypatch.setenv("NODES_EVENT_TOKEN", "event-token")
        seen = {}

        def fake_get(url, token=None, **_kw):
            seen["url"], seen["token"] = url, token
            return {"items": [{"state": "running", "input": {"findings": [{"id": "a"}]}}]}

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        assert sweep.fetch_dispatched_findings() == ({"a"}, {}, False)
        assert seen["url"].startswith("https://nodes.example/v1alpha1/runs?")
        assert "workflow_key=pr-upkeep" in seen["url"]
        assert seen["token"] == "event-token"

    def test_follows_next_cursor_until_the_listing_ends(self, monkeypatch):
        """The bug this closes: an ended run that has fallen off the newest
        page stops suppressing its finding, and the answer is re-bought."""
        monkeypatch.setenv("NODES_API_URL", "https://nodes.example/")
        monkeypatch.setenv("NODES_EVENT_TOKEN", "event-token")
        pages = [
            {
                "items": [
                    {"state": "running", "input": {"head_sha": "sha-a", "findings": [{"id": "a"}]}}
                ],
                "next_cursor": "cur-2",
            },
            {
                "items": [
                    {
                        "state": "completed",
                        "input": {"head_sha": "sha-a", "findings": [{"id": "b"}]},
                    }
                ],
            },
        ]
        urls = []

        def fake_get(url, token=None, **_kw):
            urls.append(url)
            return pages[len(urls) - 1]

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        assert sweep.fetch_dispatched_findings() == ({"a"}, {"sha-a": {"a", "b"}}, False)
        assert len(urls) == 2
        assert "cursor" not in urls[0]
        assert "cursor=cur-2" in urls[1]

    def test_a_listing_that_never_ends_stops_at_the_page_bound_and_says_so(
        self, monkeypatch, capsys
    ):
        """Past the bound the dedupe reads a window, not the population. The
        failure direction is re-emission, which is the one that costs money,
        so it is reported rather than swallowed."""
        monkeypatch.setenv("NODES_API_URL", "https://nodes.example/")
        monkeypatch.setenv("NODES_EVENT_TOKEN", "event-token")
        urls = []

        def fake_get(url, token=None, **_kw):
            urls.append(url)
            return {"items": [], "next_cursor": f"cur-{len(urls)}"}

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        # The third element is the truncation fact itself: stderr is for
        # whoever opens the node output, and this is for whatever reads the
        # report.
        assert sweep.fetch_dispatched_findings() == (set(), {}, True)
        assert len(urls) == emit.RUNS_MAX_PAGES
        assert "were NOT read this cycle" in capsys.readouterr().err

    def test_requires_the_same_grant_raise_event_requires(self, monkeypatch):
        monkeypatch.delenv("NODES_API_URL", raising=False)
        monkeypatch.delenv("NODES_EVENT_TOKEN", raising=False)
        with pytest.raises(ValueError):
            sweep.fetch_dispatched_findings()


def test_an_unreadable_runs_list_names_its_stage_and_emits_nothing(monkeypatch, capsys):
    """Degrading to "emit everything" would restore the duplicate-approval bug
    silently. The sweep fails loudly instead, like every other read surface."""
    calls = _pass(monkeypatch, head_sha="sha-a")

    def boom(*_args, **_kwargs):
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(sweep, "fetch_dispatched_findings", boom)
    assert sweep.main() == 1
    assert "pr-upkeep runs" in capsys.readouterr().err
    assert calls["events"] == []


class TestAFindingWorkedAtThisHeadIsNotDispatchedAgain:
    """The regression #268's own fix introduced, and the clause that closes it.

    Before #268 the watermark was `{head_sha, newest_comment_at}` — byte
    identical on every tick — so the control plane answered every repeat with
    `duplicate=true`. That accident was the only thing stopping a finding
    being re-dispatched at a commit where it had already been worked. Putting
    the dispatched finding id in the watermark (which #268 had to do, or a
    PR's second finding could never go out at an unmoved head) removed the
    accident: two findings that both end `no_change` would then alternate
    forever, one billable agent session every 30 minutes, each re-working a
    finding an actor had already declined.
    """

    def test_two_ended_runs_at_one_head_do_not_alternate_forever(self, monkeypatch):
        first = _pass(monkeypatch, head_sha="sha-a")
        assert sweep.main() == 0
        assert _emitted_finding_ids(first) == [["pr236-qodo-1"]]

        second = _pass(monkeypatch, head_sha="sha-a", running_findings=["pr236-qodo-1"])
        assert sweep.main() == 0
        assert _emitted_finding_ids(second) == [["pr236-qodo-2"]]

        # Both runs have now ENDED — say both answered `no_change`, so both
        # findings are still open on the source surface. Nothing about the PR
        # has changed, so nothing new may be dispatched against it.
        third = _pass(
            monkeypatch,
            head_sha="sha-a",
            worked_by_head={"sha-a": ["pr236-qodo-1", "pr236-qodo-2", "pr236-qodo-3"]},
        )
        assert sweep.main() == 0
        assert third["events"] == [], "a finding worked at this head was dispatched again"

    def test_a_push_makes_them_dispatchable_again(self, monkeypatch):
        """The refusal is scoped to the commit, not to the finding forever:
        new code is a new question, so a moved head re-opens all of them."""
        calls = _pass(
            monkeypatch,
            head_sha="sha-b",
            worked_by_head={"sha-a": ["pr236-qodo-1", "pr236-qodo-2", "pr236-qodo-3"]},
        )
        assert sweep.main() == 0
        assert _emitted_finding_ids(calls) == [["pr236-qodo-1"]]

    def test_the_summary_tells_settled_apart_from_in_flight(self, monkeypatch, capsys):
        _pass(
            monkeypatch,
            head_sha="sha-a",
            running_findings=["pr236-qodo-1"],
            worked_by_head={"sha-a": ["pr236-qodo-2", "pr236-qodo-3"]},
        )
        assert sweep.main() == 0
        report = json.loads(capsys.readouterr().out)
        assert report["skipped_findings"] == ["pr236-qodo-1"]
        assert report["worked_findings"] == ["pr236-qodo-2", "pr236-qodo-3"]
        assert report["emitted"] == 0


def test_a_waiting_or_unknown_state_counts_as_in_flight():
    """Qodo finding 2, and clause 1's safe direction. `waiting` is nonterminal
    (a run frozen behind its ticket reaches it), and a state a future control
    plane adds must not land on the permissive side: a finding wrongly held
    back is visible in `skipped_findings`, a finding wrongly released is a
    second billable session and a second approval nobody sees."""
    listed = {
        "items": [
            {"state": "waiting", "input": {"head_sha": "s", "findings": [{"id": "w"}]}},
            {"state": "created", "input": {"head_sha": "s", "findings": [{"id": "n"}]}},
            {"state": "from_the_future", "input": {"head_sha": "s", "findings": [{"id": "f"}]}},
            {"state": "completed", "input": {"head_sha": "s", "findings": [{"id": "c"}]}},
            {"state": "cancelled", "input": {"head_sha": "s", "findings": [{"id": "x"}]}},
        ]
    }
    in_flight, by_head = emit.dispatched_finding_ids(listed)
    assert in_flight == {"w", "n", "f"}, "only completed/failed/cancelled are over"
    assert by_head == {"s": {"w", "n", "f", "c", "x"}}


def test_a_finding_worked_in_another_repository_does_not_suppress_this_one():
    """Qodo finding 4: finding ids carry a PR number and an index, never a
    repository — so a fork and its upstream, which share commit shas by
    construction, would otherwise answer each other's questions."""
    listed = {
        "items": [
            {
                "state": "completed",
                "input": {
                    "repository": "other/repo",
                    "head_sha": "sha-a",
                    "findings": [{"id": "pr236-qodo-1"}],
                },
            },
            {
                "state": "completed",
                "input": {
                    "repository": "owner/repo",
                    "head_sha": "sha-a",
                    "findings": [{"id": "pr236-qodo-2"}],
                },
            },
            # No repository declared: kept, because it cannot be ruled out and
            # for a dedupe the safe direction is to suppress.
            {
                "state": "running",
                "input": {"head_sha": "sha-a", "findings": [{"id": "pr9-qodo-1"}]},
            },
        ]
    }
    in_flight, by_head = emit.dispatched_finding_ids(listed, "owner/repo")
    assert in_flight == {"pr9-qodo-1"}
    assert by_head == {"sha-a": {"pr236-qodo-2", "pr9-qodo-1"}}


def test_the_summary_says_whether_the_dedupe_read_the_whole_population(monkeypatch, capsys):
    """A degradation only stderr can see is one that gets noticed the month
    after it starts costing. `dedupe_complete` puts it in the report an
    operator's tooling reads."""
    _pass(monkeypatch, head_sha="sha-a")
    assert sweep.main() == 0
    assert json.loads(capsys.readouterr().out)["dedupe_complete"] is True
