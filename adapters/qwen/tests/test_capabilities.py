"""The qwen backend's capability surface (plan t3): the re-aimed codex
sibling template.

test_preflight.py asserts the shared measurement (byte-identical across
the four bridges). What is asserted here is qwen's OWN posture: the
in-process trust model (every supported mode grants everything the host
user can do, and says so), the ACP mode vocabulary against the seam's,
the qwen + bundled-node toolchain facts, the named boot refusal (h5), the
git answer measured with the process's own authority (the opposite of
the codex sibling's not-probed default), and the qwen section — the
deviation-d1 sibling block and its key agreement.
"""

from __future__ import annotations

import json
import os
import pwd
from pathlib import Path

import pytest

from qwen_bridge import capabilities, preflight, qwen_probe
from qwen_bridge.acp import errors
from qwen_bridge.config import Config
from qwen_bridge.qwen_cli import ACP_MODES
from qwen_bridge import __main__ as qwen_main

HERE = Path(__file__).parent
PROBE_FIXTURES = HERE / "fixtures" / "qwen_probe"
ACP_FIXTURES = HERE / "fixtures" / "acp"

#: The recorded fleet shapes the tests replay: the four-mode scratch
#: session (the committed acp measurement) and the host settings document.
SESSION_FIXTURE = json.loads(
    (ACP_FIXTURES / "session_new_measured.json").read_text(encoding="utf-8")
)["response"]["result"]
SETTINGS_FIXTURE = json.loads((PROBE_FIXTURES / "settings-host.json").read_text(encoding="utf-8"))[
    "settings"
]


def _fixture(name: str) -> dict:
    return json.loads((PROBE_FIXTURES / name).read_text(encoding="utf-8"))


def _layout(tmp_path: Path) -> tuple[Path, Path]:
    root = tmp_path / "layout"
    qwen_bin = root / "bin" / "qwen"
    node_bin = root / "node" / "bin" / "node"
    qwen_bin.parent.mkdir(parents=True)
    node_bin.parent.mkdir(parents=True)
    qwen_bin.write_text("launcher", encoding="utf-8")
    # the t2 locator honors an explicit qwen_bin as-is but demands it be
    # an executable file — the fake layout must satisfy the same contract
    qwen_bin.chmod(0o755)
    node_bin.write_text("runtime", encoding="utf-8")
    return qwen_bin, node_bin


def _versions(qwen_bin: Path, node_bin: Path, qwen: str = "0.22.0", node: str = "v22.23.2"):
    return {str(qwen_bin): qwen, str(node_bin): node}


def _host(tmp_path: Path, qwen_bin: Path, node_bin: Path, **versions_kwargs) -> dict:
    lines = _versions(qwen_bin, node_bin, **versions_kwargs)
    return capabilities.host_facts(
        Config(repo_allowlist=(str(tmp_path),), qwen_bin=str(qwen_bin)),
        version=lambda path: lines.get(path, "test-version"),
    )


def _tool(host: dict, name: str) -> dict:
    return next(fact for fact in host["toolchains"] if fact["name"] == name)


# --- the mode vocabulary ----------------------------------------------------


def test_the_supported_modes_are_exactly_the_ones_this_bridge_can_pass():
    """The capability layer's declaration and the seam's policy vocabulary
    must agree: a mode added to one without the other is a silent drift in
    the grant/confinement story, and the comparison is the demanded
    decision (the codex sibling's candidate-modes test, re-aimed)."""
    assert set(capabilities.SUPPORTED_ACP_MODES) == ACP_MODES


def test_yolo_is_unavailable_with_the_named_reason(tmp_path):
    """The fresh-session shape measured 2026-08-25 exposed a fifth mode —
    the widest grant. The shared document reports it UNAVAILABLE with the
    h15 refusal (vocabulary + unverified mapping + the fresh-session
    measurement), never as a fourth-plus grant."""
    qwen_bin, node_bin = _layout(tmp_path)
    host = _host(tmp_path, qwen_bin, node_bin)
    assert host["sandbox_modes"] == ["plan", "default", "auto-edit", "auto"]
    refused = host["sandbox_modes_unavailable"]
    assert set(refused) == {"yolo"}
    assert "h15" in refused["yolo"] and "vocabulary" in refused["yolo"]


