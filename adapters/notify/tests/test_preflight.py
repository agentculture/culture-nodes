"""The SHARED half of the preflight capability surface (issue #67, task
t15): `preflight.py` is byte-identical in all four bridges, and so is this
file but for the package it imports. It asserts the protocol shape — the
part no bridge is allowed to have its own opinion about — while
`test_capabilities.py` asserts the backend-specific FACTS this bridge
measures and pours into it.

Read `src/*/preflight.py`'s module docstring for why the split is this way.
"""

from __future__ import annotations

import socket

import pytest

from notify_bridge import preflight

# --- the protocol constants the engine also declares --------------------


def test_protocol_version_matches_the_control_plane():
    """internal/preflight.ProtocolVersion is "1.0"; a bridge that advertises
    anything else is refused at configuration time by ParseSurface. The
    cross-language pin is asserted for real in tests/lint (Go) — this is the
    Python-side statement of the same constant."""
    assert preflight.PROTOCOL_VERSION == "1.0"
    assert preflight.CAPABILITY_KEY == "preflight"
    assert preflight.CAPABILITIES_PATH == "/v1/capabilities"


# --- host_block: the agreed key set -------------------------------------


def test_host_block_carries_the_required_facts():
    host = preflight.host_block(
        hostname="build-host-1",
        commit_policy="harvest: nothing is committed here",
        writable_paths=["/srv/work/checkout"],
    )
    assert host == {
        "hostname": "build-host-1",
        "commit_policy": "harvest: nothing is committed here",
        "writable_paths": ["/srv/work/checkout"],
    }


def test_an_unmeasurable_fact_is_omitted_never_guessed():
    """The README's rule: "a bridge that cannot measure one omits it rather
    than guessing". An omitted key reads as absence; a null or an empty
    string would read as a fact about the host."""
    host = preflight.host_block(
        hostname="h", commit_policy="p", writable_paths=None, sandbox_modes=None
    )
    assert set(host) == {"hostname", "commit_policy"}


def test_writable_paths_may_be_empty_because_writing_nowhere_is_a_fact():
    """Distinct from the omission above: `[]` states that a dispatch writes
    nowhere on this host, which is what an unconfigured allowlist means."""
    host = preflight.host_block(hostname="h", commit_policy="p", writable_paths=[])
    assert host["writable_paths"] == []


def test_an_empty_unavailable_map_is_omitted():
    host = preflight.host_block(
        hostname="h", commit_policy="p", sandbox_modes=["a"], sandbox_modes_unavailable={}
    )
    assert "sandbox_modes_unavailable" not in host
    assert host["sandbox_modes"] == ["a"]


@pytest.mark.parametrize("missing", ["hostname", "commit_policy"])
def test_the_two_always_measurable_facts_are_required(missing):
    """Every bridge knows its own hostname and its own commit policy —
    neither is a measurement that can fail — so an empty one is a bug in the
    caller, not an honest absence. The engine's checkHost refuses a host
    block with no facts at all; this refuses the half-built one earlier,
    where the caller can still be pointed at."""
    fields = {"hostname": "h", "commit_policy": "p"}
    fields[missing] = ""
    with pytest.raises(preflight.SurfaceError):
        preflight.host_block(**fields)


def test_an_unagreed_key_is_refused():
    """The all-backends rule as an assertion: four bridges advertising four
    different key sets would be four protocols wearing one name. A genuinely
    new fact is added to HOST_KEYS in the shared module — which changes it in
    every bridge at once — not slipped in from one adapter."""
    with pytest.raises(preflight.SurfaceError) as exc:
        preflight.host_block(hostname="h", commit_policy="p", extra_fact="ad hoc")
    assert "extra_fact" in str(exc.value)


# --- measure_sandbox_modes: the #18/#63 lesson --------------------------


_RESTRICTED = (("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1"),)
_PERMISSIVE = (("/proc/sys/nonexistent/knob", "1"),)


def test_a_mode_needing_userns_is_unavailable_where_the_kernel_restricts_it(tmp_path, monkeypatch):
    """Issue #18/#63: `--sandbox workspace-write` was REQUESTED and silently
    degraded — every file write lost, shell commands still running. The
    surface must report what the host can do, so a mode whose confinement
    cannot start here is reported unavailable with the sysctl that says so."""
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("1\n")
    monkeypatch.setattr(
        preflight.shutil, "which", lambda name: "/usr/bin/bwrap" if name == "bwrap" else None
    )
    monkeypatch.setattr(
        preflight.subprocess,
        "run",
        lambda *args, **kwargs: preflight.subprocess.CompletedProcess(args[0], 1),
    )

    available, unavailable = preflight.measure_sandbox_modes(
        ("read-only", "workspace-write", "danger-full-access"),
        requires_userns=("read-only", "workspace-write"),
        probes=((str(knob), "1"),),
    )

    assert available == ["danger-full-access"]
    assert set(unavailable) == {"read-only", "workspace-write"}
    assert "apparmor_restrict_unprivileged_userns=1" in unavailable["workspace-write"]


