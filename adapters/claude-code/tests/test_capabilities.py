"""The BACKEND-SPECIFIC half of the preflight capability surface (issue #67,
task t15): the facts this bridge measures about the host it dispatches on.

The protocol shape is asserted in `test_preflight.py` against the shared,
byte-identical module; what is tested here is only what claude-code itself
contributes — its `--permission-mode` vocabulary, the two modes headless
dispatch cannot deliver, and the fact that nothing confines the session.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest

from claude_code_bridge import capabilities, preflight, server
from claude_code_bridge.__main__ import main
from claude_code_bridge.config import Config


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


def test_only_the_modes_that_survive_headless_dispatch_are_advertised(tmp_path):
    """`claude -p` has no TTY, so a mode that can stop and wait for an
    approval never completes. That is a fact about this bridge's dispatch
    shape, and a dispatched task pays for discovering it the hard way."""
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert host["sandbox_modes"] == ["acceptEdits", "bypassPermissions"]
    assert set(host["sandbox_modes_unavailable"]) == {"plan", "default"}
    assert "no TTY" in host["sandbox_modes_unavailable"]["plan"]


def test_the_kernel_probe_changes_nothing_on_this_backend(tmp_path):
    """claude-code's dispatch path has no bubblewrap helper, so the #18/#63
    measurement that decides codex's surface must not silently remove a mode
    here. Same shared helper, different backend facts — which is exactly the
    split the all-backends rule asks for."""
    permissive = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    restricted = capabilities.host_facts(Config(), probes=_restricted(tmp_path))
    assert permissive == restricted


def test_confinement_says_nothing_confines_a_session_here(tmp_path):
    """The honest reading of a permission mode: it governs asking, not
    reaching. Without this sentence a reader sees `bypassPermissions` in a
    list called `sandbox_modes` and concludes the host sandboxes."""
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert host["confinement"].startswith("none:")
    assert "no sandbox flag" in host["confinement"]


def test_the_default_mode_is_the_configured_one(tmp_path):
    host = capabilities.host_facts(
        Config(permission_mode="acceptEdits"), probes=_permissive(tmp_path)
    )
    assert host["default_sandbox_mode"] == "acceptEdits"


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

#: The same two agent-host shapes codex's own test injects, so the two
#: surfaces are comparable tool for tool.
THOR = {"uv": ("/snap/bin/uv", True), "gh": ("/usr/bin/gh", True)}


def _host(table, tmp_path):
    return capabilities.host_facts(
        Config(repo_allowlist=(str(tmp_path),)),
        probes=_permissive(tmp_path),
        locate=lambda name: table.get(name, (None, False)),
        version=lambda _path: "test-version",
    )


def _tool(host, name):
    return next(fact for fact in host["toolchains"] if fact["name"] == name)


def test_every_deliverable_mode_grants_everything_because_nothing_confines(tmp_path):
    """`claude -p` runs with this bridge process's privileges: a permission
    mode decides whether the session ASKS, never what it CAN. Stated in the
    shared grant vocabulary so a toolchain verdict is derivable here the same
    way it is on the codex bridge."""
    host = _host(THOR, tmp_path)
    assert set(host["dispatch_grants"]) == {"acceptEdits", "bypassPermissions"}
    for granted in host["dispatch_grants"].values():
        assert set(granted) == set(preflight.GRANTS)


def test_the_same_snap_uv_that_a_codex_dispatch_cannot_run_is_usable_here(tmp_path):
    """The contrast issue #96 is really about: the fact is per-DISPATCH, not
    per-host. thor's snap-packaged uv is unusable under codex's confined
    modes and perfectly usable through a bridge that confines nothing --
    which is why plan t5 routes Python-side verification to a claude bridge
    rather than to an agent host."""
    uv = _tool(_host(THOR, tmp_path), "uv")
    assert uv["packaging"] == "snap"
    assert uv["usable_in"] == ["acceptEdits", "bypassPermissions"]
    assert "unusable_in" not in uv


def test_an_absent_toolchain_is_still_absent_however_permissive_the_mode(tmp_path):
    go = _tool(_host(THOR, tmp_path), "go")
    assert go["state"] == "absent"
    assert go["usable_in"] == []


def test_the_surface_with_toolchains_is_still_a_document_the_engine_accepts(tmp_path):
    preflight.validate_block(preflight.capability_block(_host(THOR, tmp_path)))
