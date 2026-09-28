"""The minter is exercised with local curl and ssh shims only."""

import json
import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MINTER = ROOT / "deploy/prod/github-app-token.sh"
TOKEN = "fake-installation-token"


def _executable(path, text):
    path.write_text("#!/usr/bin/env bash\n" + text)
    path.chmod(0o700)


def _run(tmp_path, *, curl_fails=False, ssh_fails=""):
    bins = tmp_path / "bin"
    bins.mkdir()
    _executable(
        bins / "curl",
        """
printf '%s\\n' "$*" > "$FAKE_CURL_ARGS"
while (($#)); do
  if [[ "$1" == --data ]]; then shift; printf '%s' "$1" > "$FAKE_POST_BODY"; fi
  shift
done
[[ "$FAKE_CURL_FAIL" == 1 ]] && exit 22
printf '{"token":"fake-installation-token","expires_at":"2026-09-28T12:00:00Z"}'
""",
    )
    _executable(
        bins / "ssh",
        """
account=${1%@localhost}
printf '%s' "$2" > "$FAKE_OUTPUT_DIR/$account-command"
cat > "$FAKE_OUTPUT_DIR/$account"
[[ "$account" == "$FAKE_SSH_FAIL" ]] && exit 1
exit 0
""",
    )
    key = subprocess.run(
        ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    env = dict(
        os.environ,
        PATH=f"{bins}:{os.environ['PATH']}",
        GITHUB_APP_PRIVATE_KEY=key,
        GITHUB_APP_TOKEN_ACCOUNTS="alice bob",
        FAKE_OUTPUT_DIR=str(tmp_path),
        FAKE_CURL_ARGS=str(tmp_path / "curl-args"),
        FAKE_POST_BODY=str(tmp_path / "post-body"),
        FAKE_CURL_FAIL="1" if curl_fails else "0",
        FAKE_SSH_FAIL=ssh_fails,
    )
    proc = subprocess.run(["bash", str(MINTER)], env=env, capture_output=True, text=True)
    return proc, key


def test_mints_scoped_token_and_delivers_exact_two_lines(tmp_path):
    proc, key = _run(tmp_path)
    assert proc.returncode == 0, proc.stderr
    assert json.loads((tmp_path / "post-body").read_text())["permissions"] == {
        "contents": "write",
        "pull_requests": "write",
        "issues": "write",
        "metadata": "read",
        "checks": "read",
        "statuses": "read",
        "actions": "read",
    }
    assert "/app/installations/165818005/access_tokens" in (tmp_path / "curl-args").read_text()
    expected = f"GITHUB_TOKEN_WORKER={TOKEN}\nGITHUB_TOKEN_EXPIRES_AT=2026-09-28T12:00:00Z\n"
    assert (tmp_path / "alice").read_text() == expected
    assert (tmp_path / "bob").read_text() == expected
    command = (tmp_path / "alice-command").read_text()
    assert "umask 077" in command
    assert "chmod 600" in command
    assert "mv -f" in command
    assert TOKEN not in proc.stdout + proc.stderr
    assert key not in proc.stdout + proc.stderr


def test_curl_failure_stops_before_delivery(tmp_path):
    proc, _ = _run(tmp_path, curl_fails=True)
    assert proc.returncode != 0
    assert not (tmp_path / "alice").exists()


def test_one_ssh_failure_still_attempts_other_account(tmp_path):
    proc, _ = _run(tmp_path, ssh_fails="alice")
    assert proc.returncode != 0
    assert (tmp_path / "bob").exists()
    assert TOKEN not in proc.stdout + proc.stderr
