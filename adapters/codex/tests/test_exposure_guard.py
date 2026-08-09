"""The unauthenticated-exposure startup guard (qodo PR #11 finding 4)."""

import pytest
from codex_bridge import server as server_mod
from codex_bridge.config import Config


def _cfg(**overrides):
    base = {"repo_allowlist": ("/tmp",), "host": "127.0.0.1", "auth_token": None}
    base.update(overrides)
    return Config(**base)


def test_non_loopback_without_token_refuses_startup(monkeypatch):
    monkeypatch.delenv("CODEX_BRIDGE_ALLOW_UNAUTHENTICATED", raising=False)
    with pytest.raises(SystemExit) as excinfo:
        server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0"))
    assert "refusing to bind" in str(excinfo.value)


def test_loopback_without_token_is_allowed():
    server_mod._refuse_unauthenticated_exposure(_cfg(host="127.0.0.1"))


def test_non_loopback_with_token_is_allowed():
    server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0", auth_token="t0ken"))


def test_explicit_opt_in_env_allows_unauthenticated_exposure(monkeypatch):
    monkeypatch.setenv("CODEX_BRIDGE_ALLOW_UNAUTHENTICATED", "1")
    server_mod._refuse_unauthenticated_exposure(_cfg(host="0.0.0.0"))
