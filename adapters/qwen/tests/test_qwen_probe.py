"""The host-local qwen measurement (plan t3).

The qwen section of the registration document reports what the host
MEASURES — the qwen and bundled-node versions, the model identity, the
config source, the agent's mode exposure, the context budget — and these
tests replay the fleet's recorded shapes (tests/fixtures/qwen_probe/ +
the committed acp fixtures) rather than whichever host runs pytest. The
h17 bar runs through everything: names and paths, never values.
"""

from __future__ import annotations

import io
import json
from pathlib import Path

import pytest

from qwen_bridge import qwen_probe
from qwen_bridge.acp import errors
from qwen_bridge.config import Config

HERE = Path(__file__).parent
PROBE_FIXTURES = HERE / "fixtures" / "qwen_probe"
ACP_FIXTURES = HERE / "fixtures" / "acp"


def _fixture(name: str) -> dict:
    return json.loads((PROBE_FIXTURES / name).read_text(encoding="utf-8"))


def _acp_fixture(name: str) -> dict:
    return json.loads((ACP_FIXTURES / name).read_text(encoding="utf-8"))


def _fake_run(lines: dict[str, str]):
    """A run() stand-in keyed by argv[0]: the recorded version line, or an
    exit-1 when the binary did not say."""

    class _Completed:
        returncode = 0

        def __init__(self, argv0: str) -> None:
            self.stdout = (lines.get(argv0) or "").encode() + b"\n"
            if argv0 not in lines:
                self.returncode = 1

    def run(argv, **kwargs):
        return _Completed(argv[0])

    return run


def _layout(tmp_path: Path) -> tuple[Path, Path]:
    """A fake install layout: <root>/bin/qwen + <root>/node/bin/node, the
    measured shape the probe derives its node runtime from."""
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


def _cfg(qwen_bin: Path) -> Config:
    # An explicit qwen_bin naming a PATH (a separator) is honored as-is by
    # the t2 locator — the tests' layout path is such an operator choice.
    return Config(qwen_bin=str(qwen_bin))


# --- versions -------------------------------------------------------------


def test_the_qwen_version_line_is_reported_bare(tmp_path):
    """Measured 2026-08-25: `qwen --version` answers the bare version —
    the surface reports it verbatim, so a bump is visible (the codex
    sibling's version test, re-aimed at the qwen CLI)."""
    qwen_bin, _ = _layout(tmp_path)
    version = qwen_probe.qwen_version(str(qwen_bin), run=_fake_run({str(qwen_bin): "0.22.0"}))
    assert version == "0.22.0"


def test_a_version_that_will_not_say_is_none_not_invented(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    assert qwen_probe.qwen_version(str(qwen_bin), run=_fake_run({})) is None


def test_the_node_is_the_bundled_layout_runtime_not_the_path_one(tmp_path):
    """The launcher execs <layout>/node/bin/node — the host's PATH node is
    a different fact the probe never consults, and the recorded fleet
    drift (2026-08-23: v22.23.2 on spark vs v18.19.1 on thor) is visible
    only through this path. A run() that answers for the layout runtime
    and for nothing else is the assertion: a probe that looked anywhere
    else would get None."""
    qwen_bin, node_bin = _layout(tmp_path)
    assert qwen_probe.node_runtime(str(qwen_bin)) == node_bin
    assert (
        qwen_probe.node_version(str(qwen_bin), run=_fake_run({str(node_bin): "v22.23.2"}))
        == "v22.23.2"
    )


def test_an_unmeasured_layout_refuses_to_invent_a_runtime(tmp_path):
    """A binary outside the measured <root>/bin layout carries no derived
    node runtime: the honest report is absence, and the derivation itself
    names the shape it refused rather than guessing."""
    stray = tmp_path / "qwen"
    stray.write_text("x", encoding="utf-8")
    assert qwen_probe.node_runtime(str(stray)) is None
    with pytest.raises(ValueError, match="not under the measured install layout"):
        qwen_probe.qwen_root(str(stray))


# --- model identity + config source (h17) ---------------------------------


def test_the_model_identity_is_the_agents_wire_answer():
    """session/new models.currentModelId — the committed fleet shape — is
    the identity fact, verbatim (suffixed forms ride through unchanged)."""
    result = _acp_fixture("session_new_measured.json")["response"]["result"]
    assert qwen_probe.wire_model_id(result) == "unsloth/Qwen3.8-27B-NVFP4"


def test_the_wire_answer_beats_the_settings_file(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    result = _acp_fixture("session_new_measured.json")["response"]["result"]
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin),
        run=_fake_run({str(qwen_bin): "0.22.0"}),
        settings={"model": {"name": "a/different/model"}},
        session=({"protocolVersion": 1}, result),
    )
    assert facts["model_identity"] == "unsloth/Qwen3.8-27B-NVFP4"


def test_the_settings_fallback_carries_no_values(tmp_path):
    """h17, at the probe: the model NAME rides the surface; the baseUrl
    value and the API-key value do not — not in the identity, not in the
    config source, nowhere in the measured document."""
    qwen_bin, _ = _layout(tmp_path)
    fixture = _fixture("settings-host.json")
    document = fixture["settings"]
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin),
        run=_fake_run({str(qwen_bin): "0.22.0"}),
        settings=document,
        home=tmp_path,
    )
    assert facts["model_identity"] == "unsloth/Qwen3.8-27B-NVFP4"
    rendered = json.dumps(facts)
    assert document["model"]["baseUrl"] not in rendered
    assert document["env"]["QWEN_CUSTOM_API_KEY_PLACEHOLDER"] not in rendered


def test_the_config_source_is_the_settings_path(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    document = _fixture("settings-host.json")["settings"]
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin),
        run=_fake_run({str(qwen_bin): "0.22.0"}),
        settings=document,
        home=tmp_path,
    )
    assert facts["config_source"] == str(tmp_path / ".qwen" / "settings.json")


