"""A rotated installation token is visible at the next dispatch."""

import os

from pi_bridge import pi_cli, pushcred
from pi_bridge.config import Config


def test_fresh_environment_and_session_reload(tmp_path, monkeypatch):
    token_file = tmp_path / "github-token.env"
    monkeypatch.setenv("CULTURE_NODES_GITHUB_TOKEN_FILE", str(token_file))
    monkeypatch.setenv("GITHUB_TOKEN_WORKER", "old")
    original = dict(os.environ)
    base = {"GITHUB_TOKEN_WORKER": "fallback", "GH_TOKEN": "old-gh"}
    assert pushcred.fresh_environ(base) == base
    token_file.write_text('# comment\nUNKNOWN=ignore\nGITHUB_TOKEN_WORKER="first"\n')
    assert pushcred.fresh_environ(base) == {"GITHUB_TOKEN_WORKER": "first", "GH_TOKEN": "first"}
    cfg = Config()
    assert pi_cli._env(cfg)["GH_TOKEN"] == "first"
    token_file.write_text("GITHUB_TOKEN_WORKER='second'\n")
    assert pi_cli._env(cfg)["GITHUB_TOKEN_WORKER"] == "second"
    assert os.environ == original
