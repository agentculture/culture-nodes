"""The unauthenticated-exposure startup guard (qodo PR #11 finding 4).

A bridge executes work on its host: binding beyond loopback with no auth
token must refuse at startup rather than serve an unauthenticated
remote-execution surface.
"""

import pytest

from claude_code_bridge import server as server_mod
from claude_code_bridge.config import Config


def _cfg(**overrides):
    base = {"repo_allowlist": ("/tmp",), "host": "127.0.0.1", "auth_token": ""}
    base.update(overrides)
    return Config(**base)


def test_non_loopback_without_token_refuses_startup(monkeypatch):
    monkeypatch.delenv("CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED", raising=False)
    with pytest.raises(SystemExit) as excinfo:
        server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0"))
    assert "refusing to bind" in str(excinfo.value)


def test_loopback_without_token_is_allowed():
    server_mod._refuse_unauthenticated_exposure(_cfg(host="127.0.0.1"))


def test_non_loopback_with_token_is_allowed():
    server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0", auth_token="t0ken"))


def test_explicit_opt_in_env_allows_unauthenticated_exposure(monkeypatch):
    monkeypatch.setenv("CLAUDE_CODE_BRIDGE_ALLOW_UNAUTHENTICATED", "1")
    server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0"))
