"""examples/land/land.py -- the land node core (loop-closure task t6; spec
c3/c15/c35/c39/c40, honesty h9/h12/h22/h26/h27).

Real local git fixtures throughout: a scratch BARE remote standing in for the
GitHub repository, a clone of it standing in for the culture-land account's
checkout (the workspace the runner executes the node in), and a second clone
standing in for the PRODUCING actor's checkout -- the one that minted the
handover ref under refs/culture-nodes/<run-id>/ and whose working tree the
node resets to the landed tip once nothing else is active on it. No network,
no ssh, no credential: every remote is a filesystem path, so the push
credential lane is exercised only as "not required for this remote" plus a
direct unit test of the env-file reader against a fixture file.

The node is run IN-PROCESS (``land.main([])`` with the runner's environment
contract set up by ``run_land``) so a step can be intercepted where a real
deployment would have a concurrent writer -- a client-side ``pre-push`` hook
in the land checkout plays the racing lander, and the control-plane
active-attempt probe is a one-function seam monkeypatched per test. Stdout is
the record stream: one JSON line per step reached, plus the routing record on
a conflict, plus one terminal ``land_result`` line.
"""

from __future__ import annotations

import ast
import importlib.util
import io
import json
import os
import re
import socket
import subprocess
import sys
import tokenize
from pathlib import Path

import pytest

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "land"
SCRIPT = EXAMPLE_DIR / "land.py"
WORKFLOW = EXAMPLE_DIR / "workflow.yaml"

PRODUCING_RUN = "01M0PRODUCINGRUNID0000000A"
LAND_RUN = "01M0LANDRUNID00000000000B"
ACTOR_ID = "01M0ACTORROWID00000000000C"
#: The SAME actor identity, one registration revision earlier: re-registering
#: mints a new actors row (append-only, internal/store/postgres ListActors'
#: doc comment), and node runs dispatched before it keep pointing at the old
#: row id. Both rows share ACTOR_KEY.
OLD_ACTOR_ID = "01M0ACTORROWID00000000000B"
ACTOR_KEY = "codex/thor"
TARGET = "pr/loop-closure"
WORK_ITEM = "SCRUM-42"


