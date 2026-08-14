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

import socket
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

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
HOST_KEYS = (
    "hostname",
    "sandbox_modes",
    "sandbox_modes_unavailable",
    "default_sandbox_mode",
    "confinement",
    "commit_policy",
    "writable_paths",
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

    Read-only and stdlib-only: the sysctls ARE the fact, so read them rather
    than shelling out to `bwrap` to find out. An absent knob is a kernel that
    does not restrict here — never read absence as the blocking value.
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


def measure_sandbox_modes(
    candidates: Sequence[str],
    *,
    requires_userns: Iterable[str] = (),
    unsupported: Mapping[str, str] | None = None,
    probes: Sequence[tuple[str, str]] = USERNS_SYSCTLS,
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
    """
    unsupported = dict(unsupported or {})
    blockers = userns_restrictions(probes)
    needs_userns = set(requires_userns)

    available: list[str] = []
    unavailable: dict[str, str] = {}
    for mode in candidates:
        if mode in unsupported:
            unavailable[mode] = unsupported[mode]
        elif mode in needs_userns and blockers:
            unavailable[mode] = (
                "unprivileged user namespaces are restricted on this host ("
                + ", ".join(blockers)
                + "), so the sandbox helper this mode's confinement depends on cannot "
                "start here — requesting it does not fail, it silently loses every file "
                "write while shell commands keep running unconfined (#18/#63)"
            )
        else:
            available.append(mode)
    return available, unavailable


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

    host: dict[str, Any] = {}
    for key in HOST_KEYS:
        value = facts.get(key)
        if value is None:
            continue
        if key == "sandbox_modes_unavailable":
            if not value:
                continue
            host[key] = dict(value)
        elif key in ("sandbox_modes", "writable_paths"):
            host[key] = list(value)
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
