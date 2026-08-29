"""What THIS bridge knows about the host it dispatches on (issue #67, task
t15) — the backend-specific half of the preflight capability surface.
Plan task t3 of qwen-bridge-acp: the qwen rewrite of the ported
codex-baseline narrative (whose measured prose — the bubblewrap
posture, the snap-uv run ids, the .git carve-out — described codex, not
qwen, and is re-measured here against the fleet's recorded shapes).

Every bridge has one of these and it is the only per-backend code in this
feature: the protocol, the measurement helpers and the agreed key set live
once, in `preflight.py`, byte-identical across all four adapters. What
differs here is what genuinely differs between backends.

The qwen trust model (measured 2026-08-23; the fixtures under
tests/fixtures/acp/ carry the provenance): qwen-code runs its own tools
IN-PROCESS as the bridge user. The bridge — the ACP client — advertises no
fs/terminal handlers and the agent sends none; there is no sandbox
helper, no .git carve-out, no kernel boundary between a session and the
host. The ACP session modes are therefore an APPROVAL POLICY (which tools
the agent offers to run without asking), not a confinement: every mode
the bridge supports can do everything this process can do, and that is
reported as such rather than papered over with a mode-by-mode grant
matrix nobody measured. A dispatch that names no mode is refused before
the session (the gate never falls back to the agent's measured default),
so this backend reports no default mode at all — absence is the honest
statement of a policy that fails closed.

The registration document carries two sibling blocks: the shared preflight
block (the protocol 1.0 contract the engine reads — `capability_block`
of `host_facts`) and the qwen section (`qwen_probe`'s measurements: the
qwen and bundled-node versions, the model identity, the config source,
the agent's mode exposure). The engine ignores the sibling
(internal/preflight.ParseSurface reads only the preflight block; the Go
lint compares only the protocol constants), and the operator sees both
(GET /v1alpha1/actors renders Capabilities verbatim). Deviation d1
(2026-08-25) records this split against plan t3's AC1.
"""

from __future__ import annotations

import os
import pwd
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

from qwen_bridge import deployment, preflight, qwen_probe
from qwen_bridge.config import Config


def _unix_user() -> str:
    """The OS account this bridge process runs as.

    Prefixed onto the confinement sentence (task t2, issue #243) so the
    capability surface names which account a dispatch really runs as — the
    fact that decides what a session can reach once agents run as dedicated
    OS users rather than inside a shared sandbox. stdlib only: `pwd` keeps
    this adapter's zero-runtime-dependency promise intact.
    """
    return pwd.getpwuid(os.getuid()).pw_name


#: The ACP session modes a dispatch may name, in the agent's measured
#: order (tests/fixtures/acp/session_new_measured.json's availableModes).
#: Deliberately a second declaration rather than a derivation of
#: `qwen_cli.ACP_MODES`: a mode added to the seam's vocabulary needs a
#: decision about its confinement story (which for this backend means
#: "grants everything, says so in the prose"), and deriving the tuple
#: would advertise the new mode as supported without anyone making that
#: decision. The test `test_the_supported_modes_are_exactly_the_ones_this
#: bridge_can_pass` compares the two, so the decision is demanded rather
#: than skipped.
SUPPORTED_ACP_MODES = ("plan", "default", "auto-edit", "auto")

#: The one mode the measured agent can expose that the bridge's vocabulary
#: does not admit, and why — the h15 stance in the shared document's own
#: words (the qwen section carries the same refusal reason from
#: qwen_probe, so a reader sees one story in both blocks).
_MODE_UNAVAILABLE = {"yolo": qwen_probe.MODE_REFUSAL_REASON}

#: The toolchains this bridge reports on: the CLI it drives and the
#: BUNDLED node runtime it launches with — nothing else. A qwen dispatch
#: needs no other grant-bearing tool to be reported here (the session's
#: tools run in-process with the host's authority); adding one means
#: declaring what it needs, which is the point. Their versions ride here
#: to be watched: a bump of either is visible in the document (the codex
#: sibling's version test, re-aimed at these two).
TOOLCHAINS = (
    preflight.Toolchain("qwen"),
    preflight.Toolchain("node"),
)

