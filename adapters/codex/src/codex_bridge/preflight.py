"""The preflight capability surface a bridge advertises (issue #67, task
t15) — the shared, backend-agnostic half.

**This file is byte-identical in all four bridges** (`claude_code_bridge`,
`codex_bridge`, `colleague_bridge`, `notify_bridge`) and a Go lint test
(`tests/lint/preflightsurface_test.go`) fails the build if it stops being.
That is the whole point of it existing: the protocol is ONE contract, and
four adapters each inlining their own version of it is exactly the
duplication that let `resolve_actor_row_id` ship as the same bug in three
separate deploy lanes before a shared helper fixed it. Nothing
backend-specific may be added here; the backend-specific FACTS live in each
bridge's own `capabilities.py`, which is the only per-bridge code in this
feature.

It has no imports outside the stdlib and no import from its own package, so
"byte-identical" costs nothing to hold.

## What this is

The control plane composes a briefing an actor must acknowledge before its
first billable turn (`internal/preflight`, migration 0026, `nodes dispatch
pending|show|confirm`). The briefing carries, verbatim, the `host` block a
bridge advertised in its registration:

    {"preflight": {"protocol_version": "1.0", "host": {...}}}

The **protocol is engine-side and the facts are bridge-side**: the engine
never re-renders, supplements or interprets a fact it did not measure, and
a bridge never composes a briefing. See `api/actor-protocol/README.md`'s
capability-surface section for the contract, and `internal/preflight/
preflight.go` for the parser that reads what this module writes —
`ParseSurface` refuses a version this control plane does not speak, and
`checkHost` refuses a host block stating nothing at all.

## What goes in `host`

The facts a dispatched task actually depends on, and only those. Report what
the host CAN DO, never what its configuration asks for: issue #18/#63 is the
whole reason this exists — `--sandbox workspace-write` was requested on
three hosts whose kernel restricted unprivileged user namespaces, so the
confinement helper could not start, every file write was silently lost, and
shell commands kept running unconfined. A surface that echoed the config
would have advertised `workspace-write` and been wrong in the one way that
costs a whole session.

`HOST_KEYS` is the agreed key set. A bridge that cannot measure a fact OMITS
its key rather than guessing — an absent key reads as absence, where a null
or an empty string reads as a fact about the host. The one deliberate
exception is `writable_paths: []`, which is not an absence but a fact: this
bridge writes nowhere.

Adding a fact means adding it HERE, which adds it to every bridge at once;
`host_block` refuses an unagreed key so a single adapter cannot quietly grow
a fifth dialect.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, NamedTuple, Sequence

#: The capability-surface version this bridge produces. Pinned to
#: `internal/preflight.ProtocolVersion` — the control plane refuses a
#: surface declaring anything else rather than composing a document whose
#: fields might mean something different. tests/lint asserts the two
#: constants agree across the language boundary.
PROTOCOL_VERSION = "1.0"

#: The key the surface is advertised under, inside an actor registration's
#: `capabilities` document (`internal/preflight.CapabilityKey`).
CAPABILITY_KEY = "preflight"

#: Where a running bridge serves its own measured surface, so an operator
#: registering the actor can read the facts off the host that has them
#: instead of writing down what they believe about it. Authenticated like
#: the invocation route (it names a hostname and real paths); `/healthz`
#: stays the only unauthenticated route.
CAPABILITIES_PATH = "/v1/capabilities"

#: The agreed `host` keys, in the order they read best in a briefing:
#:
#: * ``hostname`` — the host this bridge dispatches on. Always present.
#: * ``sandbox_modes`` — the confinement modes a dispatch can ACTUALLY get
#:   here, in the backend's own vocabulary (codex's ``--sandbox`` values,
#:   claude-code's ``--permission-mode`` values, ``MODE_UNSANDBOXED`` where
#:   the backend confines nothing). Omitted by a bridge that runs no session.
#: * ``sandbox_modes_unavailable`` — mode → why this host cannot deliver it.
#:   The #18/#63 key: it is what makes a silent degradation loud. Omitted
#:   when empty.
#: * ``default_sandbox_mode`` — what a dispatch that names no mode gets.
#: * ``confinement`` — one sentence on what actually confines a session
#:   here, including "nothing" when that is the truth. A reader who sees
#:   only a mode list can mistake a prompting policy for a sandbox.
#: * ``commit_policy`` — whether the session commits, and where a dispatch's
#:   changes end up. Always present.
#: * ``writable_paths`` — the paths a dispatch may write in. ``[]`` means
#:   nowhere.
#: * ``artifact_publish`` — one of ``supported``, ``unsupported-by-host``,
#:   or ``not-applicable-no-workspace``. Unlike omission, the last value
#:   explicitly says that this bridge has no workspace to publish from.
#: * ``dispatch_grants`` — mode → what that mode actually GRANTS a session
#:   (see ``GRANTS``). Issue #96's key: it is what makes "gh is authenticated
#:   on this host" and "gh cannot reach api.github.com under dispatch" both
#:   true without contradicting each other. Omitted by a bridge that runs no
#:   session.
#: * ``toolchains`` — per tool, what can actually EXECUTE here and in which
#:   modes (see ``measure_toolchains``). Omitted by a bridge that dispatches
#:   no toolchain.
#: * ``deployment`` — WHICH REVISION of this bridge is running, how that was
#:   established, and whether this install shape can go stale (see
#:   ``measure_deployment``). Issue #120 item 4's key: `deploy.sh` reinstalls
#:   the bridges and nothing reported what it shipped, so "is the fleet
#:   current?" needed an ssh. Omitted by a bridge that cannot locate its own
#:   package.
HOST_KEYS = (
    "hostname",
    "sandbox_modes",
    "sandbox_modes_unavailable",
    "default_sandbox_mode",
    "confinement",
    "commit_policy",
    "writable_paths",
    "artifact_publish",
    "dispatch_grants",
    "toolchains",
    "deployment",
)

ARTIFACT_PUBLISH_VALUES = frozenset(
    ("supported", "unsupported-by-host", "not-applicable-no-workspace")
)

#: The two facts every bridge knows about itself without measuring anything.
#: Neither can honestly be absent, so an empty one is a caller bug.
_REQUIRED_HOST_KEYS = ("hostname", "commit_policy")

#: The shared mode name for "this backend confines nothing" — a stated fact
#: rather than an empty list a reader has to interpret.
MODE_UNSANDBOXED = "unsandboxed"

#: sysctl → the value that means "restricted". Ubuntu's AppArmor gate
#: (24.04+) and the older Debian-family knob; either set against us stops a
#: bubblewrap-backed sandbox from starting. Cited, not imported, from
#: `culture_nodes/cli/_commands/doctor.py`'s `_userns_check` — the adapters
#: install as standalone packages and share no runtime code with the CLI.
USERNS_SYSCTLS = (
    ("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1"),
    ("/proc/sys/kernel/unprivileged_userns_clone", "0"),
)

# --- toolchains (issue #96) -------------------------------------------------
#
# Three dispatched probe runs are the reason this section exists, and they
# are its test cases:
#
#   01M03374VAKH0KHN0GDZ466NP4 (thor)  uv is a SNAP, and snap-confine cannot
#                                      start inside codex's sandbox:
#                                      "required permitted capability
#                                      cap_dac_override not found".
#   01M0342X60F3NY8MH150G48AZ6 (orin)  uv is a STANDALONE binary, gets past
#                                      that, and dies initialising its cache:
#                                      "Read-only file system (os error 30) at
#                                      path /home/orin/.cache/uv".
#   01M0356BK8QYR3119R8VY1YY9Q (orin)  under read-only NOTHING is writable —
#                                      not /tmp, not the working directory —
#                                      so redirecting the cache has nowhere to
#                                      point.
#
# A surface reporting `uv: present` would have been TRUE on both hosts and
# useless on both: neither could run a test suite, and they failed for
# different reasons. So a toolchain fact here says what can EXECUTE, in which
# dispatch modes, and why not in the others.
#
# The fourth case is sharper and is what `dispatch_grants` exists for: run
# 01M039NZ2TZYFG68YZT93A6DC7 on thor could reach neither api.github.com nor
# pypi.org, while `gh auth status` over a plain ssh session on that same host
# reports logged in. "gh: present and authenticated" is a true fact about the
# HOST and a false one about the DISPATCH.

#: What a dispatch posture (one sandbox/permission mode) may grant a session.
#: A mode grants a subset of these; a toolchain requires a subset; the
#: difference is the reason it cannot run.
GRANT_WORKSPACE_WRITE = "workspace-write"
GRANT_TMP_WRITE = "tmp-write"
GRANT_HOME_WRITE = "home-write"
GRANT_NETWORK_EGRESS = "network-egress"
#: Whether a helper that sets up its OWN confinement (snap-confine, a nested
#: bubblewrap, a container runtime) can start. Its absence is why a
#: snap-packaged binary is unusable in a bubblewrap-confined mode while the
#: same tool's standalone build is fine.
GRANT_NESTED_CONFINEMENT = "nested-confinement"

GRANTS = (
    GRANT_WORKSPACE_WRITE,
    GRANT_TMP_WRITE,
    GRANT_HOME_WRITE,
    GRANT_NETWORK_EGRESS,
    GRANT_NESTED_CONFINEMENT,
)

#: A toolchain's PRESENCE on this host, which is separate from whether any
#: mode can run it (that is ``usable_in``).
#:
#: ``present-off-path`` is not pedantry: orin's uv lives at
#: ~/.local/bin/uv and is absent from a non-interactive shell's PATH, so a
#: dispatch that invokes `uv` by name fails on a host that has it. The
#: measuring process here IS the bridge, whose PATH is the one a dispatched
#: session inherits, which is exactly why this fact is worth measuring from
#: the bridge rather than from an operator's ssh session.
STATE_PRESENT = "present"
STATE_OFF_PATH = "present-off-path"
STATE_ABSENT = "absent"

#: How a toolchain was installed, which decides whether it needs
#: ``nested-confinement``. Derived from where the binary actually resolves to.
PACKAGING_SNAP = "snap"
PACKAGING_STANDALONE = "standalone"
PACKAGING_SYSTEM = "system"
PACKAGING_UNKNOWN = "unknown"

#: Directories consulted when PATH does not carry a tool, so a host that HAS
#: it is not reported as absent — it is reported as off-path, which is a
#: different fact with a different remedy.
TOOLCHAIN_SEARCH_DIRS = ("~/.local/bin", "/usr/local/bin", "/snap/bin", "/usr/bin", "/bin")

#: The agreed keys of one toolchain fact. Same rule as HOST_KEYS: a bridge
#: that cannot measure one omits it rather than guessing.
TOOLCHAIN_KEYS = (
    "name",
    "state",
    "path",
    "on_path",
    "packaging",
    "version",
    "requires",
    "usable_in",
    "unusable_in",
)


class SurfaceError(ValueError):
    """A capability surface this bridge would be wrong to advertise.

    Raised at build time (a bridge asking for a key the protocol does not
    agree on, or leaving a required fact empty) and by `validate_block` on a
    surface read back off the wire. Refusing here, in the bridge, means the
    operator finds out while registering rather than a run finding out
    against a gate nothing can satisfy.
    """


def hostname() -> str:
    """This host's name, as this host reports it."""
    return socket.gethostname()