def test_there_is_no_default_mode_key(tmp_path):
    """The gate never falls back to the agent's measured default — a
    mode-less dispatch is refused before the session. Reporting a default
    would claim a fallback the bridge deliberately never makes, so the
    key is ABSENT (and the confinement prose says where the refusal is)."""
    qwen_bin, node_bin = _layout(tmp_path)
    host = _host(tmp_path, qwen_bin, node_bin)
    assert "default_sandbox_mode" not in host
    assert "refused before the session" in host["confinement"]


# --- the document the engine reads ------------------------------------------


def test_the_surface_is_a_document_the_control_plane_accepts(tmp_path):
    qwen_bin, node_bin = _layout(tmp_path)
    preflight.validate_block(
        preflight.capability_block(_host(tmp_path, qwen_bin, node_bin))
    )


def test_every_supported_mode_grants_everything_the_host_can_do(tmp_path):
    """The in-process trust model, in the grants matrix: qwen-code runs
    its tools as the bridge user with no kernel boundary, so every mode
    the bridge supports can do everything this process can — stated in
    full rather than omitted, because an empty grants map reads as
    'nothing runs here', the opposite of the truth."""
    qwen_bin, node_bin = _layout(tmp_path)
    host = _host(tmp_path, qwen_bin, node_bin)
    for mode in host["sandbox_modes"]:
        assert sorted(host["dispatch_grants"][mode]) == sorted(preflight.GRANTS)


def test_confinement_names_the_in_process_trust_model(tmp_path):
    """The confinement sentence is qwen's own story — in-process tools,
    approval policy, no kernel boundary — not the ported codex bubblewrap
    prose (t3's AC3: the narrative describes what THIS backend does)."""
    qwen_bin, node_bin = _layout(tmp_path)
    prose = _host(tmp_path, qwen_bin, node_bin)["confinement"]
    account = pwd.getpwuid(os.getuid()).pw_name
    assert prose.startswith(f"unix-user:{account}: ")
    assert "in-process" in prose
    assert "approval policy" in prose
    assert "bubblewrap" not in prose


# --- the toolchains ----------------------------------------------------------


def test_the_qwen_version_is_reported_so_a_bump_is_visible(tmp_path):
    """The codex sibling's version test, re-aimed at the qwen CLI: the
    measured 0.22.0 rides the document, and a bumped 0.23.0 is a
    different fact — the surface cannot be stale without saying so."""
    qwen_bin, node_bin = _layout(tmp_path)
    assert _tool(_host(tmp_path, qwen_bin, node_bin, qwen="0.22.0"), "qwen")["version"] == "0.22.0"
    assert _tool(_host(tmp_path, qwen_bin, node_bin, qwen="0.23.0"), "qwen")["version"] == "0.23.0"


def test_the_bundled_node_versions_are_different_facts_per_host(tmp_path):
    """The recorded fleet drift (2026-08-23: v22.23.2 on spark vs
    v18.19.1 on thor; re-measured aligned 2026-08-25) is visible only
    through the layout's bundled runtime — the codex sibling's
    thor-snap/orin-standalone test, re-aimed at the two node versions.
    The PATH node is never the fact, even when it answers."""
    qwen_bin, node_bin = _layout(tmp_path)
    spark = _tool(_host(tmp_path, qwen_bin, node_bin, node="v22.23.2"), "node")
    thor = _tool(_host(tmp_path, qwen_bin, node_bin, node="v18.19.1"), "node")
    assert spark["version"] != thor["version"]
    assert spark["path"] == thor["path"] == str(node_bin)
    assert spark["on_path"] is False


def test_the_qwen_toolchain_is_the_layout_binary_not_a_path_guess(tmp_path):
    """spec s4: the fleet's qwen is absent from the non-interactive PATH
    — the toolchain fact is the located layout binary (on_path False by
    construction: the bridge never resolves it by name), not a which(1)
    answer a dispatch would not get."""
    qwen_bin, node_bin = _layout(tmp_path)
    qwen = _tool(_host(tmp_path, qwen_bin, node_bin), "qwen")
    assert qwen["path"] == str(qwen_bin)
    assert qwen["on_path"] is False


# --- the boot refusal (h5) ----------------------------------------------------


