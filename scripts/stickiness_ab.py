#!/usr/bin/env python3
"""Stickiness A/B harness — task t7 (economy-discord-graphs plan, c42/h35).

Answers one question with recorded evidence instead of belief: does
resuming a session (§13.1's ``continuation_ref``, shipped in task t5) reduce
**uncached** input tokens compared to a fresh session per task? Provider
cache TTLs mean the honest answer could go either way — a resumed
long-history session can cost MORE than a cold start (spec s26) — so this
harness measures rather than assumes.

## What this drives, and what it does not

This script speaks the actor protocol's HTTP wire surface directly
(``POST /v1/invocations``, PRD §13.1/§13.2) against a *running* bridge
process, from OUTSIDE that process. It never imports, patches, or shells
into ``adapters/*/src`` — a sibling task (t25) edits all three bridges
concurrently, and this harness's whole point is to be a client of whatever
they expose on the wire, unchanged. Point it at any bridge speaking the
protocol (claude-code, codex; colleague honestly cannot serve the resumed
arm today — its bridge hardcodes ``continuation_ref: null``, ADR 0010's
consequence note, no upstream resume verb).

## Sync or async — the harness follows whichever the bridge picks

Every request sets ``input.async: false`` AND carries a real callback block
(``CallbackReceiver`` below opens a tiny loopback HTTP listener once per
run). Most bridge configs treat the flag as a hint and answer inline
(HTTP 200); a bridge configured ``always_async: true`` (the real deployed
claude-code bridges on this fleet run this way) ignores the hint outright
and answers 202, and the harness then blocks on its own callback listener
for the terminal ``completed``/``failed`` event instead. Either path yields
the identical `InvocationOutcome` shape to the rest of this module — a task
run against a synchronous bridge and one run against an always-async bridge
are summarized exactly the same way.

## The two arms, measured identically

Both arms send the SAME task instructions through the SAME request shape:

* **cold** — one fresh session per task: no ``session_key``, no
  ``continuation_ref`` on the way in.
* **warm** — one thread, chained across every task in order: a shared
  ``session_key`` (the transport field task t5 excludes from every bridge's
  Bound-inputs block) plus ``continuation_ref`` carried forward from each
  reply's own ``continuation_ref`` into the next request.

Both arms are summarized by the exact same function (`summarize`) — the
harness never applies a different formula to the arm it hopes wins.

## What honesty requires here

* ``cached_input_tokens`` is `None` (not 0) whenever a bridge did not report
  it (ADR 0009: null means "unmeasurable", never "measured zero"). A task
  missing that field is excluded from the uncached-average, not folded in
  as if it were fully uncached or fully cached — see `summarize`'s
  ``measured_n`` bookkeeping.
* Session count is the number of DISTINCT ``usage.thread_id`` values a arm's
  replies actually reported — never assumed from the arm's shape. A cold
  arm that (bug, misconfiguration, whatever) reused one thread anyway is
  visible in this number instead of hidden by an "N tasks = N sessions"
  assumption.
* A warm-arm request only counts as "resumed" when the PRIOR reply actually
  returned a ``continuation_ref`` to carry forward — a backend that honestly
  returns null (colleague) makes every warm-arm task after the first
  degrade to a cold dispatch, and `summarize` reports that degradation
  (``resumed_n``) rather than silently pretending the chain held.
* `compare`'s verdict is `"insufficient_data"` whenever either arm has zero
  tasks, zero measured-cache tasks, or fewer than `min_n` tasks — never a
  verdict computed from a sample too thin to trust. See its docstring for
  the full decision table.

## Running it for real

    python3 scripts/stickiness_ab.py \\
        --base-url http://127.0.0.1:8765 \\
        --repo /path/to/target/repo \\
        --tasks tasks.json \\
        --out docs/deliveries/DATE-t7-stickiness-ab.md

``tasks.json`` is a JSON list of ``{"id": "...", "instruction": "..."}``
objects (see `load_tasks`). ``--auth-token`` sets the bridge's
``Authorization: Bearer`` header when the bridge requires one.

This module has no third-party dependencies (stdlib `urllib` only), matching
this repo's `culture_nodes` package convention — an operator can run it with
nothing but the Python already on the machine.
"""

from __future__ import annotations

import argparse
import dataclasses
import http.server
import json
import queue
import threading
import time
import urllib.error
import urllib.request
import uuid
from typing import Any

PROTOCOL_VERSION = "1.0"

#: The spec's own imagined budget (docs/plans/2026-08-13-economy-discord-
#: graphs.md, task t7: "ten representative tasks") — used as the minimum
#: per-arm sample size `compare` requires before it will call a reduction
#: (or its absence) anything stronger than "insufficient data". Not a
#: statistical claim of significance; a floor below which this harness
#: refuses to pretend confidence it doesn't have.
DEFAULT_MIN_N = 10

