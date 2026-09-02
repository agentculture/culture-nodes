"""Behavioral coverage for deploy.sh's runner.env replacement (issue #191)."""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "deploy/prod/deploy.sh"
RUNNER_ENV_LANE = ROOT / "deploy/prod/lanes/runner-env-write.sh"
START = "# RUNNER_ENV_WRITE_START"
END = "# RUNNER_ENV_WRITE_END"


def _env_write_block() -> str:
    script = RUNNER_ENV_LANE.read_text()
    start = script.index(START)
    end = script.index(END, start) + len(END)
    return script[start:end]


def test_deploy_sources_the_real_runner_env_lane():
    source = 'source "$SCRIPT_DIR/lanes/runner-env-write.sh"'
    assert source in DEPLOY.read_text()


def _fake_ssh(tmp_path: Path) -> Path:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)
    shim = bin_dir / "ssh"
    shim.write_text(
        "#!/usr/bin/env bash\n" "shift\n" 'exec env HOME="$FAKE_HOST_HOME" bash -c "$*"\n'
    )
    shim.chmod(0o755)
    return bin_dir


def _lane_env(tmp_path: Path, *, shell_api_url: str | None = None) -> dict[str, str]:
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
        "PR_UPKEEP_SWEEP_EMIT_SOURCE_URL": "https://example.test/emit.py",
        "PR_UPKEEP_SWEEP_EMIT_SOURCE_SHA256": "c" * 64,
    }
    if shell_api_url is not None:
        env["NODES_API_URL"] = shell_api_url
    return env


def _run_block(tmp_path: Path, *, shell_api_url: str | None = None) -> subprocess.CompletedProcess:
    env = _lane_env(tmp_path, shell_api_url=shell_api_url)
    # deploy.sh has the timestamped-backup helper in scope by the time it
    # sources this lane (task t5, issue #253), the same way it has `say` —
    # sourced here from the real file rather than stubbed, so this harness
    # exercises the backup the lane actually takes.
    snippet = (
        "set -euo pipefail\nsay() { :; }\n"
        f'. "{ROOT / "deploy/prod/lanes/env-backup.sh"}"\n' + _env_write_block()
    )
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
        b'PR_UPKEEP_REPOSITORIES=\'{"cycle":0,"repositories":[{"jira_site":"acme.example",'
        b'"jira_project":"NODE"}]}\'\n'
    )
    runner_env = runner_dir / "runner.env"
    runner_env.write_bytes(original)

    result = _run_block(tmp_path)

    assert result.returncode != 0
    assert "NODES_API_URL" in result.stderr
    assert runner_env.read_bytes() == original


# A FIRST deploy (no runner.env on the host at all) is a different state from
# a runner.env that lost a line, and t1's refusal must not block it (t1's
# colleague review). The well-known jira-less repository default is granted;
# NODES_API_URL is still required from the shell, with a hint.


def test_first_deploy_grants_the_jira_less_repositories_default(tmp_path: Path):
    runner_env = tmp_path / "host/.culture-nodes/runner.env"
    assert not runner_env.exists()

    result = _run_block(tmp_path, shell_api_url="http://192.0.2.44:18080")

    assert result.returncode == 0, result.stderr
    lines = runner_env.read_bytes().splitlines()
    assert b"NODES_API_URL=http://192.0.2.44:18080" in lines
    repositories = [line for line in lines if line.startswith(b"PR_UPKEEP_REPOSITORIES=")]
    assert len(repositories) == 1
    assert b'"github_repo":"agentculture/culture-nodes"' in repositories[0]
    assert b"jira_site" not in repositories[0]


def test_first_deploy_without_shell_api_url_refuses_with_a_hint(tmp_path: Path):
    runner_env = tmp_path / "host/.culture-nodes/runner.env"

    result = _run_block(tmp_path)

    assert result.returncode != 0
    assert "first deploy" in result.stderr
    assert "hint:" in result.stderr
    assert not runner_env.exists()


def test_redeploy_never_reintroduces_the_default_over_a_missing_jira_line(tmp_path: Path):
    # An EXISTING runner.env without PR_UPKEEP_REPOSITORIES is a lost grant,
    # not a first deploy: the default must not paper over it.
    runner_dir = tmp_path / "host/.culture-nodes"
    runner_dir.mkdir(parents=True)
    original = b"NODES_API_URL=http://192.0.2.44:18080\n"
    runner_env = runner_dir / "runner.env"
    runner_env.write_bytes(original)

    result = _run_block(tmp_path)

    assert result.returncode != 0
    assert "PR_UPKEEP_REPOSITORIES" in result.stderr
    assert runner_env.read_bytes() == original


