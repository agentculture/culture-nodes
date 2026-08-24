"""The SHARED half of the preflight capability surface (issue #67, task
t15): `preflight.py` is byte-identical in all four bridges, and so is this
file but for the package it imports. It asserts the protocol shape — the
part no bridge is allowed to have its own opinion about — while
`test_capabilities.py` asserts the backend-specific FACTS this bridge
measures and pours into it.

Read `src/*/preflight.py`'s module docstring for why the split is this way.
"""

from __future__ import annotations

import os
import socket

import pytest
from qwen_bridge import preflight

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
        lambda *args, **kwargs: preflight.subprocess.CompletedProcess(args[0], 0),
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
        lambda *args, **kwargs: preflight.subprocess.CompletedProcess(args[0], 0),
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


def test_bwrap_probe_is_authority_even_when_sysctl_looks_restricted(tmp_path, monkeypatch):
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("1\n")
    monkeypatch.setattr(
        preflight.shutil, "which", lambda name: "/usr/bin/bwrap" if name == "bwrap" else None
    )
    monkeypatch.setattr(
        preflight.subprocess,
        "run",
        lambda *args, **kwargs: preflight.subprocess.CompletedProcess(args[0], 0),
    )

    available, unavailable = preflight.measure_sandbox_modes(
        ("workspace-write",), requires_userns=("workspace-write",), probes=((str(knob), "1"),)
    )

    assert available == ["workspace-write"]
    assert unavailable == {}


def test_failed_bwrap_probe_uses_sysctl_only_to_explain_why(tmp_path, monkeypatch):
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
        ("workspace-write",), requires_userns=("workspace-write",), probes=((str(knob), "1"),)
    )

    assert available == []
    assert "bwrap capability probe failed" in unavailable["workspace-write"]
    assert "apparmor_restrict_unprivileged_userns=1" in unavailable["workspace-write"]


def test_missing_probe_tools_is_not_reported_as_available(monkeypatch):
    monkeypatch.setattr(preflight.shutil, "which", lambda _name: None)
    available, unavailable = preflight.measure_sandbox_modes(
        ("workspace-write",), requires_userns=("workspace-write",), probes=_PERMISSIVE
    )
    assert available == []
    assert "not probed" in unavailable["workspace-write"]


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


# --- toolchains: what can actually EXECUTE here (issue #96) --------------
#
# The three dispatched probe runs preflight.py's toolchain section cites are
# the test cases below, reproduced as injected measurements so the assertions
# hold on whatever host runs pytest:
#
#   thor  uv is a snap at /snap/bin/uv        (run 01M03374VAKH0KHN0GDZ466NP4)
#   orin  uv is standalone at ~/.local/bin/uv (run 01M0342X60F3NY8MH150G48AZ6)
#   both  go is absent
#
# A surface reporting `uv: present` would have been true on both hosts and
# useless on both. These tests fail if it ever says that again.


def _fake_locate(table):
    """A locator standing in for a specific host: name -> (path, on_path)."""
    return lambda name: table.get(name, (None, False))


def _fake_version(_path):
    return "0.0.0-test"


THOR_UV = ("/snap/bin/uv", True)
ORIN_UV = ("/home/orin/.local/bin/uv", False)

#: A sandbox-shaped posture map: read-only grants nothing, workspace-write
#: grants the workspace and /tmp, danger-full-access grants everything.
CONFINED_GRANTS = {
    "read-only": (),
    "workspace-write": (preflight.GRANT_WORKSPACE_WRITE, preflight.GRANT_TMP_WRITE),
    "danger-full-access": preflight.GRANTS,
}

UV = preflight.Toolchain("uv", requires=(preflight.GRANT_HOME_WRITE,))
GH = preflight.Toolchain("gh", requires=(preflight.GRANT_NETWORK_EGRESS,))
GO = preflight.Toolchain("go", requires=(preflight.GRANT_HOME_WRITE,))


