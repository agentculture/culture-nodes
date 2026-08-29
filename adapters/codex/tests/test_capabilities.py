"""The BACKEND-SPECIFIC half of the preflight capability surface (issue #67,
task t15): the facts this bridge measures about the host it dispatches on.

The protocol shape is asserted in `test_preflight.py` against the shared,
byte-identical module; what is tested here is only what codex itself
contributes — its three `--sandbox` modes, which of them this kernel can
actually enforce, and where a dispatched session's changes end up.
"""

from __future__ import annotations

import json
import os
import pwd
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
        "supported",
        "unsupported-by-host",
        "not-applicable-no-workspace",
    }


def test_a_permissive_kernel_advertises_every_sandbox_mode(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(cfg, probes=_permissive(tmp_path), capability_probe=_probe_works)
    assert host["sandbox_modes"] == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert host["artifact_publish"] == "unsupported-by-host"
    assert "sandbox_modes_unavailable" not in host


def test_a_restricting_kernel_no_longer_withholds_a_mode(tmp_path):
    """task t2 (issue #243): `_REQUIRES_USERNS` is now empty, because a
    dedicated OS-user account — not a withheld `--sandbox` mode — is what
    confines a session. A restricted kernel (and even a probe that reports
    the helper failed) must no longer shrink `sandbox_modes` or populate
    `sandbox_modes_unavailable`; issue #18/#63's silent-degrade risk is
    still named, but in `confinement`'s prose, not by hiding a mode."""
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails)

    assert host["sandbox_modes"] == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert "sandbox_modes_unavailable" not in host
    assert host["artifact_publish"] == "unsupported-by-host"


def test_the_capability_probe_no_longer_gates_sandbox_availability(tmp_path):
    """Before task t2, an unprobeable host (neither bwrap nor unshare
    installed) withheld a mode with a `not probed` reason. `_REQUIRES_USERNS`
    being empty means `measure_sandbox_modes` never calls the probe at all —
    every candidate mode is available regardless of what it would have
    reported."""
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_absent
    )

    assert host["sandbox_modes"] == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert "sandbox_modes_unavailable" not in host


def test_the_default_mode_is_reported_regardless_of_kernel_restriction(tmp_path):
    """The fact a dispatch that names no sandbox depends on, still reported
    — trivially now, since every mode this bridge names is always
    advertised as available."""
    cfg = Config(repo_allowlist=(str(tmp_path),), default_sandbox="workspace-write")
    host = capabilities.host_facts(cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails)
    assert host["default_sandbox_mode"] == "workspace-write"
    assert "sandbox_modes_unavailable" not in host


def test_confinement_names_what_actually_confines_a_session(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    account = pwd.getpwuid(os.getuid()).pw_name
    permissive = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_works
    )
    restricted = capabilities.host_facts(
        cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails
    )
    assert permissive["confinement"].startswith(f"unix-user:{account}: ")
    assert restricted["confinement"].startswith(f"unix-user:{account}: ")
    assert "user namespace" in permissive["confinement"]
    assert "user namespace" in restricted["confinement"]


def test_confinement_names_the_restriction_without_withholding_a_mode(tmp_path):
    """task t2 (issue #243) acceptance: on a host with the userns sysctl
    restricted, the prose still says so — read directly off the sysctls,
    not off `sandbox_modes_unavailable`, which stays empty/absent."""
    cfg = Config(repo_allowlist=(str(tmp_path),))
    host = capabilities.host_facts(cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails)

    assert "sandbox_modes_unavailable" not in host
    assert host["sandbox_modes"] == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert "restrict" in host["confinement"]
    assert "apparmor_restrict_unprivileged_userns=1" in host["confinement"]


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
#
# test_preflight.py asserts the shared measurement. What is asserted here is
# codex's own posture map: which of its three `--sandbox` modes grants what,
# and therefore what the two agent hosts' surfaces actually say. The two
# hosts are injected, because neither of them is the host running pytest.

