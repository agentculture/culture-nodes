"""The BACKEND-SPECIFIC half of the preflight capability surface (issue #67,
task t15): the facts this bridge measures about the host it dispatches on.

The protocol shape is asserted in `test_preflight.py` against the shared,
byte-identical module; what is tested here is only what colleague itself
contributes — that it confines nothing, that its worktree bounds where
changes land rather than what a session can reach, and that `open_pr` makes
"harvest" only half the commit policy.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest
from colleague_bridge import capabilities, preflight, server
from colleague_bridge.__main__ import main
from colleague_bridge.config import Config


def _permissive(tmp_path):
    """Probes pointing at a knob that does not exist: a kernel that does not
    restrict unprivileged user namespaces."""
    return ((str(tmp_path / "absent-knob"), "1"),)


def _restricted(tmp_path):
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("1\n")
    return ((str(knob), "1"),)


def test_the_surface_is_a_document_the_control_plane_accepts(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    block = preflight.capability_block(capabilities.host_facts(cfg, probes=_permissive(tmp_path)))
    preflight.validate_block(block)
    assert block["preflight"]["protocol_version"] == preflight.PROTOCOL_VERSION


def test_the_one_mode_a_dispatch_can_get_here_is_stated_not_omitted(tmp_path):
    """`colleague work` takes no sandbox flag. Saying so with the shared
    `unsandboxed` name is a fact; omitting the key would read as "could not
    measure", which is a different and untrue claim."""
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert host["sandbox_modes"] == [preflight.MODE_UNSANDBOXED]
    assert host["default_sandbox_mode"] == preflight.MODE_UNSANDBOXED
    assert "sandbox_modes_unavailable" not in host


def test_the_kernel_probe_changes_nothing_on_this_backend(tmp_path):
    """colleague's dispatch path has no bubblewrap helper, so the #18/#63
    measurement that decides codex's surface must not silently remove a mode
    here. Same shared helper, different backend facts — which is exactly the
    split the all-backends rule asks for."""
    assert capabilities.host_facts(Config(), probes=_permissive(tmp_path)) == (
        capabilities.host_facts(Config(), probes=_restricted(tmp_path))
    )


def test_confinement_distinguishes_where_changes_land_from_what_is_reachable(tmp_path):
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert host["confinement"].startswith("none:")
    assert "throwaway git worktree" in host["confinement"]


def test_opening_a_pr_makes_harvest_only_half_the_policy(tmp_path):
    """With `open_pr` on, colleague publishes a branch itself — a dispatched
    task that read "nothing is committed here" and stopped would be wrong
    about what its own run does."""
    harvest_only = capabilities.host_facts(Config(open_pr=False), probes=_permissive(tmp_path))
    publishing = capabilities.host_facts(Config(open_pr=True), probes=_permissive(tmp_path))
    assert "pull request" not in harvest_only["commit_policy"]
    assert "pull request" in publishing["commit_policy"]


def test_a_dirty_worktree_dispatch_is_disclosed(tmp_path):
    host = capabilities.host_facts(Config(allow_dirty=True), probes=_permissive(tmp_path))
    assert "allow_dirty" in host["commit_policy"]


def test_writable_paths_are_the_repo_allowlist(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path / "a"), str(tmp_path / "b")))
    host = capabilities.host_facts(cfg, probes=_permissive(tmp_path))
    assert host["writable_paths"] == [str(tmp_path / "a"), str(tmp_path / "b")]


def test_an_unconfigured_allowlist_states_that_it_writes_nowhere(tmp_path):
    """Not an absence: an unconfigured bridge refuses every repo, and a
    dispatched task depends on knowing that before it is billed for
    discovering it."""
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert host["writable_paths"] == []


def test_commit_policy_reflects_the_preserve_configuration_in_force(tmp_path):
    cfg = Config(preserve_on_failure=True, preserve_push=True, preserve_remote="origin")
    host = capabilities.host_facts(cfg, probes=_permissive(tmp_path))
    assert "preserve/" in host["commit_policy"]
    assert "origin" in host["commit_policy"]

    local_only = capabilities.host_facts(
        Config(preserve_on_failure=True, preserve_push=False), probes=_permissive(tmp_path)
    )
    assert "local" in local_only["commit_policy"]


# --- how the surface leaves this host -----------------------------------
#
# Two ways out, because registration happens at two different moments: an
# operator registering a bridge that is already running reads it off the
# host that has the facts, and one registering before first start prints it
# from the same code without a server.


