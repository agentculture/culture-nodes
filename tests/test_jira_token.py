"""Tests for ``nodes jira-token`` (mint | verify | install) — task t11, issue #273.

The verify tests stub ``urllib.request.urlopen`` so no test ever talks to
Atlassian; the token-leak test runs every verb in every mode with a
recognisable fake token in the environment and asserts it never reaches
stdout or stderr.
"""

from __future__ import annotations

import io
import json
import urllib.error

import pytest

from culture_nodes.cli import main
from culture_nodes.cli._commands import jira_token
from culture_nodes.explain import known_paths

FAKE_TOKEN = "FAKE-TOKEN-ATATT3xFfGF0-do-not-print"  # noqa: S105 - test fixture
FAKE_EMAIL = "culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com"


class _FakeResponse(io.BytesIO):
    """Just enough of an HTTPResponse for ``with urlopen(...) as r: r.read()``."""

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def _set_env(monkeypatch: pytest.MonkeyPatch, base: str = jira_token.GATEWAY_BASE) -> None:
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", FAKE_EMAIL)
    monkeypatch.setenv("JIRA_API_TOKEN", FAKE_TOKEN)
    monkeypatch.setenv("JIRA_API_BASE", base)


def _stub_urlopen(monkeypatch: pytest.MonkeyPatch, *, status: int = 200, body: bytes = b""):
    """Replace urlopen; record the request so tests can inspect URL/headers."""
    seen: dict[str, object] = {}

    def fake_urlopen(request, timeout=None):  # noqa: ANN001
        seen["url"] = request.full_url
        seen["auth"] = request.get_header("Authorization")
        seen["timeout"] = timeout
        if status != 200:
            raise urllib.error.HTTPError(request.full_url, status, "nope", {}, None)
        return _FakeResponse(body)

    monkeypatch.setattr(jira_token.urllib.request, "urlopen", fake_urlopen)
    return seen


# --- parser / catalog ------------------------------------------------------


