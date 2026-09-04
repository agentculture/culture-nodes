"""What revision this bridge is actually running (task t32, issue #120 item 4).

Three dispatches reported `handover=true`, committed successfully, and created
no handover ref, because the bridges installed on thor and orin predated the
code that mints them. Nothing anywhere reported a problem — `internal/handover`
correctly records nothing when there is no fetchable ref, so a stale bridge and
an honest refusal produce byte-identical evidence. It was found by running
`git for-each-ref` on the host.

These tests are about making that answerable without the ssh. They exercise
BOTH install shapes, because the two have different answers and a design that
reported only a git sha would be wrong for one of them:

* the codex bridges are `uv tool install`ed COPIES under
  ~/.local/share/uv/tools/ — no git anywhere near them, so the revision has to
  have been stamped in at install time or it is simply unknown;
* the claude bridges on spark are EDITABLE installs pointing at the repo
  source — they never go stale, and they can be running uncommitted
  working-tree code, which is its own hazard and has to be reported.
"""

from __future__ import annotations

import json
import subprocess
import sys

import pytest

from qwen_bridge import deployment, preflight

FULL_SHA = "774d5153c32a2e2fdb86f699d814977d111f1408"


def git(repo, *args):
    subprocess.run(
        ["git", *args],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
    )


def make_repo(tmp_path, name="src"):
    """A real git work tree with a package directory inside it — the shape an
    editable install points at."""
    repo = tmp_path / name
    (repo / "pkg").mkdir(parents=True)
    (repo / "pkg" / "__init__.py").write_text("")
    git(repo, "init", "-q", "-b", "main")
    git(repo, "config", "user.email", "t32@example.invalid")
    git(repo, "config", "user.name", "t32")
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", "base")
    head = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True, check=True
    ).stdout.strip()
    return repo / "pkg", head


# --------------------------------------------------------------------------
# the editable / source-tree shape: git is the live truth
# --------------------------------------------------------------------------


def test_a_package_inside_a_work_tree_reports_that_trees_head(tmp_path):
    package, head = make_repo(tmp_path)

    # direct_url={} isolates this from whatever THIS checkout's own bridge
    # install happens to be — in a dev tree it is editable, and the shape
    # under test here is the other one.
    facts = deployment.measure_deployment(
        package_dir=package, distribution="culture-nodes-qwen-bridge", direct_url={}
    )

    assert facts["revision"] == head
    assert facts["revision_source"] == deployment.REVISION_FROM_GIT
    assert facts["install_mode"] == deployment.INSTALL_SOURCE_TREE


def test_an_editable_install_is_reported_as_editable(tmp_path):
    package, head = make_repo(tmp_path)

    facts = deployment.measure_deployment(
        package_dir=package,
        distribution="culture-nodes-qwen-bridge",
        direct_url={"url": f"file://{package.parent}", "dir_info": {"editable": True}},
    )

    assert facts["install_mode"] == deployment.INSTALL_EDITABLE
    assert facts["revision"] == head


def test_an_editable_install_running_uncommitted_code_says_so(tmp_path):
    """The hazard that only exists in this mode. An editable install serves
    whatever is in the working tree, so "revision abc123" can be true and
    still not describe the code that is running."""
    package, head = make_repo(tmp_path)
    (package / "server.py").write_text("# not committed\n")

    facts = deployment.measure_deployment(
        package_dir=package,
        distribution="culture-nodes-qwen-bridge",
        direct_url={"url": f"file://{package.parent}", "dir_info": {"editable": True}},
    )

    assert facts["revision"] == head
    assert facts["revision_is_dirty"] is True
    assert "uncommitted" in facts["staleness"].lower()


def test_a_clean_work_tree_is_not_dirty(tmp_path):
    package, _ = make_repo(tmp_path)

    facts = deployment.measure_deployment(package_dir=package, distribution="x", direct_url={})

    assert facts["revision_is_dirty"] is False


# --------------------------------------------------------------------------
# the copy shape: only a stamp can answer, and its absence is stated
# --------------------------------------------------------------------------


def test_a_copy_with_no_git_and_no_stamp_reports_no_revision_at_all(tmp_path):
    """Absence, not a guess. An absent key reads as absence, where a null or
    an empty string would read as a fact about the deployment — the same rule
    HOST_KEYS states for every other fact here."""
    package = tmp_path / "site-packages" / "qwen_bridge"
    package.mkdir(parents=True)

    facts = deployment.measure_deployment(package_dir=package, distribution="x")

    assert "revision" not in facts
    assert "revision_source" not in facts
    assert facts["install_mode"] == deployment.INSTALL_COPY
    # And it says WHY it cannot answer, because "no revision" with no reason
    # is indistinguishable from a bridge that forgot to report one.
    assert "stamp" in facts["staleness"]