def _measure(specs, table, grants=None):
    return preflight.measure_toolchains(
        specs,
        grants=grants if grants is not None else CONFINED_GRANTS,
        locate=_fake_locate(table),
        version=_fake_version,
    )


def _by_name(facts, name):
    return next(fact for fact in facts if fact["name"] == name)


def test_a_snap_and_a_standalone_toolchain_are_not_the_same_fact():
    """Acceptance criterion, issue #96: `uv: present` was true on thor and on
    orin and told an operator nothing. thor's is a snap, whose own
    snap-confine cannot start inside a bubblewrap-confined mode; orin's is a
    standalone binary that gets past that. The surface must say which."""
    thor = _by_name(_measure([UV], {"uv": THOR_UV}), "uv")
    orin = _by_name(_measure([UV], {"uv": ORIN_UV}), "uv")

    assert thor["packaging"] == preflight.PACKAGING_SNAP
    assert orin["packaging"] == preflight.PACKAGING_STANDALONE

    # And the packaging is not cosmetic: it changes what can run where.
    assert thor["usable_in"] == ["danger-full-access"]
    assert preflight.GRANT_NESTED_CONFINEMENT in thor["requires"]
    assert preflight.GRANT_NESTED_CONFINEMENT not in orin["requires"]
    assert "workspace-write" in thor["unusable_in"]


def test_a_standalone_toolchain_still_fails_where_its_cache_cannot_be_written():
    """orin's standalone uv got past snap-confine and died anyway: "Could not
    create temporary file ... Read-only file system ... /home/orin/.cache/uv".
    So `packaging: standalone` alone must not read as usable."""
    orin = _by_name(_measure([UV], {"uv": ORIN_UV}), "uv")
    assert orin["usable_in"] == ["danger-full-access"]
    assert set(orin["unusable_in"]) == {"read-only", "workspace-write"}


def test_an_absent_toolchain_says_absent_in_every_mode():
    """Go is absent on both agent hosts. A dispatch that needs it should
    learn so from the briefing rather than from a failed session."""
    go = _by_name(_measure([GO], {}), "go")
    assert go["state"] == preflight.STATE_ABSENT
    assert go["usable_in"] == []
    assert set(go["unusable_in"]) == set(CONFINED_GRANTS)
    assert "path" not in go and "version" not in go


def test_a_toolchain_off_the_path_is_neither_absent_nor_simply_present():
    """orin's uv is at ~/.local/bin/uv, which a non-interactive shell's PATH
    does not carry. A dispatch that invokes `uv` by name fails on a host that
    has it, so the two cases are different facts."""
    off_path = _by_name(_measure([UV], {"uv": ORIN_UV}), "uv")
    on_path = _by_name(_measure([UV], {"uv": ("/usr/local/bin/uv", True)}), "uv")
    assert off_path["state"] == preflight.STATE_OFF_PATH
    assert off_path["on_path"] is False
    assert on_path["state"] == preflight.STATE_PRESENT
    assert on_path["on_path"] is True


def test_a_host_fact_and_a_dispatch_fact_can_disagree_without_lying():
    """The sharpest case (run 01M039NZ2TZYFG68YZT93A6DC7): `gh auth status`
    over ssh on thor reports logged in, while a dispatch on that same host
    reached neither api.github.com nor pypi.org. Both are true, of different
    things, and the surface has to be able to hold both."""
    gh = _by_name(_measure([GH], {"gh": ("/usr/bin/gh", True)}), "gh")
    assert gh["state"] == preflight.STATE_PRESENT  # true of the host
    assert gh["usable_in"] == ["danger-full-access"]  # true of the dispatch
    assert "read-only" in gh["unusable_in"]


