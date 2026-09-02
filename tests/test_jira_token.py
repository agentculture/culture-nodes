"""Tests for ``nodes jira-token`` (mint | seal | verify | install) — task t11, issue #273.

The verify tests stub ``urllib.request.urlopen`` so no test ever talks to
Atlassian; the seal tests stub ``subprocess.run`` and ``shutil.which`` so no
test ever touches a real grant store. The token-leak test runs every verb in
every mode with a recognisable fake token and asserts it never reaches
stdout or stderr — including the seal path and the getpass fallback.
"""

from __future__ import annotations

import base64
import io
import json
import subprocess
import urllib.error
from pathlib import Path

import pytest

from culture_nodes.cli import main
from culture_nodes.cli._commands import jira_token
from culture_nodes.explain import known_paths

FAKE_TOKEN = "FAKE-TOKEN-ATATT3xFfGF0-do-not-print"  # noqa: S105 - test fixture
FAKE_EMAIL = "culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com"
FAKE_GRANT = "/fake/bin/grant"
INJECT = "grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN --"
#: What /myself answers for the service account — the only identity
#: `verify` accepts, so every success-path stub returns exactly this.
_MYSELF_BODY = json.dumps({"accountId": jira_token.SERVICE_ACCOUNT_ID}).encode()


class _FakeResponse(io.BytesIO):
    """Just enough of an HTTPResponse for ``with urlopen(...) as r: r.read()``."""

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def _clear_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in ("JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN", "JIRA_API_BASE"):
        monkeypatch.delenv(name, raising=False)


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


def _stub_stdin(monkeypatch: pytest.MonkeyPatch, text: str, *, tty: bool) -> None:
    """A stdin that is (or is not) a TTY and yields ``text``."""

    class _Stdin(io.StringIO):
        def isatty(self) -> bool:
            return tty

    monkeypatch.setattr(jira_token.sys, "stdin", _Stdin(text))


def _stub_grant(
    monkeypatch: pytest.MonkeyPatch,
    *,
    present: bool = True,
    returncode: int = 0,
    stderr: str = "",
) -> list[dict[str, object]]:
    """Replace shutil.which + subprocess.run; record every call."""
    calls: list[dict[str, object]] = []
    monkeypatch.setattr(jira_token.shutil, "which", lambda name: FAKE_GRANT if present else None)

    def fake_run(argv, **kwargs):  # noqa: ANN001
        calls.append({"argv": list(argv), **kwargs})
        return subprocess.CompletedProcess(argv, returncode, stdout="", stderr=stderr)

    monkeypatch.setattr(jira_token.subprocess, "run", fake_run)
    return calls


# --- parser / catalog ------------------------------------------------------


def test_jira_token_registered_in_parser(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token"])
    assert rc == 0
    out = capsys.readouterr().out
    for verb in ("mint", "seal", "verify", "install"):
        assert verb in out


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
        ("jira-token", "seal"),
        ("jira-token", "verify"),
        ("jira-token", "install"),
    ):
        assert path in paths
        assert main(["explain", *path]) == 0
        out = capsys.readouterr().out
        assert "api.atlassian.com/ex/jira" in out
        assert "jira-service-account.env" not in out
        assert INJECT in out


def test_secret_name_is_a_valid_grant_name() -> None:
    # grant 0.9.0 on spark / 0.11.0 on thor: ^[A-Z_][A-Z0-9_]{0,63}$ — verified 2026-09-02.
    import re

    assert re.fullmatch(r"[A-Z_][A-Z0-9_]{0,63}", jira_token.SECRET_NAME)
    assert jira_token.GRANT_NAME_RE.pattern == r"[A-Z_][A-Z0-9_]{0,63}"
    assert not jira_token.GRANT_NAME_RE.fullmatch("jira-service-account-token")


# --- mint ------------------------------------------------------------------