def test_the_config_source_is_the_env_name_not_its_value(tmp_path):
    """A host whose model comes from the bridge's qwen_env is reported by
    the VARIABLE NAME — its value is the secret, and a source that only
    exists as a value is no source at all."""
    qwen_bin, _ = _layout(tmp_path)
    facts = qwen_probe.qwen_facts(
        Config(qwen_bin=str(qwen_bin), qwen_env={"QWEN_CODE_MODEL": "a/model/value"}),
        run=_fake_run({str(qwen_bin): "0.22.0"}),
        settings={},
        home=tmp_path,
    )
    assert facts["config_source"] == "QWEN_CODE_MODEL"
    assert "a/model/value" not in json.dumps(facts)


def test_a_host_with_no_measured_source_omits_it(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin), run=_fake_run({str(qwen_bin): "0.22.0"}), settings={}, home=tmp_path
    )
    assert facts["config_source"] is None
    assert facts["model_identity"] is None


# --- modes + context budget ------------------------------------------------


def test_the_modes_are_the_agents_own_order():
    """availableModes ids in the agent's order — the four-mode scratch
    shape the committed fixture records."""
    result = _acp_fixture("session_new_measured.json")["response"]["result"]
    assert qwen_probe.modes_available(result) == ("plan", "default", "auto-edit", "auto")


def test_yolo_is_in_the_supported_agent_mode_vocabulary(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    result = _fixture("session-new-with-yolo.json")["response"]["result"]
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin),
        run=_fake_run({str(qwen_bin): "0.22.0"}),
        settings={},
        home=tmp_path,
        session=({"protocolVersion": 1}, result),
    )
    assert facts["supported_modes"] == ("plan", "default", "auto-edit", "auto", "yolo")
    assert facts["modes_refused"] is None


def test_the_context_budget_is_none_where_the_agent_exposes_none():
    """The committed 2026-08-23 replay shape declares
    availableModels[].contextWindowSize and measures it null — the honest
    report is None (the section omits it), never a guessed window size."""
    result = _acp_fixture("session_new_measured.json")["response"]["result"]
    assert qwen_probe.context_budget(result) is None


def test_a_declared_budget_is_reported_from_the_measured_field():
    """The 2026-08-25 fresh-session shape measured the budget under
    _meta.contextLimit — both declared fields are honored, whichever the
    agent uses."""
    result = _fixture("session-new-with-yolo.json")["response"]["result"]
    assert qwen_probe.context_budget(result) == 262144


# --- the boot refusal + the live probe --------------------------------------


def test_the_missing_binary_is_the_named_seam_refusal(tmp_path):
    """h5, at the probe: a host that cannot locate its qwen binary gets
    the t2 seam's QwenAgentMissingError — the same refusal a dispatch
    would get, naming the configured path — never a crash and never a
    surface built on a guessed install."""
    missing = tmp_path / "elsewhere" / "qwen"
    with pytest.raises(errors.QwenAgentMissingError, match="qwen-agent-missing"):
        qwen_probe.locate(Config(qwen_bin=str(missing)))


def test_the_scratch_session_probe_degrades_to_none_when_the_agent_will_not_run(tmp_path):
    """A spawn that refuses is a degraded measurement, not an error: the
    section omits the session-measured facts and the registration still
    reports the host it could measure."""
    qwen_bin, _ = _layout(tmp_path)

    def _refused(argv, **kwargs):
        raise OSError("spawn refused")

    assert qwen_probe.scratch_session(_cfg(qwen_bin), popen=_refused) is None


class _EofPopen:
    """A Popen stand-in whose agent answers nothing — immediate EOF is the
    other failure shape the probe must degrade through."""

    def __init__(self, argv, **kwargs) -> None:
        self.stdin = io.StringIO()
        self.stdout = io.StringIO()
        self.returncode = 0

    def terminate(self) -> None:
        pass

    def wait(self, timeout=None) -> int:
        return 0

    def kill(self) -> None:
        pass


def test_the_scratch_session_probe_degrades_to_none_on_an_eof_agent(tmp_path):
    qwen_bin, _ = _layout(tmp_path)
    assert qwen_probe.scratch_session(_cfg(qwen_bin), popen=_EofPopen) is None


# --- the full recorded host shape ------------------------------------------


def test_qwen_facts_replays_the_recorded_fleet_shape(tmp_path):
    """The section's raw measurements against the recorded shapes: the
    four-mode session fixture (the committed fleet measurement), the
    settings-host document, and the version lines — the host-facts
    fixtures' node_path/config_source are host-absolute, so they are
    rebuilt here from the tmp layout; every other field must equal the
    recorded value field for field."""
    qwen_bin, node_bin = _layout(tmp_path)
    result = _acp_fixture("session_new_measured.json")["response"]["result"]
    document = _fixture("settings-host.json")["settings"]
    facts = qwen_probe.qwen_facts(
        _cfg(qwen_bin),
        run=_fake_run({str(qwen_bin): "0.22.0", str(node_bin): "v22.23.2"}),
        settings=document,
        home=tmp_path,
        session=({"protocolVersion": 1}, result),
    )
    expected = _fixture("host-facts-spark.json")["section"]
    assert facts["qwen_version"] == expected["qwen_version"]
    assert facts["node_version"] == expected["node_version"]
    assert facts["node_path"] == str(node_bin)
    assert facts["model_identity"] == expected["model_identity"]
    assert facts["config_source"] == str(tmp_path / ".qwen" / "settings.json")
    assert facts["supported_modes"] == tuple(expected["supported_modes"])
    assert facts["modes_refused"] is None
    assert facts["context_budget"] is None