#: The reduction fraction (cold_avg - warm_avg) / cold_avg must clear, at
#: min_n or above, before `compare` calls it "reduction_demonstrated" rather
#: than "no_reduction_or_regression". A resumed session that saves a
#: token or two is not the load-bearing claim c3/h2 need; a resumed session
#: that measurably changes the cost picture is.
DEFAULT_REDUCTION_THRESHOLD = 0.10


# --------------------------------------------------------------------------
# Wire client
# --------------------------------------------------------------------------


class BridgeError(Exception):
    """Raised when the bridge is unreachable or answers something the
    harness cannot parse as JSON — never raised for an ordinary domain
    failure (§13.5 execution/timeout/etc.), which is recorded as a failed
    `AttemptRecord` instead so one bad task doesn't abort the whole arm."""


@dataclasses.dataclass(frozen=True)
class InvocationOutcome:
    """One HTTP round trip's parsed result — the raw shape the wire
    returned, before `summarize` turns a list of these into an arm's
    aggregate numbers."""

    ok: bool
    status_code: int
    input_tokens: int | None
    cached_input_tokens: int | None
    thread_id: str | None
    continuation_ref: str | None
    error: str | None
    #: §13.2's own `usage.cost` (USD, when the actor prices its work — claude
    #: does; codex and colleague do not, per their bridges' own mapping.py).
    #: Not one of the task's five required columns, but already on the wire
    #: and, per task t7's own live run, the metric that actually SHOWS the
    #: resume effect when `uncached_input_tokens` cannot (see
    #: `CACHE_CONVENTION_ADDITIVE`'s docstring and the delivery artifact) —
    #: recorded alongside the required five for exactly that reason.
    cost_usd: float | None


class CallbackReceiver:
    """A minimal local §13.4 callback endpoint (stdlib ``http.server``, one
    per harness run) so `BridgeClient` can drive a bridge that dispatches
    asynchronously — whether because it decided to for this invocation, or
    because its config forces ``always_async: true`` regardless of what the
    request asked for, as the real deployed claude-code bridges do today.

    One receiver serves every invocation in a run, sequentially — this
    harness never has more than one invocation in flight at a time (both
    arms dispatch one task, wait for its outcome, then dispatch the next),
    so there is no need to correlate callback events by invocation id: the
    next terminal event this receiver sees IS the answer to the invocation
    the harness is currently waiting on.
    """

    def __init__(self) -> None:
        self.token = uuid.uuid4().hex
        self._queue: queue.Queue[dict[str, Any]] = queue.Queue()
        self._server = http.server.HTTPServer(("127.0.0.1", 0), self._build_handler())
        self._thread = threading.Thread(
            target=self._server.serve_forever, name="stickiness-ab-callback", daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}/callback"

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    def wait_for_terminal(self, timeout: float) -> tuple[str, dict[str, Any]]:
        """Block for the next ``completed``/``failed`` §13.4 event,
        discarding ``accepted``/``progress``/``heartbeat`` events along the
        way. Raises `TimeoutError` if none arrives within *timeout* seconds
        total (not per-event) — a bridge that heartbeats forever without
        ever finishing is exactly the case this must not wait through
        indefinitely.
        """
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("no terminal callback event arrived within the wait bound")
            try:
                event = self._queue.get(timeout=remaining)
            except queue.Empty:
                raise TimeoutError(
                    "no terminal callback event arrived within the wait bound"
                ) from None
            kind = event.get("kind")
            if kind in ("completed", "failed"):
                return kind, event.get("payload") or {}
            # accepted / progress / heartbeat — not the answer, keep waiting.

    def _build_handler(receiver):  # noqa: N805 - factory, not a bound method
        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, fmt, *args) -> None:  # noqa: A002 - stdlib signature
                pass  # keep test/operator output quiet; failures still raise

            def do_POST(self) -> None:  # noqa: N802 - stdlib naming
                length = int(self.headers.get("Content-Length", "0") or "0")
                raw = self.rfile.read(length) if length else b""
                header = self.headers.get("Authorization", "")
                presented = header[len("Bearer ") :] if header.startswith("Bearer ") else ""
                if presented != receiver.token:
                    self._reply(401, {"error": "bad or missing callback token"})
                    return
                try:
                    event = json.loads(raw) if raw else {}
                except ValueError:
                    self._reply(400, {"error": "callback body is not valid JSON"})
                    return
                receiver._queue.put(event)  # noqa: SLF001 - same module, intentional
                self._reply(200, {"status": "ok"})

            def _reply(self, status: int, body: dict[str, Any]) -> None:
                payload = json.dumps(body).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        return Handler


