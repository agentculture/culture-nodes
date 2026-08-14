"""Server-level tests: real HTTP over loopback, against a real (fake)
Discord webhook receiver standing in for discord.com. These prove the
issue #68 acceptance set end to end:

* a `notify` node's invocation is answered synchronously and the run
  records a `proposed` claim (`test_successful_send_is_synchronous_and_
  records_the_claim`);
* the webhook URL never appears in any response body or ledger record
  (`test_no_response_ever_carries_the_webhook_url`);
* the default is fail-open: a webhook outage leaves the outcome `sent`,
  the node not failed (`test_webhook_outage_default_settings_stays_sent`);
* `require_delivery: true` turns a failed send into the `delivery_failed`
  domain outcome (`test_require_delivery_true_routes_delivery_failed`),
  while the same outage with the default settings does not
  (`test_webhook_outage_default_settings_stays_sent`);
* an Idempotency-Key replay never re-posts to the webhook
  (`test_idempotent_replay_does_not_repost`).
"""

from __future__ import annotations

import json
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from notify_bridge import server
from notify_bridge.config import Config
from notify_bridge.webhook import ENV_PRIMARY

AUTH = {"Authorization": "Bearer s3cr3t"}


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _config(tmp_path, **overrides):
    fields = dict(state_dir=str(tmp_path / "state"), host="127.0.0.1", port=0, auth_token="s3cr3t")
    fields.update(overrides)
    return Config(**fields)


class _FakeDiscord:
    """A fake webhook receiver: records every POST it gets and answers a
    configurable status code, so tests can prove both the success and the
    outage paths without ever touching a real Discord endpoint."""

    def __init__(self, status_code: int = 204) -> None:
        self.status_code = status_code
        self.requests: list[bytes] = []
        receiver = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):  # noqa: D401 - quiet test output
                pass

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0") or "0")
                body = self.rfile.read(length)
                receiver.requests.append(body)
                self.send_response(receiver.status_code)
                self.send_header("Content-Length", "0")
                self.end_headers()

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address
        # /api/webhooks/ path so this classifies as a Discord URL.
        return f"http://{host}:{port}/api/webhooks/1/faketoken"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


@pytest.fixture()
def discord():
    d = _FakeDiscord()
    yield d
    d.close()


@pytest.fixture()
def bridge(tmp_path):
    cfg = _config(tmp_path)
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg
    srv.shutdown()
    srv.server_close()


def _invocation_body(**input_overrides):
    input_payload = {"content": "the build finished"}
    input_payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": input_payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "", "token": ""},
    }


def _invoke(base, *, idem_key="att_1", **input_overrides):
    return _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(**input_overrides),
        headers={**AUTH, "Idempotency-Key": idem_key},
    )


# -- plumbing ---------------------------------------------------------------


def test_healthz_is_open(bridge):
    base, _cfg = bridge
    status, body = _request(base, "/healthz", method="GET")
    assert status == 200
    assert body == {"status": "ok"}


def test_unknown_route_is_404(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/nope", method="GET")
    assert status == 404


# -- auth ---------------------------------------------------------------------


def test_invocation_without_auth_is_401(bridge):
    base, _cfg = bridge
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=_invocation_body(), headers={"Idempotency-Key": "att_1"}
    )
    assert status == 401
    assert body["class"] == "auth_or_policy"


def test_invocation_with_wrong_token_is_401(bridge):
    base, _cfg = bridge
    status, _ = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(),
        headers={"Authorization": "Bearer wrong", "Idempotency-Key": "att_1"},
    )
    assert status == 401


# -- invocation validation ------------------------------------------------


def test_missing_idempotency_key_is_400(bridge):
    base, _cfg = bridge
    status, body = _request(base, server.INVOCATIONS_PATH, body=_invocation_body(), headers=AUTH)
    assert status == 400
    assert "Idempotency-Key" in body["error"]


def test_wrong_protocol_version_is_400(bridge):
    base, _cfg = bridge
    payload = _invocation_body()
    payload["protocol_version"] = "2.0"
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 400
    assert "protocol_version" in body["error"]


def test_empty_input_is_400(bridge):
    base, _cfg = bridge
    payload = _invocation_body()
    payload["input"] = {}
    status, body = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 400
    assert body["class"] == "actor_rejected_input"


def test_empty_body_is_400(bridge):
    base, _cfg = bridge
    status, _ = _request(
        base,
        server.INVOCATIONS_PATH,
        body=None,
        headers={**AUTH, "Idempotency-Key": "k", "Content-Type": "application/json"},
    )
    assert status == 400


def test_oversized_body_is_413(bridge, monkeypatch):
    base, _cfg = bridge
    monkeypatch.setattr(server, "MAX_BODY_BYTES", 1024)
    payload = _invocation_body(content="x" * 4096)
    status, _ = _request(
        base, server.INVOCATIONS_PATH, body=payload, headers={**AUTH, "Idempotency-Key": "k"}
    )
    assert status == 413


