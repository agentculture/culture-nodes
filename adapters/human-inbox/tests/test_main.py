"""The human-facing CLI subcommands (`list`, `submit`) against a live
bridge: a thin urllib client over the same HTTP surface the tests above
prove directly."""

from __future__ import annotations

import json

import pytest

from human_inbox_bridge import __main__ as cli
from human_inbox_bridge import server
from human_inbox_bridge.config import Config

from ._fakes import FakeCallbackReceiver
from .test_server_unit import _invoke


@pytest.fixture()
def live(tmp_path):
    cfg = Config(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        callback_timeout_seconds=5.0,
        callback_max_retries=1,
        callback_retry_backoff_seconds=0.01,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    receiver = FakeCallbackReceiver()
    yield f"http://{host}:{port}", receiver
    receiver.close()
    srv.shutdown()
    srv.server_close()


def test_list_shows_pending_tasks(live, capsys):
    base, receiver = live
    _, body = _invoke(base, receiver)
    rc = cli.main(["list", "--url", base, "--token", "s3cr3t"])
    assert rc == 0
    out = capsys.readouterr().out
    assert body["invocation_id"] in out
    assert "approve the release" in out


def test_submit_completes_the_task(live, capsys):
    base, receiver = live
    _, body = _invoke(base, receiver)
    invocation_id = body["invocation_id"]
    rc = cli.main(
        [
            "submit",
            invocation_id,
            "--url",
            base,
            "--token",
            "s3cr3t",
            "--outcome",
            "approved",
            "--output",
            json.dumps({"verdict": "ship"}),
            "--note",
            "cli submission",
        ]
    )
    assert rc == 0
    ev = receiver.wait_for_kind("completed", timeout=10.0)
    assert ev is not None
    assert ev["payload"]["outcome"] == "approved"
    assert ev["payload"]["output"] == {"verdict": "ship"}


def test_submit_rejects_invalid_output_json(live, capsys):
    base, _receiver = live
    rc = cli.main(
        [
            "submit",
            "hit_x",
            "--url",
            base,
            "--token",
            "s3cr3t",
            "--outcome",
            "ok",
            "--output",
            "{not json",
        ]
    )
    assert rc == 2