def userns_restrictions(
    probes: Sequence[tuple[str, str]] = USERNS_SYSCTLS,
) -> tuple[str, ...]:
    """The `name=value` of every probed sysctl currently restricting
    unprivileged user namespaces, empty when none is.

    These values are diagnostic hints, not the capability fact. The
    executable bwrap/unshare probe in :func:`measure_sandbox_modes` decides
    whether the capability works; sysctls are read only to explain a failed
    probe. An absent knob contributes no explanation.
    *probes* is injectable so a test can assert both kinds of kernel rather
    than whichever one happens to be running the suite.
    """
    blockers = []
    for path, blocking_value in probes:
        try:
            value = Path(path).read_text(encoding="utf-8").strip()
        except OSError:
            continue
        if value == blocking_value:
            blockers.append(f"{Path(path).name}={value}")
    return tuple(blockers)


def _userns_capability() -> tuple[str, str]:
    """Return ``(state, probe)`` for an executable user-namespace probe.

    ``state`` is ``available``, ``unavailable``, or ``not-probed``. bwrap is
    authoritative when installed because it is the mechanism codex uses;
    unshare is a capability-level fallback only when bwrap is absent.
    """
    if shutil.which("bwrap"):
        probe = "bwrap"
        argv = ["bwrap", "--unshare-user", "--unshare-net", "--ro-bind", "/", "/", "/bin/true"]
    elif shutil.which("unshare"):
        probe = "unshare"
        argv = ["unshare", "--user", "--map-root-user", "true"]
    else:
        return "not-probed", "neither bwrap nor unshare is installed"
    try:
        completed = subprocess.run(
            argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10, check=False
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return "unavailable", f"{probe} capability probe could not run ({exc})"
    if completed.returncode == 0:
        return "available", f"{probe} capability probe succeeded"
    return "unavailable", f"{probe} capability probe failed (exit {completed.returncode})"


def measure_sandbox_modes(
    candidates: Sequence[str],
    *,
    requires_userns: Iterable[str] = (),
    unsupported: Mapping[str, str] | None = None,
    probes: Sequence[tuple[str, str]] = USERNS_SYSCTLS,
    capability_probe: Callable[[], tuple[str, str]] | None = None,
) -> tuple[list[str], dict[str, str]]:
    """Split *candidates* into (modes this host can actually deliver, modes
    it cannot with a reason each).

    Two sources of unavailability, both reported the same way so a reader
    gets one list of what they can actually get:

    * *unsupported* — a mode this bridge's own dispatch shape cannot deliver
      whatever the host is (claude-code's `default` permission mode has no
      TTY to answer a prompt under headless dispatch). Known directly, not
      measured.
    * *requires_userns* — a mode whose confinement is a bubblewrap helper
      backed by unprivileged user namespaces. Where the kernel restricts
      them the helper cannot start, and codex's own behaviour then is to
      lose every file write while still running shell commands unconfined
      (#18/#63). MEASURED, per dispatch host, at the moment it is asked.

    *capability_probe* is injectable alongside *probes* for the same reason
    *probes* is: since the executable probe — not the sysctls — decides the
    answer, a test that could only inject sysctls could no longer assert
    both kinds of kernel. It returns the same ``(state, reason)`` pair as
    :func:`_userns_capability`, which is the default.
    """
    unsupported = dict(unsupported or {})
    needs_userns = set(requires_userns)
    measure = capability_probe or _userns_capability
    capability_state, capability_reason = (
        measure() if needs_userns else ("available", "not required")
    )
    blockers = userns_restrictions(probes) if capability_state == "unavailable" else ()

    available: list[str] = []
    unavailable: dict[str, str] = {}
    for mode in candidates:
        if mode in unsupported:
            unavailable[mode] = unsupported[mode]
        elif mode in needs_userns and capability_state != "available":
            if capability_state == "not-probed":
                unavailable[mode] = f"user-namespace capability not probed: {capability_reason}"
            else:
                diagnostic = (
                    "; likely kernel setting(s): " + ", ".join(blockers) if blockers else ""
                )
                unavailable[mode] = capability_reason + diagnostic
        else:
            available.append(mode)
    return available, unavailable


class Toolchain(NamedTuple):
    """One toolchain a dispatch to this backend might need.

    *requires* are the grants the tool needs to do its job — measured
    requirements, not guesses: uv needs a writable cache under $HOME (probe
    01M0342X60F3NY8MH150G48AZ6 died on exactly that), gh needs network
    egress (probe 01M039NZ2TZYFG68YZT93A6DC7 could reach nothing). A
    requirement that came from a packaging fact rather than from the tool is
    added by :func:`measure_toolchains`, not declared here.
    """

    name: str
    requires: tuple[str, ...] = ()


def locate_toolchain(
    name: str,
    *,
    which: Callable[[str], str | None] = shutil.which,
    search_dirs: Sequence[str] = TOOLCHAIN_SEARCH_DIRS,
) -> tuple[str | None, bool]:
    """Return ``(path, on_path)`` for *name*.

    PATH first, because that is what a dispatched session actually gets;
    then the known install directories, so a tool that exists but is
    unreachable by name reads as ``present-off-path`` rather than as absent.
    """
    found = which(name)
    if found:
        return found, True
    for directory in search_dirs:
        candidate = Path(directory).expanduser() / name
        if candidate.exists():
            return str(candidate), False
    return None, False


def toolchain_packaging(path: str) -> str:
    """How the binary at *path* was installed.

    Snap is the one that changes what a dispatch can do (its own
    snap-confine cannot start inside a bubblewrap-confined mode), and it is
    detectable two ways — the /snap/bin shim, and the fact that the shim
    resolves to the snap runtime rather than to the tool. Both are checked,
    because /snap/bin/uv is a RELATIVE symlink to astral-uv.uv whose
    realpath is /usr/bin/snap.
    """
    resolved = str(Path(path).resolve())
    if path.startswith("/snap/") or resolved.startswith("/snap/") or resolved.endswith("/snap"):
        return PACKAGING_SNAP
    if resolved.startswith(("/usr/bin/", "/bin/", "/usr/sbin/", "/sbin/")):
        return PACKAGING_SYSTEM
    return PACKAGING_STANDALONE


#: How a tool is asked its version, in the order tried. Two spellings
#: because `go --version` exits non-zero and `go version` is the one that
#: answers; a tool that refuses both reports no version rather than an
#: invented one.
VERSION_ARGV = (("--version",), ("version",))


def toolchain_version(
    path: str,
    *,
    run: Callable[..., Any] = subprocess.run,
) -> str | None:
    """The version *path* reports, or None when it will not say.

    This runs the tool AS THIS PROCESS, outside any sandbox — so it is a
    fact about the host, not about a dispatch, and the surface keeps it next
    to per-mode usability rather than instead of it. Its job is to make a
    toolchain bump visible: the recorded baseline changes, and the probe
    findings get re-checked.
    """
    for argv in VERSION_ARGV:
        try:
            completed = run(
                [path, *argv],
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                timeout=10,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            return None
        if completed.returncode != 0:
            continue
        output = completed.stdout
        if isinstance(output, bytes):
            output = output.decode("utf-8", "replace")
        first = (output or "").strip().splitlines()
        if first:
            return first[0].strip()
    return None


def toolchain_fact(**facts: Any) -> dict[str, Any]:
    """Build one toolchain fact, dropping what could not be measured.

    Accepts exactly `TOOLCHAIN_KEYS`; `name` and `state` are required. Like
    `host_block`, an unagreed key is a `SurfaceError` rather than a fifth
    dialect quietly carried by one adapter.
    """
    unknown = sorted(set(facts) - set(TOOLCHAIN_KEYS))
    if unknown:
        raise SurfaceError(
            f"unagreed toolchain fact(s) {', '.join(unknown)}: add a new one to "
            f"preflight.TOOLCHAIN_KEYS, which adds it to every bridge at once"
        )
    for key in ("name", "state"):
        if not facts.get(key):
            raise SurfaceError(f"toolchain fact {key!r} is required and must be non-empty")
    if facts["state"] not in (STATE_PRESENT, STATE_OFF_PATH, STATE_ABSENT):
        raise SurfaceError(
            f"toolchain state {facts['state']!r} is not one of {STATE_PRESENT!r}, "
            f"{STATE_OFF_PATH!r}, {STATE_ABSENT!r}"
        )
    fact: dict[str, Any] = {}
    for key in TOOLCHAIN_KEYS:
        value = facts.get(key)
        if value is None:
            continue
        if key in ("requires", "usable_in"):
            fact[key] = list(value)
        elif key == "unusable_in":
            if not value:
                continue
            fact[key] = dict(value)
        else:
            fact[key] = value
    return fact


def dispatch_grants(grants: Mapping[str, Sequence[str]]) -> dict[str, list[str]]:
    """Validate and normalise a mode → grants map.

    A grant this protocol does not agree on is refused here rather than
    advertised: a reader who sees an unknown word in this map cannot tell
    whether it means more or less than the ones they know.
    """
    validated: dict[str, list[str]] = {}
    for mode, granted in grants.items():
        unknown = sorted(set(granted) - set(GRANTS))
        if unknown:
            raise SurfaceError(
                f"mode {mode!r} declares unagreed grant(s) {', '.join(unknown)}: the vocabulary is "
                f"preflight.GRANTS ({', '.join(GRANTS)})"
            )
        validated[mode] = list(granted)
    return validated


def measure_toolchains(
    specs: Sequence[Toolchain],
    *,
    grants: Mapping[str, Sequence[str]],
    grant_absence_reasons: Mapping[str, str] | None = None,
    locate: Callable[[str], tuple[str | None, bool]] = locate_toolchain,
    version: Callable[[str], str | None] = toolchain_version,
) -> list[dict[str, Any]]:
    """Measure each toolchain and say which dispatch modes can actually run it.

    *grants* is the mode → grants map for the modes this host can ACTUALLY
    deliver (pass `measure_sandbox_modes`' available list through the
    backend's own posture map, so a mode the kernel already ruled out is not
    reported as a place a tool works).

    Two things make a tool unusable in a mode, and both are reported the same
    way so a reader gets one list per tool:

    * a grant the mode does not confer (uv without a writable cache, gh
      without egress), and
    * a grant the tool needs BECAUSE OF HOW IT IS PACKAGED — a snap needs
      `nested-confinement`, because snap-confine cannot start inside a
      bubblewrap-confined mode (thor, run 01M03374VAKH0KHN0GDZ466NP4). This
      is the difference between thor's uv and orin's, from one declaration.

    *grant_absence_reasons* supplies the backend's own sentence for each
    missing grant — the place a measured probe run id belongs, so the
    briefing says why rather than only what.
    """
    reasons = dict(grant_absence_reasons or {})
    measured: list[dict[str, Any]] = []
    for spec in specs:
        path, on_path = locate(spec.name)
        if path is None:
            measured.append(
                toolchain_fact(
                    name=spec.name,
                    state=STATE_ABSENT,
                    requires=spec.requires,
                    usable_in=[],
                    unusable_in={mode: "not installed on this host" for mode in grants},
                )
            )
            continue

        packaging = toolchain_packaging(path)
        required = list(spec.requires)
        if packaging == PACKAGING_SNAP and GRANT_NESTED_CONFINEMENT not in required:
            required.append(GRANT_NESTED_CONFINEMENT)

        usable_in: list[str] = []
        unusable_in: dict[str, str] = {}
        for mode, granted in grants.items():
            missing = [grant for grant in required if grant not in granted]
            if not missing:
                usable_in.append(mode)
                continue
            unusable_in[mode] = "; ".join(
                reasons.get(grant, f"this mode grants no {grant}") for grant in missing
            )

        measured.append(
            toolchain_fact(
                name=spec.name,
                state=STATE_PRESENT if on_path else STATE_OFF_PATH,
                path=path,
                on_path=on_path,
                packaging=packaging,
                version=version(path),
                requires=required,
                usable_in=usable_in,
                unusable_in=unusable_in,
            )
        )
    return measured


def harvest_commit_policy(
    *,
    preserve_on_failure: bool,
    branch_prefix: str,
    push: bool,
    remote: str,
    extra: str | None = None,
) -> str:
    """The commit/harvest policy in force for a bridge that dispatches a
    session into a real checkout.

    Shared because it is not backend-specific: claude-code, codex and
    colleague all issue no commit of their own and all carry the same
    preserve-on-failure knobs (`preserve.py`, task t25). *extra* is where a
    backend appends the one clause that IS its own (colleague opening a PR).
    """
    policy = (
        "harvest: this bridge issues no commit of its own — a dispatched session's changes "
        "stay in the workspace for the operator to harvest"
    )
    if not preserve_on_failure:
        policy += (
            "; a technically failed dispatch leaves its changes not preserved on any branch, "
            "so they survive only as long as the workspace does"
        )
    elif push:
        policy += (
            f"; on a technical failure the workspace is preserved on a {branch_prefix}* branch "
            f"and pushed best-effort to {remote}"
        )
    else:
        policy += (
            f"; on a technical failure the workspace is preserved on a {branch_prefix}* branch, "
            "kept local to this host and never pushed"
        )
    if extra:
        policy += f"; {extra}"
    return policy


# --- deployed revision (task t32, issue #120 item 4) ------------------------
#
# The `deployment` host fact is MEASURED in the sibling shared module
# `deployment.py`, which is byte-identical across the bridges exactly as this
# file is (tests/lint/preflightsurface_test.go guards both). It lives there
# rather than here for one blunt reason: this file was 79 lines from the
# repo's 1000-line hard limit, and the measurement is a self-contained
# concern — it reads git, a build stamp and PEP 610 metadata, and touches
# nothing else in the protocol. `deployment` stays in HOST_KEYS above, because
# the AGREED KEY SET is this file's job even when the measurement is not.


def host_block(**facts: Any) -> dict[str, Any]:
    """Build the `host` block from measured facts, dropping the ones this
    bridge could not measure.

    Accepts exactly `HOST_KEYS` (an unagreed key is a `SurfaceError`, not a
    silently carried fifth dialect). `hostname` and `commit_policy` are
    required and must be non-empty. Every other key is omitted when `None`,
    and `sandbox_modes_unavailable` is additionally omitted when empty —
    an empty map is "nothing is degraded here", which absence already says.
    `writable_paths: []` is kept: writing nowhere is a fact.
    """
    unknown = sorted(set(facts) - set(HOST_KEYS))
    if unknown:
        raise SurfaceError(
            f"unagreed host fact(s) {', '.join(unknown)}: the capability surface is one contract "
            f"across all bridges — add a new fact to preflight.HOST_KEYS (which adds it "
            f"everywhere at once), never to one adapter"
        )

    for key in _REQUIRED_HOST_KEYS:
        if not facts.get(key):
            raise SurfaceError(
                f"host fact {key!r} is required and must be non-empty: every bridge knows its own "
                f"{key} without measuring anything, so an empty one is a caller bug rather than an "
                f"honest absence"
            )

    artifact_publish = facts.get("artifact_publish")
    if artifact_publish is not None and artifact_publish not in ARTIFACT_PUBLISH_VALUES:
        raise SurfaceError(
            "host fact 'artifact_publish' must be supported, unsupported-by-host, "
            "or not-applicable-no-workspace"
        )

    host: dict[str, Any] = {}
    for key in HOST_KEYS:
        value = facts.get(key)
        if value is None:
            continue
        if key in ("sandbox_modes_unavailable", "dispatch_grants"):
            # An empty map is "nothing is degraded here" / "no mode grants
            # anything", which absence already says without inviting the
            # reader to interpret an empty object.
            if not value:
                continue
            host[key] = dict(value)
        elif key in ("sandbox_modes", "writable_paths"):
            host[key] = list(value)
        elif key == "toolchains":
            if not value:
                continue
            host[key] = [toolchain_fact(**fact) for fact in value]
        else:
            host[key] = value
    return host


def surface(host: Mapping[str, Any]) -> dict[str, Any]:
    """The `capabilities.preflight` value: a protocol version and the host
    block, and deliberately nothing else."""
    if not host:
        raise SurfaceError(
            "the host block states no fact at all: a gate that briefs an actor about nothing "
            "would refuse its dispatch for no gain (mirrors internal/preflight.checkHost)"
        )
    unknown = sorted(set(host) - set(HOST_KEYS))
    if unknown:
        raise SurfaceError(f"unagreed host fact(s) {', '.join(unknown)}")
    return {"protocol_version": PROTOCOL_VERSION, "host": dict(host)}


def capability_block(host: Mapping[str, Any]) -> dict[str, Any]:
    """The complete document an actor registration carries as its
    `capabilities` — copy it verbatim into `POST /v1alpha1/actors`."""
    return {CAPABILITY_KEY: surface(host)}


def validate_block(block: Any) -> None:
    """Raise `SurfaceError` unless *block* is a capability document this
    control plane would accept.

    The mirror of `internal/preflight.ParseSurface` + `checkHost`, so a
    bridge's own tests (and the conformance kit against a live endpoint) can
    check the shape without a running control plane. Deliberately strict
    about the same three things the engine is: the version, the presence of
    a host object, and that the host states at least one fact.
    """
    if not isinstance(block, Mapping) or CAPABILITY_KEY not in block:
        raise SurfaceError(f"a capability document must carry a {CAPABILITY_KEY!r} block")
    advertised = block[CAPABILITY_KEY]
    if not isinstance(advertised, Mapping):
        raise SurfaceError(f"{CAPABILITY_KEY} must be an object")
    version = advertised.get("protocol_version")
    if version != PROTOCOL_VERSION:
        raise SurfaceError(
            f"{CAPABILITY_KEY}.protocol_version is {version!r}, not the {PROTOCOL_VERSION!r} this "
            "control plane speaks"
        )
    host = advertised.get("host")
    if not isinstance(host, Mapping):
        raise SurfaceError(f"{CAPABILITY_KEY}.host must be an object of measured facts")
    surface(host)


def _raw_toolchain_facts(names: Sequence[str]) -> list[dict[str, Any]]:
    """The HOST half of each named toolchain's facts: where it is, whether it
    is on PATH, how it was packaged, what version it reports.

    Deliberately no per-mode verdict: that half needs the backend's posture
    map, which lives in a bridge's own `capabilities.py`. This is the part
    that can be measured anywhere, by anyone, with nothing installed.
    """
    facts = []
    for name in names:
        path, on_path = locate_toolchain(name)
        if path is None:
            facts.append({"name": name, "state": STATE_ABSENT})
            continue
        facts.append(
            {
                "name": name,
                "state": STATE_PRESENT if on_path else STATE_OFF_PATH,
                "path": path,
                "on_path": on_path,
                "packaging": toolchain_packaging(path),
                "version": toolchain_version(path),
            }
        )
    return facts


def _main(argv: Sequence[str]) -> int:
    """Measure this host's toolchains with the same code the surface uses.

        python3 -m <bridge>.preflight uv go gh
        cat preflight.py | ssh <host> python3 - uv go gh

    The second form is the point. A host where this bridge is not installed
    — or is installed at a version that predates a fact — can still be
    measured by the module that DEFINES the fact, rather than by a second
    implementation of `which` and `readlink` in a shell script that drifts
    from this one. `scripts/toolchain-baseline.sh` is the caller.
    """
    import json

    names = list(argv)
    if not names:
        print("usage: preflight.py <toolchain> [<toolchain> ...]", file=sys.stderr)
        return 2
    envelope = {
        "hostname": hostname(),
        # The PATH this measurement searched, recorded because `on_path` is
        # relative to it: a probe run over ssh sees a login shell's PATH,
        # while a dispatched session inherits the bridge process's. A
        # baseline that does not say which one it used cannot be compared.
        "search_path": os.environ.get("PATH", ""),
        "toolchains": _raw_toolchain_facts(names),
    }
    print(json.dumps(envelope, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(_main(sys.argv[1:]))