# --- the qwen section (deviation d1, 2026-08-25) --------------------------
#
# The agreed keys of the qwen-namespaced sibling block. Same discipline as
# HOST_KEYS, at its own scale: a fact this backend cannot measure is
# OMITTED (an absent key reads as absence, a null would read as a fact),
# and an unagreed key is a SurfaceError rather than a quietly carried
# fifth dialect. Adding a key here is a qwen-section decision — it does
# NOT touch the shared contract, which is the whole point of the split.
QWEN_SECTION_KEY = "qwen"
QWEN_SECTION_KEYS = (
    "qwen_version",
    "node_version",
    "node_path",
    "model_identity",
    "config_source",
    "supported_modes",
    "modes_refused",
    "context_budget",
)


def _layout_locate(qwen_bin: str) -> Callable[[str], tuple[str | None, bool]]:
    """This backend's toolchain locate: qwen and node resolve at their
    MEASURED install-layout positions (spec s4 — the fleet's qwen is
    absent from the non-interactive PATH on thor/orin, and the node the
    launcher execs is the layout's, not the host's), so the versions the
    surface reports are the ones a dispatch actually runs. *on_path* is
    False for both by construction: the bridge never resolves them by
    name, and reporting a PATH visibility nobody consulted would be the
    fact this surface exists to not report. Other toolchains fall back to
    the shared locate, unmodified."""

    def locate(name: str) -> tuple[str | None, bool]:
        if name == "qwen":
            return qwen_bin, False
        if name == "node":
            runtime = qwen_probe.node_runtime(qwen_bin)
            return (str(runtime), False) if runtime is not None else (None, False)
        return preflight.locate_toolchain(name)

    return locate


def host_facts(
    cfg: Config,
    *,
    probes: Sequence[tuple[str, str]] = preflight.USERNS_SYSCTLS,
    capability_probe: Callable[[], tuple[str, str]] | None = None,
    locate: Callable[[str], tuple[str | None, bool]] | None = None,
    version: Callable[[str], str | None] = preflight.toolchain_version,
    git_probe: Callable[[Path], bool] = preflight.probe_git_metadata_write,
) -> dict[str, Any]:
    """Measure this host and return the `host` block for its capability
    surface.

    Every measurement input is injectable so a test can assert the fleet's
    recorded shapes rather than whichever host is running the suite:
    *capability_probe* and *probes* keep the shared measure_sandbox_modes
    signature (unused for this backend — no mode needs unprivileged user
    namespaces, because no mode confines anything at the kernel level),
    *locate*/*version* are how the toolchains are found and asked their
    versions, and *git_probe* is the write attempt behind
    `git_metadata_writable` (issue #94).

    The qwen difference from the codex baseline is in that last default:
    this backend's sessions run with THIS process's own authority
    (in-process tools, no confinement — the docstring above), so the
    process's answer IS the dispatch's answer and the default probe
    measures it. The codex sibling's sessions are confined more tightly
    than its process, so its honest default is `not-probed` there and a
    measured answer here.

    A host that cannot locate its qwen binary gets the t2 seam's named
    boot refusal (h5) — `locate_qwen_bin`'s QwenAgentMissingError naming
    the probe paths — rather than a surface built on a guessed install.
    """
    qwen_bin = qwen_probe.locate(cfg)
    locate = locate or _layout_locate(qwen_bin)
    available, unavailable = preflight.measure_sandbox_modes(
        SUPPORTED_ACP_MODES + ("yolo",),
        unsupported=_MODE_UNAVAILABLE,
        probes=probes,
        capability_probe=capability_probe,
    )
    # Every supported mode grants everything the host user can do: the
    # in-process tools have no kernel boundary to be refused by, and the
    # mode's approval policy is the agent's internal concern, not a host
    # grant. Stated in full rather than omitted — an empty grants map
    # reads as "nothing runs here", the opposite of the truth.
    grants = preflight.dispatch_grants({mode: preflight.GRANTS for mode in available})
    writable_paths = list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes)
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=available,
        sandbox_modes_unavailable=unavailable,
        # No default_sandbox_mode: the gate refuses a mode-less dispatch
        # before the session (h15) — reporting a default would claim a
        # fallback the bridge deliberately never makes. `confinement`
        # says it.
        confinement=_confinement(),
        dispatch_grants=grants,
        toolchains=preflight.measure_toolchains(
            TOOLCHAINS,
            grants=grants,
            locate=locate,
            version=version,
        ),
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=writable_paths,
        git_metadata_writable=preflight.measure_git_metadata_writable(
            writable_paths, probe=git_probe
        ),
        artifact_publish="unsupported-by-host",
        # Which revision of THIS bridge is answering (task t32, issue #120
        # item 4): measured from the module object, so it describes the
        # code that is actually running.
        deployment=deployment.deployment_facts(
            sys.modules[__package__], "culture-nodes-qwen-bridge"
        ),
    )


