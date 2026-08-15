import importlib.util
import json
from pathlib import Path

import pytest


SCRIPT = Path(__file__).parents[1] / "scripts" / "render-work-package.py"
SPEC = importlib.util.spec_from_file_location("render_work_package", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(MODULE)


def host(grants=None):
    return {
        "hostname": "thor",
        "confinement": "bubblewrap",
        "dispatch_grants": grants
        or {"workspace-write": ["workspace-write", "tmp-write"]},
        "commit_policy": "local handover ref only",
        "toolchains": [
            {
                "name": "go",
                "state": "present-off-path",
                "path": "/home/thor/.local/bin/go",
                "packaging": "standalone",
                "version": "go1.26.6",
                "usable_in": ["workspace-write"],
            }
        ],
    }


def test_render_uses_plan_contract_and_actor_surface():
    task = MODULE.load_task("close-the-backlog", "t31")
    text = MODULE.render(
        "close-the-backlog",
        "t31",
        "company/codex-thor",
        task,
        host(),
        sandbox="workspace-write",
        repo="/work/culture-nodes",
        branch="ctb/t31",
        base="442393f",
    )
    assert task["summary"] in text
    assert task["instruction"] in text
    assert all(item in text for item in task["acceptance_criteria"])
    assert all(item in text for item in task["covers"])
    assert "/home/thor/.local/bin/go" in text
    assert "cannot pull a remote checkout" in text
    assert "ctb/t31 at 442393f" in text


def test_network_capability_changes_checkout_statement():
    task = MODULE.load_task("close-the-backlog", "t31")
    text = MODULE.render(
        "close-the-backlog",
        "t31",
        "company/developer",
        task,
        host({"unsandboxed": ["network-egress"]}),
        sandbox="unsandboxed",
        repo="/work/culture-nodes",
        branch=None,
        base=None,
    )
    assert "posture advertises network egress" in text
    assert "cannot pull" not in text


def test_refuses_a_sandbox_the_actor_did_not_advertise():
    task = MODULE.load_task("close-the-backlog", "t31")
    with pytest.raises(MODULE.BriefError, match="does not advertise"):
        MODULE.render(
            "close-the-backlog",
            "t31",
            "company/codex-thor",
            task,
            host(),
            sandbox="read-only",
            repo="/work/culture-nodes",
            branch=None,
            base=None,
        )


def test_capability_host_accepts_registry_shape(tmp_path):
    document = {
        "actor_key": "company/codex-thor",
        "capabilities": {"preflight": {"version": 1, "host": host()}},
    }
    path = tmp_path / "actor.json"
    path.write_text(json.dumps(document), encoding="utf-8")
    assert (
        MODULE.capability_host(
            MODULE.read_json(str(path)), "company/codex-thor"
        )["hostname"]
        == "thor"
    )


def test_refuses_capabilities_from_a_different_actor():
    document = {
        "actor_key": "company/codex-orin",
        "capabilities": {"preflight": {"version": 1, "host": host()}},
    }
    with pytest.raises(MODULE.BriefError, match="belongs to actor"):
        MODULE.capability_host(document, "company/codex-thor")