THOR = {  # snap-packaged uv, gh from the distro, codex off PATH
    "uv": ("/snap/bin/uv", True),
    "gh": ("/usr/bin/gh", True),
    "codex": ("/home/thor/.local/bin/codex", False),
}
ORIN = {  # standalone uv under ~/.local/bin
    "uv": ("/home/orin/.local/bin/uv", True),
    "gh": ("/home/orin/.local/bin/gh", True),
    "codex": ("/home/orin/.local/bin/codex", True),
}


def _host(table, tmp_path, versions=None):
    versions = versions or {}
    return capabilities.host_facts(
        Config(repo_allowlist=(str(tmp_path),)),
        probes=_permissive(tmp_path),
        capability_probe=_probe_works,
        locate=lambda name: table.get(name, (None, False)),
        version=lambda path: versions.get(path, "test-version"),
    )


def _tool(host, name):
    return next(fact for fact in host["toolchains"] if fact["name"] == name)


def test_thors_snap_uv_and_orins_standalone_one_are_different_facts(tmp_path):
    """Issue #96's acceptance criterion, against codex's real posture map. A
    surface saying `uv: present` was true on both hosts and useless on both:
    neither could run a Python suite, and they failed for different reasons
    (runs 01M03374VAKH0KHN0GDZ466NP4 and 01M0342X60F3NY8MH150G48AZ6)."""
    thor_uv = _tool(_host(THOR, tmp_path), "uv")
    orin_uv = _tool(_host(ORIN, tmp_path), "uv")

    assert thor_uv["packaging"] == "snap"
    assert orin_uv["packaging"] == "standalone"
    assert "snap-confine" in thor_uv["unusable_in"]["workspace-write"]
    assert "01M03374VAKH0KHN0GDZ466NP4" in thor_uv["unusable_in"]["workspace-write"]
    assert "01M0342X60F3NY8MH150G48AZ6" in orin_uv["unusable_in"]["workspace-write"]

    # Both end up unusable under both confined modes -- for DIFFERENT stated
    # reasons, which is the whole point.
    for uv in (thor_uv, orin_uv):
        assert uv["usable_in"] == ["danger-full-access"]


def test_go_is_absent_on_an_agent_host_and_the_surface_says_so(tmp_path):
    go = _tool(_host(THOR, tmp_path), "go")
    assert go["state"] == "absent"
    assert go["usable_in"] == []


def test_gh_is_authenticated_on_the_host_and_unreachable_under_dispatch(tmp_path):
    """Run 01M039NZ2TZYFG68YZT93A6DC7: a codex session on thor could reach
    neither api.github.com nor pypi.org, while `gh auth status` over a plain
    ssh session on that same host reports logged in. `gh: present` is a true
    fact about the host and a false one about the dispatch."""
    gh = _tool(_host(THOR, tmp_path), "gh")
    assert gh["state"] == "present"
    assert gh["usable_in"] == ["danger-full-access"]
    assert "01M039NZ2TZYFG68YZT93A6DC7" in gh["unusable_in"]["read-only"]
    assert "01M039NZ2TZYFG68YZT93A6DC7" in gh["unusable_in"]["workspace-write"]


def test_the_confined_modes_grant_no_egress_and_nothing_under_home(tmp_path):
    """The grants map is what makes those reasons derivable rather than
    hand-written per tool. Probe 3 (01M0356BK8QYR3119R8VY1YY9Q) found neither
    /tmp nor the working directory writable under read-only."""
    grants = _host(THOR, tmp_path)["dispatch_grants"]
    assert grants["read-only"] == []
    assert set(grants["workspace-write"]) == {"workspace-write", "tmp-write"}
    assert set(grants["danger-full-access"]) == set(preflight.GRANTS)


def test_toolchain_availability_no_longer_depends_on_the_userns_probe(tmp_path):
    """Before task t2, a restricted kernel shrank `dispatch_grants` to
    `danger-full-access` alone. `_REQUIRES_USERNS` is now empty, so the
    grants map — and every toolchain verdict read against it — is the same
    whether the kernel probe reports restricted or not; the account
    boundary is what actually confines a session now."""
    host = capabilities.host_facts(
        Config(repo_allowlist=(str(tmp_path),)),
        probes=_restricted(tmp_path),
        capability_probe=_probe_fails,
        locate=lambda name: THOR.get(name, (None, False)),
        version=lambda _path: "test-version",
    )
    assert list(host["dispatch_grants"]) == list(capabilities.SANDBOX_MODE_CANDIDATES)
    assert _tool(host, "uv")["usable_in"] == ["danger-full-access"]
    assert "unusable_in" in _tool(host, "uv")