def _load_land():
    spec = importlib.util.spec_from_file_location("land", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    # Registered BEFORE exec: `from __future__ import annotations` +
    # @dataclass resolves the class's module through sys.modules at class
    # definition time (the same note tests/test_decide_reply.py carries).
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


land = _load_land()


def load_land_probe():
    """The control-plane probe module (the checkout lease's other half),
    registered under the name land.py's `active_attempts` imports."""
    if "land_probe" in sys.modules:
        return sys.modules["land_probe"]
    spec = importlib.util.spec_from_file_location("land_probe", EXAMPLE_DIR / "land_probe.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def unchecked_probe(*_a, **_k):
    """The seam every test that does not care about the control plane uses:
    the answer a lander gets when NODES_API_URL is unset."""
    probe_module = load_land_probe()
    return probe_module.AttemptProbe(None, probe_module.NOT_CONFIGURED, "no control plane in tests")


def load_land_reply():
    """The t8 sibling, registered under the name land.py's hooks import, so a
    test can seam its control-plane read the way `active_attempts` is."""
    if "land_reply" in sys.modules:
        return sys.modules["land_reply"]
    spec = importlib.util.spec_from_file_location("land_reply", EXAMPLE_DIR / "land_reply.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


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
        env={
            **os.environ,
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_AUTHOR_NAME": "land-test",
            "GIT_AUTHOR_EMAIL": "land-test@example.invalid",
            "GIT_COMMITTER_NAME": "land-test",
            "GIT_COMMITTER_EMAIL": "land-test@example.invalid",
        },
    )
    return proc.stdout.strip()


def commit_file(repo: Path, path: str, body: str, message: str) -> str:
    target = repo / path
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body)
    git(repo, "add", path)
    git(repo, "commit", "-q", "-m", message)
    return git(repo, "rev-parse", "HEAD")


def mint_handover(repo: Path, run_id: str, path: str, body: str, message: str) -> tuple[str, str]:
    """Mint a handover commit the way a bridge does (preserve.py:395-426): a
    commit on top of the checkout's HEAD, pointed at by a ref under
    refs/culture-nodes/<run-id>/<tail>, never on a branch and never pushed.
    Returns (ref, sha) and leaves the checkout on the branch it was on."""
    original = git(repo, "rev-parse", "--abbrev-ref", "HEAD")
    git(repo, "checkout", "-q", "--detach")
    sha = commit_file(repo, path, body, message)
    ref = f"refs/culture-nodes/{run_id}/20260907T000000Z-{path.replace('/', '-')}"
    git(repo, "update-ref", ref, sha)
    git(repo, "checkout", "-q", original)
    return ref, sha


def log_subjects(repo: Path, rev: str) -> list[str]:
    out = git(repo, "log", "--format=%s", rev)
    return out.splitlines() if out else []


# ---------------------------------------------------------------------------
# fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
def origin(tmp_path: Path) -> Path:
    """The scratch bare remote: main plus the PR branch TARGET."""
    seed = tmp_path / "seed"
    seed.mkdir()
    git(seed, "init", "-q", "-b", "main")
    commit_file(seed, "README.md", "base\n", "base")
    git(seed, "checkout", "-q", "-b", TARGET)
    commit_file(seed, "feature.txt", "feature work\n", "feature work")
    bare = tmp_path / "origin.git"
    git(tmp_path, "clone", "-q", "--bare", str(seed), str(bare))
    return bare


@pytest.fixture
def land_ws(tmp_path: Path, origin: Path) -> Path:
    """The culture-land account's checkout: NODES_WORKSPACE for the node."""
    ws = tmp_path / "culture-nodes-land"
    git(tmp_path, "clone", "-q", "-b", TARGET, str(origin), str(ws))
    return ws


@pytest.fixture
def actor_checkout(tmp_path: Path, origin: Path) -> Path:
    """The producing actor's checkout, on TARGET, where the handover ref is
    minted. Its path doubles as the actor's `handover_remote`."""
    co = tmp_path / "culture-nodes-developer"
    git(tmp_path, "clone", "-q", "-b", TARGET, str(origin), str(co))
    return co


@pytest.fixture
def competitor(tmp_path: Path, origin: Path) -> Path:
    """A third clone that pushes to TARGET behind the lander's back."""
    co = tmp_path / "competitor"
    git(tmp_path, "clone", "-q", "-b", TARGET, str(origin), str(co))
    return co


@pytest.fixture(autouse=True)
def _quiet_env(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.delenv("NODES_API_URL", raising=False)
    monkeypatch.delenv("GITHUB_TOKEN_WORKER", raising=False)
    monkeypatch.setenv("LAND_LEASE_WAIT_SECONDS", "0")
    monkeypatch.setenv("LAND_PUSH_ENV_FILE", "/nonexistent/bridge-push.env")
    # The probe is a seam: default to "no control plane configured" so a
    # test that does not care never reaches the network.
    monkeypatch.setattr(land, "active_attempts", unchecked_probe)
    # The gate chain (t7, land_gate.py) needs a toolchain and a repository
    # shaped like this one; tests/test_land_gate.py runs it for real against
    # such an origin. Here the scratch remotes carry neither, so the hook is
    # a recorded stub -- a test-side seam, not a production knob.
    monkeypatch.setattr(
        land, "gate_hook", lambda _ctx: {"outcome": "stubbed", "by": "tests/test_land_gate.py"}
    )


def run_land(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    *,
    workspace: Path,
    handover_ref: str,
    handover_remote: Path | str,
    target: str = TARGET,
    run_id: str = PRODUCING_RUN,
    land_run_id: str = LAND_RUN,
    attempt: str = "01M0ATTEMPT0000000000000D",
) -> tuple[int, list[dict]]:
    payload = {
        "handover_ref": handover_ref,
        "handover_remote": str(handover_remote),
        "target_branch": target,
        "work_item": WORK_ITEM,
        "actor_id": ACTOR_ID,
        "run_id": run_id,
    }
    monkeypatch.setenv("NODES_INPUT_JSON", json.dumps(payload))
    monkeypatch.setenv("NODES_WORKSPACE", str(workspace))
    monkeypatch.setenv("NODES_RUN_ID", land_run_id)
    monkeypatch.setenv("NODES_NODE_RUN_ID", "01M0NODERUN000000000000000E")
    monkeypatch.setenv("NODES_ATTEMPT_ID", attempt)
    capsys.readouterr()
    code = land.main([])
    out = capsys.readouterr().out
    records = [json.loads(line) for line in out.splitlines() if line.strip()]
    return code, records


def steps(records: list[dict]) -> dict[str, dict]:
    """Last record per step name (a step reached twice keeps its latest)."""
    return {r["step"]: r for r in records if r.get("record") == "land_step"}


def result(records: list[dict]) -> dict:
    finals = [r for r in records if r.get("record") == "land_result"]
    assert len(finals) == 1, records
    return finals[0]


# ---------------------------------------------------------------------------
# the happy path: one ref, one commit, every step recorded
# ---------------------------------------------------------------------------


def test_a_ref_lands_as_one_commit_on_the_branch(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    ref, sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    after = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert after != before
    assert (
        git(origin, "rev-parse", f"{after}^") == before
    ), "exactly one commit on top of the old tip"
    assert log_subjects(origin, after)[0] == "t6: add a"
    # The handover base was already the tip, so the landed commit IS the
    # handover commit -- no rewrite when none is needed.
    assert after == sha

    res = result(records)
    assert res["outcome"] == "landed"
    assert res["landed_commit"] == after
    assert res["work_item"] == WORK_ITEM

    by = steps(records)
    for name in land.STEPS:
        assert name in by, f"no record for step {name}: {sorted(by)}"
    assert by["fetch"]["outcome"] == "ok"
    assert by["fetch"]["commit"] == sha
    assert by["lease"]["outcome"] == "ok"
    assert by["lease"]["scope"] == "target_branch"
    assert by["rebase"]["outcome"] == "ok"
    assert by["push"]["outcome"] == "ok"
    assert by["push"]["rounds"] == 1
    assert by["push"]["credential"] == "not_required"
    assert by["gate"]["outcome"] == "stubbed", by["gate"]
    # t8's steps are wired; with no control plane configured they say so.
    for hook in ("reply", "resolve"):
        assert by[hook]["outcome"] == "skipped"
        assert by[hook]["reason"] == "no_control_plane"
    assert by["checkout_lease"]["outcome"] == "ok"
    assert by["reset"]["outcome"] == "ok"
    # Every record names the join key and both runs.
    for rec in records:
        assert rec["work_item"] == WORK_ITEM
        assert rec["run_id"] == PRODUCING_RUN
        assert rec["land_run_id"] == LAND_RUN

    # The producing actor's checkout was reset to the landed tip, on TARGET.
    assert git(actor_checkout, "rev-parse", "HEAD") == after
    assert git(actor_checkout, "rev-parse", "--abbrev-ref", "HEAD") == TARGET
    # And no scratch worktree was left behind in the land checkout.
    assert len(git(land_ws, "worktree", "list", "--porcelain").split("\n\n")) == 1


def test_a_ref_behind_the_tip_is_rebased_not_merged(
    monkeypatch, capsys, origin, land_ws, actor_checkout, competitor
):
    ref, sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    # The branch moves before the land node runs (an unrelated file).
    other = commit_file(competitor, "docs/note.md", "note\n", "competitor: note")
    git(competitor, "push", "-q", "origin", f"HEAD:refs/heads/{TARGET}")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert tip != sha, "the commit was rewritten onto the new tip"
    assert git(origin, "rev-parse", f"{tip}^") == other
    assert git(origin, "rev-list", "--merges", "--count", tip) == "0", "never a merge commit"
    assert log_subjects(origin, tip)[:2] == ["t6: add a", "competitor: note"]
    assert steps(records)["rebase"]["rewritten"] is True


# ---------------------------------------------------------------------------
# staleness: a racing writer between rebase and push
# ---------------------------------------------------------------------------


def install_racing_pre_push(land_ws: Path, competitor: Path, *, times: int) -> Path:
    """A client-side pre-push hook in the land checkout that, the first
    `times` pushes, lands a competing commit on TARGET first -- so the
    lander's push is rejected non-fast-forward exactly that many times."""
    hooks = land_ws / ".git" / "hooks"
    hooks.mkdir(exist_ok=True)
    counter = land_ws / ".git" / "race-count"
    counter.write_text("0")
    hook = hooks / "pre-push"
    hook.write_text(
        "#!/bin/sh\n"
        f"n=$(cat '{counter}')\n"
        f'if [ "$n" -lt {times} ]; then\n'
        f"  echo $((n + 1)) > '{counter}'\n"
        f"  cd '{competitor}' && echo \"race $n\" >> race.txt && git add race.txt "
        f'&& git -c user.name=race -c user.email=race@example.invalid commit -q -m "race $n" '
        f"&& git push -q origin HEAD:refs/heads/{TARGET}\n"
        "fi\n"
        "exit 0\n"
    )
    hook.chmod(0o755)
    return counter


def test_a_stale_rebase_refetches_once_then_lands(
    monkeypatch, capsys, origin, land_ws, actor_checkout, competitor
):
    ref, _sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    install_racing_pre_push(land_ws, competitor, times=1)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    push = steps(records)["push"]
    assert push["outcome"] == "ok"
    assert push["rounds"] == 2
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert log_subjects(origin, tip)[:2] == ["t6: add a", "race 0"]


def test_a_rebase_stale_twice_routes_to_a_human_and_pushes_nothing(
    monkeypatch, capsys, origin, land_ws, actor_checkout, competitor
):
    ref, _sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    install_racing_pre_push(land_ws, competitor, times=5)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ROUTED_HUMAN, records
    assert result(records)["outcome"] == "routed_human"
    push = steps(records)["push"]
    assert push["outcome"] == "routed"
    assert push["rounds"] == land.MAX_LAND_ROUNDS == 2
    routing = [r for r in records if r.get("record") == "routing"]
    assert len(routing) == 1
    assert routing[0]["data"]["reason"] == land.REASON_STALE_AFTER_RETRY
    # Only the two racing commits reached the branch; ours never did.
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert log_subjects(origin, tip)[:2] == ["race 1", "race 0"]
    assert "t6: add a" not in log_subjects(origin, tip)
    # The producing checkout was NOT reset -- nothing landed.
    assert "reset" not in steps(records)


def test_a_remote_policy_rejection_is_a_refusal_naming_what_the_remote_said(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    """A `[remote rejected]` from branch protection or a pre-receive hook is
    not the branch moving: rebasing again changes nothing and the second
    push is rejected for the same reason. It is a refusal naming the
    remote's own message, not `stale_after_retry`."""
    hook = origin / "hooks" / "pre-receive"
    hook.parent.mkdir(parents=True, exist_ok=True)
    hook.write_text(
        "#!/bin/sh\n" 'echo "protected branch hook declined: review required" >&2\n' "exit 1\n"
    )
    hook.chmod(0o755)
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_REFUSED, records
    outcome = result(records)
    assert outcome["outcome"] == "refused"
    assert "review required" in outcome["error"], "the remote's own message is the record"
    assert not [
        r for r in records if r.get("record") == "routing"
    ], "a policy refusal is not a repair route: nothing about it is retryable"
    assert "push" not in steps(records)
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == before
    assert "reset" not in steps(records)


# ---------------------------------------------------------------------------
# the per-target-branch lease
# ---------------------------------------------------------------------------


def test_two_refs_on_one_branch_land_in_sequence_with_the_second_waiting(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    ref_a, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    run_b = "01M0PRODUCINGRUNID0000000B"
    ref_b, _ = mint_handover(actor_checkout, run_b, "src/b.py", "b = 2\n", "t6: add b")

    code, _ = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref_a, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED
    after_a = git(origin, "rev-parse", f"refs/heads/{TARGET}")

    # A first lander still holds the branch lease (a live process on this
    # host -- the test's own pid) when the second arrives.
    lock = land.branch_lock_path(land_ws, TARGET)
    with land.LocalLock(lock).held(holder={"land_run_id": "01M0OTHERLANDER", "pid": os.getpid()}):
        code, records = run_land(
            monkeypatch,
            capsys,
            workspace=land_ws,
            handover_ref=ref_b,
            handover_remote=actor_checkout,
            run_id=run_b,
            land_run_id="01M0LANDRUNID00000000000C",
        )
    assert code == land.EXIT_WAITING, records
    assert result(records)["outcome"] == "waiting"
    lease = steps(records)["lease"]
    assert lease["outcome"] == "waiting"
    assert lease["scope"] == "target_branch"
    assert lease["holder"]["land_run_id"] == "01M0OTHERLANDER"
    assert "push" not in steps(records), "nothing past the lease ran"
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == after_a

    # Lease released: the second lands on top of the first, no side branch.
    code, records = run_land(
        monkeypatch,
        capsys,
        workspace=land_ws,
        handover_ref=ref_b,
        handover_remote=actor_checkout,
        run_id=run_b,
        land_run_id="01M0LANDRUNID00000000000C",
    )
    assert code == land.EXIT_LANDED, records
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert log_subjects(origin, tip)[:3] == ["t6: add b", "t6: add a", "feature work"]
    assert git(origin, "for-each-ref", "--format=%(refname)", "refs/heads/").split() == [
        "refs/heads/main",
        f"refs/heads/{TARGET}",
    ]


def test_a_stale_branch_lease_from_a_dead_process_is_reclaimed(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    lock = land.branch_lock_path(land_ws, TARGET)
    lock.mkdir(parents=True)
    # A pid that cannot be alive on this host: pid_max caps real ones far lower.
    (lock / "holder.json").write_text(
        json.dumps({"pid": 2**22 + 12345, "host": socket.gethostname()})
    )

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED, records
    assert steps(records)["lease"]["reclaimed_stale"] is True


# ---------------------------------------------------------------------------
# idempotency: re-running after the push adds nothing
# ---------------------------------------------------------------------------


def test_a_rerun_after_push_adds_no_second_commit(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    code, _ = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    count = git(origin, "rev-list", "--count", tip)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED, records
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip
    assert git(origin, "rev-list", "--count", tip) == count
    by = steps(records)
    assert by["rebase"]["outcome"] == "skipped" and by["rebase"]["reason"] == "already_on_branch"
    assert by["push"]["outcome"] == "skipped" and by["push"]["reason"] == "already_on_branch"
    assert result(records)["landed_commit"] == tip


def test_a_rerun_detects_an_equivalent_rewritten_commit(
    monkeypatch, capsys, origin, land_ws, actor_checkout, competitor
):
    """After a rebase the landed sha differs from the handover sha; the
    re-run must still recognise the patch is on the branch (git cherry)."""
    ref, sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    commit_file(competitor, "docs/note.md", "note\n", "competitor: note")
    git(competitor, "push", "-q", "origin", f"HEAD:refs/heads/{TARGET}")
    code, _ = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert tip != sha

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED, records
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip
    assert steps(records)["push"]["outcome"] == "skipped"


# ---------------------------------------------------------------------------
# a seeded conflict: the routing record, and no push
# ---------------------------------------------------------------------------


def test_a_rebase_conflict_routes_to_a_human_and_pushes_nothing(
    monkeypatch, capsys, origin, land_ws, actor_checkout, competitor
):
    ref, _ = mint_handover(
        actor_checkout, PRODUCING_RUN, "feature.txt", "actor's version\n", "t6: rewrite"
    )
    commit_file(competitor, "feature.txt", "competitor's version\n", "competitor: rewrite")
    git(competitor, "push", "-q", "origin", f"HEAD:refs/heads/{TARGET}")
    tip_before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    head_before = git(actor_checkout, "rev-parse", "HEAD")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ROUTED_HUMAN, records
    assert result(records)["outcome"] == "routed_human"
    by = steps(records)
    assert by["rebase"]["outcome"] == "routed"
    assert by["rebase"]["conflicted_paths"] == ["feature.txt"]
    assert "push" not in by and "reset" not in by

    routing = [r for r in records if r.get("record") == "routing"]
    assert len(routing) == 1
    rec = routing[0]
    # The internal/repair shape: a derived decision by an identified
    # deterministic producer, selecting a human, saying it dispatched nothing.
    assert rec["record_type"] == "decision"
    assert rec["authority"] == "derived"
    assert rec["origin"]["kind"] == "validator"
    assert rec["origin"]["actor_id"]
    assert rec["run_id"] == LAND_RUN
    data = rec["data"]
    assert data["selected"] == "human"
    assert data["options"] == ["repair", "human"]
    assert data["reason"] == land.REASON_REBASE_CONFLICT == "rebase_conflict"
    assert data["dispatched"] is False
    assert data["router"] == land.ROUTER_COLLECTION_METHOD
    assert data["bound"]["max_attempts"] == 2 and data["bound"]["window_seconds"] == 86400
    assert data["bound"]["at_ceiling"] == "route to a human node"
    assert data["conflicted_paths"] == ["feature.txt"]
    assert "feature.txt" in data["rationale"]

    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip_before
    assert git(actor_checkout, "rev-parse", "HEAD") == head_before
    assert len(git(land_ws, "worktree", "list", "--porcelain").split("\n\n")) == 1
    assert not (land_ws / ".git" / "rebase-merge").exists()


def test_a_dot_github_change_in_the_handover_routes_to_a_human(
    monkeypatch, capsys, origin, land_ws, actor_checkout
):
    ref, _ = mint_handover(
        actor_checkout, PRODUCING_RUN, ".github/workflows/x.yml", "on: push\n", "t6: ci"
    )
    tip_before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_ROUTED_HUMAN, records
    routing = [r for r in records if r.get("record") == "routing"][0]
    assert routing["data"]["reason"] == "out_of_workflow_scope"
    assert routing["data"]["guarded_paths"] == [".github/workflows/x.yml"]
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip_before


# ---------------------------------------------------------------------------
# the fences: what the script must never do, grep-asserted
# ---------------------------------------------------------------------------


def code_only(source: str) -> str:
    """The script minus its docstrings and comments: the grep guards below
    read what EXECUTES, so prose saying "never --force" cannot trip them
    and a flag hidden in code cannot hide behind them."""
    tree = ast.parse(source)
    lines = source.splitlines(keepends=True)
    for node in ast.walk(tree):
        if isinstance(node, (ast.Module, ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            body = node.body
            if body and isinstance(body[0], ast.Expr) and isinstance(body[0].value, ast.Constant):
                doc = body[0]
                for i in range(doc.lineno - 1, doc.end_lineno):
                    lines[i] = "\n"
    stripped = []
    for tok in tokenize.generate_tokens(io.StringIO("".join(lines)).readline):
        if tok.type == tokenize.COMMENT:
            continue
        stripped.append(tok.string)
    return " ".join(stripped)


def test_the_script_never_forces_a_push():
    code = code_only(SCRIPT.read_text(encoding="utf-8"))
    assert "--force" not in code
    assert "force-with-lease" not in code
    assert not re.search(r"['\"]\+refs/", code), "a leading + on a push refspec is a force"
    assert not re.search(r"['\"]push['\"][^\n]*['\"]-f['\"]", code)
    # And the executable text still contains the one push it is allowed.
    assert '"push"' in code


def test_the_script_makes_no_merge_api_call():
    code = code_only(SCRIPT.read_text(encoding="utf-8"))
    assert not re.search(r"/pulls/[^\n\"']*/merge", code)
    assert not re.search(r"/merges\b", code)
    assert "api.github.com" not in code
    assert "merge" not in code.replace("merge-base", "").replace(
        "git-common-dir", ""
    ), "the only `merge` the script may spell is git merge-base; a PR merge is a human's act"
    # The only HTTP the node performs is the control-plane read, and it lives
    # in the probe module -- git talks to GitHub through the git binary,
    # never through a REST client here.
    urlopen = r"urllib\s*\.\s*request\s*\.\s*urlopen"
    assert not re.findall(urlopen, code), "land.py itself opens no URL"
    probe_code = code_only((EXAMPLE_DIR / "land_probe.py").read_text(encoding="utf-8"))
    assert len(re.findall(urlopen, probe_code)) == 1
    assert "api.github.com" not in probe_code
    assert '"push"' not in probe_code, "the probe reads the control plane and pushes nothing"
    assert "subprocess" not in probe_code, "the probe reads the control plane and runs nothing"


def test_the_handover_ref_fence(monkeypatch, capsys, land_ws, actor_checkout):
    commit = git(actor_checkout, "rev-parse", "HEAD")
    git(actor_checkout, "update-ref", "refs/heads/sneaky", commit)
    code, records = run_land(
        monkeypatch,
        capsys,
        workspace=land_ws,
        handover_ref="refs/heads/sneaky",
        handover_remote=actor_checkout,
    )
    assert code == land.EXIT_REFUSED
    assert result(records)["outcome"] == "refused"
    assert not [r for r in records if r.get("record") == "land_step"]

    ref, _ = mint_handover(actor_checkout, "SOMEOTHERRUN", "src/a.py", "a\n", "x")
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert (
        code == land.EXIT_REFUSED
    ), "a ref minted under another run's id is not this run's handover"


# ---------------------------------------------------------------------------
# the checkout the job started in (the deployment precondition, measured)
# ---------------------------------------------------------------------------


def test_a_workspace_that_is_not_a_checkout_is_refused_by_name(
    monkeypatch, capsys, tmp_path, actor_checkout
):
    """A runner that started this job in an empty directory -- the shape a
    container-backed runner with an ephemeral workspace gives -- is a named
    deployment refusal, not `fatal: not a git repository` inside the fetch."""
    empty = tmp_path / "empty-workspace"
    empty.mkdir()
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a\n", "x")
    code, records = run_land(
        monkeypatch, capsys, workspace=empty, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_ENVIRONMENT
    step = steps(records)["workspace"]
    assert step["outcome"] == "refused"
    assert step["reason"] == "workspace_not_a_checkout"
    assert step["workspace"] == str(empty.resolve())
    # Nothing past the precondition ran: no fetch, no lease, no push.
    assert set(steps(records)) == {"workspace"}
    res = result(records)
    assert res["outcome"] == "environment"
    assert "NODES_WORKSPACE" in res["hint"]


def test_a_checkout_without_an_origin_is_refused_by_name(
    monkeypatch, capsys, land_ws, actor_checkout
):
    git(land_ws, "remote", "remove", "origin")
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a\n", "x")
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_ENVIRONMENT
    step = steps(records)["workspace"]
    assert step["outcome"] == "refused"
    assert step["reason"] == "origin_not_configured"
    assert set(steps(records)) == {"workspace"}


def test_a_real_checkout_records_the_workspace_it_measured(
    monkeypatch, capsys, land_ws, actor_checkout
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a\n", "x")
    _, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    step = steps(records)["workspace"]
    assert step["outcome"] == "ok"
    assert step["workspace"] == str(land_ws.resolve())
    assert step["git_dir"].endswith(".git")


def test_an_option_shaped_remote_is_refused(monkeypatch, capsys, land_ws, actor_checkout):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a\n", "x")
    code, _ = run_land(
        monkeypatch,
        capsys,
        workspace=land_ws,
        handover_ref=ref,
        handover_remote="--upload-pack=evil",
    )
    assert code == land.EXIT_REFUSED


# ---------------------------------------------------------------------------
# the push credential lane
# ---------------------------------------------------------------------------


def test_push_token_is_read_from_bridge_push_env_and_never_from_input(tmp_path, monkeypatch):
    env_file = tmp_path / "bridge-push.env"
    env_file.write_text("# comment\nGITHUB_TOKEN_WORKER=tok-from-file\nOTHER=x\n")
    monkeypatch.setenv("LAND_PUSH_ENV_FILE", str(env_file))
    monkeypatch.delenv("GITHUB_TOKEN_WORKER", raising=False)
    assert land.push_token() == "tok-from-file"
    monkeypatch.setenv("GITHUB_TOKEN_WORKER", "tok-from-env")
    assert land.push_token() == "tok-from-env", "the process environment wins over the file"
    monkeypatch.delenv("GITHUB_TOKEN_WORKER")
    monkeypatch.setenv("LAND_PUSH_ENV_FILE", str(tmp_path / "missing.env"))
    assert land.push_token() is None


def test_an_https_origin_without_a_token_is_an_environment_refusal(
    monkeypatch, capsys, land_ws, actor_checkout
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a\n", "x")
    git(land_ws, "remote", "set-url", "origin", "https://github.example.invalid/org/repo.git")
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_ENVIRONMENT, records
    assert result(records)["outcome"] == "environment"
    assert "GITHUB_TOKEN_WORKER" in result(records)["error"]


def test_askpass_answers_username_and_password_from_its_own_environment(tmp_path):
    script = tmp_path / "askpass.sh"
    script.write_text(land.ASKPASS_SCRIPT)
    script.chmod(0o700)
    env = {**os.environ, "GITHUB_TOKEN_WORKER": "tok"}
    user = subprocess.run(
        [str(script), "Username for 'https://github.com': "],
        capture_output=True,
        text=True,
        env=env,
    )
    # git's real prompt names the user (`https://<user>@github.com`); a bare host
    # here keeps the committed fixture free of anything shaped like an account.
    pw = subprocess.run(
        [str(script), "Password for 'https://github.com': "],
        capture_output=True,
        text=True,
        env=env,
    )
    assert user.stdout.strip() == "x-access-token"
    assert pw.stdout.strip() == "tok"


# ---------------------------------------------------------------------------
# the workflow document
# ---------------------------------------------------------------------------


def test_workflow_declares_the_inputs_and_routes_every_exit_code():
    yaml = pytest.importorskip("yaml")
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    spec = doc["spec"]
    required = set(spec["contract"]["input"]["schema"]["required"])
    assert required == {
        "handover_ref",
        "handover_remote",
        "target_branch",
        "work_item",
        "actor_id",
        "run_id",
    }

    land_node = spec["nodes"]["land"]
    assert land_node["kind"] == "code"
    assert set(land_node["contract"]["outcomes"]) == {"passed", "failed"}
    assert land_node["ledger"]["observe"] == ["evidence"]
    assert land_node["policy"]["retry"]["maxAttempts"] == 1
    argv = " ".join(land_node["operation"]["argv"])
    assert "LAND_SOURCE_URL" in argv and "LAND_SOURCE_SHA256" in argv
    for ref in ("LAND_SOURCE_URL", "LAND_SOURCE_SHA256", "GITHUB_TOKEN_WORKER", "NODES_API_URL"):
        assert ref in land_node["operation"]["environmentRefs"]
    # Every module land.py reaches for is fetched beside it, by granted digest.
    for module in ("land_gate.py", "land_probe.py", "land_reply.py"):
        assert module in argv, module
        stem = module.removesuffix(".py").removeprefix("land").strip("_").upper()
        for ref in (f"LAND_{stem}_SOURCE_URL", f"LAND_{stem}_SOURCE_SHA256"):
            assert ref in land_node["operation"]["environmentRefs"], ref

    verdict = spec["nodes"]["land-verdict"]
    assert verdict["kind"] == "decision"
    selected = {s["outcome"]: s["when"] for s in verdict["select"]}
    assert f"== {land.EXIT_ROUTED_HUMAN}" in selected["routed_human"]
    assert f"== {land.EXIT_WAITING}" in selected["waiting"]
    edges = {(e["from"], e["to"]) for e in spec["edges"]}
    assert ("land.passed", "landed") in edges
    assert ("land.failed", "land-verdict") in edges
    assert ("land-verdict.routed_human", "needs-human") in edges
    assert ("land-verdict.waiting", "waiting") in edges
    assert spec["nodes"]["landed"]["kind"] == "end"
    assert spec["nodes"]["needs-human"]["kind"] == "approval"


def test_workflow_prose_names_every_granted_value():
    yaml = pytest.importorskip("yaml")
    text = WORKFLOW.read_text(encoding="utf-8")
    assert "Deployment configuration" in text
    for name in (
        "LAND_SOURCE_URL",
        "LAND_SOURCE_SHA256",
        "LAND_PROBE_SOURCE_URL",
        "LAND_PROBE_SOURCE_SHA256",
        "GITHUB_TOKEN_WORKER",
        "runner://headspace/land",
    ):
        assert name in text
    # The checkout is a DEPLOYMENT grant like the rest: workspaceRef cannot
    # carry it, so the prose has to, and the node has to measure it.
    assert "THE CHECKOUT THIS NODE RUNS IN" in text
    assert "NODES_WORKSPACE" in text
    assert "workspace_not_a_checkout" in text
    assert "workspaceRef" not in yaml.safe_load(text)["spec"]["nodes"]["land"]["operation"]
