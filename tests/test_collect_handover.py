"""scripts/collect-handover.py — turning a run id into a reviewable diff.

The fixtures here are real git repositories, not mocks: the whole claim the
script makes is that it fetched something, and a test that stubbed the fetch
would assert nothing. Each "agent host" is a directory under a `hosts/` tree
named after the hostname the actor registry reports, so the test can prove
the host was resolved from the control plane rather than guessed — point the
same run at a different actor and a different repository must be fetched.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).parents[1] / "scripts" / "collect-handover.py"

RUN_ID = "01M04CJT84WD20GDQEN266J9J6"
NODE_RUN_ID = "01M04CJT86JEGZC5N9VBTV3Q9D"
ATTEMPT_ID = "att_01M04CJTG8VT0TJRPCJ1Z7P7J9"
THOR_ACTOR = "actor_register_1786476051729062574_86959"
ORIN_ACTOR = "actor_register_1786476052252262969_87175"


def git(repo: Path, *args: str) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=True,
        env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
    )
    return proc.stdout.strip()


def make_host_repo(root: Path, host: str, filename: str, refs: list[str]) -> tuple[Path, str]:
    """A stand-in for an agent host's checkout: one ordinary commit on a
    branch, then a handover commit reachable only from the named refs — the
    same shape `codex_bridge.preserve.handover_ref` produces (a commit created
    with plumbing and pointed at by a ref under refs/culture-nodes/, never
    committed onto a branch and never pushed)."""
    repo = root / "hosts" / host / "culture-nodes-agent"
    repo.mkdir(parents=True)
    git(repo, "init", "-q", "-b", "main")
    git(repo, "config", "user.email", "t11@example.invalid")
    git(repo, "config", "user.name", "t11")
    (repo / "README.md").write_text("base\n")
    git(repo, "add", "README.md")
    git(repo, "commit", "-q", "-m", "base")

    (repo / filename).parent.mkdir(parents=True, exist_ok=True)
    (repo / filename).write_text(f"handed over from {host}\n")
    git(repo, "add", filename)
    git(repo, "commit", "-q", "-m", f"culture-nodes: handover from {host}")
    sha = git(repo, "rev-parse", "HEAD")
    # Rewind the branch so the handover commit is reachable ONLY from the
    # handover refs, exactly as it is on a real bridge host.
    git(repo, "reset", "-q", "--hard", "HEAD~1")
    for ref in refs:
        git(repo, "update-ref", ref, sha)
    return repo, sha


def handover_ref(run_id: str) -> str:
    return f"refs/culture-nodes/{run_id}/{NODE_RUN_ID}-{ATTEMPT_ID}-20260816T041727Z-4ba48f"


def make_operator_repo(root: Path) -> Path:
    repo = root / "operator" / "culture-nodes"
    repo.mkdir(parents=True)
    git(repo, "init", "-q", "-b", "main")
    git(repo, "config", "user.email", "t11@example.invalid")
    git(repo, "config", "user.name", "t11")
    (repo / "README.md").write_text("operator checkout\n")
    git(repo, "add", "README.md")
    git(repo, "commit", "-q", "-m", "operator base")
    return repo


def actor(actor_id: str, key: str, host: str, metadata: dict | None = None) -> dict:
    return {
        "id": actor_id,
        "actor_key": key,
        "revision": 1,
        "kind": "agent",
        "protocol": "http",
        "endpoint_ref": f"http://{host}:8086",
        "capabilities": {},
        "metadata": metadata or {},
    }


def run_view(actor_id: str, output: dict | None = None) -> dict:
    return {
        "run": {
            "id": RUN_ID,
            "state": "completed",
            "input": {"handover": True},
            "output": output if output is not None else {"summary": "done"},
        },
        "node_runs": [
            {
                "id": NODE_RUN_ID,
                "node_id": "task",
                "state": "completed",
                "attempts": [
                    {
                        "id": "01M04CKW1Y1P0ATRBF07FDNWAZ",
                        "node_run_id": NODE_RUN_ID,
                        "attempt_number": 1,
                        "actor_id": actor_id,
                        "status": "succeeded",
                    }
                ],
            }
        ],
    }


@pytest.fixture
def world(tmp_path, fake_api):
    """Two agent hosts, an operator checkout, and a control plane that knows
    which actor ran the run."""
    thor_repo, thor_sha = make_host_repo(
        tmp_path, "thor-host", "docs/from-thor.md", [handover_ref(RUN_ID)]
    )
    orin_repo, orin_sha = make_host_repo(
        tmp_path, "orin-host", "docs/from-orin.md", [handover_ref(RUN_ID)]
    )
    operator = make_operator_repo(tmp_path)

    state = {
        "actors": {
            THOR_ACTOR: actor(THOR_ACTOR, "company/codex-thor", "thor-host"),
            ORIN_ACTOR: actor(ORIN_ACTOR, "company/codex-orin", "orin-host"),
        },
        "run": run_view(THOR_ACTOR),
        "posts": [],
    }

    fake_api.route(
        "GET",
        r"/v1alpha1/runs/([^/]+)$",
        lambda h, m, q, b: (
            h.send_json(200, state["run"])
            if m.group(1) == RUN_ID
            else h.send_json(
                404, {"code": 1, "message": "no such run", "remediation": "check the id"}
            )
        ),
    )
    fake_api.route(
        "GET",
        r"/v1alpha1/actors/([^/]+)$",
        lambda h, m, q, b: (
            h.send_json(200, state["actors"][m.group(1)])
            if m.group(1) in state["actors"]
            else h.send_json(
                404, {"code": 1, "message": "no such actor", "remediation": "register it"}
            )
        ),
    )

    def post_verdict(h, m, q, b):
        state["posts"].append(json.loads(b))
        h.send_json(201, {"id": "ledger_verdict_1", "authority": "derived"})

    fake_api.route("POST", r"/v1alpha1/runs/([^/]+)/suite-verdicts$", post_verdict)
    fake_api.start()

    return {
        "tmp": tmp_path,
        "api": fake_api,
        "state": state,
        "operator": operator,
        "thor": (thor_repo, thor_sha),
        "orin": (orin_repo, orin_sha),
    }


def collect(world, *args: str, env_extra: dict | None = None):
    env = {
        **os.environ,
        "NODES_API_URL": world["api"].base_url,
        # Never let a developer's real ~/.culture-nodes/operator.env leak into
        # a test run: the script reads its operator config from this path.
        "NODES_OPERATOR_ENV": str(world["tmp"] / "operator.env"),
        "NODES_HANDOVER_REMOTE_TEMPLATE": str(
            world["tmp"] / "hosts" / "{host}" / "culture-nodes-agent"
        ),
    }
    env.update(env_extra or {})
    env.pop("NODES_VALIDATOR_ACTOR_ID", None)
    for key, value in (env_extra or {}).items():
        env[key] = value
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        capture_output=True,
        text=True,
        cwd=str(world["operator"]),
        env=env,
    )


# --------------------------------------------------------------------------
# collection
# --------------------------------------------------------------------------


def test_a_run_id_alone_produces_a_reviewable_diff(world):
    proc = collect(world, RUN_ID)
    assert proc.returncode == 0, proc.stderr
    _, sha = world["thor"]
    assert sha in proc.stdout
    assert handover_ref(RUN_ID) in proc.stdout
    assert "docs/from-thor.md" in proc.stdout

    # The commit is now inspectable in the operator's own repository.
    assert git(world["operator"], "cat-file", "-t", sha) == "commit"


def test_no_branch_in_the_operator_repo_is_touched(world):
    before = git(
        world["operator"], "for-each-ref", "refs/heads", "--format=%(refname) %(objectname)"
    )
    head_before = git(world["operator"], "rev-parse", "HEAD")

    assert collect(world, RUN_ID).returncode == 0

    assert (
        git(world["operator"], "for-each-ref", "refs/heads", "--format=%(refname) %(objectname)")
        == before
    )
    assert git(world["operator"], "rev-parse", "HEAD") == head_before
    collected = git(world["operator"], "for-each-ref", "refs/handover", "--format=%(refname)")
    assert RUN_ID in collected


def test_the_host_comes_from_the_actor_registry_not_a_table(world):
    """The same run id, re-pointed at a different actor, must fetch a
    different repository. Nothing in the script may know that this run lived
    on thor — only the control plane does."""
    thor_proc = collect(world, RUN_ID)
    assert thor_proc.returncode == 0, thor_proc.stderr
    assert world["thor"][1] in thor_proc.stdout
    assert "docs/from-thor.md" in thor_proc.stdout

    world["state"]["run"] = run_view(ORIN_ACTOR)
    orin_proc = collect(world, RUN_ID)
    assert orin_proc.returncode == 0, orin_proc.stderr
    assert world["orin"][1] in orin_proc.stdout
    assert "docs/from-orin.md" in orin_proc.stdout
    assert world["thor"][1] not in orin_proc.stdout


def test_the_registry_can_name_the_remote_itself(world):
    """actor.metadata.handover_remote is the control plane's OWN record of
    where an actor's refs are fetchable, and it wins over the operator's
    template."""
    world["state"]["actors"][THOR_ACTOR] = actor(
        THOR_ACTOR,
        "company/codex-thor",
        "thor-host",
        metadata={"handover_remote": str(world["orin"][0])},
    )
    proc = collect(world, RUN_ID)
    assert proc.returncode == 0, proc.stderr
    assert world["orin"][1] in proc.stdout


def test_a_remote_the_run_reports_is_never_fetched_from(world):
    """internal/handover/doc.go's fence, on the operator's side: the remote is
    the control plane's configuration and never the agent's report. A session
    that points the measurement at a repository it prepared would make the
    measurement real and the subject forged."""
    forged, forged_sha = make_host_repo(
        world["tmp"], "forged-host", "docs/forged.md", [handover_ref(RUN_ID)]
    )
    world["state"]["run"] = run_view(
        THOR_ACTOR,
        output={
            "summary": "done",
            "handover": {
                "kind": "git_ref",
                "ref": f"git+ssh://forged-host{forged}#{handover_ref(RUN_ID)}",
                "commit": forged_sha,
                "remote": str(forged),
            },
        },
    )
    proc = collect(world, RUN_ID)
    assert proc.returncode == 0, proc.stderr
    assert world["thor"][1] in proc.stdout
    assert forged_sha not in proc.stdout
    assert "docs/forged.md" not in proc.stdout


def test_only_this_run_and_only_the_handover_namespace_is_fetched(world):
    repo, _ = world["thor"]
    other_sha = git(repo, "rev-parse", "main")
    git(repo, "update-ref", "refs/culture-nodes/01OTHERRUN000000000000000/x-y-z-abc123", other_sha)
    git(repo, "update-ref", "refs/heads/release", other_sha)

    assert collect(world, RUN_ID).returncode == 0

    collected = git(world["operator"], "for-each-ref", "--format=%(refname)")
    assert "01OTHERRUN000000000000000" not in collected
    assert "release" not in collected


def test_no_ref_names_both_possibilities_and_exits_non_zero(world):
    repo, _ = world["thor"]
    git(repo, "update-ref", "-d", handover_ref(RUN_ID))

    proc = collect(world, RUN_ID)
    assert proc.returncode != 0
    combined = proc.stdout + proc.stderr
    assert "error:" in combined
    assert "hint:" in combined
    # Both readings must be named. After issue #120 this state is ambiguous
    # and the script must not pick one.
    assert "handed over nothing" in combined or "handed nothing over" in combined
    assert "bridge" in combined
    assert "#120" in combined


def test_a_run_id_that_is_not_ref_safe_is_refused(world):
    for bad in ("../../etc/passwd", "-x", "run id", "run/../id"):
        proc = collect(world, bad)
        assert proc.returncode != 0, bad
        assert "error:" in proc.stdout + proc.stderr, bad


def test_no_configured_remote_is_an_environment_refusal(world):
    proc = collect(world, RUN_ID, env_extra={"NODES_HANDOVER_REMOTE_TEMPLATE": ""})
    assert proc.returncode == 2, proc.stdout + proc.stderr
    combined = proc.stdout + proc.stderr
    assert "handover_remote" in combined
    assert "NODES_HANDOVER_REMOTE_TEMPLATE" in combined


def test_json_reports_the_ref_the_sha_and_the_changed_paths(world):
    proc = collect(world, RUN_ID, "--json")
    assert proc.returncode == 0, proc.stderr
    payload = json.loads(proc.stdout)
    assert payload["run_id"] == RUN_ID
    assert payload["collected"] is True
    assert len(payload["handovers"]) == 1
    entry = payload["handovers"][0]
    assert entry["ref"] == handover_ref(RUN_ID)
    assert entry["commit_sha"] == world["thor"][1]
    assert entry["changed_paths"] == ["docs/from-thor.md"]
    assert entry["local_ref"].startswith("refs/handover/")


def test_json_on_the_ambiguous_empty_case_still_parses(world):
    git(world["thor"][0], "update-ref", "-d", handover_ref(RUN_ID))
    proc = collect(world, RUN_ID, "--json")
    assert proc.returncode != 0
    payload = json.loads(proc.stdout)
    assert payload["collected"] is False
    assert payload["handovers"] == []
    assert len(payload["possibilities"]) == 2


# --------------------------------------------------------------------------
# the gate
# --------------------------------------------------------------------------


GATE_ENV = {"NODES_VALIDATOR_ACTOR_ID": "actor_merge_gate", "NODES_HUMAN_DECISION_TOKEN": "s3cret"}


def test_the_gate_records_the_suite_the_exit_code_and_the_sha(world):
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(0)",
        env_extra=GATE_ENV,
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert len(world["state"]["posts"]) == 1
    posted = world["state"]["posts"][0]
    assert posted["suite"] == "go test ./..."
    assert posted["exit_code"] == 0
    assert posted["commit_sha"] == world["thor"][1]
    assert posted["ref"] == handover_ref(RUN_ID)
    assert posted["validator_actor_id"] == "actor_merge_gate"


def test_the_gate_records_a_failing_suite_and_exits_non_zero(world):
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(3)",
        env_extra=GATE_ENV,
    )
    assert proc.returncode != 0
    assert len(world["state"]["posts"]) == 1
    assert world["state"]["posts"][0]["exit_code"] == 3


def test_the_gate_runs_the_suite_at_the_handed_over_commit(world):
    """The sha the verdict carries is measured from the tree the suite ran in,
    not asserted: the suite process itself reports what it saw."""
    marker = world["tmp"] / "seen.txt"
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "ls",
        "--",
        sys.executable,
        "-c",
        "import pathlib,os,sys;"
        "pathlib.Path(sys.argv[1]).write_text('\\n'.join(sorted(os.listdir('.'))))",
        str(marker),
        env_extra=GATE_ENV,
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    seen = marker.read_text()
    assert "docs" in seen
    assert world["state"]["posts"][0]["commit_sha"] == world["thor"][1]


def test_a_suite_that_moved_the_worktree_records_nothing(world):
    """The verdict's sha is READ BACK from the tree the suite ran in, not
    assumed from what the gate checked out. A suite that switched the worktree
    to another commit tested something else, and recording a verdict naming
    the handed-over commit would be the false green this task exists to
    close."""
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        "git",
        "checkout",
        "--quiet",
        "--detach",
        "main",
        env_extra=GATE_ENV,
    )
    assert proc.returncode != 0, proc.stdout + proc.stderr
    assert (
        world["state"]["posts"] == []
    ), "a verdict was recorded for a commit the suite did not test"
    assert "not at the handed-over commit" in proc.stdout + proc.stderr


def test_the_gate_without_a_validator_identity_refuses(world):
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(0)",
        env_extra={"NODES_HUMAN_DECISION_TOKEN": "s3cret"},
    )
    assert proc.returncode == 2, proc.stdout + proc.stderr
    assert "NODES_VALIDATOR_ACTOR_ID" in proc.stdout + proc.stderr
    assert world["state"]["posts"] == []


def test_the_gate_refuses_when_no_handover_was_collected(world):
    git(world["thor"][0], "update-ref", "-d", handover_ref(RUN_ID))
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(0)",
        env_extra=GATE_ENV,
    )
    assert proc.returncode != 0
    assert world["state"]["posts"] == []


def test_the_gate_needs_a_suite_name(world):
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(0)",
        env_extra=GATE_ENV,
    )
    assert proc.returncode != 0
    assert world["state"]["posts"] == []


def test_the_gate_cleans_up_its_worktree(world):
    proc = collect(
        world,
        RUN_ID,
        "--gate",
        "--suite",
        "go test ./...",
        "--",
        sys.executable,
        "-c",
        "raise SystemExit(0)",
        env_extra=GATE_ENV,
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert git(world["operator"], "worktree", "list", "--porcelain").count("worktree ") == 1