def test_jira_token_registered_in_parser(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "mint" in out and "verify" in out and "install" in out


def test_jira_token_unknown_verb_errors(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc:
        main(["jira-token", "bogus"])
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert err.startswith("error:")
    assert "hint:" in err


def test_jira_token_catalog_paths_present(capsys: pytest.CaptureFixture[str]) -> None:
    paths = set(known_paths())
    for path in (
        ("jira-token",),
        ("jira-token", "mint"),
        ("jira-token", "verify"),
        ("jira-token", "install"),
    ):
        assert path in paths
        assert main(["explain", *path]) == 0
        assert "api.atlassian.com/ex/jira" in capsys.readouterr().out


# --- mint ------------------------------------------------------------------


def test_mint_text_names_admin_path_and_gateway(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("JIRA_ACCOUNT_EMAIL", raising=False)
    monkeypatch.delenv("JIRA_API_TOKEN", raising=False)
    monkeypatch.delenv("JIRA_API_BASE", raising=False)
    rc = main(["jira-token", "mint"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "admin.atlassian.com -> Directory -> Service accounts -> culture-nodes" in out
    assert jira_token.GATEWAY_BASE in out
    assert jira_token.SERVICE_ACCOUNT_ID in out
    assert "answers 401" in out
    assert "jira-service-account.env" in out
    assert "docs/operations/jira-service-account.md" in out


def test_mint_json_shape(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "mint", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["account_id"] == jira_token.SERVICE_ACCOUNT_ID
    assert payload["api_base"] == jira_token.GATEWAY_BASE
    assert payload["mints_via_api"] is False
    assert payload["env_keys"] == ["JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN", "JIRA_API_BASE"]


# --- verify ----------------------------------------------------------------


def test_verify_200_prints_account_id(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch)
    seen = _stub_urlopen(
        monkeypatch, body=json.dumps({"accountId": jira_token.SERVICE_ACCOUNT_ID}).encode()
    )
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 0
    assert captured.out == f"accountId: {jira_token.SERVICE_ACCOUNT_ID}\n"
    assert captured.err == ""
    assert seen["url"] == jira_token.GATEWAY_BASE + "/rest/api/3/myself"
    assert str(seen["auth"]).startswith("Basic ")
    assert seen["timeout"] is not None and float(seen["timeout"]) <= 30


def test_verify_json_shape(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch, base=jira_token.GATEWAY_BASE + "/")
    _stub_urlopen(monkeypatch, body=json.dumps({"accountId": "712020:abc"}).encode())
    rc = main(["jira-token", "verify", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload == {
        "account_id": "712020:abc",
        "email": FAKE_EMAIL,
        "api_base": jira_token.GATEWAY_BASE,
    }


@pytest.mark.parametrize("status", [401, 403])
def test_verify_unauthorized_exits_2_and_names_gateway(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch, status: int
) -> None:
    _set_env(monkeypatch, base="https://agentculture.atlassian.net")
    _stub_urlopen(monkeypatch, status=status)
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.out == ""
    assert captured.err.startswith("error: https://agentculture.atlassian.net/rest/api/3/myself")
    assert f"HTTP {status}" in captured.err
    assert "hint:" in captured.err
    assert "site URL" in captured.err and "401" in captured.err
    assert jira_token.GATEWAY_BASE in captured.err


def test_verify_other_http_error_exits_2(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, status=503)
    assert main(["jira-token", "verify"]) == 2
    assert "HTTP 503" in capsys.readouterr().err


def test_verify_network_error_exits_2(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch)

    def boom(request, timeout=None):  # noqa: ANN001
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(jira_token.urllib.request, "urlopen", boom)
    rc = main(["jira-token", "verify", "--json"])
    assert rc == 2
    captured = capsys.readouterr()
    assert captured.out == ""
    payload = json.loads(captured.err)
    assert payload["code"] == 2
    assert "could not reach" in payload["message"]


def test_verify_200_without_account_id_exits_2(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, body=b"<html>login</html>")
    assert main(["jira-token", "verify"]) == 2
    assert "non-JSON" in capsys.readouterr().err


def test_verify_missing_env_exits_2_naming_the_missing_names(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", FAKE_EMAIL)
    monkeypatch.delenv("JIRA_API_TOKEN", raising=False)
    monkeypatch.delenv("JIRA_API_BASE", raising=False)
    calls: list[object] = []
    monkeypatch.setattr(
        jira_token.urllib.request, "urlopen", lambda *a, **k: calls.append(a)  # noqa: ARG005
    )
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 2
    assert calls == []
    assert captured.err.startswith("error: missing environment variable(s): ")
    assert "JIRA_API_TOKEN" in captured.err and "JIRA_API_BASE" in captured.err
    assert "JIRA_ACCOUNT_EMAIL" not in captured.err.splitlines()[0]
    assert "hint:" in captured.err


def test_verify_non_https_base_is_user_error(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch, base="http://api.atlassian.com/ex/jira/x")
    calls: list[object] = []
    monkeypatch.setattr(
        jira_token.urllib.request, "urlopen", lambda *a, **k: calls.append(a)  # noqa: ARG005
    )
    assert main(["jira-token", "verify"]) == 1
    assert calls == []
    err = capsys.readouterr().err
    assert "must be an https:// URL" in err
    assert "hint:" in err


# --- install ---------------------------------------------------------------


def test_install_lists_the_five_steps(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "install"])
    assert rc == 0
    out = capsys.readouterr().out
    for n in range(1, 6):
        assert f"\n{n}. " in out
    assert "6. " not in out
    assert "nodes jira-token verify" in out
    assert "deploy/prod/install-secrets.sh" in out
    assert "pgrep -af '[j]ira'" in out
    assert "deploy/prod/deploy.sh thor" in out
    assert "deploy/prod/lanes/runner-env-write.sh" in out
    assert f'"jira_bot_account_id":"{jira_token.SERVICE_ACCOUNT_ID}"' in out
    assert "systemctl --user restart nodes-runner" in out
    assert "self-echo" in out
    assert "rotation:" in out
    # The order is the contract: verify before install-secrets before deploy
    # before the re-grant.
    assert (
        out.index("nodes jira-token verify")
        < out.index("install-secrets.sh")
        < out.index("pgrep")
        < out.index("deploy.sh thor")
        < out.index("runner-env-write.sh")
        < out.index("restart nodes-runner")
    )


def test_install_json_shape(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "install", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert [step["n"] for step in payload["steps"]] == [1, 2, 3, 4, 5]
    assert all(step["commands"] for step in payload["steps"])
    assert payload["steps"][1]["commands"] == ["nodes jira-token verify"]
    assert payload["doc"] == "docs/operations/jira-service-account.md"


# --- the token never leaks ---------------------------------------------------


@pytest.mark.parametrize(
    "argv",
    [
        ["jira-token"],
        ["jira-token", "mint"],
        ["jira-token", "mint", "--json"],
        ["jira-token", "install"],
        ["jira-token", "install", "--json"],
        ["jira-token", "verify"],
        ["jira-token", "verify", "--json"],
        ["jira-token", "bogus"],
        ["explain", "jira-token"],
    ],
)
@pytest.mark.parametrize("status", [200, 401, 500])
def test_token_never_appears_in_any_output(
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
    argv: list[str],
    status: int,
) -> None:
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, status=status, body=b'{"accountId":"712020:abc"}')
    try:
        main(argv)
    except SystemExit:
        pass
    captured = capsys.readouterr()
    assert FAKE_TOKEN not in captured.out
    assert FAKE_TOKEN not in captured.err
    # Nor its Basic-auth encoding.
    import base64

    encoded = base64.b64encode(f"{FAKE_EMAIL}:{FAKE_TOKEN}".encode()).decode()
    assert encoded not in captured.out
    assert encoded not in captured.err
