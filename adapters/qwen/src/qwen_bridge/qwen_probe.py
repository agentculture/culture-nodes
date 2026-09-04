"""Host-local qwen measurement for the capability surface (plan t3).

Plan task t3 of qwen-bridge-acp. The qwen bridge's registration document
carries the shared preflight host block (capabilities.py + preflight.py,
the protocol 1.0 contract the engine reads) and, beside it, the qwen
section: the host-local facts only this backend measures. Every fact here
is MEASURED on the host the bridge runs on, never assumed, and every
measurement input is injectable so the test suite replays the fleet's
recorded shapes (tests/fixtures/qwen_probe/ + tests/fixtures/acp/) rather
than whichever host happens to run pytest:

* the qwen CLI version (`qwen --version` — measured bare `0.22.0`, first
  line) and the BUNDLED node runtime the launcher execs from the install
  layout (`<layout>/node/bin/node --version` — the launcher script,
  measured 2026-08-25, runs `$ROOT/node/bin/node $ROOT/lib/cli-entry.js`;
  the host's PATH node is a different fact this probe never consults),
* the model identity: the agent's own WIRE answer (session/new
  models.currentModelId — the committed fixture
  tests/fixtures/acp/session_new_measured.json carries the fleet's
  measured value) ahead of the host settings file (model.name) as the
  no-session fallback,
* the config source: the settings path, or the environment variable NAME
  the bridge configures — names and paths only, never values (h17: the
  baseUrl and the API-key value stay off the surface),
* the agent's declared mode exposure (session/new
  modes.availableModes, in the agent's own order) and the bridge's
  refusal of the modes outside its vocabulary (wire.ACP_MODES — h15),
* the context budget: the budget fields the agent declares for the
  running model (availableModels[].contextWindowSize, or the
  _meta.contextLimit the 2026-08-25 fresh-session re-measurement
  exposed) — None where neither is exposed, which the section omits
  rather than guesses.

The scratch-session probe (initialize + session/new, no prompt) is the
same measured-evidence workflow the committed acp fixtures record (each
fixture's provenance field names its probe). It degrades to None on any
failure: the section omits what it could not measure rather than the
whole registration fails on a host whose model credentials the operator's
shell does not carry. The dispatch path (the t2 driver) fails closed where
it must — this surface is for operators reading a host, not for turns.
"""

from __future__ import annotations

import io
import json
import subprocess
import tempfile
from pathlib import Path
from typing import Any, Callable, Mapping

from qwen_bridge.acp import errors
from qwen_bridge.acp import probe as acp_probe
from qwen_bridge.acp import transport, wire
from qwen_bridge.config import Config

#: Where the host keeps the qwen settings, relative to $HOME (measured on
#: all three fleet hosts, 2026-08-25: the model identity and endpoint sit
#: under model.name / model.baseUrl, the API-key variable under env.<NAME>).
SETTINGS_DIRNAME = ".qwen"
SETTINGS_FILENAME = "settings.json"

#: The bundled node runtime's position inside the install layout, relative
#: to the layout root. Measured, not guessed: the launcher (the sh script
#: at <root>/bin/qwen, read 2026-08-25) execs
#: `"$ROOT/node/bin/node" "$ROOT/lib/cli-entry.js"` — the runtime sits
#: under node/bin/ in the layout, and the fleet's recorded drift
#: (2026-08-23: v22.23.2 on spark, v18.19.1 on thor; re-measured aligned
#: v22.23.2 on all three hosts 2026-08-25) is visible ONLY through this
#: path, never through the host's PATH node.
NODE_RUNTIME_RELATIVE = ("node", "bin", "node")

#: The environment variable name the bridge's qwen_env may use to name a
#: model (Config's documented passthrough). Reported by NAME only —
#: never by value (h17).
MODEL_ENV_NAME = "QWEN_CODE_MODEL"

#: Why a measured mode outside the bridge's vocabulary is refused — the
#: h15 stance, in the surface's own words: the per-mode mapping stays
#: unverified until it is checked live, so the document names the gap
#: instead of guessing the grant.
MODE_REFUSAL_REASON = (
    "outside the bridge's mode vocabulary (wire.ACP_MODES): the per-mode "
    "mapping stays unverified until the h15 live check — the agent "
    "exposes this mode only on fresh sessions (measured 2026-08-25) and a "
    "dispatch naming it is refused before the session"
)

#: The scratch probe's wall-clock bound: a registration-time measurement
#: that hangs is a broken host fact, not a slow one.
SCRATCH_TIMEOUT_SECONDS = 30.0


def locate(cfg: Config) -> str:
    """The located qwen binary, or the t2 seam's named refusal.

    By reference, not by copy: the boot refusal (QwenAgentMissingError,
    naming the probe paths) is the seam's (acp.probe.locate_qwen_bin), and
    the capability surface degrades to exactly that refusal rather than
    inventing a second one (h5: a missing binary is a named refusal,
    never a crash).
    """
    return acp_probe.locate_qwen_bin(cfg)