def test_the_reason_a_mode_cannot_run_a_tool_is_the_backends_own_sentence():
    """`measure_toolchains` never invents an explanation: a backend supplies
    one per missing grant (which is where a measured run id belongs), and a
    grant with no supplied sentence gets a plain statement of what is
    missing rather than a guess at why."""
    measured = preflight.measure_toolchains(
        [GH],
        grants={"read-only": ()},
        grant_absence_reasons={preflight.GRANT_NETWORK_EGRESS: "no egress here (run RUN-ID)"},
        locate=_fake_locate({"gh": ("/usr/bin/gh", True)}),
        version=_fake_version,
    )
    assert measured[0]["unusable_in"]["read-only"] == "no egress here (run RUN-ID)"

    unexplained = preflight.measure_toolchains(
        [GH],
        grants={"read-only": ()},
        locate=_fake_locate({"gh": ("/usr/bin/gh", True)}),
        version=_fake_version,
    )
    assert preflight.GRANT_NETWORK_EGRESS in unexplained[0]["unusable_in"]["read-only"]


def test_an_unconfined_posture_reports_the_same_tools_as_usable():
    """The contrast that makes the key worth having: the same host, measured
    for a backend that confines nothing, reports the same binaries as usable.
    Plan t5 routes Python-side verification to a claude bridge on spark for
    exactly this reason."""
    unconfined = {preflight.MODE_UNSANDBOXED: preflight.GRANTS}
    uv = _by_name(_measure([UV], {"uv": ORIN_UV}, grants=unconfined), "uv")
    assert uv["usable_in"] == [preflight.MODE_UNSANDBOXED]
    assert "unusable_in" not in uv


# --- the toolchain fact's own shape -------------------------------------


def test_packaging_sees_through_the_snap_shim():
    """/snap/bin/uv is a RELATIVE symlink to astral-uv.uv whose realpath is
    /usr/bin/snap — so neither the path nor the resolved target alone is
    enough, and both are checked."""
    assert preflight.toolchain_packaging("/snap/bin/uv") == preflight.PACKAGING_SNAP
    assert preflight.toolchain_packaging("/usr/bin/gh") == preflight.PACKAGING_SYSTEM
    assert (
        preflight.toolchain_packaging("/home/orin/.local/bin/uv") == preflight.PACKAGING_STANDALONE
    )


def test_an_unagreed_toolchain_key_is_refused():
    with pytest.raises(preflight.SurfaceError) as excinfo:
        preflight.toolchain_fact(name="uv", state=preflight.STATE_PRESENT, vibes="good")
    assert "vibes" in str(excinfo.value)


def test_a_toolchain_fact_needs_a_name_and_a_state():
    with pytest.raises(preflight.SurfaceError):
        preflight.toolchain_fact(name="uv")
    with pytest.raises(preflight.SurfaceError):
        preflight.toolchain_fact(name="uv", state="probably-fine")


def test_an_unagreed_grant_is_refused_rather_than_advertised():
    """A reader who meets an unknown word in the grants map cannot tell
    whether it means more or less than the ones they know."""
    with pytest.raises(preflight.SurfaceError) as excinfo:
        preflight.dispatch_grants({"read-only": ("sudo",)})
    assert "sudo" in str(excinfo.value)


def test_the_new_keys_are_omitted_by_a_bridge_that_measures_neither():
    """notify runs no session: `toolchains: []` there would read as "this
    host has no uv", a claim about a host nobody measured."""
    host = preflight.host_block(
        hostname="notify-host",
        commit_policy="no workspace",
        dispatch_grants={},
        toolchains=[],
    )
    assert "dispatch_grants" not in host
    assert "toolchains" not in host


def test_a_host_block_carrying_toolchains_still_validates_as_a_surface():
    host = preflight.host_block(
        hostname="agent-host",
        commit_policy="harvest: nothing is committed here",
        dispatch_grants=preflight.dispatch_grants({"read-only": ()}),
        toolchains=_measure([UV, GO], {"uv": THOR_UV}, grants={"read-only": ()}),
    )
    preflight.validate_block(preflight.capability_block(host))
    assert [fact["name"] for fact in host["toolchains"]] == ["uv", "go"]