def test_the_codex_cli_version_is_reported_so_a_bump_is_visible(tmp_path):
    """The probe findings above are pinned to codex-cli's behaviour, so a
    bump has to re-open them. Recording the version in the surface is what
    makes `scripts/toolchain-baseline.sh check` notice one."""
    host = _host(THOR, tmp_path, versions={"/home/thor/.local/bin/codex": "codex-cli 0.147.0"})
    codex_fact = _tool(host, "codex")
    assert codex_fact["version"] == "codex-cli 0.147.0"
    # codex itself is what CREATES the sandbox, so it requires nothing and
    # runs in every mode this host offers.
    assert codex_fact["requires"] == []
    assert codex_fact["usable_in"] == list(capabilities.SANDBOX_MODE_CANDIDATES)


def test_the_surface_with_toolchains_is_still_a_document_the_engine_accepts(tmp_path):
    preflight.validate_block(preflight.capability_block(_host(THOR, tmp_path)))


# --- git_metadata_writable (issue #94) ------------------------------------


def test_this_bridge_does_not_report_its_own_processs_git_answer(tmp_path):
    """The whole point of the key on THIS backend. The bridge process is
    inside no sandbox, so a write under `.git` succeeds here — while a
    `workspace-write` session on the same host cannot write one (commit
    df7d974 at refs/culture-nodes/probe on thor measured both halves). A
    surface reporting `supported` would state the opposite of what a dispatch
    gets, in the one field a consumer reads to decide whether a handover ref
    can be created."""
    repo = tmp_path / "checkout"
    (repo / ".git").mkdir(parents=True)
    cfg = Config(repo_allowlist=(str(repo),))

    # The bridge process really can write there — that is what makes the
    # reported value a decision rather than a coincidence.
    assert preflight.probe_git_metadata_write(repo / ".git") is True

    host = capabilities.host_facts(cfg, probes=_permissive(tmp_path))
    assert host["git_metadata_writable"] == preflight.GIT_METADATA_NOT_PROBED


def test_a_caller_with_a_dispatched_sessions_authority_measures_it(tmp_path):
    """`not-probed` is this bridge's default, not a hard-coded answer: hand
    `host_facts` a probe that CAN attempt the write the way a dispatch would
    and the same key reports what that probe found, both ways round."""
    repo = tmp_path / "checkout"
    (repo / ".git").mkdir(parents=True)
    cfg = Config(repo_allowlist=(str(repo),))

    writable = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), git_probe=lambda _git_dir: True
    )
    refused = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), git_probe=lambda _git_dir: False
    )
    assert writable["git_metadata_writable"] == preflight.GIT_METADATA_SUPPORTED
    assert refused["git_metadata_writable"] == preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX


def test_the_git_answer_is_not_read_off_the_sandbox_mode_name(tmp_path):
    """`git_metadata_writable` is measured by its own probe, never derived
    from `sandbox_modes` — true before task t2 (when a restricted kernel
    changed `sandbox_modes`) and still true now that it does not: the same
    `git_probe` answer must come back the same way regardless of what the
    userns sysctls say."""
    repo = tmp_path / "checkout"
    (repo / ".git").mkdir(parents=True)
    cfg = Config(repo_allowlist=(str(repo),))
    probe = lambda _git_dir: True  # noqa: E731 - one expression, named for the assertion

    permissive = capabilities.host_facts(
        cfg, probes=_permissive(tmp_path), capability_probe=_probe_works, git_probe=probe
    )
    restricted = capabilities.host_facts(
        cfg, probes=_restricted(tmp_path), capability_probe=_probe_fails, git_probe=probe
    )
    assert permissive["sandbox_modes"] == restricted["sandbox_modes"]
    assert (
        permissive["git_metadata_writable"]
        == restricted["git_metadata_writable"]
        == preflight.GIT_METADATA_SUPPORTED
    )
