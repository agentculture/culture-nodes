"""The ACP child reads a rotated token on every launch."""

import os

from qwen_bridge import pushcred
from qwen_bridge.acp import driver


def test_fresh_environment_and_session_reload(tmp_path, monkeypatch):
    token_file = tmp_path / "github-token.env"
    monkeypatch.setenv("CULTURE_NODES_GITHUB_TOKEN_FILE", str(token_file))
    monkeypatch.setenv("GITHUB_TOKEN_WORKER", "old")
    original = dict(os.environ)
    base = {"GITHUB_TOKEN_WORKER": "fallback", "GH_TOKEN": "old-gh"}
    assert pushcred.fresh_environ(base) == base
    token_file.write_text('# comment\nUNKNOWN=ignore\nGITHUB_TOKEN_WORKER="first"\n')
    assert pushcred.fresh_environ(base) == {"GITHUB_TOKEN_WORKER": "first", "GH_TOKEN": "first"}
    captured = []

    def fake_popen(*args, **kwargs):
        captured.append(kwargs["env"])
        raise OSError("fake agent")

    monkeypatch.setattr(driver.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(driver.signal, "signal", lambda *_: None)
    instance = driver._Driver(
        qwen_bin="qwen",
        cwd=str(tmp_path),
        instruction="test",
        mode=None,
        model=None,
        sandbox=None,
        state_dir=str(tmp_path),
        run_id="test",
        qwen_env=None,
    )
    assert instance.run() != 0
    token_file.write_text("GITHUB_TOKEN_WORKER='second'\n")
    assert instance.run() != 0
    assert [env["GH_TOKEN"] for env in captured] == ["first", "second"]
    assert os.environ == original
