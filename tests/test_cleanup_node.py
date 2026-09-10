"""examples/cleanup/cleanup.py — the cleanup node (plan loop-closure t13).

Real local git fixtures throughout (a scratch bare remote standing in for
`origin`, a seeding clone) plus tests/fake_api.FakeNodesAPI standing in for
the control plane — the same posture tests/test_combining_loop_node.py takes,
for the same reason: the whole claim this program makes is that it read real
reachability and moved (or refused to move) real refs, and a test that
stubbed git would assert nothing about that claim.

Never touches a real remote, never opens a network connection beyond the
loopback fake API.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent))
from fake_api import FakeNodesAPI  # noqa: E402

SCRIPT = Path(__file__).parents[1] / "examples" / "cleanup" / "cleanup.py"
WORK_ITEM = "SCRUM-42"
RUN_PARKED_REACHABLE = "01M0CLEANUPRUNREACHABLE00"
RUN_PARKED_UNREACHABLE = "01M0CLEANUPRUNUNREACHABLE"
RUN_COMPLETED = "01M0CLEANUPRUNCOMPLETED00"
RUN_OTHER_ITEM = "01M0CLEANUPRUNOTHERITEM00"
PARKED_NODE = "human-merges-pr"


# ---------------------------------------------------------------------------
# git helpers
# ---------------------------------------------------------------------------


def git(repo: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=check,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
    )
    return proc.stdout.strip()


def remote_refs(bare: Path) -> dict[str, str]:
    out = git(bare, "--git-dir", str(bare), "for-each-ref", "--format=%(refname) %(objectname)")
    return dict(line.split(" ", 1) for line in out.splitlines()) if out else {}


def _commit(repo: Path, path: str, message: str) -> str:
    (repo / path).write_text(f"{message}\n")
    git(repo, "add", path)
    git(repo, "commit", "-q", "-m", message)
    return git(repo, "rev-parse", "HEAD")


@pytest.fixture
def remote(tmp_path: Path) -> tuple[Path, dict[str, str]]:
    """A bare `origin` seeded with:

    * `main` at two commits (base, landed);
    * `refs/heads/review-fix/<RUN_PARKED_REACHABLE>-…` at the landed commit
      (reachable from main: the fix was merged);
    * `refs/culture-nodes/<RUN_PARKED_REACHABLE>/…` at the base commit
      (reachable: an ancestor of main);
    * `refs/heads/review-fix/<RUN_PARKED_UNREACHABLE>-…` and
      `refs/culture-nodes/<RUN_PARKED_UNREACHABLE>/…` at a side commit no
      branch merged (unreachable from main);
    * `refs/heads/review-fix/<RUN_OTHER_ITEM>-…`, a ref of a run that does
      NOT belong to the work item, at the landed commit (reachable, but not
      ours to delete).
    """
    bare = tmp_path / "origin.git"
    subprocess.run(["git", "init", "-q", "--bare", "--initial-branch=main", str(bare)], check=True)
    seed = tmp_path / "seed"
    subprocess.run(["git", "init", "-q", "--initial-branch=main", str(seed)], check=True)
    git(seed, "config", "user.email", "cleanup-test@example.invalid")
    git(seed, "config", "user.name", "cleanup-test")
    git(seed, "remote", "add", "origin", str(bare))

    base = _commit(seed, "README.md", "base")
    landed = _commit(seed, "fix.py", "landed fix")
    git(seed, "push", "-q", "origin", "main")

    git(seed, "checkout", "-q", "-b", "side", base)
    side = _commit(seed, "side.py", "unmerged side work")
    git(seed, "checkout", "-q", "main")

    refs = {
        f"refs/heads/review-fix/{RUN_PARKED_REACHABLE}-fix-20260907T000000Z-abc123": landed,
        f"refs/culture-nodes/{RUN_PARKED_REACHABLE}/20260907T000000Z-abc123": base,
        f"refs/heads/review-fix/{RUN_PARKED_UNREACHABLE}-fix-20260907T000001Z-def456": side,
        f"refs/culture-nodes/{RUN_PARKED_UNREACHABLE}/20260907T000001Z-def456": side,
        f"refs/heads/review-fix/{RUN_OTHER_ITEM}-fix-20260907T000002Z-0a0a0a": landed,
    }
    for ref, sha in refs.items():
        git(seed, "push", "-q", "origin", f"{sha}:{ref}")
    # `side` is never pushed: the unreachable refs point at a commit no
    # remote branch holds, which is exactly what makes them unreachable.
    return bare, {**refs, "refs/heads/main": landed}


# ---------------------------------------------------------------------------
# fake control plane
# ---------------------------------------------------------------------------


def _run(run_id: str, state: str, work_item: str = WORK_ITEM) -> dict:
    return {"id": run_id, "state": state, "work_item": work_item, "workflow_key": "pr-upkeep"}


def _view(run_id: str, node_id: str, node_state: str) -> dict:
    return {
        "run": _run(run_id, "waiting" if node_state != "completed" else "completed"),
        "tokens": [],
        "node_runs": [
            {"id": f"nr-{run_id}-fix", "node_id": "fix", "state": "completed", "attempts": []},
            {
                "id": f"nr-{run_id}-{node_id}",
                "node_id": node_id,
                "state": node_state,
                "attempts": [],
            },
        ],
    }


@pytest.fixture
def api():
    """The item's runs: two parked at human-merges-pr, one already completed.
    Records every cancel POST (path + decoded body) for the assertions.

    `fake.views` and `fake.cancel_status` are the two knobs a test turns: the
    first changes what `GET /v1alpha1/runs/{id}` reports, the second makes the
    cancel endpoint answer something other than 200 — 412 being the control
    plane refusing the `parked_at` precondition."""
    fake = FakeNodesAPI()
    cancels: list[tuple[str, dict | None]] = []
    views = {
        RUN_PARKED_REACHABLE: _view(RUN_PARKED_REACHABLE, PARKED_NODE, "waiting"),
        RUN_PARKED_UNREACHABLE: _view(RUN_PARKED_UNREACHABLE, PARKED_NODE, "running"),
        RUN_COMPLETED: _view(RUN_COMPLETED, PARKED_NODE, "completed"),
    }

    def list_runs(handler, _match, query, _body):
        assert query.get("work_item") == [WORK_ITEM], query
        items = [
            _run(RUN_PARKED_REACHABLE, "waiting"),
            _run(RUN_PARKED_UNREACHABLE, "running"),
            _run(RUN_COMPLETED, "completed"),
        ]
        handler.send_json(200, {"items": items})

    def get_run(handler, match, _query, _body):
        view = fake.views.get(match.group(1))
        if view is None:
            handler.send_json(404, {"error": {"message": "no such run"}})
            return
        handler.send_json(200, view)

    def cancel(handler, match, _query, body):
        decoded = json.loads(body) if body else None
        cancels.append((match.group(1), decoded))
        if fake.cancel_status != 200:
            handler.send_json(
                fake.cancel_status,
                {"error": {"message": "the run is no longer at that node", "code": 1}},
            )
            return
        handler.send_json(
            200, {**_run(match.group(1), "cancelled"), "reason": (decoded or {}).get("reason", "")}
        )

    fake.route("GET", r"^/v1alpha1/runs$", list_runs)
    fake.route("GET", r"^/v1alpha1/runs/([^/]+)$", get_run)
    fake.route("POST", r"^/v1alpha1/runs/([^/]+)/cancel$", cancel)
    fake.cancels = cancels  # type: ignore[attr-defined]
    fake.views = views  # type: ignore[attr-defined]
    fake.cancel_status = 200  # type: ignore[attr-defined]
    fake.start()
    yield fake
    fake.stop()


# ---------------------------------------------------------------------------
# driver
# ---------------------------------------------------------------------------


def run_cleanup(
    bare: Path, api: FakeNodesAPI, payload: dict, tmp_path: Path, **env_overrides: str
) -> tuple[subprocess.CompletedProcess[str], dict | None]:
    env = {
        **os.environ,
        "NODES_INPUT_JSON": json.dumps(payload),
        "NODES_API_URL": api.base_url,
        "CLEANUP_REMOTE": str(bare),
        "CLEANUP_DEFAULT_BRANCH": "main",
        "CLEANUP_RECORD_PATH": str(tmp_path / "record.json"),
        "GIT_TERMINAL_PROMPT": "0",
        **env_overrides,
    }
    proc = subprocess.run(
        [sys.executable, str(SCRIPT)], capture_output=True, text=True, env=env, check=False
    )
    record = None
    lines = [line for line in proc.stdout.splitlines() if line.strip()]
    if lines:
        record = json.loads(lines[-1])
    return proc, record


def merged_fact(**extra) -> dict:
    return {
        "source": "github_pr",
        "repository": "agentculture/culture-nodes",
        "number": 307,
        "url": "https://github.com/agentculture/culture-nodes/pull/307",
        "merged_at": "2026-09-07T10:00:00Z",
        "issue_key": WORK_ITEM,
        **extra,
    }


def closed_fact(**extra) -> dict:
    return {
        "source": "github_pr",
        "repository": "agentculture/culture-nodes",
        "number": 308,
        "head_sha": "deadbeef",
        "closed_at": "2026-09-07T11:00:00Z",
        "work_item": WORK_ITEM,
        **extra,
    }


# ---------------------------------------------------------------------------
# tests
# ---------------------------------------------------------------------------


def test_merged_pr_deletes_reachable_refs_and_declines_unreachable_ones(remote, api, tmp_path):
    bare, before = remote
    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert record is not None, proc.stdout

    after = remote_refs(bare)
    reachable_branch = f"refs/heads/review-fix/{RUN_PARKED_REACHABLE}-fix-20260907T000000Z-abc123"
    reachable_handover = f"refs/culture-nodes/{RUN_PARKED_REACHABLE}/20260907T000000Z-abc123"
    unreachable_branch = (
        f"refs/heads/review-fix/{RUN_PARKED_UNREACHABLE}-fix-20260907T000001Z-def456"
    )
    unreachable_handover = f"refs/culture-nodes/{RUN_PARKED_UNREACHABLE}/20260907T000001Z-def456"

    # Reachable refs of the item are gone from the remote ...
    assert reachable_branch not in after
    assert reachable_handover not in after
    # ... unreachable refs are still there, untouched ...
    assert after[unreachable_branch] == before[unreachable_branch]
    assert after[unreachable_handover] == before[unreachable_handover]
    # ... and main itself was never moved.
    assert after["refs/heads/main"] == before["refs/heads/main"]

    deleted = {entry["ref"]: entry["commit"] for entry in record["deleted"]}
    assert deleted == {
        reachable_branch: before[reachable_branch],
        reachable_handover: before[reachable_handover],
    }
    declined = {entry["ref"]: entry for entry in record["declined"]}
    assert set(declined) == {unreachable_branch, unreachable_handover}
    assert all(entry["why"] == "unreachable" for entry in declined.values())
    assert declined[unreachable_branch]["commit"] == before[unreachable_branch]


def test_a_reachable_ref_of_another_work_item_is_reported_not_deleted(remote, api, tmp_path):
    bare, before = remote
    other = f"refs/heads/review-fix/{RUN_OTHER_ITEM}-fix-20260907T000002Z-0a0a0a"
    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert remote_refs(bare)[other] == before[other]
    assert other in record["unattributed"]
    assert other not in {entry["ref"] for entry in record["deleted"]}


def test_merged_pr_cancels_parked_runs_with_reason_pr_merged(remote, api, tmp_path):
    bare, _ = remote
    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr

    cancelled = {run_id: body for run_id, body in api.cancels}
    # Each POST carries the reason AND the `parked_at` precondition naming the
    # node the view above reported: the control plane re-checks it inside the
    # cancel transaction, so a run the approval advanced in between is refused
    # rather than cancelled out from under its live downstream nodes.
    assert cancelled == {
        RUN_PARKED_REACHABLE: {"reason": "pr_merged", "parked_at": PARKED_NODE},
        RUN_PARKED_UNREACHABLE: {"reason": "pr_merged", "parked_at": PARKED_NODE},
    }
    assert record["reason"] == "pr_merged"
    assert sorted(entry["run_id"] for entry in record["cancelled_runs"]) == sorted(
        [RUN_PARKED_REACHABLE, RUN_PARKED_UNREACHABLE]
    )
    assert all(entry["reason"] == "pr_merged" for entry in record["cancelled_runs"])
    # The completed run was skipped and says so; it was never POSTed.
    skipped = {entry["run_id"]: entry["why"] for entry in record["skipped_runs"]}
    assert skipped == {RUN_COMPLETED: "already_terminal"}


def test_closed_pr_cancels_parked_runs_with_reason_pr_closed_and_declines_its_refs(
    remote, api, tmp_path
):
    bare, before = remote
    proc, record = run_cleanup(bare, api, closed_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr

    assert {
        body for _, body in map(lambda c: (c[0], json.dumps(c[1], sort_keys=True)), api.cancels)
    } == {json.dumps({"reason": "pr_closed", "parked_at": PARKED_NODE}, sort_keys=True)}
    assert {run_id for run_id, _ in api.cancels} == {RUN_PARKED_REACHABLE, RUN_PARKED_UNREACHABLE}
    assert record["reason"] == "pr_closed"
    assert record["pull_request"] == 308
    # The unreachable refs are declined, never deleted, on a close too.
    after = remote_refs(bare)
    unreachable_branch = (
        f"refs/heads/review-fix/{RUN_PARKED_UNREACHABLE}-fix-20260907T000001Z-def456"
    )
    assert after[unreachable_branch] == before[unreachable_branch]
    assert unreachable_branch in {entry["ref"] for entry in record["declined"]}


def test_the_record_is_one_derived_shaped_json_line_listing_everything(remote, api, tmp_path):
    bare, _ = remote
    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr

    lines = [line for line in proc.stdout.splitlines() if line.strip()]
    assert len(lines) == 1, proc.stdout
    assert record["record"] == "cleanup"
    assert record["authority_shape"] == "derived"
    assert record["work_item"] == WORK_ITEM
    assert record["repository"] == "agentculture/culture-nodes"
    for key in (
        "deleted",
        "declined",
        "unattributed",
        "cancelled_runs",
        "skipped_runs",
        "sweep_state",
        "failures",
    ):
        assert key in record, key
    assert record["failures"] == []
    # Per-item sweep state: the sweep keeps none outside the control plane's
    # own watermark rows, and the record says so rather than claiming a drop.
    assert record["sweep_state"]["dropped"] == []
    assert "watermark" in record["sweep_state"]["note"]
    # The same record was written to the granted path, byte-for-byte.
    assert json.loads((tmp_path / "record.json").read_text()) == record


def test_a_fact_with_neither_merged_nor_closed_timestamp_is_refused(remote, api, tmp_path):
    bare, before = remote
    payload = merged_fact()
    del payload["merged_at"]
    proc, _ = run_cleanup(bare, api, payload, tmp_path)
    assert proc.returncode == 1
    assert "merged_at" in proc.stderr and "closed_at" in proc.stderr
    assert remote_refs(bare) == before
    assert api.cancels == []


def test_a_remote_that_refuses_the_deletion_is_a_recorded_failure_not_a_claimed_removal(
    remote, api, tmp_path
):
    """The remote rejects every ref deletion (a pre-receive hook standing in
    for a revoked credential or a protected ref). Nothing may be listed as
    deleted, the refusal is a named failure, the cancels still happen (they
    use the API principal, not the git credential), and the exit is 2 -- an
    environment failure the operator must see, never a domain outcome and
    never a silent success."""
    bare, before = remote
    hook = bare / "hooks" / "pre-receive"
    hook.write_text("#!/bin/sh\necho 'deletions refused by policy' >&2\nexit 1\n")
    hook.chmod(0o755)

    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 2, proc.stderr
    assert record is not None
    assert record["deleted"] == []
    failed_steps = {(f["step"], f.get("ref")) for f in record["failures"]}
    reachable_branch = f"refs/heads/review-fix/{RUN_PARKED_REACHABLE}-fix-20260907T000000Z-abc123"
    reachable_handover = f"refs/culture-nodes/{RUN_PARKED_REACHABLE}/20260907T000000Z-abc123"
    assert ("delete", reachable_branch) in failed_steps
    assert ("delete", reachable_handover) in failed_steps
    # The remote is exactly as it was.
    assert remote_refs(bare) == before
    # And the item's parked runs were still cancelled with the reason.
    assert {run_id for run_id, _ in api.cancels} == {RUN_PARKED_REACHABLE, RUN_PARKED_UNREACHABLE}


def test_a_run_that_advanced_past_the_approval_node_is_left_running_not_cancelled(
    remote, api, tmp_path
):
    """The control plane refuses the cancel with 412: between this node's
    `run_view` and its POST, the approval that merged the PR advanced the run
    off `human-merges-pr`, so cancelling would have killed the downstream
    nodes the approval just made live. Nothing may be claimed as cancelled,
    the run is recorded as left running with the reason, and the exit stays 0
    -- a refused precondition is the honest answer, not an environment
    failure."""
    bare, _ = remote
    api.cancel_status = 412  # type: ignore[attr-defined]

    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert record is not None

    # Both live runs were attempted, both carrying the precondition ...
    assert {run_id for run_id, _ in api.cancels} == {RUN_PARKED_REACHABLE, RUN_PARKED_UNREACHABLE}
    assert all(body["parked_at"] == PARKED_NODE for _, body in api.cancels)
    # ... and neither is claimed as cancelled.
    assert record["cancelled_runs"] == []
    left = {entry["run_id"]: entry["why"] for entry in record["left_running"]}
    assert left == {
        RUN_PARKED_REACHABLE: f"advanced_past_{PARKED_NODE}",
        RUN_PARKED_UNREACHABLE: f"advanced_past_{PARKED_NODE}",
    }
    assert record["failures"] == []


def test_a_run_not_at_the_approval_node_is_left_running_and_never_posted(remote, api, tmp_path):
    """The view already shows the run somewhere else: the node does not even
    POST, and says which node it saw live."""
    bare, _ = remote
    api.views[RUN_PARKED_REACHABLE] = _view(  # type: ignore[attr-defined]
        RUN_PARKED_REACHABLE, "run-suite", "running"
    )

    proc, record = run_cleanup(bare, api, merged_fact(), tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert record is not None

    assert {run_id for run_id, _ in api.cancels} == {RUN_PARKED_UNREACHABLE}
    left = {entry["run_id"]: entry for entry in record["left_running"]}
    assert set(left) == {RUN_PARKED_REACHABLE}
    assert left[RUN_PARKED_REACHABLE]["why"] == f"not_at_{PARKED_NODE}"
    assert left[RUN_PARKED_REACHABLE]["live_nodes"] == ["run-suite"]
