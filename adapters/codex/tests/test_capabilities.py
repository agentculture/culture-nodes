"""The BACKEND-SPECIFIC half of the preflight capability surface (issue #67,
task t15): the facts this bridge measures about the host it dispatches on.

The protocol shape is asserted in `test_preflight.py` against the shared,
byte-identical module; what is tested here is only what codex itself
contributes — its three `--sandbox` modes, which of them this kernel can
actually enforce, and where a dispatched session's changes end up.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest
from codex_bridge import capabilities, preflight, server
from codex_bridge.__main__ import main
from codex_bridge.codex_cli import SANDBOX_MODES
from codex_bridge.config import Config


def _permissive(tmp_path):
    """Probes pointing at a knob that does not exist: a kernel that does not
    restrict unprivileged user namespaces."""
    return ((str(tmp_path / "absent-knob"), "1"),)


def _restricted(tmp_path):
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("1\n")
    return ((str(knob), "1"),)


def _probe_works():
    """The executable capability probe on a host where the helper starts.

    Injected rather than monkeypatched: since t3 made the probe — not the
    sysctls — authoritative, this is the input that decides the answer, and
    a test that only injected sysctls would be asserting nothing.
    """
    return "available", "bwrap capability probe succeeded"


def _probe_fails():
    return "unavailable", "bwrap capability probe failed (exit 1)"


def _probe_absent():
    return "not-probed", "neither bwrap nor unshare is installed"


def test_the_candidate_modes_are_exactly_the_ones_this_bridge_can_pass(tmp_path):
    """A mode codex accepts but this surface never mentions would be a fact
    the briefing omits; a mode this surface advertises but `codex exec`
    rejects would be a fact it invents. Pinned to the same constant the
    dispatch path validates against."""
    assert set(capabilities.SANDBOX_MODE_CANDIDATES) == SANDBOX_MODES


def test_the_surface_is_a_document_the_control_plane_accepts(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    block = preflight.capability_block(capabilities.host_facts(cfg, probes=_permissive(tmp_path)))
    preflight.validate_block(block)
    assert block["preflight"]["protocol_version"] == preflight.PROTOCOL_VERSION
    assert block["preflight"]["host"]["artifact_publish"] in {
        "supported", "unsupported-by-host", "not-applicable-no-workspace"
    }


def test_a_permissive_kernel_advertises_every_sandbox_mode(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_works
    )
    assert host["sandbox_modes"] == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert host["artifact_publish"] == "unsupported-by-host"
    assert "sandbox_modes_unavailable" not in host


def test_a_restricting_kernel_advertises_only_what_it_can_enforce(tmp_path):
    """Issue #18/#63 on this bridge: `workspace-write` was requested on hosts
    whose kernel restricted unprivileged user namespaces, and every file
    write was silently lost. The surface reports what the host can do."""
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(
        cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails
    )

    assert host["sandbox_modes"] == ["danger-full-access"]
    assert set(host["sandbox_modes_unavailable"]) == {"read-only", "workspace-write"}
    assert "bwrap capability probe failed" in host["sandbox_modes_unavailable"]["workspace-write"]
    assert host["artifact_publish"] == "unsupported-by-host"


def test_an_unprobeable_host_says_so_rather_than_guessing_either_way(tmp_path):
    """The third state t3 introduced, and the reason the probe is worth
    having: with neither bwrap nor unshare installed there is no measurement
    to report. Reporting `available` would invent a fact; reporting the
    restricted wording would blame a kernel nobody asked. The mode is
    withheld, and the reason says the probe never ran."""
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_absent
    )

    assert host["sandbox_modes"] == ["danger-full-access"]
    reason = host["sandbox_modes_unavailable"]["workspace-write"]
    assert "not probed" in reason
    assert "neither bwrap nor unshare is installed" in reason


def test_the_default_mode_is_reported_even_when_this_host_cannot_deliver_it(tmp_path):
    """The fact a dispatch that names no sandbox depends on. Reported next to
    the unavailability rather than quietly rewritten to something that works
    — the bridge advertises, the operator decides."""
    cfg = Config(repo_allowlist=(str(tmp_path),), default_sandbox="workspace-write")
    host = capabilities.host_facts(
        cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails
    )
    assert host["default_sandbox_mode"] == "workspace-write"
    assert "workspace-write" in host["sandbox_modes_unavailable"]


def test_confinement_names_what_actually_confines_a_session(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    permissive = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_works
    )
    restricted = capabilities.host_facts(
        cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails
    )
    assert "user namespace" in permissive["confinement"]
    assert "nothing is confined" in restricted["confinement"]


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
