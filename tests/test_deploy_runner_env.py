"""Behavioral coverage for deploy.sh's runner.env replacement (issue #191)."""

from __future__ import annotations

import os
from pathlib import Path
import subprocess


ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "deploy/prod/deploy.sh"
START = "# RUNNER_ENV_WRITE_START"
END = "# RUNNER_ENV_WRITE_END"


def _env_write_block() -> str:
    script = DEPLOY.read_text()
    start = script.index(START)
    end = script.index(END, start) + len(END)
    return script[start:end]


def _fake_ssh(tmp_path: Path) -> Path:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)
    shim = bin_dir / "ssh"
    shim.write_text(
        "#!/usr/bin/env bash\n"
        "shift\n"
        'exec env HOME="$FAKE_HOST_HOME" bash -c "$*"\n'
    )
    shim.chmod(0o755)
    return bin_dir


def _run_block(tmp_path: Path, *, shell_api_url: str | None = None) -> subprocess.CompletedProcess:
    host_home = tmp_path / "host"
    host_home.mkdir(exist_ok=True)
    bin_dir = _fake_ssh(tmp_path)
    env = {
        "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        "HOME": str(tmp_path / "operator"),
        "FAKE_HOST_HOME": str(host_home),
        "HOST": "fake-host",
        "REVISION": "0123456789abcdef0123456789abcdef01234567",
        "PR_UPKEEP_SWEEP_SOURCE_URL": "https://example.test/sweep.py",
        "PR_UPKEEP_SWEEP_SOURCE_SHA256": "a" * 64,
        "PR_UPKEEP_SWEEP_JIRA_SOURCE_URL": "https://example.test/jira.py",
        "PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256": "b" * 64,
    }
    if shell_api_url is not None:
        env["NODES_API_URL"] = shell_api_url
    snippet = "set -euo pipefail\nsay() { :; }\n" + _env_write_block()
    return subprocess.run(  # nosec B603 - fixed bash and extracted repository script
        ["bash", "-c", snippet],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def test_existing_api_url_and_jira_repositories_survive_two_writes_byte_exact(tmp_path: Path):
    runner_dir = tmp_path / "host/.culture-nodes"
    runner_dir.mkdir(parents=True)
    api_line = "NODES_API_URL=http://192.0.2.44:18080/path?mode=exact"
    repositories_line = (
        "PR_UPKEEP_REPOSITORIES='"
        '{"cycle":7,"repositories":[{"github_repo":"agentculture/culture-nodes",'
        '"sonar_component":"agentculture_culture-nodes","jira_site":"acme.example",'
        '"jira_project":"NODE"}]}'
        "'"
    )
    runner_env = runner_dir / "runner.env"
    runner_env.write_text(f"OLD_KEY=discarded\n{api_line}\n{repositories_line}\n")

    for _ in range(2):
        result = _run_block(tmp_path)
        assert result.returncode == 0, result.stderr
        lines = runner_env.read_bytes().splitlines()
        assert lines.count(api_line.encode()) == 1
        assert lines.count(repositories_line.encode()) == 1


def test_missing_nodes_api_url_refuses_before_touching_runner_env(tmp_path: Path):
    runner_dir = tmp_path / "host/.culture-nodes"
    runner_dir.mkdir(parents=True)
    original = (
        b"KEEP=this-file-byte-exact\n"
        b"PR_UPKEEP_REPOSITORIES='{\"cycle\":0,\"repositories\":[{\"jira_site\":\"acme.example\","
        b"\"jira_project\":\"NODE\"}]}'\n"
    )
    runner_env = runner_dir / "runner.env"
    runner_env.write_bytes(original)

    result = _run_block(tmp_path)

    assert result.returncode != 0
    assert "NODES_API_URL" in result.stderr
    assert runner_env.read_bytes() == original