class BridgeClient:
    """A thin, dependency-free client for §13.1's invocation surface —
    sync or async, whichever the bridge picks. One instance per bridge base
    URL; pass a `CallbackReceiver` to let it follow a bridge that answers
    202 (its own decision, or `always_async` config)."""

    def __init__(
        self,
        base_url: str,
        *,
        auth_token: str | None = None,
        timeout: float = 600.0,
        callback: "CallbackReceiver | None" = None,
        async_wait_seconds: float = 180.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.auth_token = auth_token
        self.timeout = timeout
        self.callback = callback
        self.async_wait_seconds = async_wait_seconds

    def invoke(
        self,
        *,
        run_id: str,
        instruction: str,
        repo: str,
        session_key: str | None = None,
        continuation_ref: str | None = None,
        model: str | None = None,
    ) -> InvocationOutcome:
        """POST one §13.1 invocation and return its terminal outcome,
        following an async 202 through this client's `CallbackReceiver`
        when the bridge chooses (or is configured) to answer that way.

        `continuation_ref` rides top-level in the body (ADR 0010 §1 — a
        sibling of run_id, not nested under `input`); `session_key` rides
        inside `input` (the transport field all three bridges exclude from
        Bound-inputs, task t5/t6). `input.async: false` is sent as a hint;
        a bridge configured `always_async` ignores it and answers 202
        regardless, which this method follows rather than treats as an
        error.
        """
        body: dict[str, Any] = {
            "protocol_version": PROTOCOL_VERSION,
            "run_id": run_id,
            "input": {
                "instruction": instruction,
                "repo": repo,
                "async": False,
            },
        }
        if session_key:
            body["input"]["session_key"] = session_key
        if model:
            body["input"]["model"] = model
        if continuation_ref:
            body["continuation_ref"] = continuation_ref
        if self.callback is not None:
            body["callback"] = {"url": self.callback.url, "token": self.callback.token}

        payload = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            f"{self.base_url}/v1/invocations",
            data=payload,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Idempotency-Key": str(uuid.uuid4()),
            },
        )
        if self.auth_token:
            req.add_header("Authorization", f"Bearer {self.auth_token}")

        try:
            # nosec B310 - self.base_url is an operator-supplied CLI arg
            # (--base-url), naming the bridge to drive, not untrusted input;
            # the same pattern adapters/*/callbacks.py comments identically.
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # nosec B310
                status = resp.status
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            status = exc.code
            raw = exc.read()
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise BridgeError(f"could not reach bridge at {self.base_url}: {exc}") from exc

        try:
            parsed = json.loads(raw) if raw else {}
        except ValueError as exc:
            raise BridgeError(f"bridge answered non-JSON body: {raw[:200]!r}") from exc

        if status == 202:
            if self.callback is None:
                raise BridgeError(
                    "bridge dispatched asynchronously (202) but this client has no "
                    "CallbackReceiver configured to follow it"
                )
            kind, event_payload = self.callback.wait_for_terminal(self.async_wait_seconds)
            return _outcome_from_terminal_event(kind, event_payload)

        return _outcome_from_response(status, parsed)


def _outcome_from_response(status: int, body: dict[str, Any]) -> InvocationOutcome:
    """Turn one parsed §13.2 synchronous response body (200, or a failure
    like 500/408) into an `InvocationOutcome`."""
    ok = status == 200 and "error" not in body
    error = None if ok else (body.get("error") or f"HTTP {status}")
    return _build_outcome(status_code=status, ok=ok, body=body, error=error)


def _outcome_from_terminal_event(kind: str, payload: dict[str, Any]) -> InvocationOutcome:
    """Turn one parsed §13.4 terminal callback payload (`kind` `completed`
    or `failed`) into the same `InvocationOutcome` shape `_outcome_from_response`
    builds for the synchronous path — so `run_cold_arm`/`run_warm_arm` never
    need to know which path a given task took.
    """
    ok = kind == "completed"
    error = None if ok else (payload.get("message") or payload.get("class") or f"callback:{kind}")
    return _build_outcome(status_code=200 if ok else 500, ok=ok, body=payload, error=error)


def _build_outcome(
    *, status_code: int, ok: bool, body: dict[str, Any], error: str | None
) -> InvocationOutcome:
    """Shared field extraction for both the synchronous response body and
    the async terminal callback payload — both carry `usage` and
    `continuation_ref` at the same shallow keys (ADR 0009/0010's whole
    point: an actor that finished late reports exactly the same shape as
    one that finished inline). Mirrors what every bridge's `mapping.py`
    already promises: `usage` rides on BOTH a success body/payload and,
    when the bridge managed to parse a terminal result before failing, the
    failure one too (issue #32) — so a failed task's real burned tokens
    still count toward the arm's totals instead of vanishing.
    """
    usage = body.get("usage") or {}
    input_tokens = usage.get("input_tokens")
    cached = usage.get("cached_input_tokens")
    thread_id = usage.get("thread_id")
    cost = usage.get("cost")
    continuation_ref = body.get("continuation_ref") if ok else None
    return InvocationOutcome(
        ok=ok,
        status_code=status_code,
        input_tokens=int(input_tokens) if isinstance(input_tokens, (int, float)) else None,
        cached_input_tokens=int(cached) if isinstance(cached, (int, float)) else None,
        thread_id=thread_id if isinstance(thread_id, str) and thread_id else None,
        continuation_ref=(
            continuation_ref if isinstance(continuation_ref, str) and continuation_ref else None
        ),
        cost_usd=float(cost) if isinstance(cost, (int, float)) else None,
        error=error,
    )