def _get(base_url, path, headers=None):
    req = urllib.request.Request(base_url + path, method="GET", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


@pytest.fixture()
def bridge_url(tmp_path):
    cfg = Config(
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg
    srv.shutdown()
    srv.server_close()


def test_the_running_bridge_serves_its_measured_surface(bridge_url):
    base, _cfg = bridge_url
    status, body = _get(base, preflight.CAPABILITIES_PATH, {"Authorization": "Bearer s3cr3t"})
    assert status == 200
    preflight.validate_block(body)
    assert body["preflight"]["host"]["hostname"] == preflight.hostname()


def test_the_surface_is_authenticated_like_every_other_protocol_route(bridge_url):
    """It names a hostname and real filesystem paths. `/healthz` stays the
    only unauthenticated route on every bridge."""
    base, _cfg = bridge_url
    status, _body = _get(base, preflight.CAPABILITIES_PATH)
    assert status == 401


def test_print_capabilities_emits_the_registration_document(capsys, tmp_path):
    """The pre-start path: an operator registers the actor before the bridge
    has ever run, and must not have to hand-write facts about a host they
    are guessing at."""
    rc = main(["--print-capabilities"])
    assert rc == 0
    printed = json.loads(capsys.readouterr().out)
    preflight.validate_block(printed)


# --- toolchains under THIS backend's postures (issue #96) ----------------


def _host(table, tmp_path):
    return capabilities.host_facts(
        Config(repo_allowlist=(str(tmp_path),)),
        probes=_permissive(tmp_path),
        locate=lambda name: table.get(name, (None, False)),
        version=lambda _path: "test-version",
    )


def _tool(host, name):
    return next(fact for fact in host["toolchains"] if fact["name"] == name)


def test_the_one_mode_grants_everything_this_bridge_process_has(tmp_path):
    """`colleague work` takes no sandbox flag: the worktree bounds where
    changes LAND, not what the session can reach. Said in the shared grant
    vocabulary so the toolchain verdicts below are derivable the same way
    they are on the codex bridge."""
    host = _host({}, tmp_path)
    assert set(host["dispatch_grants"]) == {preflight.MODE_UNSANDBOXED}
    assert set(host["dispatch_grants"][preflight.MODE_UNSANDBOXED]) == set(preflight.GRANTS)


def test_the_same_snap_uv_a_codex_dispatch_cannot_run_is_usable_here(tmp_path):
    """thor's snap-packaged uv is unusable under codex's confined modes and
    usable through a bridge that confines nothing — the fact is about the
    DISPATCH, not the host (issue #96)."""
    uv = _tool(_host({"uv": ("/snap/bin/uv", True)}, tmp_path), "uv")
    assert uv["packaging"] == "snap"
    assert uv["usable_in"] == [preflight.MODE_UNSANDBOXED]
    assert "unusable_in" not in uv


def test_an_absent_toolchain_is_still_absent(tmp_path):
    assert _tool(_host({}, tmp_path), "go")["state"] == "absent"


def test_the_surface_with_toolchains_is_still_a_document_the_engine_accepts(tmp_path):
    preflight.validate_block(preflight.capability_block(_host({}, tmp_path)))


# --- git_metadata_writable (issue #94) ------------------------------------


def test_the_git_answer_is_measured_because_a_session_has_this_processs_authority(tmp_path):
    """This backend takes no sandbox flag and runs a session with the bridge
    process's own privileges, so the write this process attempts is the write
    a dispatch attempts. Both answers come from the attempt, never from a mode
    name."""
    repo = tmp_path / "checkout"
    (repo / ".git").mkdir(parents=True)
    cfg = Config(repo_allowlist=(str(repo),))

    measured = capabilities.host_facts(cfg, probes=_permissive(tmp_path))
    assert measured["git_metadata_writable"] == preflight.GIT_METADATA_SUPPORTED

    refused = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), git_probe=lambda _git_dir: False
    )
    assert refused["git_metadata_writable"] == preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX


def test_a_host_with_no_checkout_to_probe_says_so(tmp_path):
    """`not-probed` rather than a refusal: an allowlist that names no checkout
    gives this bridge nothing to attempt, and reporting
    `unsupported-by-sandbox` would blame a sandbox that refused nothing."""
    empty = tmp_path / "no-checkout"
    empty.mkdir()
    host = capabilities.host_facts(
        Config(repo_allowlist=(str(empty),)), probes=_permissive(tmp_path)
    )
    assert host["git_metadata_writable"] == preflight.GIT_METADATA_NOT_PROBED