def test_the_version_probe_tries_the_other_spelling_before_giving_up():
    """`go --version` exits non-zero; `go version` answers. A tool that
    refuses both reports no version rather than an invented one."""
    calls = []

    class _Completed:
        def __init__(self, code, out):
            self.returncode = code
            self.stdout = out

    def fake_run(argv, **_kwargs):
        calls.append(argv[1:])
        if argv[1:] == ["version"]:
            return _Completed(0, "go version go1.26.5 linux/arm64\n")
        return _Completed(2, "flag provided but not defined: -version\n")

    assert preflight.toolchain_version("/usr/bin/go", run=fake_run) == (
        "go version go1.26.5 linux/arm64"
    )
    assert calls == [["--version"], ["version"]]

    def always_fails(argv, **_kwargs):
        return _Completed(1, "")

    assert preflight.toolchain_version("/usr/bin/mystery", run=always_fails) is None


# --- git_metadata_writable (issue #94) ----------------------------------
#
# The key `writable_paths` could not carry. Every test here goes through the
# real attempt or through an injected one that stands where the real attempt
# stands; none of them decides the answer from a sandbox mode name, because
# that is precisely the derivation the key exists to replace.


def _checkout(root, name="repo"):
    """A directory shaped like a plain clone: a real `.git` DIRECTORY."""
    repo = root / name
    (repo / ".git").mkdir(parents=True)
    return repo


def test_git_metadata_writable_is_measured_by_attempting_the_write(tmp_path):
    """The positive answer is a write that actually landed. Nothing here
    consults a mode name, a config value or a sysctl."""
    repo = _checkout(tmp_path)
    assert preflight.measure_git_metadata_writable([str(repo)]) == preflight.GIT_METADATA_SUPPORTED


def test_a_git_dir_that_refuses_the_write_reports_unsupported_by_sandbox(tmp_path):
    """The negative answer is a write that was actually refused — the state
    a `workspace-write` dispatch is in, where the worktree accepts a file and
    `.git` does not (issue #94)."""
    repo = _checkout(tmp_path)
    refused = []

    def refuse(git_dir):
        refused.append(git_dir)
        return False

    assert preflight.measure_git_metadata_writable([str(repo)], probe=refuse) == (
        preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX
    )
    assert refused == [repo / ".git"], "the attempt must be made against .git itself"


@pytest.mark.skipif(os.geteuid() == 0, reason="root writes through a read-only directory mode")
def test_the_refusal_is_the_real_one_a_read_only_git_dir_produces(tmp_path):
    """The same negative without an injected probe, so the default attempt is
    proven to REPORT a refusal rather than to raise or to swallow it."""
    repo = _checkout(tmp_path)
    git_dir = repo / ".git"
    git_dir.chmod(0o500)
    try:
        assert preflight.measure_git_metadata_writable([str(repo)]) == (
            preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX
        )
    finally:
        git_dir.chmod(0o700)


def test_a_linked_worktree_is_followed_to_the_metadata_dir_a_ref_lands_in(tmp_path):
    """`.git` is a FILE in a linked worktree — which is how this repo's own
    workforce lanes are checked out, and how colleague isolates a work item.
    Probing the pointer file's own directory would measure the worktree, which
    is the half already known to be writable."""
    repo = _checkout(tmp_path)
    worktree = tmp_path / "wt"
    worktree.mkdir()
    (worktree / ".git").write_text(f"gitdir: {repo / '.git'}\n", encoding="utf-8")

    seen = []

    def observe(git_dir):
        seen.append(git_dir)
        return True

    assert preflight.measure_git_metadata_writable([str(worktree)], probe=observe) == (
        preflight.GIT_METADATA_SUPPORTED
    )
    assert seen == [repo / ".git"]


def test_a_relative_worktree_pointer_resolves_against_the_worktree(tmp_path):
    repo = _checkout(tmp_path)
    worktree = tmp_path / "wt"
    worktree.mkdir()
    (worktree / ".git").write_text("gitdir: ../repo/.git\n", encoding="utf-8")
    assert preflight.git_metadata_dir(worktree) == (repo / ".git").resolve()