def test_a_copy_with_a_build_stamp_reports_the_revision_the_deploy_shipped(tmp_path):
    package = tmp_path / "site-packages" / "qwen_bridge"
    package.mkdir(parents=True)
    (package / deployment.REVISION_STAMP_FILE).write_text(
        json.dumps(
            {"revision": FULL_SHA, "stamped_at": "2026-08-16T08:00:00Z", "source": "deploy.sh"}
        )
    )

    facts = deployment.measure_deployment(package_dir=package, distribution="x")

    assert facts["revision"] == FULL_SHA
    assert facts["revision_source"] == deployment.REVISION_FROM_STAMP
    assert facts["install_mode"] == deployment.INSTALL_COPY
    assert facts["stamped_at"] == "2026-08-16T08:00:00Z"


def test_a_vcs_install_reports_the_commit_pip_recorded(tmp_path):
    """PEP 610 records the exact commit for a VCS install. It is a real
    answer and must not be thrown away just because there is no work tree."""
    package = tmp_path / "site-packages" / "qwen_bridge"
    package.mkdir(parents=True)

    facts = deployment.measure_deployment(
        package_dir=package,
        distribution="x",
        direct_url={
            "url": "https://github.com/agentculture/culture-nodes",
            "vcs_info": {"vcs": "git", "commit_id": FULL_SHA},
        },
    )

    assert facts["revision"] == FULL_SHA
    assert facts["revision_source"] == deployment.REVISION_FROM_VCS_METADATA


def test_a_work_tree_outranks_a_stale_stamp(tmp_path):
    """Precedence matters and is not arbitrary: git is LIVE and a stamp is a
    record of one past install. An editable install whose tree has moved on
    since the stamp was written is running the tree, not the stamp."""
    package, head = make_repo(tmp_path)
    (package / deployment.REVISION_STAMP_FILE).write_text(json.dumps({"revision": FULL_SHA}))

    facts = deployment.measure_deployment(package_dir=package, distribution="x", direct_url={})

    assert facts["revision"] == head
    assert facts["revision_source"] == deployment.REVISION_FROM_GIT


def test_a_malformed_stamp_is_ignored_rather_than_reported(tmp_path):
    package = tmp_path / "site-packages" / "qwen_bridge"
    package.mkdir(parents=True)
    (package / deployment.REVISION_STAMP_FILE).write_text("{not json")

    facts = deployment.measure_deployment(package_dir=package, distribution="x")

    assert "revision" not in facts


@pytest.mark.parametrize("bad", ["HEAD", "main", "774d515", "", "X" * 40, FULL_SHA.upper()])
def test_a_stamp_that_does_not_name_a_full_commit_is_refused(tmp_path, bad):
    """The same refusal internal/handover's validateFullSHA makes, for the
    same reason: a revision that is not an unambiguous 40-hex commit means
    something different tomorrow, and a deploy record nobody can resolve
    later is not a record."""
    package = tmp_path / "site-packages" / "qwen_bridge"
    package.mkdir(parents=True)
    (package / deployment.REVISION_STAMP_FILE).write_text(json.dumps({"revision": bad}))

    facts = deployment.measure_deployment(package_dir=package, distribution="x")

    assert "revision" not in facts, f"{bad!r} was accepted as a revision"


# --------------------------------------------------------------------------
# it reaches the surface
# --------------------------------------------------------------------------


def test_deployment_is_an_agreed_host_key():
    assert "deployment" in preflight.HOST_KEYS


def test_the_host_block_carries_the_deployment_facts():
    host = preflight.host_block(
        hostname="thor",
        commit_policy={"commits": True},
        deployment={"install_mode": "copy", "revision": FULL_SHA},
    )

    assert host["deployment"]["revision"] == FULL_SHA


def test_a_bridge_that_cannot_measure_its_deployment_omits_the_key():
    host = preflight.host_block(hostname="thor", commit_policy={"commits": True}, deployment=None)

    assert "deployment" not in host


def test_the_real_bridge_advertises_its_own_deployment(monkeypatch, tmp_path):
    """End to end through the backend's own capabilities module: whatever this
    installation is, the surface says which shape it is."""
    from qwen_bridge import capabilities
    from qwen_bridge.config import Config

    cfg = Config(actor_id="a", auth_token="t", qwen_bin=sys.executable)
    facts = capabilities.host_facts(cfg, capability_probe=lambda: ("bwrap", ""))

    assert "deployment" in facts
    assert facts["deployment"]["install_mode"] in {
        deployment.INSTALL_EDITABLE,
        deployment.INSTALL_COPY,
        deployment.INSTALL_SOURCE_TREE,
        deployment.INSTALL_UNKNOWN,
    }
    assert facts["deployment"]["package"] == "qwen_bridge"