# -- acceptance: synchronous send, proposed claim, dashboard-visible ----------


def test_successful_send_is_synchronous_and_records_the_claim(bridge, discord, monkeypatch):
    base, _cfg = bridge
    monkeypatch.setenv(ENV_PRIMARY, discord.url)

    status, body = _invoke(base, content="the build finished", title="CI")
    assert status == 200  # synchronous InvocationResult, never a 202 park
    assert body["outcome"] == "sent"
    assert body["output"] == {"delivered": True, "status_code": 204}

    record = body["ledger_delta"]["records"][0]
    assert record["authority"] == "proposed"
    assert record["origin"]["kind"] == "agent"
    assert record["run_id"] == "run_1"
    assert record["attempt_id"] == "att_1"
    assert record["data"]["status_code"] == 204
    assert record["data"]["delivered"] is True

    # The message really reached the fake Discord endpoint. The fake runs
    # on a loopback IP, not a discord.com host, so `webhook.is_discord_url`
    # correctly classifies it as non-Discord and the bridge posts the
    # generic flat-JSON shape (payload.py's `_build_generic`) rather than
    # a Discord embed -- proven separately, against a discord.com-hosted
    # URL, in test_payload.py.
    assert len(discord.requests) == 1
    sent = json.loads(discord.requests[0])
    assert sent["title"] == "CI"
    assert sent["content"] == "the build finished"


def test_no_response_ever_carries_the_webhook_url(bridge, discord, monkeypatch):
    monkeypatch.setenv(ENV_PRIMARY, discord.url)
    base, _cfg = bridge
    status, body = _invoke(base)
    assert status == 200
    assert discord.url not in json.dumps(body)


# -- acceptance: fail-open by default -----------------------------------------


def test_webhook_outage_default_settings_stays_sent(bridge, monkeypatch):
    """No webhook configured at all (env unset by the hermetic conftest
    fixture): the node still answers 200 outcome `sent` -- a dead/absent
    webhook must never fail the run."""
    base, _cfg = bridge
    status, body = _invoke(base)
    assert status == 200
    assert body["outcome"] == "sent"
    assert body["output"]["delivered"] is False


def test_webhook_5xx_default_settings_stays_sent(bridge, discord, monkeypatch):
    discord.status_code = 503
    monkeypatch.setenv(ENV_PRIMARY, discord.url)
    base, _cfg = bridge
    status, body = _invoke(base)
    assert status == 200
    assert body["outcome"] == "sent"
    assert body["output"] == {"delivered": False, "status_code": 503}


# -- acceptance: require_delivery routes a domain outcome ---------------------


def test_require_delivery_true_routes_delivery_failed(bridge, discord, monkeypatch):
    discord.status_code = 500
    monkeypatch.setenv(ENV_PRIMARY, discord.url)
    base, _cfg = bridge
    status, body = _invoke(base, require_delivery=True)
    assert status == 200  # still a normal completed answer, not an engine failure
    assert body["outcome"] == "delivery_failed"
    assert body["output"] == {"delivered": False, "status_code": 500}


def test_require_delivery_true_stays_sent_on_success(bridge, discord, monkeypatch):
    monkeypatch.setenv(ENV_PRIMARY, discord.url)
    base, _cfg = bridge
    status, body = _invoke(base, require_delivery=True)
    assert status == 200
    assert body["outcome"] == "sent"


# -- idempotency ---------------------------------------------------------------


def test_idempotent_replay_does_not_repost(bridge, discord, monkeypatch):
    monkeypatch.setenv(ENV_PRIMARY, discord.url)
    base, _cfg = bridge
    status1, first = _invoke(base, idem_key="att_same")
    status2, replay = _invoke(base, idem_key="att_same")
    assert status1 == status2 == 200
    assert replay == first
    assert len(discord.requests) == 1


# -- cancellation ---------------------------------------------------------------


def test_cancel_always_answers_202(bridge):
    base, _cfg = bridge
    status, resp = _request(base, "/v1/invocations/anything/cancel", body={}, headers=AUTH)
    assert status == 202
    assert resp["status"] == "cancel-requested"


def test_delete_alias_cancels(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/v1/invocations/anything", method="DELETE", headers=AUTH)
    assert status == 202


def test_cancel_requires_auth(bridge):
    base, _cfg = bridge
    status, _ = _request(base, "/v1/invocations/anything/cancel", body={})
    assert status == 401


# -- exposure guard -------------------------------------------------------------


def test_non_loopback_bind_without_token_is_refused(tmp_path):
    cfg = _config(tmp_path, auth_token=None, host="0.0.0.0")  # noqa: S104 - the refused case
    with pytest.raises(SystemExit):
        server.make_server(cfg)