def test_the_missing_binary_is_a_named_refusal_not_a_crash(tmp_path):
    """AC2: a host that cannot locate its qwen binary gets the t2 seam's
    QwenAgentMissingError — naming the configured path, the same refusal
    a dispatch would get — rather than a half-measured surface or a
    traceback."""
    missing = tmp_path / "elsewhere" / "qwen"
    with pytest.raises(errors.QwenAgentMissingError, match="qwen-agent-missing"):
        capabilities.host_facts(Config(repo_allowlist=(str(tmp_path),), qwen_bin=str(missing)))


def test_the_missing_binary_prints_the_named_error_not_a_traceback(
    tmp_path, capsys, monkeypatch
):
    """The operator leg of h5: `--print-capabilities` on a host whose qwen
    install is missing exits 2 with the named refusal (the same message a
    dispatch would get — the remedy, not a traceback)."""
    missing = tmp_path / "elsewhere" / "qwen"
    monkeypatch.setattr(
        qwen_main.Config,
        "load",
        staticmethod(lambda _path=None: Config(qwen_bin=str(missing))),
    )
    assert qwen_main.main(["--print-capabilities"]) == 2
    assert "qwen-agent-missing" in capsys.readouterr().err


# --- the host facts the codex template kept ----------------------------------


def test_writable_paths_are_the_repo_allowlist(tmp_path):
    qwen_bin, node_bin = _layout(tmp_path)
    host = capabilities.host_facts(Config(repo_allowlist=(str(tmp_path),), qwen_bin=str(qwen_bin)))
    assert host["writable_paths"] == [str(tmp_path)]


def test_an_unconfigured_allowlist_states_that_it_writes_nowhere(tmp_path):
    qwen_bin, node_bin = _layout(tmp_path)
    host = capabilities.host_facts(Config(qwen_bin=str(qwen_bin)))
    assert host["writable_paths"] == []


def test_commit_policy_reflects_the_preserve_configuration_in_force(tmp_path):
    qwen_bin, node_bin = _layout(tmp_path)
    base = Config(qwen_bin=str(qwen_bin))
    assert "a technically failed dispatch leaves its changes not preserved" not in (
        capabilities.host_facts(base, version=lambda _p: "x")["commit_policy"]
    )
    local = Config(qwen_bin=str(qwen_bin), preserve_push=False)
    policy = capabilities.host_facts(local, version=lambda _p: "x")["commit_policy"]
    assert "kept local to this host and never pushed" in policy
    pushed = Config(qwen_bin=str(qwen_bin), preserve_on_failure=False)
    assert (
        "a technically failed dispatch leaves its changes not preserved"
        in capabilities.host_facts(pushed, version=lambda _p: "x")["commit_policy"]
    )


# --- git_metadata_writable (issue #94) ---------------------------------------


def test_the_git_answer_is_measured_with_the_process_authority(tmp_path):
    """The qwen difference from the codex sibling, in the default: this
    backend's sessions run with THIS process's own authority (in-process
    tools, no confinement), so the process's answer IS the dispatch's
    answer and the default probe measures it — `supported` on a checkout
    it can write. The codex sibling confines its sessions more tightly
    than its process, so its honest default is `not-probed` there and a
    measurement here; a dispatch-authority probe passed in still wins,
    both ways round."""
    repo = tmp_path / "checkout"
    (repo / ".git").mkdir(parents=True)
    qwen_bin, node_bin = _layout(tmp_path)
    cfg = Config(repo_allowlist=(str(repo),), qwen_bin=str(qwen_bin))

    host = capabilities.host_facts(cfg, version=lambda _p: "x")
    assert host["git_metadata_writable"] == preflight.GIT_METADATA_SUPPORTED

    refused = capabilities.host_facts(
        cfg, version=lambda _p: "x", git_probe=lambda _git_dir: False
    )
    assert refused["git_metadata_writable"] == preflight.GIT_METADATA_UNSUPPORTED_BY_SANDBOX


# --- the qwen section (deviation d1) ----------------------------------------


def _qwen_probe_run(qwen_bin: Path, node_bin: Path):
    lines = {str(qwen_bin): "0.22.0", str(node_bin): "v22.23.2"}

    class _Completed:
        returncode = 0

        def __init__(self, argv0: str) -> None:
            self.stdout = (lines.get(argv0) or "").encode() + b"\n"
            if argv0 not in lines:
                self.returncode = 1

    def run(argv, **kwargs):
        return _Completed(argv[0])

    return run