def qwen_root(qwen_bin: str) -> Path:
    """The install-layout root the launcher at <root>/bin/qwen runs from
    (the measured fleet layout, spec s4).

    Refuses a binary outside the layout with a named error rather than
    deriving a runtime path from an unmeasured shape: a qwen installed
    elsewhere (the operator's explicit qwen_bin, say) may carry no node
    at all, and the honest report is `node_runtime: absent`, which a
    guessed derivation could not produce.
    """
    bin_dir = Path(qwen_bin).resolve().parent
    if bin_dir.name != "bin":
        raise ValueError(
            f"qwen capability probe: {qwen_bin!r} is not under the measured install "
            "layout (<root>/bin/qwen) — refusing to derive a node runtime from an "
            "unmeasured path"
        )
    return bin_dir.parent


def node_runtime(qwen_bin: str) -> Path | None:
    """The bundled node runtime under the layout, or None when the layout
    does not carry one (a measurement, not a guess)."""
    try:
        runtime = qwen_root(qwen_bin).joinpath(*NODE_RUNTIME_RELATIVE)
    except ValueError:
        return None
    return runtime if runtime.is_file() else None


def _first_line(argv: list[str], run: Callable[..., Any] = subprocess.run) -> str | None:
    """The first line *argv* reports under `--version`, or None when it
    will not say — the same honesty as preflight.toolchain_version (a
    tool that refuses reports no version rather than an invented one),
    kept local so the probe's injectable *run* flows through one call."""
    try:
        completed = run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=SCRATCH_TIMEOUT_SECONDS,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if completed.returncode != 0:
        return None
    lines = (completed.stdout or b"").decode("utf-8", "replace").strip().splitlines()
    return lines[0].strip() if lines else None


def qwen_version(qwen_bin: str, *, run: Callable[..., Any] = subprocess.run) -> str | None:
    """The qwen CLI's own version line (measured: the bare `0.22.0`)."""
    return _first_line([qwen_bin, "--version"], run=run)


def node_version(qwen_bin: str, *, run: Callable[..., Any] = subprocess.run) -> str | None:
    """The BUNDLED runtime's version — the one the launcher execs, never
    the host's PATH node (a version drift between the two is exactly the
    fact this exists to make visible)."""
    runtime = node_runtime(qwen_bin)
    if runtime is None:
        return None
    return _first_line([str(runtime), "--version"], run=run)


def settings_path(home: str | Path | None = None) -> Path:
    """Where this host keeps the qwen settings (measured: all three fleet
    hosts, 2026-08-25)."""
    base = Path(home) if home is not None else Path.home()
    return base / SETTINGS_DIRNAME / SETTINGS_FILENAME


def read_settings(path: Path) -> dict[str, Any]:
    """The settings document, or {} when absent/unreadable/malformed —
    absence is a measurement the section can honestly report, never an
    error that kills a registration on a host the operator is guessing
    at."""
    try:
        loaded = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return loaded if isinstance(loaded, dict) else {}


def model_from_settings(settings: Mapping[str, Any]) -> str | None:
    """The model identity the host's settings name (model.name) — an
    identifier the fleet reports in public run records, not a secret."""
    model = settings.get("model")
    if not isinstance(model, dict):
        return None
    name = model.get("name")
    return name if isinstance(name, str) and name else None


def config_source(
    settings: Mapping[str, Any],
    path: Path,
    qwen_env: Mapping[str, str],
) -> str | None:
    """Where the host's model configuration comes from — the settings
    path, or the environment variable NAME the bridge configures. Never a
    value (h17): model.baseUrl and the API-key value stay off the
    surface, and a source that only exists as a value is no source at
    all — it is omitted rather than quoted.
    """
    if model_from_settings(settings) is not None:
        return str(path)
    if MODEL_ENV_NAME in qwen_env:
        return MODEL_ENV_NAME
    return None


def wire_model_id(session_new_result: Mapping[str, Any]) -> str | None:
    """The agent's own wire answer for the running model (session/new
    models.currentModelId, verbatim — the suffixed and unsuffixed forms
    both ride through unchanged, the way the seam's facts do it)."""
    models = session_new_result.get("models")
    if not isinstance(models, dict):
        return None
    current = models.get("currentModelId")
    return current if isinstance(current, str) and current else None


def modes_available(session_new_result: Mapping[str, Any]) -> tuple[str, ...]:
    """The agent's declared mode exposure, in the agent's own order
    (session/new modes.availableModes ids — the measured vocabulary the
    gate's per-session check runs against)."""
    modes = session_new_result.get("modes")
    if not isinstance(modes, dict):
        return ()
    available = modes.get("availableModes")
    if not isinstance(available, list):
        return ()
    return tuple(
        entry["id"]
        for entry in available
        if isinstance(entry, dict) and isinstance(entry.get("id"), str)
    )