def test_the_same_modes_are_available_where_the_kernel_permits_it(tmp_path, monkeypatch):
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("0\n")
    monkeypatch.setattr(
        preflight.shutil, "which", lambda name: "/usr/bin/bwrap" if name == "bwrap" else None
    )
    monkeypatch.setattr(
        preflight.subprocess,
        "run",
        lambda *a, **kw: preflight.subprocess.CompletedProcess(a[0], 0),
    )

    available, unavailable = preflight.measure_sandbox_modes(
        ("read-only", "workspace-write"),
        requires_userns=("read-only", "workspace-write"),
        probes=((str(knob), "1"),),
    )

    assert available == ["read-only", "workspace-write"]
    assert unavailable == {}


def test_an_absent_knob_is_not_a_restriction(monkeypatch):
    """A kernel with no such sysctl does not restrict here — absence must not
    be read as the blocking value."""
    monkeypatch.setattr(
        preflight.shutil, "which", lambda name: "/usr/bin/bwrap" if name == "bwrap" else None
    )
    monkeypatch.setattr(
        preflight.subprocess,
        "run",
        lambda *a, **kw: preflight.subprocess.CompletedProcess(a[0], 0),
    )
    available, unavailable = preflight.measure_sandbox_modes(
        ("workspace-write",), requires_userns=("workspace-write",), probes=_PERMISSIVE
    )
    assert available == ["workspace-write"]
    assert unavailable == {}


def test_a_structurally_unsupported_mode_is_unavailable_with_its_own_reason(tmp_path):
    """Not every unavailability is a kernel measurement: a mode this bridge's
    own dispatch shape cannot deliver (claude-code's `default` permission
    mode has no TTY to answer a prompt) is reported the same way, so a reader
    sees one list of "what you can actually get"."""
    available, unavailable = preflight.measure_sandbox_modes(
        ("bypassPermissions", "default"),
        unsupported={"default": "headless dispatch has no TTY"},
        probes=_PERMISSIVE,
    )
    assert available == ["bypassPermissions"]
    assert unavailable == {"default": "headless dispatch has no TTY"}


def test_userns_restrictions_reports_every_blocking_knob(tmp_path):
    a = tmp_path / "a"
    a.write_text("1")
    b = tmp_path / "b"
    b.write_text("0")
    assert preflight.userns_restrictions(((str(a), "1"), (str(b), "0"))) == ("a=1", "b=0")


# --- harvest_commit_policy: shared because it is not backend-specific ----


def test_commit_policy_states_where_a_failed_dispatch_leaves_its_changes():
    policy = preflight.harvest_commit_policy(
        preserve_on_failure=True, branch_prefix="preserve/", push=True, remote="origin"
    )
    assert "preserve/" in policy and "origin" in policy


def test_commit_policy_says_local_only_when_the_bridge_does_not_push():
    policy = preflight.harvest_commit_policy(
        preserve_on_failure=True, branch_prefix="preserve/", push=False, remote="origin"
    )
    assert "local" in policy
    assert "origin" not in policy


def test_commit_policy_says_so_when_nothing_is_preserved_at_all():
    policy = preflight.harvest_commit_policy(
        preserve_on_failure=False, branch_prefix="preserve/", push=True, remote="origin"
    )
    assert "not preserved" in policy


def test_commit_policy_carries_a_backend_specific_clause_when_given():
    policy = preflight.harvest_commit_policy(
        preserve_on_failure=False,
        branch_prefix="preserve/",
        push=False,
        remote="origin",
        extra="colleague opens a PR for a completed work item",
    )
    assert policy.endswith("colleague opens a PR for a completed work item")


# --- the surface and the capability block -------------------------------


def test_capability_block_is_exactly_what_a_registration_carries():
    block = preflight.capability_block({"hostname": "h", "commit_policy": "p"})
    assert block == {
        "preflight": {
            "protocol_version": "1.0",
            "host": {"hostname": "h", "commit_policy": "p"},
        }
    }


def test_a_host_block_with_no_facts_is_refused():
    """Mirrors internal/preflight.checkHost: a gate that briefs an actor about
    nothing would refuse dispatch for no gain."""
    with pytest.raises(preflight.SurfaceError):
        preflight.capability_block({})


def test_validate_block_accepts_what_capability_block_produces():
    preflight.validate_block(preflight.capability_block({"hostname": "h", "commit_policy": "p"}))


@pytest.mark.parametrize(
    "block",
    [
        {},
        {"preflight": {}},
        {"preflight": {"protocol_version": "0.9", "host": {"hostname": "h"}}},
        {"preflight": {"protocol_version": "1.0", "host": {}}},
        {"preflight": {"protocol_version": "1.0", "host": {"nope": 1}}},
        {"preflight": {"protocol_version": "1.0"}},
    ],
)
def test_validate_block_refuses_a_shape_the_engine_would_refuse(block):
    with pytest.raises(preflight.SurfaceError):
        preflight.validate_block(block)


def test_hostname_is_this_host():
    assert preflight.hostname() == socket.gethostname()