# --------------------------------------------------------------------------
# Records and pure comparison logic (the part fixture tests pin exactly)
# --------------------------------------------------------------------------


#: Whether a bridge's `cached_input_tokens` is counted SEPARATELY from
#: `input_tokens` (Anthropic's real, documented convention for the Messages
#: API: `input_tokens` already EXCLUDES both `cache_read_input_tokens` and
#: `cache_creation_input_tokens` — confirmed empirically running this
#: harness live against the fleet's claude-code bridges, task t7's live
#: measurement: a single-task cold call showed cached_input_tokens exceeding
#: input_tokens outright, which is only possible if the two counts are
#: disjoint). "Uncached" for this convention is `input_tokens` itself — no
#: subtraction, because none of the cached tokens are in that count to
#: begin with.
CACHE_CONVENTION_ADDITIVE = "additive"

#: Whether `cached_input_tokens` is a SUBSET of `input_tokens` — the OpenAI
#: family's documented convention (`usage.prompt_tokens_details.cached_tokens`
#: is counted WITHIN `prompt_tokens`). "Uncached" for this convention is
#: `input_tokens - cached_input_tokens`. codex_bridge/mapping.py passes
#: codex's own `usage.cached_input_tokens` through unexamined (no bridge-side
#: arithmetic), so this harness cannot yet confirm which convention codex's
#: own event stream actually uses without a live codex measurement — this
#: value is offered for that future run, not verified by this artifact.
CACHE_CONVENTION_SUBSET = "subset"

CACHE_CONVENTIONS = (CACHE_CONVENTION_ADDITIVE, CACHE_CONVENTION_SUBSET)


@dataclasses.dataclass(frozen=True)
class AttemptRecord:
    """One task's measured outcome in one arm — everything `summarize` and
    `compare` need, and nothing else. Built by `run_arm` from the wire, or
    built by hand in tests to exercise `summarize`/`compare` without any
    HTTP at all.

    Deliberately carries the RAW `input_tokens`/`cached_input_tokens` pair
    and nothing derived: which one of them counts as "uncached" depends on
    the backend's own accounting convention (see `CACHE_CONVENTION_*`
    above), a fact about the BACKEND, not about any one task — so it is
    applied once, explicitly, at `summarize`/`uncached_input_tokens` time,
    never baked into the record itself.
    """

    task_id: str
    arm: str  # "cold" | "warm"
    ok: bool
    input_tokens: int | None
    cached_input_tokens: int | None
    thread_id: str | None
    resumed: bool  # True iff this request actually carried a continuation_ref
    wall_time_seconds: float
    error: str | None = None
    #: §13.2 `usage.cost` (USD), when the actor prices its work. See
    #: `InvocationOutcome.cost_usd`'s docstring for why this rides alongside
    #: the five required columns rather than replacing any of them.
    cost_usd: float | None = None


def uncached_input_tokens(record: AttemptRecord, *, cache_convention: str) -> int | None:
    """The tokens *record* paid for at full (non-cached) price, per
    *cache_convention* — see `CACHE_CONVENTION_ADDITIVE`/`_SUBSET`.

    `None` (unmeasurable), never a guess, when the bridge didn't report
    cache telemetry for this task at all (ADR 0009's own rule for the
    field this harness exists to act on) — regardless of which convention
    applies, absence stays absence.
    """
    if cache_convention not in CACHE_CONVENTIONS:
        raise ValueError(f"unknown cache_convention {cache_convention!r}")
    if record.input_tokens is None or record.cached_input_tokens is None:
        return None
    if cache_convention == CACHE_CONVENTION_ADDITIVE:
        return record.input_tokens
    return record.input_tokens - record.cached_input_tokens