def context_budget(session_new_result: Mapping[str, Any]) -> int | None:
    """The context budget the agent declares for the running model —
    availableModels[].contextWindowSize, or the _meta.contextLimit the
    2026-08-25 fresh-session re-measurement exposed (the field is
    environment-dependent, so both measured shapes are honored). None
    where neither carries a number: the 2026-08-23 committed replay
    shape declares contextWindowSize null, and the honest report is
    omission, not a guessed window size."""
    models = session_new_result.get("models")
    if not isinstance(models, dict):
        return None
    current = models.get("currentModelId")
    available = models.get("availableModels")
    if not isinstance(current, str) or not isinstance(available, list):
        return None
    for entry in available:
        if not isinstance(entry, dict) or entry.get("modelId") != current:
            continue
        meta = entry.get("_meta")
        for field in (entry.get("contextWindowSize"), (meta or {}).get("contextLimit")):
            if isinstance(field, int) and field > 0:
                return field
    return None


def scratch_session(
    cfg: Config,
    *,
    popen: Callable[..., Any] = subprocess.Popen,
    timeout: float = SCRATCH_TIMEOUT_SECONDS,
) -> tuple[dict[str, Any], dict[str, Any]] | None:
    """Run initialize + session/new against the located agent — no
    prompt, no turn — and return the two results (the same wire shapes
    the committed acp fixtures record).

    Any failure — a spawn that will not start, a refused handshake, an
    EOF — returns None: the section omits the session-measured facts and
    the registration still reports the host it could measure. The binary
    itself missing is the t2 seam's named refusal (h5) and propagates,
    because a host that cannot locate its agent has no capability
    surface to advertise at all.
    """
    qwen_bin = locate(cfg)
    with tempfile.TemporaryDirectory(prefix="qwen-probe-") as workdir:
        try:
            proc = popen(
                [qwen_bin, "--acp"],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1,
            )
        except (OSError, ValueError):
            return None
        try:
            link = transport.AgentLink(
                proc, transcript_path=Path(workdir) / "probe.jsonl", stdout=io.StringIO()
            )
            link.send(
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": wire.METHOD_INITIALIZE,
                    "params": {
                        "protocolVersion": 1,
                        # the v1 client obligations, stated on the wire
                        # (the measured initialize request, fixture
                        # tests/fixtures/acp/initialize_measured.json): no
                        # fs/terminal handlers in the first cut
                        "clientCapabilities": {
                            "fs": {"readTextFile": False, "writeTextFile": False},
                            "terminal": False,
                        },
                    },
                }
            )
            init_response = link.wait_response(1)
            if not isinstance(init_response, dict) or "result" not in init_response:
                return None
            link.send(
                {
                    "jsonrpc": "2.0",
                    "id": 2,
                    "method": wire.METHOD_SESSION_NEW,
                    "params": {"cwd": str(Path(workdir)), "mcpServers": []},
                }
            )
            new_response = link.wait_response(2)
            if not isinstance(new_response, dict) or "result" not in new_response:
                return None
            return init_response["result"], new_response["result"]
        except errors.DriverTerminated:
            return None
        finally:
            try:
                proc.terminate()
                proc.wait(timeout=timeout)
            except (OSError, subprocess.SubprocessError):
                try:
                    proc.kill()
                except (OSError, AttributeError):
                    pass


def qwen_facts(
    cfg: Config,
    *,
    run: Callable[..., Any] = subprocess.run,
    home: str | Path | None = None,
    settings: Mapping[str, Any] | None = None,
    session: tuple[dict[str, Any], dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """The raw qwen-section measurements for this host — plain data, ready
    for capabilities.qwen_section's key agreement.

    *run* injects the version lines; *home*/*settings* inject the
    configuration (a settings document passed in is used as-is, else the
    file at *settings_path(home)* is read); *session* injects the
    scratch-session results (a caller that did not measure — or whose
    measurement degraded to None — passes nothing, and the section omits
    the session-measured facts rather than guess them). The model
    identity prefers the agent's WIRE answer over the settings file: the
    surface reports what the agent runs, and the settings path stays as
    the config-source fact (c20/h17).
    """
    qwen_bin = locate(cfg)
    runtime = node_runtime(qwen_bin)
    path = settings_path(home)
    document = dict(settings) if settings is not None else read_settings(path)
    wire_model = None
    agent_modes: tuple[str, ...] = ()
    budget = None
    if session is not None:
        _init_result, new_result = session
        wire_model = wire_model_id(new_result)
        agent_modes = modes_available(new_result)
        budget = context_budget(new_result)
    supported = tuple(mode for mode in agent_modes if mode in wire.ACP_MODES)
    refused = {mode: MODE_REFUSAL_REASON for mode in agent_modes if mode not in wire.ACP_MODES}
    return {
        "qwen_version": qwen_version(qwen_bin, run=run),
        "node_version": node_version(qwen_bin, run=run),
        "node_path": str(runtime) if runtime is not None else None,
        "model_identity": wire_model or model_from_settings(document),
        "config_source": config_source(document, path, cfg.qwen_env),
        "supported_modes": supported or None,
        "modes_refused": refused or None,
        "context_budget": budget,
    }
