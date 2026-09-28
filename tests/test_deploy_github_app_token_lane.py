"""Spark's installer uses local shims; no service or account is touched."""

import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
LANE = ROOT / "deploy/prod/lanes/github-app-token.sh"


def _write(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!/usr/bin/env bash\n" + body)
    path.chmod(0o700)


def _run(tmp_path, *, grant=True, grant_fails=False):
    home = tmp_path / "home"
    home.mkdir()
    bins = tmp_path / "bin"
    bins.mkdir()
    _write(
        bins / "ssh",
        'echo "ssh $1" >> "$FAKE_LOG"; cat > "$FAKE_HELPERS_DIR/${1%@localhost}"\n',
    )
    _write(bins / "systemctl", 'echo "systemctl $*" >> "$FAKE_LOG"\n')
    if grant:
        _write(
            home / ".local/bin/grant",
            'echo "grant $*" >> "$FAKE_LOG"\n' + ("exit 1\n" if grant_fails else ""),
        )
    env = dict(
        os.environ,
        HOME=str(home),
        PATH=f"{bins}:{os.environ['PATH']}",
        FAKE_LOG=str(tmp_path / "calls"),
        FAKE_HELPERS_DIR=str(tmp_path),
    )
    body = f"""set -euo pipefail
SCRIPT_DIR="{ROOT / 'deploy/prod'}"
say() {{ echo "$*"; }}
source "{LANE}"
github_app_token_lane
echo deploy-continues
"""
    return (
        subprocess.run(["bash", "-c", body], env=env, capture_output=True, text=True),
        home,
        tmp_path / "calls",
    )


def test_installs_units_helper_and_starts_timer(tmp_path):
    proc, home, log = _run(tmp_path)
    assert proc.returncode == 0, proc.stderr
    calls = log.read_text()
    assert calls.count("ssh culture-") == 2
    assert "systemctl --user daemon-reload" in calls
    assert "systemctl --user enable --now culture-nodes-github-app-token.timer" in calls
    assert "systemctl --user start culture-nodes-github-app-token.service" in calls
    assert (home / ".culture-nodes/bin/github-app-token.sh").exists()
    assert (home / ".config/systemd/user/culture-nodes-github-app-token.timer").exists()
    helper = (tmp_path / "culture-claude").read_text()
    assert helper == (tmp_path / "culture-qwen").read_text()
    assert 'test "$1" = get' in helper
    assert "~/.culture-nodes/github-token.env" in helper


def test_missing_grant_reports_hint_and_deploy_continues(tmp_path):
    proc, home, log = _run(tmp_path, grant=False)
    assert proc.returncode == 0, proc.stderr
    assert "grant missing" in proc.stdout
    assert "deploy-continues" in proc.stdout
    assert "systemctl --user daemon-reload" in log.read_text()
    assert "systemctl --user enable" not in log.read_text()


def test_missing_private_key_grant_reports_hint_and_deploy_continues(tmp_path):
    proc, _, log = _run(tmp_path, grant_fails=True)
    assert proc.returncode == 0, proc.stderr
    assert "GITHUB_APP_PRIVATE_KEY grant missing" in proc.stdout
    assert "deploy-continues" in proc.stdout
    assert "systemctl --user daemon-reload" in log.read_text()
    assert "systemctl --user enable" not in log.read_text()