def test_the_registration_document_carries_the_shared_block_and_the_qwen_section(
    tmp_path,
):
    """AC1, as adjudicated by deviation d1: the document the registration
    carries reports the qwen version, the bundled node version, the model
    identity, the config source (the settings path — no values), and the
    supported session modes — parsed from the measured fixtures, with the
    shared block untouched and still engine-valid."""
    qwen_bin, node_bin = _layout(tmp_path)
    document = capabilities.registration_capabilities(
        Config(qwen_bin=str(qwen_bin)),
        run=_qwen_probe_run(qwen_bin, node_bin),
        settings=SETTINGS_FIXTURE,
        session=({"protocolVersion": 1}, SESSION_FIXTURE),
    )
    preflight.validate_block(document)  # the shared block, engine shape
    section = document["qwen"]
    expected = _fixture("host-facts-spark.json")["section"]
    assert section["qwen_version"] == expected["qwen_version"]
    assert section["node_version"] == expected["node_version"]
    assert section["model_identity"] == expected["model_identity"]
    assert section["supported_modes"] == expected["supported_modes"]
    # the host-absolute fields are rebuilt from the tmp layout here; the
    # recorded fixtures hold the fleet's real values (measured 2026-08-25)
    assert section["node_path"] == str(node_bin)
    assert section["config_source"].endswith(".qwen/settings.json")
    assert "context_budget" not in section  # the 2026-08-23 shape: null


def test_the_qwen_section_refuses_an_unagreed_key(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    with pytest.raises(preflight.SurfaceError, match="unagreed qwen section fact"):
        capabilities.qwen_section(
            Config(qwen_bin=str(qwen_bin)),
            facts={"qwen_version": "0.22.0", "a_guess": "made-up"},
        )


def test_the_section_omits_what_it_could_not_measure(tmp_path):
    """No session measured, no settings found, a version line that will
    not say, a binary outside the measured layout (no bundled runtime to
    derive): the section is what WAS measured — nothing else, no nulls
    standing in for gaps, no guesses."""
    stray = tmp_path / "qwen"
    stray.write_text("x", encoding="utf-8")
    stray.chmod(0o755)  # an explicit qwen_bin must be executable to be honored

    class _Silent:
        returncode = 1
        stdout = b""

    section = capabilities.qwen_section(
        Config(qwen_bin=str(stray)),
        run=lambda argv, **kwargs: _Silent(),
        settings={},
        session=None,
    )
    assert section == {}


def test_the_section_carries_no_settings_values(tmp_path):
    """h17, at the section: the recorded settings document's baseUrl and
    API-key value never reach the registration document — names and
    paths only."""
    qwen_bin, node_bin = _layout(tmp_path)
    document = capabilities.registration_capabilities(
        Config(qwen_bin=str(qwen_bin)),
        run=_qwen_probe_run(qwen_bin, node_bin),
        settings=SETTINGS_FIXTURE,
        session=({"protocolVersion": 1}, SESSION_FIXTURE),
    )
    rendered = json.dumps(document)
    assert SETTINGS_FIXTURE["model"]["baseUrl"] not in rendered
    assert SETTINGS_FIXTURE["env"]["QWEN_CUSTOM_API_KEY_PLACEHOLDER"] not in rendered


# --- the pre-start path -------------------------------------------------------


def test_print_capabilities_emits_the_registration_document(tmp_path, capsys, monkeypatch):
    """The operator's path: `--print-capabilities` before the bridge has
    ever served emits the WHOLE registration document — the shared block
    plus the qwen section — so a registration built from it carries both,
    and the engine still accepts the shared block verbatim. The scratch
    session is injected (the committed fixture), so no agent spawns."""
    qwen_bin, node_bin = _layout(tmp_path)
    monkeypatch.setattr(
        qwen_probe, "scratch_session", lambda cfg: ({"protocolVersion": 1}, SESSION_FIXTURE)
    )
    monkeypatch.setattr(
        qwen_main.Config,
        "load",
        staticmethod(lambda _path=None: Config(qwen_bin=str(qwen_bin))),
    )
    assert qwen_main.main(["--print-capabilities"]) == 0
    printed = json.loads(capsys.readouterr().out)
    preflight.validate_block(printed)
    assert printed["qwen"]["model_identity"] == SESSION_FIXTURE["models"]["currentModelId"]
    assert printed["qwen"]["supported_modes"] == ["plan", "default", "auto-edit", "auto"]
