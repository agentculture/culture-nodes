from pathlib import Path

from pi_bridge import capabilities
from pi_bridge.config import Config

ROOT = Path(__file__).parents[2]


def test_four_shared_modules_are_byte_identical():
    for name in ("preflight.py", "dialin.py", "deployment.py", "reap.py"):
        assert (ROOT / "pi/src/pi_bridge" / name).read_bytes() == (
            ROOT / "qwen/src/qwen_bridge" / name
        ).read_bytes()


def test_confinement_names_unix_user_and_pi_limitations():
    text = capabilities._confinement()
    assert text.startswith("unix-user:")
    assert "no sandbox" in text and "no tool-approval prompt" in text


def test_config_exposes_pi_fields(tmp_path):
    cfg = Config(
        pi_bin="/x/pi",
        pi_env={"PATH": "/x"},
        provider="p",
        model="m",
        state_dir=str(tmp_path),
        repo_allowlist=(str(tmp_path),),
    )
    assert cfg.repo_allowed(str(tmp_path))
    assert cfg.pi_bin == "/x/pi" and cfg.provider == "p" and cfg.model == "m"


def test_no_acp_package_exists():
    assert not (ROOT / "pi/src/pi_bridge/acp").exists()
