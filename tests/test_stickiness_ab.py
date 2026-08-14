"""Fixture-driven validation for `scripts/stickiness_ab.py` (task t7,
economy-discord-graphs plan, c42/h35).

t7's job is to leave the stickiness default exactly where the evidence puts
it — which means the comparison logic itself has to be provably correct
BEFORE it is trusted to render a verdict. This module is that proof: pure
unit tests pin `summarize`/`compare`'s arithmetic and decision table exactly
(no network, no clock — every input is hand-built), including the
`cache_convention` split (`uncached_input_tokens`'s own tests below) that a
live run against the real claude-code fleet forced into existence — the
first live measurement (task t7's run log) showed a single task's
`cached_input_tokens` EXCEEDING `input_tokens`, which is only possible if
Anthropic's `input_tokens` already excludes cache reads (additive
accounting) rather than including them (subset accounting, the convention
this module originally — and wrongly — assumed universal). One integration
test drives the harness's real HTTP client against a fake bridge (reusing
`tests/fake_api.FakeNodesAPI`, the same stdlib fake-server pattern the rest
of this suite already uses) to prove the cold and warm arms are measured
through the identical code path end to end.

None of this is a substitute for the live economy measurement — see the
delivery artifact (`docs/deliveries/2026-08-14-t7-stickiness-ab.md`) for
what was actually measured against the real backend, at what N, and why.

`scripts/` is not an importable package (no `__init__.py`, deliberately —
it holds standalone operator scripts, not library code), so the module
under test is loaded by path.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

import pytest

_SCRIPT_PATH = Path(__file__).resolve().parents[1] / "scripts" / "stickiness_ab.py"
_spec = importlib.util.spec_from_file_location("stickiness_ab", _SCRIPT_PATH)
assert _spec is not None
assert _spec.loader is not None
stickiness_ab = importlib.util.module_from_spec(_spec)
# Registered in sys.modules BEFORE exec: dataclasses (this module's own
# `@dataclass` fields use `from __future__ import annotations`) resolves a
# class's module via `sys.modules[cls.__module__]` while building it, so an
# unregistered module fails with a bare AttributeError at class-definition
# time rather than an import error.
sys.modules[_spec.name] = stickiness_ab
_spec.loader.exec_module(stickiness_ab)

AttemptRecord = stickiness_ab.AttemptRecord
uncached_input_tokens = stickiness_ab.uncached_input_tokens
summarize = stickiness_ab.summarize
compare = stickiness_ab.compare
BridgeClient = stickiness_ab.BridgeClient
run_cold_arm = stickiness_ab.run_cold_arm
run_warm_arm = stickiness_ab.run_warm_arm
render_markdown = stickiness_ab.render_markdown
load_tasks = stickiness_ab.load_tasks
CACHE_CONVENTION_ADDITIVE = stickiness_ab.CACHE_CONVENTION_ADDITIVE
CACHE_CONVENTION_SUBSET = stickiness_ab.CACHE_CONVENTION_SUBSET

# The whole existing test suite below was written against the SUBSET
# convention's arithmetic (uncached = input - cached) before the live run
# forced the additive/subset split into existence. Rather than rewrite every
# fixture number, `summarize` is called throughout via this SUBSET-pinned
# wrapper, so every existing assertion keeps testing exactly the arithmetic
# it always tested. Tests specific to the additive convention (the one
# empirically confirmed live) call `summarize`/`uncached_input_tokens`
# directly with `cache_convention=CACHE_CONVENTION_ADDITIVE`.


def _summarize(records, *, arm):
    return summarize(records, arm=arm, cache_convention=CACHE_CONVENTION_SUBSET)


def _record(
    task_id: str,
    arm: str,
    *,
    ok: bool = True,
    input_tokens: int | None = 1000,
    cached_input_tokens: int | None = 100,
    thread_id: str | None = "t-1",
    resumed: bool = False,
    wall_time_seconds: float = 1.0,
    error: str | None = None,
) -> AttemptRecord:
    return AttemptRecord(
        task_id=task_id,
        arm=arm,
        ok=ok,
        input_tokens=input_tokens,
        cached_input_tokens=cached_input_tokens,
        thread_id=thread_id,
        resumed=resumed,
        wall_time_seconds=wall_time_seconds,
        error=error,
    )


# --------------------------------------------------------------------------
# uncached_input_tokens — the cache_convention split
# --------------------------------------------------------------------------


def test_subset_convention_subtracts_cached_from_input():
    r = _record("t1", "cold", input_tokens=1000, cached_input_tokens=300)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_SUBSET) == 700


def test_additive_convention_uses_input_tokens_directly():
    # The Anthropic-confirmed shape: input_tokens is ALREADY exclusive of
    # cache reads, so the "uncached" count is input_tokens itself, with no
    # subtraction — subtracting again would double-discount the cache.
    r = _record("t1", "cold", input_tokens=1000, cached_input_tokens=300)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_ADDITIVE) == 1000


def test_additive_convention_handles_cached_exceeding_input():
    # This exact shape is what task t7's live run against the real
    # claude-code fleet actually produced: cached_input_tokens (36739)
    # larger than input_tokens for the same task — impossible under a
    # SUBSET reading (would go negative, as the original harness bug did),
    # unremarkable under ADDITIVE (the two counts are simply disjoint).
    r = _record("t1", "warm", input_tokens=6, cached_input_tokens=36739)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_ADDITIVE) == 6


def test_subset_convention_can_go_negative_which_is_exactly_the_original_bug():
    # Documents why ADDITIVE exists: feeding real Anthropic-shaped data
    # through the SUBSET formula produces a nonsensical negative "uncached"
    # count. This is not a desired behavior — it is the regression the live
    # run caught, pinned so nobody "fixes" ADDITIVE back into a subtraction.
    r = _record("t1", "warm", input_tokens=6, cached_input_tokens=36739)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_SUBSET) == 6 - 36739


def test_uncached_input_tokens_is_none_when_cache_telemetry_absent():
    # ADR 0009: null means unmeasurable, never "measured zero". A bridge
    # that reports no cache field at all must not have this harness assume
    # 0 cached (i.e. 100% uncached) on its behalf — true under either
    # convention.
    r = _record("t1", "cold", input_tokens=1000, cached_input_tokens=None)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_ADDITIVE) is None
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_SUBSET) is None


def test_uncached_input_tokens_is_none_when_input_tokens_absent():
    r = _record("t1", "cold", input_tokens=None, cached_input_tokens=100)
    assert uncached_input_tokens(r, cache_convention=CACHE_CONVENTION_ADDITIVE) is None


def test_uncached_input_tokens_rejects_an_unknown_convention():
    r = _record("t1", "cold")
    with pytest.raises(ValueError):
        uncached_input_tokens(r, cache_convention="not-a-real-convention")


def test_summarize_requires_cache_convention_to_be_given_explicitly():
    # No silently-safe default: which convention applies is a fact about
    # the BACKEND being measured, and guessing wrong makes the headline
    # number wrong while still looking like a real measurement.
    # Built outside the raises block so only `summarize` can satisfy it —
    # a TypeError out of `_record` would otherwise pass this test for the
    # wrong reason.
    records = [_record("t1", "cold")]
    with pytest.raises(TypeError):
        summarize(records, arm="cold")


# --------------------------------------------------------------------------
# summarize
# --------------------------------------------------------------------------


def test_summarize_computes_totals_and_average():
    records = [
        _record("t1", "cold", input_tokens=1000, cached_input_tokens=200, thread_id="s1"),
        _record("t2", "cold", input_tokens=800, cached_input_tokens=100, thread_id="s2"),
    ]
    summary = _summarize(records, arm="cold")
    assert summary.n == 2
    assert summary.measured_n == 2
    assert summary.failures == 0
    assert summary.total_uncached_input_tokens == 800 + 700  # (1000-200)+(800-100)
    assert summary.total_cached_input_tokens == 300
    assert summary.avg_uncached_input_tokens == pytest.approx(750.0)
    assert summary.distinct_sessions == 2


def test_summarize_excludes_unmeasured_tasks_from_the_average():
    records = [
        _record("t1", "cold", input_tokens=1000, cached_input_tokens=200),
        _record("t2", "cold", input_tokens=800, cached_input_tokens=None),  # unmeasurable
    ]
    summary = _summarize(records, arm="cold")
    assert summary.n == 2
    assert summary.measured_n == 1
    assert summary.avg_uncached_input_tokens == pytest.approx(800.0)  # only t1 counted


def test_summarize_reports_none_average_when_nothing_was_measured():
    records = [_record("t1", "cold", cached_input_tokens=None)]
    summary = _summarize(records, arm="cold")
    assert summary.measured_n == 0
    assert summary.avg_uncached_input_tokens is None
    assert summary.total_uncached_input_tokens is None


def test_summarize_counts_distinct_sessions_from_thread_id_not_from_n():
    # Three tasks, but the bridge (bug, misconfiguration, or genuinely one
    # persistent thread) only ever reported two distinct thread ids. The
    # session count must say 2, never assume 3-tasks-means-3-sessions.
    records = [
        _record("t1", "warm", thread_id="s1"),
        _record("t2", "warm", thread_id="s1"),
        _record("t3", "warm", thread_id="s2"),
    ]
    summary = _summarize(records, arm="warm")
    assert summary.n == 3
    assert summary.distinct_sessions == 2


def test_summarize_excludes_failed_attempts_from_the_average_but_not_from_counts():
    records = [
        _record("t1", "cold", input_tokens=1000, cached_input_tokens=100),
        _record(
            "t2",
            "cold",
            ok=False,
            input_tokens=50,
            cached_input_tokens=0,
            error="claude reported subtype=error_during_execution",
            wall_time_seconds=3.0,
        ),
    ]
    summary = _summarize(records, arm="cold")
    assert summary.n == 2
    assert summary.failures == 1
    assert summary.measured_n == 1  # only the successful task
    assert summary.avg_uncached_input_tokens == pytest.approx(900.0)
    assert summary.total_wall_time_seconds == pytest.approx(4.0)  # both tasks' wall time counts


def test_summarize_counts_resumed_only_when_the_request_actually_carried_a_ref():
    records = [
        _record("t1", "warm", resumed=False),  # first turn: nothing to resume yet
        _record("t2", "warm", resumed=True),
        _record("t3", "warm", resumed=False),  # e.g. colleague: bridge returned no ref to chain
    ]
    summary = _summarize(records, arm="warm")
    assert summary.resumed_n == 1


def test_summarize_applies_the_identical_formula_to_both_arm_labels():
    """The honesty condition this harness exists to satisfy: cold and warm
    are measured the SAME way. Build one set of records, summarize it once
    labelled 'cold' and once labelled 'warm', and assert every field but the
    label itself matches — proving `summarize` has no arm-specific branch.
    """
    base = [
        _record("t1", "cold", input_tokens=1000, cached_input_tokens=100, thread_id="a"),
        _record("t2", "cold", input_tokens=900, cached_input_tokens=50, thread_id="b"),
    ]
    relabelled = [
        AttemptRecord(**{**vars(r), "arm": "warm"}) for r in base  # noqa: SIM118 - dataclass fields
    ]
    cold_summary = _summarize(base, arm="cold")
    warm_summary = _summarize(relabelled, arm="warm")
    assert dataclasses_equal_ignoring_arm(cold_summary, warm_summary)


def dataclasses_equal_ignoring_arm(a, b) -> bool:
    import dataclasses as _dc

    fa = {k: v for k, v in _dc.asdict(a).items() if k != "arm"}
    fb = {k: v for k, v in _dc.asdict(b).items() if k != "arm"}
    return fa == fb


# --------------------------------------------------------------------------
# compare — the decision table, pinned branch by branch
# --------------------------------------------------------------------------


def test_compare_insufficient_data_when_an_arm_ran_zero_tasks():
    cold = _summarize([], arm="cold")
    warm = _summarize([_record("t1", "warm")], arm="warm")
    result = compare(cold, warm)
    assert result.verdict == stickiness_ab.VERDICT_INSUFFICIENT_DATA
    assert result.recommend_default_on is False
    assert result.reduction_fraction is None


def test_compare_insufficient_data_when_no_cache_telemetry_anywhere():
    cold = _summarize([_record("t1", "cold", cached_input_tokens=None)], arm="cold")
    warm = _summarize([_record("t1", "warm", cached_input_tokens=None)], arm="warm")
    result = compare(cold, warm)
    assert result.verdict == stickiness_ab.VERDICT_INSUFFICIENT_DATA
    assert result.recommend_default_on is False


def test_compare_insufficient_data_on_zero_baseline():
    # cold's average uncached input is 0 — a reduction fraction is
    # undefined, not "100% reduction".
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=100, cached_input_tokens=100) for i in range(10)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=100, cached_input_tokens=50) for i in range(10)
    ]
    cold = _summarize(cold_records, arm="cold")
    warm = _summarize(warm_records, arm="warm")
    assert cold.avg_uncached_input_tokens == 0
    result = compare(cold, warm)
    assert result.verdict == stickiness_ab.VERDICT_INSUFFICIENT_DATA
    assert result.reduction_fraction is None


def test_compare_no_reduction_when_warm_costs_the_same_or_more():
    # Spec risk s26 made real: a resumed long-history session can cost MORE
    # than a cold start once the provider's cache TTL has expired.
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=1000, cached_input_tokens=500) for i in range(10)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=1400, cached_input_tokens=200) for i in range(10)
    ]
    cold = _summarize(cold_records, arm="cold")
    warm = _summarize(warm_records, arm="warm")
    result = compare(cold, warm)
    assert result.verdict == stickiness_ab.VERDICT_NO_REDUCTION
    assert result.reduction_fraction < 0
    assert result.recommend_default_on is False


def test_compare_underpowered_positive_below_min_n():
    # A real, positive reduction — but only 3 tasks per arm, well under the
    # spec's own stated ten-task budget. Direction is not yet confidence.
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=1000, cached_input_tokens=100) for i in range(3)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=1000, cached_input_tokens=800) for i in range(3)
    ]
    cold = _summarize(cold_records, arm="cold")
    warm = _summarize(warm_records, arm="warm")
    result = compare(cold, warm, min_n=10)
    assert result.verdict == stickiness_ab.VERDICT_UNDERPOWERED_POSITIVE
    assert result.reduction_fraction > 0
    assert result.recommend_default_on is False


def test_compare_underpowered_positive_below_threshold_even_at_full_n():
    # min_n is satisfied, but the reduction itself is too small to call
    # meaningful (2% < the 10% default threshold).
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=1000, cached_input_tokens=100) for i in range(10)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=1000, cached_input_tokens=118) for i in range(10)
    ]
    cold = _summarize(cold_records, arm="cold")
    warm = _summarize(warm_records, arm="warm")
    result = compare(cold, warm, min_n=10, reduction_threshold=0.10)
    assert result.verdict == stickiness_ab.VERDICT_UNDERPOWERED_POSITIVE
    assert result.recommend_default_on is False


def test_compare_reduction_demonstrated_flips_the_recommendation():
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=1000, cached_input_tokens=100) for i in range(10)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=1000, cached_input_tokens=800) for i in range(10)
    ]
    cold = _summarize(cold_records, arm="cold")
    warm = _summarize(warm_records, arm="warm")
    result = compare(cold, warm, min_n=10, reduction_threshold=0.10)
    assert result.verdict == stickiness_ab.VERDICT_REDUCTION_DEMONSTRATED
    assert result.recommend_default_on is True
    assert result.reduction_fraction == pytest.approx(0.7777777, rel=1e-4)


def test_compare_reduction_demonstrated_under_additive_convention_too():
    # The decision table itself doesn't care which convention produced the
    # ArmSummary — only that BOTH arms went through the same one. Rebuild
    # the same scenario as the SUBSET version above but under ADDITIVE, with
    # numbers shaped like the real live data (small input_tokens, cache
    # sometimes exceeding it), and confirm the verdict math still holds.
    cold_records = [
        _record(f"c{i}", "cold", input_tokens=1000, cached_input_tokens=50) for i in range(10)
    ]
    warm_records = [
        _record(f"w{i}", "warm", input_tokens=100, cached_input_tokens=9000) for i in range(10)
    ]
    cold = summarize(cold_records, arm="cold", cache_convention=CACHE_CONVENTION_ADDITIVE)
    warm = summarize(warm_records, arm="warm", cache_convention=CACHE_CONVENTION_ADDITIVE)
    assert cold.avg_uncached_input_tokens == pytest.approx(1000.0)
    assert warm.avg_uncached_input_tokens == pytest.approx(100.0)
    result = compare(cold, warm, min_n=10, reduction_threshold=0.10)
    assert result.verdict == stickiness_ab.VERDICT_REDUCTION_DEMONSTRATED
    assert result.reduction_fraction == pytest.approx(0.9)


@pytest.mark.parametrize(
    "verdict",
    [
        stickiness_ab.VERDICT_INSUFFICIENT_DATA,
        stickiness_ab.VERDICT_UNDERPOWERED_POSITIVE,
        stickiness_ab.VERDICT_NO_REDUCTION,
    ],
)
def test_every_verdict_but_one_keeps_the_gate_opt_in(verdict):
    assert verdict in stickiness_ab._VERDICTS_KEEPING_OPT_IN
    assert (
        stickiness_ab.VERDICT_REDUCTION_DEMONSTRATED not in stickiness_ab._VERDICTS_KEEPING_OPT_IN
    )


# --------------------------------------------------------------------------
# load_tasks
# --------------------------------------------------------------------------


def test_load_tasks_reads_a_valid_file(tmp_path):
    path = tmp_path / "tasks.json"
    path.write_text(json.dumps([{"id": "t1", "instruction": "do the thing"}]))
    tasks = load_tasks(str(path))
    assert tasks == [{"id": "t1", "instruction": "do the thing"}]


def test_load_tasks_rejects_a_malformed_file(tmp_path):
    path = tmp_path / "tasks.json"
    path.write_text(json.dumps([{"id": "t1"}]))  # missing "instruction"
    with pytest.raises(ValueError):
        load_tasks(str(path))


# --------------------------------------------------------------------------
# render_markdown
# --------------------------------------------------------------------------


def test_render_markdown_includes_the_five_required_columns_and_the_verdict():
    cold = _summarize(
        [_record("t1", "cold", input_tokens=1000, cached_input_tokens=100)], arm="cold"
    )
    warm = _summarize(
        [_record("t1", "warm", input_tokens=1000, cached_input_tokens=100)], arm="warm"
    )
    result = compare(cold, warm)
    doc = render_markdown(result, meta={"run_id": "r1"})
    for column in (
        "uncached input tokens",
        "cached input tokens",
        "provider sessions",
        "failures",
        "wall time",
    ):
        assert column in doc
    assert result.verdict in doc
    assert "r1" in doc


# --------------------------------------------------------------------------
# Integration: the real HTTP client against a fake bridge (reuses the
# repo's own FakeNodesAPI pattern — stdlib http.server, no new dependency)
# --------------------------------------------------------------------------


def test_arms_driven_through_real_http_are_measured_the_same_way_and_compare_correctly(fake_api):
    """Proves the whole chain — BridgeClient -> run_cold_arm/run_warm_arm ->
    summarize -> compare — end to end over a real (loopback) HTTP round
    trip, against a fake bridge that speaks exactly the wire shape the real
    claude-code/codex bridges promise (usage.input_tokens,
    usage.cached_input_tokens, usage.thread_id, continuation_ref). Uses the
    SUBSET convention deliberately, to keep this fixture's numbers
    independent of the additive-vs-subset question this file's other tests
    already pin directly.

    This is a harness self-test with synthetic fixture data, NOT a claim
    about real provider economics — see the delivery artifact for the
    honest live-N accounting.
    """
    seen_bodies: list[dict] = []
    cold_session_counter = {"n": 0}

    def handle_invocation(handler, match, query, body):
        parsed = json.loads(body)
        seen_bodies.append(parsed)
        assert parsed["protocol_version"] == "1.0"
        assert parsed["input"]["async"] is False
        assert handler.headers.get("Idempotency-Key")  # required by the real protocol

        session_key = parsed["input"].get("session_key")
        continuation_ref = parsed.get("continuation_ref")

        if session_key == "warm-sk":
            if continuation_ref is None:
                # First turn of the resumed thread: genuinely a cold start,
                # so it costs as much as any other cold task.
                usage = {
                    "input_tokens": 1000,
                    "output_tokens": 50,
                    "cached_input_tokens": 100,
                    "thread_id": "warm-thread-1",
                }
            else:
                assert continuation_ref == "resume-token-abc"
                usage = {
                    "input_tokens": 400,
                    "output_tokens": 50,
                    "cached_input_tokens": 350,
                    "thread_id": "warm-thread-1",
                }
            handler.send_json(
                200,
                {
                    "outcome": "success",
                    "output": {"summary": "ok"},
                    "ledger_delta": {"records": []},
                    "artifact_refs": [],
                    "continuation_ref": "resume-token-abc",
                    "usage": usage,
                },
            )
            return

        # Cold arm: no session_key at all, always a fresh session.
        cold_session_counter["n"] += 1
        handler.send_json(
            200,
            {
                "outcome": "success",
                "output": {"summary": "ok"},
                "ledger_delta": {"records": []},
                "artifact_refs": [],
                # Cold arm never sends this back on its NEXT request (run_cold_arm
                # never reads it), even though the bridge honestly offers one —
                # exactly like a real bridge would on any successful turn.
                "continuation_ref": "offered-but-unused",
                "usage": {
                    "input_tokens": 1000,
                    "output_tokens": 50,
                    "cached_input_tokens": 100,
                    "thread_id": f"cold-thread-{cold_session_counter['n']}",
                },
            },
        )

    fake_api.route("POST", r"/v1/invocations", handle_invocation)
    fake_api.start()

    client = BridgeClient(fake_api.base_url)
    tasks = [{"id": f"t{i}", "instruction": f"do task {i}"} for i in range(10)]

    cold_records = run_cold_arm(client, tasks, repo="/tmp/repo", run_id="run-1")
    warm_records = run_warm_arm(
        client, tasks, repo="/tmp/repo", run_id="run-1", session_key="warm-sk"
    )

    cold_summary = _summarize(cold_records, arm="cold")
    warm_summary = _summarize(warm_records, arm="warm")

    # Cold arm: every task is its own fresh session at the same (high) cost.
    assert cold_summary.n == 10
    assert cold_summary.failures == 0
    assert cold_summary.distinct_sessions == 10
    assert cold_summary.resumed_n == 0
    assert cold_summary.avg_uncached_input_tokens == pytest.approx(900.0)

    # Warm arm: one persistent thread; only the first turn pays cold-start
    # price, the rest resume at a much lower uncached cost.
    assert warm_summary.n == 10
    assert warm_summary.distinct_sessions == 1
    assert warm_summary.resumed_n == 9  # every task but the first
    expected_avg = (900 + 50 * 9) / 10
    assert warm_summary.avg_uncached_input_tokens == pytest.approx(expected_avg)

    result = compare(cold_summary, warm_summary, min_n=10, reduction_threshold=0.10)
    assert result.verdict == stickiness_ab.VERDICT_REDUCTION_DEMONSTRATED
    assert result.recommend_default_on is True
    assert result.reduction_fraction == pytest.approx((900 - expected_avg) / 900)

    # `continuation_ref` rode top-level on the wire (ADR 0010 §1), never
    # nested under `input` — confirms the request shape the real bridges'
    # server.py handlers actually parse.
    warm_bodies = [b for b in seen_bodies if b["input"].get("session_key") == "warm-sk"]
    assert "continuation_ref" not in warm_bodies[0]["input"]
    assert warm_bodies[1]["continuation_ref"] == "resume-token-abc"


def test_a_failed_task_does_not_abort_the_rest_of_the_arm(fake_api):
    def handle_invocation(handler, match, query, body):
        parsed = json.loads(body)
        idx = int(parsed["input"]["instruction"].split()[-1])
        if idx == 1:
            handler.send_json(
                500,
                {
                    "error": "claude reported a provider capacity refusal",
                    "class": "capacity_exhausted",
                    "usage": {
                        "input_tokens": 20,
                        "output_tokens": 0,
                        "cached_input_tokens": 0,
                        "thread_id": "cold-thread-x",
                    },
                },
            )
            return
        handler.send_json(
            200,
            {
                "outcome": "success",
                "output": {"summary": "ok"},
                "ledger_delta": {"records": []},
                "artifact_refs": [],
                "continuation_ref": None,
                "usage": {
                    "input_tokens": 500,
                    "output_tokens": 10,
                    "cached_input_tokens": 50,
                    "thread_id": f"cold-thread-{idx}",
                },
            },
        )

    fake_api.route("POST", r"/v1/invocations", handle_invocation)
    fake_api.start()

    client = BridgeClient(fake_api.base_url)
    tasks = [{"id": f"t{i}", "instruction": f"do task {i}"} for i in range(3)]
    records = run_cold_arm(client, tasks, repo="/tmp/repo", run_id="run-1")

    assert len(records) == 3  # the failure did not stop the arm
    summary = _summarize(records, arm="cold")
    assert summary.failures == 1
    assert summary.measured_n == 2  # the failed task's usage is excluded from the average