def test_runner_env_paths_are_absolute_on_the_target(tmp_path):
    """A literal $HOME in runner.env is a dead runner (t7 on thor, 2026-08-29).

    The file is written by `cat` on the target and read by systemd's
    EnvironmentFile; neither expands anything, so the target's home must be
    resolved by the deploy and written as an absolute path.
    """
    result = _run_block(tmp_path, shell_api_url="http://api.test")
    assert result.returncode == 0, result.stderr
    written = (tmp_path / "host" / ".culture-nodes" / "runner.env").read_text()
    assert "$HOME" not in written, written
    assert (
        f"NODES_RUNNER_SECRET_FILE={tmp_path / 'host'}/.culture-nodes/runner.secret" in written
    ), written


# --- the standalone entry (PR #282 review, Qodo "Standalone lane exits with
# --- failure") ---------------------------------------------------------------
#
# Step 5 of docs/operations/jira-service-account.md re-grants runner.env
# WITHOUT a deploy: `bash deploy/prod/lanes/runner-env-write.sh`. The whole
# file runs, not the block above, and deploy.sh's `say` and backup_env_file
# are not in scope — so the lane has to supply them itself. It did not, and
# the documented step wrote the file and then exited 127.


def _run_lane_standalone(
    tmp_path: Path, *, shell_api_url: str | None = None
) -> subprocess.CompletedProcess:
    return subprocess.run(  # nosec B603 - fixed bash and a repository script path
        ["bash", str(RUNNER_ENV_LANE)],
        env=_lane_env(tmp_path, shell_api_url=shell_api_url),
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )


def test_standalone_lane_succeeds_and_takes_the_backup(tmp_path: Path):
    runner_dir = tmp_path / "host/.culture-nodes"
    runner_dir.mkdir(parents=True)
    original = b"NODES_API_URL=http://192.0.2.44:18080\nPR_UPKEEP_REPOSITORIES='{\"cycle\":3}'\n"
    (runner_dir / "runner.env").write_bytes(original)

    result = _run_lane_standalone(tmp_path)

    assert result.returncode == 0, result.stderr
    assert "command not found" not in result.stderr, result.stderr
    # The backup is the point of the exercise, not just the exit code: a
    # `command not found` at backup_env_file left the replaced bytes nowhere.
    backups = list(runner_dir.glob("runner.env.bak-*"))
    assert len(backups) == 1, sorted(p.name for p in runner_dir.iterdir())
    assert backups[0].read_bytes() == original
    assert "restore it with:" in result.stdout


def test_standalone_lane_still_refuses_a_missing_grant(tmp_path: Path):
    # `set -euo pipefail` is supplied to the standalone run too, so a refusal
    # reads the same either way — a nonzero exit has to keep MEANING refused.
    runner_dir = tmp_path / "host/.culture-nodes"
    runner_dir.mkdir(parents=True)
    original = b"KEEP=this-file-byte-exact\n"
    (runner_dir / "runner.env").write_bytes(original)

    result = _run_lane_standalone(tmp_path)

    assert result.returncode != 0
    assert "refusing:" in result.stderr
    assert (runner_dir / "runner.env").read_bytes() == original


def test_deploy_sh_keeps_its_own_helpers_when_it_sources_the_lane(tmp_path: Path):
    # The guards are on absence: a caller that already defines `say` must keep
    # its own, or deploy.sh's log formatting silently becomes the lane's.
    marker = tmp_path / "said"
    snippet = (
        "set -euo pipefail\n"
        f'say() {{ printf "%s\\n" "$*" >> "{marker}"; }}\n'
        "backup_env_file() { :; }\n"
        f'. "{RUNNER_ENV_LANE}"\n'
    )
    result = subprocess.run(  # nosec B603 - fixed bash and extracted repository script
        ["bash", "-c", snippet],
        env=_lane_env(tmp_path, shell_api_url="http://api.test"),
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert marker.exists() and "granted the pr-upkeep sweep source" in marker.read_text()