@dataclasses.dataclass(frozen=True)
class ArmSummary:
    """The five columns the task asks for (uncached input, cached input,
    session count, failures, wall time), plus the bookkeeping needed to
    tell a real measurement from a thin one."""

    arm: str
    n: int
    failures: int
    total_wall_time_seconds: float
    #: Tasks where BOTH input_tokens and cached_input_tokens were reported —
    #: the denominator every "average uncached/cached" figure below divides
    #: by. Never assumed equal to `n`.
    measured_n: int
    total_uncached_input_tokens: int | None
    total_cached_input_tokens: int | None
    avg_uncached_input_tokens: float | None
    distinct_sessions: int
    #: Warm-arm only in practice (always 0 for "cold" by construction):
    #: count of tasks whose request actually carried a continuation_ref —
    #: i.e. actually resumed, as opposed to having been asked to but forced
    #: cold because the prior reply returned none.
    resumed_n: int
    #: Tasks (successful, non-null cost) the average below divides by — its
    #: own denominator, independent of `measured_n`: a bridge can price its
    #: work (claude does) without also reporting cache telemetry, or vice
    #: versa (colleague/codex report neither/some), so cost presence is not
    #: assumed to track cache-field presence.
    cost_measured_n: int
    total_cost_usd: float | None
    avg_cost_usd: float | None


def summarize(records: list[AttemptRecord], *, arm: str, cache_convention: str) -> ArmSummary:
    """Reduce one arm's `AttemptRecord`s to its `ArmSummary`.

    Applies the identical formula regardless of which arm's records it is
    given — there is no "cold" or "warm" branch here, on purpose: the
    honesty condition this harness exists to satisfy is that the two arms
    are measured the same way, and a summarizer that special-cased either
    one would be exactly the bug that condition guards against.

    *cache_convention* is REQUIRED, not defaulted: which backend produced
    *records* determines whether `cached_input_tokens` is additive with
    `input_tokens` or a subset of it (see `CACHE_CONVENTION_ADDITIVE`/
    `_SUBSET`), and guessing wrong silently would make `avg_uncached_
    input_tokens` — the one number the whole comparison turns on — wrong in
    a way that still looks like a plausible measurement.
    """
    arm_records = [r for r in records if r.arm == arm]
    n = len(arm_records)
    failures = sum(1 for r in arm_records if not r.ok)
    total_wall = sum(r.wall_time_seconds for r in arm_records)
    resumed_n = sum(1 for r in arm_records if r.resumed)

    # A failed task's partial usage (issue #32: a failed session still burns
    # real tokens) stays visible in `failures` and `total_wall_time_seconds`
    # above, but is excluded from the uncached-average below: a turn that
    # never reached a domain outcome is not a representative "cost of one
    # task" sample, successful or not, and letting a truncated failure drag
    # the average down (or up) would make the comparison less honest, not
    # more.
    uncached_by_record = {
        r.task_id: uncached_input_tokens(r, cache_convention=cache_convention)
        for r in arm_records
        if r.ok
    }
    measured = [r for r in arm_records if r.ok and uncached_by_record[r.task_id] is not None]
    measured_n = len(measured)
    if measured_n:
        total_uncached = sum(uncached_by_record[r.task_id] for r in measured)  # type: ignore[misc]
        total_cached = sum(r.cached_input_tokens for r in measured)  # type: ignore[misc]
        avg_uncached = total_uncached / measured_n
    else:
        total_uncached = None
        total_cached = None
        avg_uncached = None

    distinct_sessions = len({r.thread_id for r in arm_records if r.thread_id})

    cost_measured = [r for r in arm_records if r.ok and r.cost_usd is not None]
    cost_measured_n = len(cost_measured)
    if cost_measured_n:
        total_cost = sum(r.cost_usd for r in cost_measured)  # type: ignore[misc]
        avg_cost = total_cost / cost_measured_n
    else:
        total_cost = None
        avg_cost = None

    return ArmSummary(
        arm=arm,
        n=n,
        failures=failures,
        total_wall_time_seconds=total_wall,
        measured_n=measured_n,
        total_uncached_input_tokens=total_uncached,
        total_cached_input_tokens=total_cached,
        avg_uncached_input_tokens=avg_uncached,
        distinct_sessions=distinct_sessions,
        resumed_n=resumed_n,
        cost_measured_n=cost_measured_n,
        total_cost_usd=total_cost,
        avg_cost_usd=avg_cost,
    )


#: `Comparison.verdict` values, and what each means for the stickiness
#: default (spec h2: "if resumed sessions do not measurably reduce uncached
#: input, stickiness stays opt-in").
VERDICT_INSUFFICIENT_DATA = "insufficient_data"
VERDICT_UNDERPOWERED_POSITIVE = "directionally_positive_but_underpowered"
VERDICT_NO_REDUCTION = "no_reduction_or_regression"
VERDICT_REDUCTION_DEMONSTRATED = "reduction_demonstrated"

#: Every verdict but one keeps the default off. Only a genuinely
#: demonstrated reduction, at or above `min_n` per arm, earns
#: `recommend_default_on = True` from `compare` — see its docstring.
_VERDICTS_KEEPING_OPT_IN = frozenset(
    {
        VERDICT_INSUFFICIENT_DATA,
        VERDICT_UNDERPOWERED_POSITIVE,
        VERDICT_NO_REDUCTION,
    }
)