def _confinement() -> str:
    """One sentence on what actually confines a qwen session here:
    nothing at the kernel level, and the mode axis is an approval policy.

    Stated separately from the mode list because a reader who sees only a
    mode vocabulary can mistake it for a sandbox — and for this backend
    the mistake runs backwards: the modes grant less APPROVAL (which tools
    the agent offers to run unasked), never less authority. The kernel
    boundary between a session and the host does not exist; the boundary
    that does exist is the one the image (plan t6) and the operator's
    sandboxing draw around the bridge process itself."""
    return (
        f"unix-user:{_unix_user()}: "
        "qwen-code runs its own tools in-process as the bridge user (measured 2026-08-23: "
        "no fs/terminal client requests, no sandbox helper) — the ACP session modes are an "
        "approval policy, not a kernel confinement: every supported mode can do everything "
        "this process can do, and a dispatch that names no mode is refused before the "
        "session — the gate never falls back to the agent's measured default"
    )


def qwen_section(
    cfg: Config,
    *,
    facts: Mapping[str, Any] | None = None,
    run: Callable[..., Any] = subprocess.run,
    home: str | Path | None = None,
    settings: Mapping[str, Any] | None = None,
    session: tuple[dict[str, Any], dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """The qwen section of the registration document — the host-local
    facts the shared preflight block does not carry (deviation d1).

    *facts* injects a pre-measured document (the tests' path); otherwise
    the measurements run through qwen_probe with *run*/*home*/*settings*
    injected the same way. *session* is the scratch-session results — a
    caller that did not measure them (or whose measurement degraded to
    None) passes nothing, and the section omits the session-measured
    facts (supported_modes, modes_refused, the wire model identity, the
    context budget) rather than guess them.

    Key agreement is enforced exactly as the shared block enforces its
    own: an unagreed fact is a SurfaceError, and a None measurement is
    omitted (absence is the honest statement of "not measured here").
    """
    measured = (
        dict(facts)
        if facts is not None
        else qwen_probe.qwen_facts(cfg, run=run, home=home, settings=settings, session=session)
    )
    unknown = sorted(set(measured) - set(QWEN_SECTION_KEYS))
    if unknown:
        raise preflight.SurfaceError(
            f"unagreed qwen section fact(s) {', '.join(unknown)}: the section is one contract "
            f"for this backend — add a new fact to capabilities.QWEN_SECTION_KEYS, never "
            "carry it quietly"
        )
    section: dict[str, Any] = {}
    for key in QWEN_SECTION_KEYS:
        value = measured.get(key)
        if value is None:
            continue
        if key in ("supported_modes", "modes_refused") and not value:
            continue
        section[key] = (
            dict(value)
            if key == "modes_refused"
            else (list(value) if key == "supported_modes" else value)
        )
    return section


def registration_capabilities(
    cfg: Config,
    *,
    run: Callable[..., Any] = subprocess.run,
    home: str | Path | None = None,
    settings: Mapping[str, Any] | None = None,
    session: tuple[dict[str, Any], dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """The complete document an actor registration carries as its
    capabilities — the shared preflight block and the qwen section beside
    it, and what it says, nothing more (deviation d1, 2026-08-25).

    Build the shared block FIRST: a host that cannot locate its qwen
    binary gets the t2 seam's named boot refusal (h5) before anything
    half-measured is advertised. The section's live measurement is the
    scratch session (qwen_probe.scratch_session — initialize +
    session/new, no prompt); callers that inject *session* (or
    *settings*, which marks the measurement as fully injected) skip it.

    The shared value is preflight.SURFACE of the host block, not
    capability_block: capability_block is the complete single-key
    document (what GET /v1/capabilities serves, verbatim), and this
    document carries the qwen section beside that same value — the
    nesting is one level, exactly the shape internal/preflight's
    ParseSurface reads (the preflight block under its key) and the
    shape validate_block mirrors.
    """
    host = host_facts(cfg)
    if session is None and settings is None:
        session = qwen_probe.scratch_session(cfg)
    return {
        preflight.CAPABILITY_KEY: preflight.surface(host),
        QWEN_SECTION_KEY: qwen_section(cfg, run=run, home=home, settings=settings, session=session),
    }