def test_mint_text_names_admin_path_gateway_and_seal(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _clear_env(monkeypatch)
    rc = main(["jira-token", "mint"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "admin.atlassian.com -> Directory -> Service accounts -> culture-nodes" in out
    assert jira_token.GATEWAY_BASE in out
    assert jira_token.SERVICE_ACCOUNT_ID in out
    assert "answers 401" in out
    assert "nodes jira-token seal" in out
    assert f"{INJECT} <cmd>" in out
    assert "jira-service-account.env" not in out
    assert "docs/operations/jira-service-account.md" in out


def test_mint_json_shape(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "mint", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["account_id"] == jira_token.SERVICE_ACCOUNT_ID
    assert payload["api_base"] == jira_token.GATEWAY_BASE
    assert payload["mints_via_api"] is False
    assert payload["secret_store"] == "grant"
    assert payload["secret_name"] == "JIRA_SERVICE_ACCOUNT_TOKEN"
    assert payload["inject"] == f"{INJECT} <cmd>"
    assert payload["env_keys"] == ["JIRA_ACCOUNT_EMAIL", "JIRA_API_TOKEN", "JIRA_API_BASE"]
    assert "env_file" not in payload
    assert "env_file_mode" not in payload


# --- seal ------------------------------------------------------------------


def test_seal_pipes_token_on_stdin_never_in_argv(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, FAKE_TOKEN + "\n", tty=False)
    calls = _stub_grant(monkeypatch)
    rc = main(["jira-token", "seal"])
    captured = capsys.readouterr()
    assert rc == 0
    assert captured.err == ""
    assert captured.out.splitlines()[0] == "sealed: JIRA_SERVICE_ACCOUNT_TOKEN (hidden)"
    assert f"{INJECT} nodes jira-token verify" in captured.out
    assert len(calls) == 1
    call = calls[0]
    argv = call["argv"]
    assert argv[:5] == [FAKE_GRANT, "set", "JIRA_SERVICE_ACCOUNT_TOKEN", "-", "--hidden"]
    assert "--purpose" in argv
    assert "--rotate-howto" in argv
    assert call["input"] == FAKE_TOKEN
    assert FAKE_TOKEN not in " ".join(argv)
    assert call.get("shell", False) is False


def test_seal_json_shape(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, FAKE_TOKEN, tty=False)
    _stub_grant(monkeypatch)
    assert main(["jira-token", "seal", "--json"]) == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload == {
        "sealed": "JIRA_SERVICE_ACCOUNT_TOKEN",
        "hidden": True,
        "secret_store": "grant",
        "next": f"{INJECT} nodes jira-token verify",
    }


def test_seal_uses_getpass_on_a_tty(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, "NOT-READ-FROM-STDIN\n", tty=True)
    prompts: list[str] = []

    def fake_getpass(prompt: str = "") -> str:
        prompts.append(prompt)
        return FAKE_TOKEN

    monkeypatch.setattr(jira_token.getpass, "getpass", fake_getpass)
    calls = _stub_grant(monkeypatch)
    assert main(["jira-token", "seal"]) == 0
    assert prompts
    assert "no echo" in prompts[0]
    assert calls[0]["input"] == FAKE_TOKEN
    assert "NOT-READ-FROM-STDIN" not in capsys.readouterr().out


def test_seal_empty_token_is_user_error(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, "\n", tty=False)
    calls = _stub_grant(monkeypatch)
    rc = main(["jira-token", "seal"])
    captured = capsys.readouterr()
    assert rc == 1
    assert calls == []
    assert captured.out == ""
    assert captured.err.startswith("error: empty token")
    assert "hint:" in captured.err


def test_seal_grant_missing_is_env_error(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, FAKE_TOKEN, tty=False)
    calls = _stub_grant(monkeypatch, present=False)
    rc = main(["jira-token", "seal"])
    captured = capsys.readouterr()
    assert rc == 2
    assert calls == []
    assert "grant (the secrets manager) is not on PATH" in captured.err
    assert "hint:" in captured.err
    assert "uv tool install grant" in captured.err


def test_seal_grant_failure_quotes_scrubbed_stderr(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _stub_stdin(monkeypatch, FAKE_TOKEN, tty=False)
    _stub_grant(monkeypatch, returncode=64, stderr=f"grant: error: invalid name; got {FAKE_TOKEN}")
    rc = main(["jira-token", "seal", "--json"])
    captured = capsys.readouterr()
    assert rc == 2
    payload = json.loads(captured.err)
    assert payload["code"] == 2
    assert "grant set exited 64" in payload["message"]
    assert "invalid name" in payload["message"]
    assert "<redacted>" in payload["message"]
    assert FAKE_TOKEN not in captured.err


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
    # Order is load-bearing: float(None) raises, so the presence check guards the bound.
    assert seen["timeout"] is not None
    assert float(seen["timeout"]) <= 30


@pytest.mark.parametrize("other", ["712020:9999aaaa-0000-1111-2222-333344445555", "5f0a1b2c3d"])
def test_verify_other_account_exits_2_and_names_both_ids(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch, other: str
) -> None:
    """A valid token for the wrong Jira account must not pass verification.

    It authenticates fine — the gateway answers 200 — and every later step
    would install it as the bot credential, which the sweep then fails to
    recognise as its own.
    """
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, body=json.dumps({"accountId": other}).encode())
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.out == ""
    assert other in captured.err
    assert jira_token.SERVICE_ACCOUNT_ID in captured.err
    assert "hint:" in captured.err
    assert FAKE_TOKEN not in captured.err


def test_verify_other_account_json_error_is_not_a_result(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, body=json.dumps({"accountId": "712020:not-ours"}).encode())
    rc = main(["jira-token", "verify", "--json"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.out == ""
    payload = json.loads(captured.err)
    assert payload["code"] == 2
    assert "712020:not-ours" in payload["message"]
    assert jira_token.SERVICE_ACCOUNT_ID in payload["message"]


def test_verify_defaults_email_and_base_from_constants(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _clear_env(monkeypatch)
    monkeypatch.setenv("JIRA_API_TOKEN", FAKE_TOKEN)
    _stub_stdin(monkeypatch, "", tty=False)
    seen = _stub_urlopen(monkeypatch, body=_MYSELF_BODY)
    rc = main(["jira-token", "verify", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload == {
        "account_id": jira_token.SERVICE_ACCOUNT_ID,
        "email": jira_token.SERVICE_ACCOUNT_EMAIL,
        "api_base": jira_token.GATEWAY_BASE,
    }
    assert seen["url"] == jira_token.GATEWAY_BASE + "/rest/api/3/myself"
    expected = base64.b64encode(
        f"{jira_token.SERVICE_ACCOUNT_EMAIL}:{FAKE_TOKEN}".encode()
    ).decode()
    assert seen["auth"] == f"Basic {expected}"


def test_verify_json_shape(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _set_env(monkeypatch, base=jira_token.GATEWAY_BASE + "/")
    _stub_urlopen(monkeypatch, body=_MYSELF_BODY)
    rc = main(["jira-token", "verify", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload == {
        "account_id": jira_token.SERVICE_ACCOUNT_ID,
        "email": FAKE_EMAIL,
        "api_base": jira_token.GATEWAY_BASE,
    }


@pytest.mark.parametrize("status", [401, 403])
def test_verify_unauthorized_exits_2_and_names_gateway(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch, status: int
) -> None:
    _set_env(monkeypatch)
    _stub_urlopen(monkeypatch, status=status)
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 2
    assert captured.out == ""
    assert captured.err.startswith(f"error: {jira_token.GATEWAY_BASE}/rest/api/3/myself")
    assert f"HTTP {status}" in captured.err
    assert "hint:" in captured.err
    assert "site URL" in captured.err
    assert "401" in captured.err
    assert "re-mint" in captured.err
    assert "re-seal" in captured.err
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


def test_verify_no_token_non_tty_exits_2_with_grant_run_hint(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _clear_env(monkeypatch)
    _stub_stdin(monkeypatch, "", tty=False)
    monkeypatch.setattr(
        jira_token.getpass, "getpass", lambda *a, **k: pytest.fail("getpass on a non-TTY")
    )
    calls: list[object] = []
    monkeypatch.setattr(
        jira_token.urllib.request, "urlopen", lambda *a, **k: calls.append(a)  # noqa: ARG005
    )
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 2
    assert calls == []
    assert captured.err.startswith("error: JIRA_API_TOKEN is not set")
    assert f"hint: {INJECT} nodes jira-token verify" in captured.err


def test_verify_prompts_with_getpass_on_a_tty(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    _clear_env(monkeypatch)
    _stub_stdin(monkeypatch, "", tty=True)
    prompts: list[str] = []

    def fake_getpass(prompt: str = "") -> str:
        prompts.append(prompt)
        return FAKE_TOKEN

    monkeypatch.setattr(jira_token.getpass, "getpass", fake_getpass)
    seen = _stub_urlopen(monkeypatch, body=_MYSELF_BODY)
    rc = main(["jira-token", "verify"])
    captured = capsys.readouterr()
    assert rc == 0
    assert prompts
    assert "no echo" in prompts[0]
    assert captured.out == f"accountId: {jira_token.SERVICE_ACCOUNT_ID}\n"
    expected = base64.b64encode(
        f"{jira_token.SERVICE_ACCOUNT_EMAIL}:{FAKE_TOKEN}".encode()
    ).decode()
    assert seen["auth"] == f"Basic {expected}"


@pytest.mark.parametrize(
    "base",
    [
        # The scheme is the least of it: every one of these would have been an
        # https:// URL good enough for the old prefix check, and every one of
        # them is a host that would have received the Basic credential.
        "http://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c",
        "https://evil.example.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c",
        "https://api.atlassian.com.evil.example.com/ex/jira/0610b05c",
        "https://api.atlassian.com@evil.example.com/ex/jira/0610b05c",
        "https://api.atlassian.com/ex/jira/00000000-0000-0000-0000-000000000000",
        "https://agentculture.atlassian.net",
    ],
)
def test_verify_refuses_any_base_but_the_gateway(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch, base: str
) -> None:
    _set_env(monkeypatch, base=base)
    calls: list[object] = []
    monkeypatch.setattr(
        jira_token.urllib.request, "urlopen", lambda *a, **k: calls.append(a)  # noqa: ARG005
    )
    assert main(["jira-token", "verify"]) == 1
    # The point is not the exit code: no request was built, so the token was
    # never encoded into an Authorization header for that host.
    assert calls == []
    err = capsys.readouterr().err
    assert "must be the gateway base" in err
    assert jira_token.GATEWAY_BASE in err
    assert "hint:" in err
    assert FAKE_TOKEN not in err


def test_verify_refuses_the_base_before_asking_for_a_token(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    """A base that will be refused never gets as far as collecting the secret."""
    _clear_env(monkeypatch)
    monkeypatch.setenv("JIRA_API_BASE", "https://evil.example.com/ex/jira/x")
    monkeypatch.setattr(
        jira_token.getpass,
        "getpass",
        lambda *a, **k: pytest.fail("prompted for the token for a refused base"),  # noqa: ARG005
    )
    _stub_stdin(monkeypatch, "", tty=True)
    assert main(["jira-token", "verify"]) == 1
    assert "must be the gateway base" in capsys.readouterr().err


@pytest.mark.parametrize(
    "base",
    [
        jira_token.GATEWAY_BASE,
        jira_token.GATEWAY_BASE + "/",
        jira_token.GATEWAY_BASE.replace("api.atlassian.com", "API.Atlassian.Com"),
    ],
)
def test_verify_accepts_the_gateway_however_it_is_spelled(
    monkeypatch: pytest.MonkeyPatch, base: str
) -> None:
    _set_env(monkeypatch, base=base)
    seen = _stub_urlopen(monkeypatch, body=_MYSELF_BODY)
    assert main(["jira-token", "verify"]) == 0
    # However it was spelled, one canonical address is what gets requested.
    assert seen["url"] == jira_token.GATEWAY_BASE + "/rest/api/3/myself"


# --- install ---------------------------------------------------------------


def test_install_lists_the_five_steps(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "install"])
    assert rc == 0
    out = capsys.readouterr().out
    for n in range(1, 6):
        assert f"\n{n}. " in out
    assert "6. " not in out
    assert "nodes jira-token seal" in out
    assert f"{INJECT} nodes jira-token verify" in out
    assert f"{INJECT} deploy/prod/install-secrets.sh" in out
    assert "pgrep -af '[j]ira'" in out
    assert f"{INJECT} deploy/prod/deploy.sh thor" in out
    assert "deploy/prod/lanes/runner-env-write.sh" in out
    assert f'"jira_bot_account_id":"{jira_token.SERVICE_ACCOUNT_ID}"' in out
    assert "systemctl --user restart nodes-runner" in out
    assert "hand edit on thor" in out
    assert "self-echo" in out
    assert "rotation:" in out
    assert "repeat steps 2-5" in out
    # The order is the contract: seal before verify before install-secrets
    # before deploy before the re-grant.
    assert (
        out.index("nodes jira-token seal")
        < out.index("nodes jira-token verify")
        < out.index("install-secrets.sh")
        < out.index("pgrep")
        < out.index("deploy.sh thor")
        < out.index("runner-env-write.sh")
        < out.index("restart nodes-runner")
    )


def test_install_step_four_exports_the_site_before_deploy(
    capsys: pytest.CaptureFixture[str],
) -> None:
    """deploy_jira returns at its unset-JIRA_SITE guard, silently and with 0.

    A step 4 that only injects the token runs a deploy that says one line
    mid-log and merges nothing -- the bridge is never reconfigured and never
    restarted, and the operator has no failure to notice.
    """
    rc = main(["jira-token", "install"])
    assert rc == 0
    out = capsys.readouterr().out
    export = f"export JIRA_SITE={jira_token.SITE_HOST}"
    assert export in out
    assert out.index(export) < out.index("deploy.sh thor")
    # A bare host: the bridge refuses a scheme outright, and a path with it.
    assert "://" not in jira_token.SITE_HOST
    assert "/" not in jira_token.SITE_HOST
    assert jira_token.SITE_URL.endswith(jira_token.SITE_HOST)


def test_deploy_jira_still_guards_on_the_key_step_four_exports() -> None:
    """The other half of the coupling, so neither side drifts alone.

    If the guard is ever removed or renamed, this fails next to the step
    that exists only to satisfy it.
    """
    deploy_sh = (Path(__file__).resolve().parents[1] / "deploy/prod/deploy.sh").read_text(
        encoding="utf-8"
    )
    body = deploy_sh.split("deploy_jira() {", 1)[1].split("\n}\n", 1)[0]
    assert f'if [ -z "${{{jira_token.ENV_SITE}:-}}" ]; then' in body
    assert "return 0" in body


def test_install_no_step_sources_a_file() -> None:
    for _title, commands, note in jira_token.INSTALL_STEPS:
        for line in (*commands, note):
            assert "jira-service-account.env" not in line
            assert "set -a" not in line
            assert not line.lstrip().startswith(". ")


def test_install_json_shape(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["jira-token", "install", "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert [step["n"] for step in payload["steps"]] == [1, 2, 3, 4, 5]
    assert all(step["commands"] for step in payload["steps"])
    assert payload["steps"][0]["commands"] == ["nodes jira-token seal"]
    assert payload["steps"][1]["commands"] == [f"{INJECT} nodes jira-token verify"]
    assert payload["steps"][2]["commands"][0].startswith("export JIRA_ACCOUNT_EMAIL=")
    assert payload["secret_store"] == "grant"
    assert payload["secret_name"] == "JIRA_SERVICE_ACCOUNT_TOKEN"
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
        ["jira-token", "seal"],
        ["jira-token", "seal", "--json"],
        ["jira-token", "bogus"],
        ["explain", "jira-token"],
    ],
)
@pytest.mark.parametrize("status", [200, 401, 500])
@pytest.mark.parametrize("token_source", ["env", "getpass", "stdin"])
@pytest.mark.parametrize("grant_rc", [0, 64])
def test_token_never_appears_in_any_output(
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
    argv: list[str],
    status: int,
    token_source: str,
    grant_rc: int,
) -> None:
    _clear_env(monkeypatch)
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", FAKE_EMAIL)
    monkeypatch.setenv("JIRA_API_BASE", jira_token.GATEWAY_BASE)
    if token_source == "env":
        monkeypatch.setenv("JIRA_API_TOKEN", FAKE_TOKEN)
        _stub_stdin(monkeypatch, FAKE_TOKEN + "\n", tty=False)
    elif token_source == "getpass":
        _stub_stdin(monkeypatch, "", tty=True)
    else:
        _stub_stdin(monkeypatch, FAKE_TOKEN + "\n", tty=False)
    monkeypatch.setattr(jira_token.getpass, "getpass", lambda *a, **k: FAKE_TOKEN)
    _stub_urlopen(monkeypatch, status=status, body=_MYSELF_BODY)
    _stub_grant(monkeypatch, returncode=grant_rc, stderr=f"grant: boom {FAKE_TOKEN}")
    try:
        main(argv)
    except SystemExit:
        pass
    captured = capsys.readouterr()
    assert FAKE_TOKEN not in captured.out
    assert FAKE_TOKEN not in captured.err
    # Nor its Basic-auth encoding.
    for email in (FAKE_EMAIL, jira_token.SERVICE_ACCOUNT_EMAIL):
        encoded = base64.b64encode(f"{email}:{FAKE_TOKEN}".encode()).decode()
        assert encoded not in captured.out
        assert encoded not in captured.err
