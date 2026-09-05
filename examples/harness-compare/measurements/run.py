#!/usr/bin/env python3
"""The measurement runner (task t11, plan
`docs/plans/2026-09-05-harness-hardening-and-compare.md`; spec claims c29,
c30 and honesty conditions h24, h28).

Reads a measurement manifest (``manifest.py``, task t7), dispatches each of
its rules to each of its actors through the ordinary control-plane API,
applies each rule's mechanical check to the answer (``checks.py``), and
posts a grade **as an agent principal**. It is importable as a module and
runnable as a CLI.

Zero third-party dependencies: ``argparse``, ``json``, ``time`` here, and
``urllib`` in the sibling ``fleet.py``. The runtime package this repo ships has
``dependencies = []`` and this runner is held to the same bar, so it can be
copied onto a deploy host with nothing but python3.

FOUR THINGS THIS RUNNER IS CAREFUL ABOUT
----------------------------------------

1. **The revision gate (c30 / h24).** Before any dispatch it reads each
   actor's bridge ``GET <endpoint_ref>/v1/capabilities`` and pulls the
   ``deployment`` block out of it (``revision``, ``install_mode``,
   ``revision_is_dirty``). With ``--expect-revision <sha>`` it refuses,
   naming every actor whose revision differs, before a single billable
   session is spent. Without the flag it refuses nothing and simply
   *records* what it saw — into the run input and into every grade's notes,
   so a measurement can never be read back without knowing which build
   produced it. A measurement against an unknown build is the defect this
   gate exists to prevent (#104 / #120).

2. **Serial dispatch, always.** pi, qwen and colleague are served by ONE
   model on one host: two concurrent runs do not measure two actors, they
   measure a contended queue. So the runner creates ONE run at a time
   across ALL actors and rules, and waits for it to reach a terminal state
   before creating the next. A watch timeout is not an exception to that:
   the runner cancels the run and waits for the control plane to report it
   terminal before dispatching anything else, because a run it merely
   stopped watching is still a run holding the model. There is deliberately
   **no** ``--parallel`` flag (user constraint, 2026-09-05); adding one
   would silently change what every recorded duration means.

3. **The grading principal (c29 / h28).** Grades are posted with
   ``grading_actor_id`` = the agent actor named by
   ``MEASURE_RUNNER_ACTOR_ID`` (or ``--as``). The control plane makes a
   *human* grader's grade land ``confirmed`` immediately
   (``internal/api/grades.go``), so a runner wearing a human identity would
   mint confirmed grades for work no human read. This runner refuses to
   start without an explicit principal, and refuses one whose registered
   kind is ``human``. Every grade it posts lands ``proposed`` and waits for
   the ordinary review surface.

4. **Append, never edit.** The report is JSON Lines: one object per run,
   appended. Re-running a manifest — or a changed manifest — adds lines
   beside the old ones and creates new runs and new grades; it never
   rewrites either. That is the ledger's own rule (records are immutable;
   corrections append with ``supersedes``) applied to the artifact the
   ledger does not hold.

WHAT IS COPIED FROM THE OPERATOR SCRIPT
---------------------------------------

Every request shape below is the one
``.claude/skills/nodes-operator/scripts/nodes-op.sh`` already uses against
the live control plane, so this runner cannot drift from the operator lane:

- auth: ``Cookie: CF_Authorization=$NODES_OP_COOKIE`` (and the optional
  ``Authorization: Bearer $NODES_OP_BEARER``) against ``$NODES_API_URL``
  (nodes-op.sh ``auth_args`` / ``api_get`` / ``api_post``).
- publish: ``POST /v1alpha1/workflows {"format":"yaml","source":...}`` ->
  ``{"digest":...}`` (nodes-op.sh ``publish``).
- create: ``POST /v1alpha1/runs {"workflow_digest","input","category"}`` ->
  ``{"id","state"}`` (nodes-op.sh ``create``).
- watch: ``GET /v1alpha1/runs/{id}`` until ``run.state`` is terminal
  (nodes-op.sh ``watch`` / ``run``).
- cancel: ``POST /v1alpha1/runs/{id}/cancel {}`` -> ``{"state":...}``
  (nodes-op.sh ``cancel``), used on a watch timeout only.
- grade: ``POST /v1alpha1/runs/{id}/grades`` with ``{"rating",
  "rationale","evaluated_actor_id","grading_actor_id","category"}``
  (nodes-op.sh ``grade``).
- actors: ``GET /v1alpha1/actors`` -> ``{"items":[{id, actor_key, revision,
  kind, endpoint_ref, metadata}]}``, newest revision per key wins
  (nodes-op.sh ``actors``).

The cookie is read from the environment only and is never printed, never
written to the report, and never included in an error message.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import sys
import time
from pathlib import Path
from typing import Any, Sequence

HERE = Path(__file__).resolve().parent

#: The manifest a bare ``run.py`` runs. It is the THOR-ONLY set, not
#: ``basic.json``, and that is deliberate: ``basic.json`` names all four
#: thor/orin actors, while examples/harness-compare/workflow.yaml pins slot
#: ``pi`` to ``company/pi-thor`` and slot ``qwen`` to ``company/qwen-thor``,
#: so those four collapse onto two slots and ``refuse_slot_collisions``
#: aborts the pass before a single dispatch (issue #304). A default that
#: cannot reach dispatch is not a default, so the default is the actor set
#: this graph can actually serve; ``--manifest basic.json`` still selects the
#: four-actor set for a deployment (or a future graph) that has a slot per
#: host. tests/test_measurement_default_manifest.py holds this to it.
DEFAULT_MANIFEST = HERE / "basic-thor.json"
DEFAULT_WORKFLOW = HERE.parent / "workflow.yaml"

#: The checkout this runner can actually read, used only for the best-effort
#: fabrication probe. The actors read their own checkouts on their own hosts;
#: see checks.fabricated_paths.
REPO_ROOT = HERE.parents[2]


def _load_sibling(name: str) -> Any:
    """Bootstrap import for ``fleet.py``, which owns the general loader.

    Identical to ``fleet.load_sibling``; it exists only because something
    has to import fleet before fleet's own loader is available.
    """
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_measurement_{name}", HERE / f"{name}.py"
    )
    if spec is None or spec.loader is None:  # pragma: no cover - packaging accident
        raise SystemExit(f"error: could not load {name}.py from beside run.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


fleet = _load_sibling("fleet")
checks = _load_sibling("checks")

# Re-exported so a caller (and the tests) can reach the fleet half through
# `run.py` alone — the runner is one tool, split across two files only
# because of the file-length contract.
ApiClient = fleet.ApiClient
RunnerError = fleet.RunnerError
EXIT_OK = fleet.EXIT_OK
EXIT_USER_ERROR = fleet.EXIT_USER_ERROR
EXIT_ENV_ERROR = fleet.EXIT_ENV_ERROR
DEFAULT_QWEN_MODE = fleet.DEFAULT_QWEN_MODE
TERMINAL_STATES = fleet.TERMINAL_STATES
extract_deployment = fleet.extract_deployment
fetch_deployment = fleet.fetch_deployment
gate_revisions = fleet.gate_revisions
newest_actor_rows = fleet.newest_actor_rows
refuse_slot_collisions = fleet.refuse_slot_collisions
require_grading_principal = fleet.require_grading_principal
resolve_actors = fleet.resolve_actors
resolve_grading_actor = fleet.resolve_grading_actor
served_actor_id = fleet.served_actor_id
slot_for = fleet.slot_for


# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------


def build_run_input(
    rule: dict[str, Any],
    actor: dict[str, Any],
    repo: str,
    manifest_digest: str,
    bridge_revisions: dict[str, str | None],
    qwen_mode: str,
) -> dict[str, Any]:
    """The run input for ONE rule against ONE actor.

    Exactly one slot is named, so the graph's split fires exactly one branch
    and the join has exactly one arrival — one run measures one actor.

    ``measurement`` is the workflow's own object and is closed
    (``additionalProperties: false``): manifest digest and rule id only.
    The bridge revisions and the rule's sandbox therefore go in a
    **top-level** ``measurement_context`` — the input schema's top level
    declares ``required`` and ``properties`` but no
    ``additionalProperties: false``, so an extra top-level key is admitted
    (verified against examples/harness-compare/workflow.yaml, and this is
    the same "top-level input admits extra keys" the spec's s17 probe
    recorded).
    """
    slot_input: dict[str, Any] = {"repo": repo}
    if actor["slot"] == "qwen":
        slot_input["mode"] = qwen_mode
    return {
        "instruction": rule["instruction"],
        "sandbox": rule.get("sandbox", "read-only"),
        "handover": False,
        "actors": {actor["slot"]: slot_input},
        "measurement": {
            "manifest_digest": manifest_digest,
            "rule_id": rule["id"],
        },
        "measurement_context": {
            "sandbox": rule.get("sandbox", "read-only"),
            "bridge_revisions": dict(bridge_revisions),
        },
    }


def publish_workflow(api: ApiClient, workflow_path: Path) -> str:
    """Publish the harness-compare graph; return its digest.

    Idempotent: publishing an identical source returns the same
    content-addressed digest, so re-running a manifest re-publishes nothing
    new (nodes-op.sh ``publish``).
    """
    source = workflow_path.read_text(encoding="utf-8")
    result = api.post("/v1alpha1/workflows", {"format": "yaml", "source": source})
    digest = result.get("digest", "") if isinstance(result, dict) else ""
    if not digest:
        raise RunnerError(
            f"publishing {workflow_path} returned no digest",
            "run `nodes-op.sh validate examples/harness-compare/workflow.yaml` to see why",
            EXIT_ENV_ERROR,
        )
    return digest


def create_run(api: ApiClient, workflow_digest: str, run_input: dict[str, Any], category: str):
    """POST /v1alpha1/runs — category is the rule id (decision q9)."""
    body = {"workflow_digest": workflow_digest, "input": run_input, "category": category}
    result = api.post("/v1alpha1/runs", body)
    run_id = result.get("id", "") if isinstance(result, dict) else ""
    if not run_id:
        raise RunnerError(
            "the control plane accepted the run but returned no id",
            "check the API version with GET /v1alpha1/version",
            EXIT_ENV_ERROR,
        )
    return run_id


#: How long a cancelled run is given to reach a terminal state before the
#: pass refuses to continue. Cancellation is committed by the control plane
#: *before* it answers (internal/api/runs.go ``cancelRunWithReason``), so a
#: run that is still not terminal after this long means the cancel never
#: landed — not that it is taking its time.
CANCEL_GRACE = 120.0


def cancel_run(api: ApiClient, run_id: str) -> str:
    """Ask the control plane to cancel a run. Best-effort; never raises.

    ``POST /v1alpha1/runs/{id}/cancel`` (nodes-op.sh ``cancel``) commits the
    cancellation before it answers and then asks the actor to stop the
    session it parked (internal/api/cancelpropagate.go). Two answers are
    expected here and neither is fatal: ``409`` because the run reached a
    terminal state between the last poll and this call, or a transport
    failure because the call did not land at all. Both are resolved by
    ``settle_run``, which reads the control plane rather than trusting this
    response — so the refusal to raise here costs nothing.
    """
    try:
        result = api.post(f"/v1alpha1/runs/{run_id}/cancel", {})
    except RunnerError:
        return ""
    return result.get("state", "") if isinstance(result, dict) else ""


def settle_run(
    api: ApiClient,
    run_id: str,
    interval: float,
    sleep,
    grace: float = CANCEL_GRACE,
) -> tuple[dict[str, Any], str]:
    """Poll until the control plane reports the run terminal, or refuse.

    This is the half that makes the timeout path honest. A run the runner
    stopped watching is still a run, and the next dispatch would land on the
    same model host — measuring a contended queue, which is precisely what
    seriality exists to prevent. So the pass continues only once the control
    plane itself says the run is over; if it will not say so, the pass stops
    rather than recording durations that mean nothing.
    """
    deadline = time.monotonic() + grace
    while True:
        view = api.get(f"/v1alpha1/runs/{run_id}")
        state = (view.get("run") or {}).get("state", "")
        if state in TERMINAL_STATES:
            return view, state
        if time.monotonic() >= deadline:
            raise RunnerError(
                f"run {run_id} timed out, was asked to cancel, and is still "
                f"{state or 'in an unknown state'} {grace:.0f}s later",
                f"cancel it by hand (nodes-op.sh cancel {run_id}) and re-run — the pass is "
                "serial, and dispatching the next measurement while this one may still hold "
                "the model would measure a contended queue rather than an actor",
                EXIT_ENV_ERROR,
            )
        sleep(interval)


def watch_run(
    api: ApiClient,
    run_id: str,
    timeout: float = 1800.0,
    interval: float = 5.0,
    sleep=time.sleep,
    grace: float = CANCEL_GRACE,
) -> dict[str, Any]:
    """Poll ``GET /v1alpha1/runs/{id}`` to a terminal state.

    The blocking half of the serial constraint: nothing else is dispatched
    until this returns. A timeout therefore may not just *stop watching* —
    it cancels the run and waits for the control plane to report it
    terminal, because until then the run is still the shared model's work
    and the next dispatch would overlap it.

    The view comes back with a synthesised ``timed_out`` state, so one hung
    actor still costs one 1-rated grade instead of abandoning the whole
    pass; the state the control plane actually settled on is kept beside it
    as ``settled_state``. A run that completed in the gap between the last
    poll and the cancel keeps its real ``completed`` state and its answer —
    rating that 1 would be a claim about the actor that is not true.

    What this can and cannot promise: the cancellation is durable in the
    ledger before the call answers, and the actor is *asked* to stop, but
    that propagation is best-effort by design (PRD §13.6). An actor that
    ignores the ask is a limitation of the protocol, not something this
    runner can wait out.
    """
    deadline = time.monotonic() + timeout
    while True:
        view = api.get(f"/v1alpha1/runs/{run_id}")
        state = (view.get("run") or {}).get("state", "")
        if state in TERMINAL_STATES:
            return view
        if time.monotonic() >= deadline:
            cancel_run(api, run_id)
            view, settled = settle_run(api, run_id, interval, sleep, grace)
            run = view.setdefault("run", {})
            run["settled_state"] = settled
            if settled != "completed":
                run["state"] = "timed_out"
            return view
        sleep(interval)


def extract_answer(view: dict[str, Any], slot: str) -> dict[str, Any]:
    """The actor's summary and its node outcome out of a run view.

    The graph's end node emits the join's arrival array
    (``/nodes/gather/output``), so a completed run's ``run.output`` is a
    list of ``{from_node, outcome, output:{summary,...}}`` — one element,
    since one slot was named (tests/e2e/harnesscompare_test.go asserts that
    exact shape). A run that never reached the end node has no such output,
    so the node run's own last attempt result is read as a fallback.
    """
    run = view.get("run") or {}
    output = run.get("output")
    if isinstance(output, list):
        for arrival in output:
            if not isinstance(arrival, dict):
                continue
            if arrival.get("from_node") in (slot, None) or len(output) == 1:
                inner = arrival.get("output")
                summary = inner.get("summary") if isinstance(inner, dict) else None
                return {"summary": summary, "outcome": arrival.get("outcome", "")}
    if isinstance(output, dict) and isinstance(output.get("summary"), str):
        return {"summary": output["summary"], "outcome": ""}
    for node_run in view.get("node_runs") or []:
        if node_run.get("node_id") != slot:
            continue
        outcome = node_run.get("outcome", "")
        for attempt in reversed(node_run.get("attempts") or []):
            result = attempt.get("result")
            if not isinstance(result, dict):
                continue
            inner = result.get("output")
            if isinstance(inner, dict) and isinstance(inner.get("summary"), str):
                return {"summary": inner["summary"], "outcome": outcome}
            if isinstance(result.get("summary"), str):
                return {"summary": result["summary"], "outcome": outcome}
        return {"summary": None, "outcome": outcome}
    return {"summary": None, "outcome": ""}


def grade_notes(
    rule: dict[str, Any],
    manifest_digest: str,
    verdict: dict[str, Any],
    fabricated: list[str],
    revision: str | None,
) -> str:
    """The rationale a grade carries. Every fact h28 asks for, in one string.

    Read by a human deciding the proposed grade, so it names the rule, the
    manifest it came from, what was checked, what was expected, what the
    check found, whether anything looked fabricated, and which build of the
    bridge produced the answer.
    """
    lines = [
        f"measurement rule={rule['id']} category={rule['category']}",
        f"manifest_digest={manifest_digest}",
        f"check={verdict['kind']} expected={verdict['expect']!r}",
        f"verdict={verdict['verdict']} passed={str(verdict['passed']).lower()} "
        f"rating={verdict['rating']}",
        f"reason: {verdict['reason']}",
        f"fabrication_flag={'true' if fabricated else 'false'}"
        + (
            f" (paths not found in this checkout: {', '.join(fabricated[:5])})"
            if fabricated
            else ""
        ),
        f"bridge_revision={revision or 'unknown'}",
        "This grade is a proposed record from an automated runner; the mechanical check "
        "reads the answer's TEXT, not its understanding — decide it against the rule's "
        "anchors before confirming.",
    ]
    return "\n".join(lines)


def post_grade(
    api: ApiClient,
    run_id: str,
    rating: int,
    rationale: str,
    evaluated_actor_id: str,
    grading_actor_id: str,
    category: str,
) -> dict[str, Any]:
    """POST /v1alpha1/runs/{id}/grades (nodes-op.sh ``grade``'s body)."""
    if not grading_actor_id:
        raise RunnerError(
            "refusing to post a grade with no grading principal",
            "set MEASURE_RUNNER_ACTOR_ID to the runner's agent actor id",
        )
    body = {
        "rating": rating,
        "rationale": rationale,
        "evaluated_actor_id": evaluated_actor_id,
        "grading_actor_id": grading_actor_id,
        "category": category,
    }
    appended = api.post(f"/v1alpha1/runs/{run_id}/grades", body)
    assert_grade_landed_as(appended, grading_actor_id)
    return appended


def assert_grade_landed_as(appended: dict[str, Any], grading_actor_id: str) -> None:
    """Abort the pass if the ledger recorded the grade under another actor.

    internal/api/grades.go resolves the grading actor from the request's
    BOUND PRINCIPAL (principalActor) and only warns about the body's
    grading_actor_id — so a runner wearing the operator's Access cookie
    mints confirmed human grades under the operator (issue #306, deviation
    d4 of plan harness-hardening-and-compare). One such grade is one too
    many: stop before the next dispatch rather than finish a pass whose
    grades are not what the manifest promised.
    """
    origin = appended.get("origin") or {}
    recorded = origin.get("actor_id") or appended.get("origin_actor_id") or ""
    authority = appended.get("authority", "")
    warning = appended.get("warning", "")
    if "overridden" in warning or (recorded and recorded != grading_actor_id):
        raise RunnerError(
            f"the ledger recorded this grade under {recorded or 'an unnamed actor'} "
            f"(authority {authority or 'unknown'}), not the named agent principal "
            f"{grading_actor_id}: {warning or 'grading_actor_id was not honoured'}",
            "authenticate as the agent principal itself (its own credential), or stop "
            "grading through a human identity — see issue #306",
        )
    if authority and authority != "proposed":
        raise RunnerError(
            f"the ledger recorded this grade with authority {authority}; a manifest "
            f"run's grades must land proposed",
            "see issue #306",
        )


# ---------------------------------------------------------------------------
# The pass
# ---------------------------------------------------------------------------


def plan_dispatches(manifest: dict[str, Any], actors: Sequence[dict[str, Any]]) -> list[tuple]:
    """Every (rule, actor, attempt-index) this pass will dispatch, in order.

    Rule-major, then actor, then repeat — so a pass that is interrupted has
    covered whole rules across the whole fleet rather than one actor's
    everything.
    """
    plan = []
    for rule in manifest["rules"]:
        for repeat in range(int(rule.get("runs_per_actor", 1))):
            for actor in actors:
                plan.append((rule, actor, repeat + 1))
    return plan


def run_pass(
    api: ApiClient,
    manifest: dict[str, Any],
    manifest_digest: str,
    actors: Sequence[dict[str, Any]],
    repo_map: dict[str, str],
    deployments: dict[str, dict[str, Any]],
    grading_actor_id: str,
    workflow_digest: str,
    report_path: Path | None,
    qwen_mode: str = DEFAULT_QWEN_MODE,
    timeout: float = 1800.0,
    interval: float = 5.0,
    grace: float = CANCEL_GRACE,
    sleep=time.sleep,
    out=None,
) -> list[dict[str, Any]]:
    """Dispatch the whole manifest SERIALLY and return one record per run.

    One run at a time, start to terminal, across every actor and rule — see
    the module docstring's point 2. Each record is appended to the report as
    soon as it is complete, so an interrupted pass keeps everything it
    already measured.
    """
    out = sys.stdout if out is None else out
    revisions = {
        key: (block.get("revision") if block else None) for key, block in deployments.items()
    }
    records: list[dict[str, Any]] = []
    for rule, actor, repeat in plan_dispatches(manifest, actors):
        repo = repo_map.get(actor["actor_key"]) or repo_map.get(actor["slot"], "")
        if not repo:
            raise RunnerError(
                f"no checkout path for {actor['actor_key']} (slot {actor['slot']})",
                "pass --repo-map <slot-or-actor-key>=/path/on/that/host — the runner does "
                "not guess a path, because a path is meaningful on exactly one machine",
            )
        run_input = build_run_input(rule, actor, repo, manifest_digest, revisions, qwen_mode)
        started = time.time()
        run_id = create_run(api, workflow_digest, run_input, rule["id"])
        view = watch_run(api, run_id, timeout=timeout, interval=interval, sleep=sleep, grace=grace)
        elapsed = time.time() - started
        run_view = view.get("run") or {}
        state = run_view.get("state", "")
        answer = extract_answer(view, actor["slot"])
        if state != "completed":
            verdict = checks.rate(rule["check"]["kind"], rule["check"]["expect"], None)
            verdict["reason"] = f"the run ended {state or 'in an unknown state'} with no answer"
        else:
            verdict = checks.rate(rule["check"]["kind"], rule["check"]["expect"], answer["summary"])
        fabricated = checks.fabricated_paths(answer["summary"], [REPO_ROOT])
        evaluated_actor_id = served_actor_id(view, actor["slot"], actor["actor_id"])
        notes = grade_notes(
            rule, manifest_digest, verdict, fabricated, revisions.get(actor["actor_key"])
        )
        if evaluated_actor_id != actor["actor_id"]:
            notes += (
                f"\nrouting_mismatch: the run was addressed to {actor['actor_key']} "
                f"({actor['actor_id']}) but was served by {evaluated_actor_id}; "
                "this grade is filed against the actor that actually served it"
            )
        grade = post_grade(
            api,
            run_id,
            verdict["rating"],
            notes,
            evaluated_actor_id,
            grading_actor_id,
            rule["id"],
        )
        record = {
            "run_id": run_id,
            "run_state": state,
            # What the control plane settled on, which differs from
            # `run_state` only for a run this pass timed out and cancelled:
            # `timed_out` is the runner's word, `cancelled` is the ledger's.
            "settled_state": run_view.get("settled_state", state),
            "node_outcome": answer["outcome"],
            "actor_key": actor["actor_key"],
            "actor_id": actor["actor_id"],
            "evaluated_actor_id": evaluated_actor_id,
            "routing_mismatch": evaluated_actor_id != actor["actor_id"],
            "slot": actor["slot"],
            "rule_id": rule["id"],
            "category": rule["id"],
            "rule_category": rule["category"],
            "manifest_digest": manifest_digest,
            "workflow_digest": workflow_digest,
            "attempt": repeat,
            "bridge_revision": revisions.get(actor["actor_key"]),
            "check": verdict,
            "rating": verdict["rating"],
            "fabrication_flag": bool(fabricated),
            "fabricated_paths": fabricated,
            "grade_id": grade.get("id", "") if isinstance(grade, dict) else "",
            "grade_authority": grade.get("authority", "") if isinstance(grade, dict) else "",
            "grading_actor_id": grading_actor_id,
            "duration_seconds": round(elapsed, 3),
        }
        records.append(record)
        if report_path is not None:
            append_report(report_path, record)
        print(
            f"  {actor['actor_key']:<24} {rule['id']:<24} run={run_id} "
            f"state={state} rating={verdict['rating']} ({verdict['verdict']})",
            file=out,
        )
    return records


def append_report(path: Path, record: dict[str, Any]) -> None:
    """Append one record as a JSON Lines row. Never rewrites a line."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(record, sort_keys=True) + "\n")


def render_table(records: Sequence[dict[str, Any]], out=None) -> None:
    """The final table: one row per run, plus a mean rating per actor."""
    out = sys.stdout if out is None else out
    header = f"{'actor':<24} {'rule':<24} {'state':<10} {'verdict':<10} {'rating':>6}  run"
    print("", file=out)
    print(header, file=out)
    print("-" * len(header), file=out)
    for record in records:
        print(
            f"{record['actor_key']:<24} {record['rule_id']:<24} "
            f"{record['run_state']:<10} {record['check']['verdict']:<10} "
            f"{record['rating']:>6}  {record['run_id']}",
            file=out,
        )
    by_actor: dict[str, list[int]] = {}
    for record in records:
        by_actor.setdefault(record["actor_key"], []).append(record["rating"])
    print("", file=out)
    print(f"{'actor':<24} {'runs':>5} {'mean rating':>12}", file=out)
    for actor_key in sorted(by_actor):
        ratings = by_actor[actor_key]
        mean = sum(ratings) / len(ratings)
        print(f"{actor_key:<24} {len(ratings):>5} {mean:>12.2f}", file=out)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def parse_pairs(values: Sequence[str] | None, flag: str) -> dict[str, str]:
    """``key=value`` repeated flags into a dict, refusing a malformed one."""
    pairs: dict[str, str] = {}
    for value in values or []:
        if "=" not in value:
            raise RunnerError(
                f"{flag} expects key=value (got {value!r})",
                f"e.g. {flag} pi=/home/culture-pi/git/culture-nodes-agent",
            )
        key, _, val = value.partition("=")
        pairs[key.strip()] = val.strip()
    return pairs


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="run.py",
        description=(
            "Dispatch a measurement manifest's rules to its actors, one run at a time, "
            "check each answer and post a proposed grade as an agent principal."
        ),
    )
    parser.add_argument(
        "--manifest",
        default=str(DEFAULT_MANIFEST),
        help=f"manifest file (default: {DEFAULT_MANIFEST.name}, the actor set this graph reaches)",
    )
    parser.add_argument("--workflow", default=str(DEFAULT_WORKFLOW), help="harness-compare graph")
    parser.add_argument("--api-url", default="", help="control plane base URL ($NODES_API_URL)")
    parser.add_argument(
        "--as",
        dest="as_actor",
        default="",
        help="grading principal actor id ($MEASURE_RUNNER_ACTOR_ID); must be kind=agent",
    )
    parser.add_argument(
        "--repo-map",
        action="append",
        metavar="SLOT_OR_KEY=PATH",
        help="checkout path per slot or actor key, on that actor's own host (repeatable)",
    )
    parser.add_argument(
        "--slot",
        action="append",
        metavar="ACTOR_KEY=SLOT",
        help="override the actor-key -> workflow-slot mapping (repeatable)",
    )
    parser.add_argument(
        "--bridge-token",
        action="append",
        metavar="SLOT_OR_KEY=TOKEN",
        help="bearer for a bridge's authenticated /v1/capabilities (repeatable)",
    )
    parser.add_argument(
        "--expect-revision",
        default="",
        help="refuse to dispatch unless every bridge reports this revision",
    )
    parser.add_argument(
        "--qwen-mode", default=DEFAULT_QWEN_MODE, help="ACP session mode for the qwen slot"
    )
    parser.add_argument("--report", default="", help="JSON Lines report path (appended to)")
    parser.add_argument("--timeout", type=float, default=1800.0, help="per-run watch timeout (s)")
    parser.add_argument("--poll-interval", type=float, default=5.0, help="watch poll interval (s)")
    parser.add_argument(
        "--cancel-grace",
        type=float,
        default=CANCEL_GRACE,
        help="how long a timed-out run gets to go terminal after being cancelled (s)",
    )
    parser.add_argument(
        "--gate-only",
        action="store_true",
        help="run the revision gate and print what each bridge is serving, then stop",
    )
    parser.add_argument(
        "--yes",
        action="store_true",
        help="required: this dispatches real, billable agent sessions",
    )
    return parser


def main(argv: Sequence[str] | None = None, out=None, err=None) -> int:
    # Resolved HERE, not as a default argument: a default is bound at import
    # time, which under pytest's capsys is the pre-capture stream — the
    # runner would print to a stream nobody is reading.
    out = sys.stdout if out is None else out
    err = sys.stderr if err is None else err
    args = build_parser().parse_args(list(argv) if argv is not None else None)
    try:
        return _run(args, out, err)
    except RunnerError as exc:
        print(f"error: {exc}", file=err)
        if exc.hint:
            print(f"hint: {exc.hint}", file=err)
        return exc.code


def _run(args: argparse.Namespace, out, err) -> int:
    manifest_module = fleet.load_sibling("manifest")
    try:
        manifest = manifest_module.load_manifest(args.manifest)
        manifest_module.validate_manifest(manifest)
        manifest_digest = manifest_module.digest_manifest(manifest)
    except manifest_module.YamlUnavailableError as exc:
        raise RunnerError(str(exc), "author the manifest as JSON instead", EXIT_ENV_ERROR) from None
    except manifest_module.ManifestError as exc:
        raise RunnerError(str(exc), "fix the manifest and re-run") from None

    # Before the network: no default principal, ever (c29 / h28).
    grading_actor_id = args.as_actor or os.environ.get("MEASURE_RUNNER_ACTOR_ID", "")
    require_grading_principal(grading_actor_id)

    api_url = args.api_url or os.environ.get("NODES_API_URL", "")
    if not api_url:
        raise RunnerError(
            "no control plane URL: --api-url was not given and NODES_API_URL is unset",
            "export NODES_API_URL=https://nodes.culture.dev (LAN writes are 401 since 0.47.0)",
            EXIT_ENV_ERROR,
        )
    api = ApiClient(
        api_url,
        cookie=os.environ.get("NODES_OP_COOKIE", ""),
        bearer=os.environ.get("NODES_OP_BEARER", ""),
    )

    slot_overrides = parse_pairs(args.slot, "--slot")
    repo_map = parse_pairs(args.repo_map, "--repo-map")
    bridge_tokens = parse_pairs(args.bridge_token, "--bridge-token")
    default_bridge_token = os.environ.get("NODES_BRIDGE_TOKEN", "")

    actors = resolve_actors(api, manifest["actors"], slot_overrides)
    refuse_slot_collisions(actors)
    for actor in actors:
        # NODES_BRIDGE_TOKEN_<SLOT> (e.g. NODES_BRIDGE_TOKEN_PI) keeps a
        # per-bridge secret off argv, where `ps` would show it; the flag
        # form stays for tests and one-offs.
        per_slot = os.environ.get(f"NODES_BRIDGE_TOKEN_{actor['slot'].upper()}", "")
        if per_slot:
            bridge_tokens.setdefault(actor["actor_key"], per_slot)
        elif default_bridge_token:
            bridge_tokens.setdefault(actor["actor_key"], default_bridge_token)

    deployments = gate_revisions(actors, args.expect_revision or None, bridge_tokens)
    print(f"manifest: {args.manifest}  digest: {manifest_digest}", file=out)
    for actor in actors:
        block = deployments.get(actor["actor_key"], {})
        print(
            f"  {actor['actor_key']:<24} slot={actor['slot']:<10} "
            f"revision={block.get('revision') or 'unknown'} "
            f"install_mode={block.get('install_mode', 'unknown')} "
            f"dirty={block.get('revision_is_dirty', 'unknown')}",
            file=out,
        )
    if args.gate_only:
        return EXIT_OK

    resolve_grading_actor(api, grading_actor_id)

    if not args.yes:
        raise RunnerError(
            "refusing: this dispatches real, billable agent sessions — re-run with --yes",
            f"the pass is {len(plan_dispatches(manifest, actors))} serial runs; "
            "use --gate-only to check revisions without dispatching",
        )

    workflow_digest = publish_workflow(api, Path(args.workflow))
    report_path = Path(args.report) if args.report else None
    records = run_pass(
        api,
        manifest,
        manifest_digest,
        actors,
        repo_map,
        deployments,
        grading_actor_id,
        workflow_digest,
        report_path,
        qwen_mode=args.qwen_mode,
        timeout=args.timeout,
        interval=args.poll_interval,
        grace=args.cancel_grace,
        out=out,
    )
    render_table(records, out=out)
    if report_path is not None:
        print(f"\nreport appended: {report_path}", file=out)
    return EXIT_OK


if __name__ == "__main__":  # pragma: no cover - CLI entry
    sys.exit(main())