@dataclasses.dataclass(frozen=True)
class Comparison:
    cold: ArmSummary
    warm: ArmSummary
    reduction_fraction: float | None
    verdict: str
    recommend_default_on: bool
    note: str


def compare(
    cold: ArmSummary,
    warm: ArmSummary,
    *,
    min_n: int = DEFAULT_MIN_N,
    reduction_threshold: float = DEFAULT_REDUCTION_THRESHOLD,
) -> Comparison:
    """Decide what the two arms' summaries mean, per the decision table
    below. Pure function — no I/O, no clock, no randomness — so a fixture
    test can pin its output exactly against hand-built `ArmSummary` values.

    Decision table, checked in order:

    1. Either arm has `n == 0`, or either arm has `measured_n == 0` (no
       task in that arm reported cache telemetry at all) →
       `insufficient_data`. This is the honest verdict for a harness that
       has not been run live yet, or was run against a bridge whose
       contract never reports `cached_input_tokens`.
    2. `cold.avg_uncached_input_tokens == 0` → `insufficient_data` (a
       reduction fraction is undefined against a zero baseline; refusing to
       divide by zero is not the same as claiming no data).
    3. `reduction_fraction <= 0` → `no_reduction_or_regression`. The
       resumed arm cost the same or more uncached input than cold — spec
       s26's counter-evidence lens (provider cache TTLs) made real.
    4. `0 < reduction_fraction < reduction_threshold`, OR either arm's
       `measured_n < min_n` → `directionally_positive_but_underpowered`.
       Some reduction showed up, but not enough of it, or not on enough
       tasks, to act on.
    5. `reduction_fraction >= reduction_threshold` AND both arms'
       `measured_n >= min_n` → `reduction_demonstrated`, the only verdict
       that sets `recommend_default_on = True`.
    """
    if cold.n == 0 or warm.n == 0:
        return Comparison(
            cold=cold,
            warm=warm,
            reduction_fraction=None,
            verdict=VERDICT_INSUFFICIENT_DATA,
            recommend_default_on=False,
            note="one or both arms ran zero tasks — no comparison is possible",
        )
    if cold.measured_n == 0 or warm.measured_n == 0:
        return Comparison(
            cold=cold,
            warm=warm,
            reduction_fraction=None,
            verdict=VERDICT_INSUFFICIENT_DATA,
            recommend_default_on=False,
            note=(
                "one or both arms reported no cache telemetry on any task "
                "(cached_input_tokens was null throughout) — the reduction "
                "this gate needs to see is unmeasurable, not merely absent"
            ),
        )
    if not cold.avg_uncached_input_tokens:
        return Comparison(
            cold=cold,
            warm=warm,
            reduction_fraction=None,
            verdict=VERDICT_INSUFFICIENT_DATA,
            recommend_default_on=False,
            note="cold arm's average uncached input is zero — a reduction fraction is undefined",
        )

    reduction = (
        cold.avg_uncached_input_tokens - warm.avg_uncached_input_tokens
    ) / cold.avg_uncached_input_tokens

    if reduction <= 0:
        return Comparison(
            cold=cold,
            warm=warm,
            reduction_fraction=reduction,
            verdict=VERDICT_NO_REDUCTION,
            recommend_default_on=False,
            note=(
                "the resumed arm's average uncached input was the same as or higher than "
                "cold's — exactly the failure mode spec risk s26 named (provider cache TTL "
                "expiry makes a long-history resume cost more than a fresh start)"
            ),
        )

    if reduction < reduction_threshold or cold.measured_n < min_n or warm.measured_n < min_n:
        return Comparison(
            cold=cold,
            warm=warm,
            reduction_fraction=reduction,
            verdict=VERDICT_UNDERPOWERED_POSITIVE,
            recommend_default_on=False,
            note=(
                f"reduction={reduction:.1%} measured over cold.measured_n={cold.measured_n}, "
                f"warm.measured_n={warm.measured_n} (min_n={min_n}, threshold="
                f"{reduction_threshold:.0%}) — a positive direction, not yet a demonstrated one"
            ),
        )

    return Comparison(
        cold=cold,
        warm=warm,
        reduction_fraction=reduction,
        verdict=VERDICT_REDUCTION_DEMONSTRATED,
        recommend_default_on=True,
        note=(
            f"reduction={reduction:.1%} at or above the {reduction_threshold:.0%} threshold, "
            f"over at least min_n={min_n} measured tasks per arm"
        ),
    )


# --------------------------------------------------------------------------
# Driving the two arms
# --------------------------------------------------------------------------