def test_a_checkout_one_level_under_an_allowlist_prefix_is_found(tmp_path):
    """`writable_paths` carries repo allowlist PREFIXES as well as repos, and
    a prefix has no `.git` of its own. A bridge configured only with prefixes
    would otherwise report `not-probed` on a host full of checkouts."""
    prefix = tmp_path / "work"
    repo = _checkout(prefix, "culture-nodes-agent")
    assert preflight.measure_git_metadata_writable([str(prefix)]) == (
        preflight.GIT_METADATA_SUPPORTED
    )
    assert preflight.git_metadata_dir(repo) == repo / ".git"


def test_a_path_that_holds_no_checkout_is_not_probed(tmp_path):
    """Not `unsupported-by-sandbox`: nothing refused anything. A bridge that
    reported a refusal here would be inventing the very fact it failed to
    measure."""
    (tmp_path / "empty").mkdir()
    assert preflight.measure_git_metadata_writable([str(tmp_path / "empty")]) == (
        preflight.GIT_METADATA_NOT_PROBED
    )
    assert preflight.measure_git_metadata_writable([]) == preflight.GIT_METADATA_NOT_PROBED
    assert preflight.git_metadata_dir(tmp_path / "nowhere") is None


def test_a_bridge_that_cannot_attempt_the_write_reports_not_probed(tmp_path):
    """`probe=None` is a bridge saying it cannot run the attempt with a
    DISPATCHED SESSION's authority — qwen, whose sessions are confined by a
    helper the bridge process is not inside. Reporting what the bridge process
    can do would answer a different question with the same word."""
    repo = _checkout(tmp_path)
    assert preflight.measure_git_metadata_writable([str(repo)], probe=None) == (
        preflight.GIT_METADATA_NOT_PROBED
    )


def test_the_write_probe_leaves_the_git_dir_as_it_found_it(tmp_path):
    repo = _checkout(tmp_path)
    git_dir = repo / ".git"
    (git_dir / "HEAD").write_text("ref: refs/heads/main\n", encoding="utf-8")
    assert preflight.probe_git_metadata_write(git_dir) is True
    assert sorted(p.name for p in git_dir.iterdir()) == ["HEAD"]


def test_the_probe_reports_a_refusal_rather_than_raising(tmp_path):
    assert preflight.probe_git_metadata_write(tmp_path / "no-such-dir") is False


def test_git_metadata_writable_rides_the_surface_the_engine_accepts(tmp_path):
    host = preflight.host_block(
        hostname="build-host-1",
        commit_policy="harvest: nothing is committed here",
        writable_paths=["/srv/work/checkout"],
        git_metadata_writable=preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX,
    )
    assert host["git_metadata_writable"] == "unsupported-by-sandbox"
    preflight.validate_block(preflight.capability_block(host))


def test_an_unmeasured_git_metadata_fact_is_omitted_not_nulled():
    host = preflight.host_block(
        hostname="build-host-1",
        commit_policy="harvest: nothing is committed here",
    )
    assert "git_metadata_writable" not in host


@pytest.mark.parametrize("value", ["yes", "writable", "unsupported", "", "unsupported-by-host"])
def test_a_git_metadata_value_outside_the_agreed_three_is_refused(value):
    """Same refusal `artifact_publish` gets, and for the same reason: a
    consumer that meets a fourth word cannot tell whether it means more or
    less than the three it knows. `unsupported-by-host` is in the list on
    purpose — it is the neighbouring key's vocabulary, and the two say
    different things."""
    with pytest.raises(preflight.SurfaceError):
        preflight.host_block(
            hostname="build-host-1",
            commit_policy="harvest: nothing is committed here",
            git_metadata_writable=value,
        )


def test_the_agreed_values_are_exactly_the_three_the_issue_names():
    assert preflight.GIT_METADATA_WRITABLE_VALUES == {
        "supported",
        "unsupported-by-sandbox",
        "not-probed",
    }
    assert "git_metadata_writable" in preflight.HOST_KEYS