def run_cold_arm(
    client: BridgeClient, tasks: list[dict[str, str]], *, repo: str, run_id: str
) -> list[AttemptRecord]:
    """One fresh session per task: no session_key, no continuation_ref."""
    records = []
    for task in tasks:
        start = time.monotonic()
        try:
            outcome = client.invoke(run_id=run_id, instruction=task["instruction"], repo=repo)
            elapsed = time.monotonic() - start
            records.append(
                AttemptRecord(
                    task_id=task["id"],
                    arm="cold",
                    ok=outcome.ok,
                    input_tokens=outcome.input_tokens,
                    cached_input_tokens=outcome.cached_input_tokens,
                    thread_id=outcome.thread_id,
                    resumed=False,
                    wall_time_seconds=elapsed,
                    error=outcome.error,
                    cost_usd=outcome.cost_usd,
                )
            )
        except BridgeError as exc:
            elapsed = time.monotonic() - start
            records.append(
                AttemptRecord(
                    task_id=task["id"],
                    arm="cold",
                    ok=False,
                    input_tokens=None,
                    cached_input_tokens=None,
                    thread_id=None,
                    resumed=False,
                    wall_time_seconds=elapsed,
                    error=str(exc),
                )
            )
    return records


def run_warm_arm(
    client: BridgeClient,
    tasks: list[dict[str, str]],
    *,
    repo: str,
    run_id: str,
    session_key: str,
) -> list[AttemptRecord]:
    """One resumed thread across every task, in order. The FIRST task
    necessarily dispatches cold (nothing to resume yet); every task after
    it carries the previous reply's own `continuation_ref` — and only
    counts as `resumed=True` when that ref was actually present, so a
    backend that stops returning one mid-chain (or never did) is visible in
    `resumed_n` rather than silently assumed to have kept resuming.
    """
    records = []
    prior_ref: str | None = None
    for task in tasks:
        sending_ref = prior_ref
        start = time.monotonic()
        try:
            outcome = client.invoke(
                run_id=run_id,
                instruction=task["instruction"],
                repo=repo,
                session_key=session_key,
                continuation_ref=sending_ref,
            )
            elapsed = time.monotonic() - start
            records.append(
                AttemptRecord(
                    task_id=task["id"],
                    arm="warm",
                    ok=outcome.ok,
                    input_tokens=outcome.input_tokens,
                    cached_input_tokens=outcome.cached_input_tokens,
                    thread_id=outcome.thread_id,
                    resumed=sending_ref is not None,
                    wall_time_seconds=elapsed,
                    error=outcome.error,
                    cost_usd=outcome.cost_usd,
                )
            )
            # Whatever the bridge offers next (possibly None — e.g.
            # colleague, or a backend that stops offering resume mid-chain)
            # is exactly what the NEXT task sends. No fallback substitution.
            prior_ref = outcome.continuation_ref
        except BridgeError as exc:
            elapsed = time.monotonic() - start
            records.append(
                AttemptRecord(
                    task_id=task["id"],
                    arm="warm",
                    ok=False,
                    input_tokens=None,
                    cached_input_tokens=None,
                    thread_id=None,
                    resumed=sending_ref is not None,
                    wall_time_seconds=elapsed,
                    error=str(exc),
                )
            )
            # A transport failure tells us nothing about whether the
            # session is still resumable; do not fabricate a ref, and do
            # not silently drop the chain either — the next task simply
            # tries with whatever the last SUCCESSFUL reply offered.
    return records


def load_tasks(path: str) -> list[dict[str, str]]:
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
    if not isinstance(data, list) or not all(
        isinstance(t, dict) and "id" in t and "instruction" in t for t in data
    ):
        raise ValueError(f"{path} must be a JSON list of {{'id', 'instruction'}} objects")
    return data


# --------------------------------------------------------------------------
# Markdown rendering
# --------------------------------------------------------------------------


def _fmt(value: Any) -> str:
    if value is None:
        return "—"
    if isinstance(value, float):
        return f"{value:.2f}"
    return str(value)


def _fmt_usd(value: float | None) -> str:
    # Real observed costs for tiny tasks run a few cents apart (e.g.
    # $0.0194 vs $0.1871) — `_fmt`'s 2-decimal rounding would collapse both
    # to "$0.02"/"$0.19" and lose exactly the digits the comparison needs.
    if value is None:
        return "—"
    return f"{value:.4f}"


def render_markdown(comparison: Comparison, *, meta: dict[str, str]) -> str:
    """Render the five-column table the task asks for, plus the verdict and
    the gate decision, as a committable artifact body (no front matter —
    the caller wraps this in the delivery-doc shell)."""
    cold, warm = comparison.cold, comparison.warm
    lines = [
        "| column | cold (fresh session per task) | warm (one resumed thread) |",
        "| --- | --- | --- |",
        f"| tasks run (n) | {cold.n} | {warm.n} |",
        f"| tasks with cache telemetry (measured_n) | {cold.measured_n} | {warm.measured_n} |",
        f"| failures | {cold.failures} | {warm.failures} |",
        (
            "| avg uncached input tokens/task | "
            f"{_fmt(cold.avg_uncached_input_tokens)} | {_fmt(warm.avg_uncached_input_tokens)} |"
        ),
        (
            "| total cached input tokens | "
            f"{_fmt(cold.total_cached_input_tokens)} | {_fmt(warm.total_cached_input_tokens)} |"
        ),
        (
            "| distinct provider sessions (thread_id) | "
            f"{cold.distinct_sessions} | {warm.distinct_sessions} |"
        ),
        f"| tasks actually resumed | {cold.resumed_n} | {warm.resumed_n} |",
        (
            "| total wall time (s) | "
            f"{_fmt(cold.total_wall_time_seconds)} | {_fmt(warm.total_wall_time_seconds)} |"
        ),
        (
            "| avg cost/task (USD, bonus — not one of the five required columns) | "
            f"{_fmt_usd(cold.avg_cost_usd)} | {_fmt_usd(warm.avg_cost_usd)} |"
        ),
    ]
    reduction_str = (
        f"{comparison.reduction_fraction:.1%}"
        if comparison.reduction_fraction is not None
        else "n/a"
    )
    lines += [
        "",
        f"**Reduction (cold avg → warm avg):** {reduction_str}",
        f"**Verdict:** `{comparison.verdict}`",
        f"**Recommend default-on:** {comparison.recommend_default_on}",
        f"**Note:** {comparison.note}",
    ]
    if meta:
        lines.append("")
        lines.append("Run metadata:")
        for key, value in meta.items():
            lines.append(f"- {key}: {value}")
    return "\n".join(lines) + "\n"


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--base-url", required=True, help="bridge base URL, e.g. http://127.0.0.1:8765"
    )
    parser.add_argument("--repo", required=True, help="repo path the bridge dispatches against")
    parser.add_argument("--tasks", required=True, help="path to a JSON list of {id, instruction}")
    parser.add_argument("--run-id", default=None, help="defaults to a fresh uuid4")
    parser.add_argument("--session-key", default="stickiness-ab", help="warm arm's session_key")
    parser.add_argument("--auth-token", default=None)
    parser.add_argument("--min-n", type=int, default=DEFAULT_MIN_N)
    parser.add_argument("--reduction-threshold", type=float, default=DEFAULT_REDUCTION_THRESHOLD)
    parser.add_argument(
        "--async-wait-seconds",
        type=float,
        default=180.0,
        help="how long to wait for a terminal callback event when a bridge answers 202",
    )
    parser.add_argument(
        "--cache-convention",
        choices=CACHE_CONVENTIONS,
        default=CACHE_CONVENTION_ADDITIVE,
        help=(
            "how this backend relates cached_input_tokens to input_tokens — "
            f"{CACHE_CONVENTION_ADDITIVE!r} (input_tokens excludes cache, Anthropic's "
            "documented behavior, confirmed live against claude-code by this harness) or "
            f"{CACHE_CONVENTION_SUBSET!r} (cached_input_tokens is counted within input_tokens, "
            "OpenAI's documented behavior — NOT yet confirmed live for codex by this harness). "
            "Guessing wrong makes the headline reduction number wrong while still looking "
            "like a real measurement, so this has no silently-safe default; verify before "
            "trusting a run against an unconfirmed backend."
        ),
    )
    parser.add_argument("--out", default=None, help="write the markdown table here; else stdout")
    args = parser.parse_args(argv)

    tasks = load_tasks(args.tasks)
    run_id = args.run_id or str(uuid.uuid4())
    # A CallbackReceiver is always started: it costs nothing when the bridge
    # answers synchronously (never contacted), and is what makes an
    # `always_async` bridge — every real deployed claude-code bridge on this
    # fleet — reachable at all.
    receiver = CallbackReceiver()
    try:
        client = BridgeClient(
            args.base_url,
            auth_token=args.auth_token,
            callback=receiver,
            async_wait_seconds=args.async_wait_seconds,
        )

        cold_records = run_cold_arm(client, tasks, repo=args.repo, run_id=run_id)
        warm_records = run_warm_arm(
            client, tasks, repo=args.repo, run_id=run_id, session_key=args.session_key
        )
    finally:
        receiver.stop()

    cold_summary = summarize(cold_records, arm="cold", cache_convention=args.cache_convention)
    warm_summary = summarize(warm_records, arm="warm", cache_convention=args.cache_convention)
    comparison = compare(
        cold_summary, warm_summary, min_n=args.min_n, reduction_threshold=args.reduction_threshold
    )

    doc = render_markdown(
        comparison,
        meta={
            "base_url": args.base_url,
            "run_id": run_id,
            "task_count": str(len(tasks)),
            "cache_convention": args.cache_convention,
            "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        },
    )
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(doc)
    else:
        print(doc)
    return 0


if __name__ == "__main__":  # pragma: no cover - exercised via main(), not import
    raise SystemExit(main())
