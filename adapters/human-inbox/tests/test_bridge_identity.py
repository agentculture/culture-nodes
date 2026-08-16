"""The bridge's own identity surface, and the store-identity proof under it.

Issue #72's startup guard used to be answered by an ADDRESS: the tracker
compared the actor's registered `endpoint_ref` against the bridge URL it
submits through. Migration 0036 (issue #121) removes that column, so the
bridge has to be able to answer "who am I, and which durable store do I
own" itself. These tests pin that answer — the tracker-side half lives in
test_tracker_identity.py.
"""

from __future__ import annotations

import json
import os
import stat
import urllib.error
import urllib.request

import pytest

from human_inbox_bridge import identity, server
from human_inbox_bridge.config import Config

ACTOR_KEY = "company/human-ops"


def _config(tmp_path, **overrides):
    fields = dict(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        actor_id=ACTOR_KEY,
    )
    fields.update(overrides)
    return Config(**fields)


def _get(base_url, path, *, headers=None):
    req = urllib.request.Request(base_url + path, method="GET", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


@pytest.fixture()
def bridge(tmp_path):
    cfg = _config(tmp_path)
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg
    srv.shutdown()
    srv.server_close()


# -- the store identity itself ---------------------------------------------


def test_store_identity_is_minted_once_and_reread_afterwards(tmp_path):
    """It identifies the STORE, not the process: a restart over the same
    state dir must keep the same value, or every bridge restart would break
    a tracker that is otherwise correctly co-located."""
    state = tmp_path / "state"
    first = identity.ensure_store_identity(state)
    second = identity.ensure_store_identity(state)
    assert first.store_id
    assert first.store_id == second.store_id
    assert identity.read_store_identity(state).store_id == first.store_id


def test_a_different_state_dir_is_a_different_store(tmp_path):
    a = identity.ensure_store_identity(tmp_path / "a")
    b = identity.ensure_store_identity(tmp_path / "b")
    assert a.store_id != b.store_id


def test_store_identity_is_unreadable_by_other_users(tmp_path):
    """The value is a co-location proof — anyone who can read it can claim
    to own the store, so it gets the same 0600 the task files carry."""
    state = tmp_path / "state"
    identity.ensure_store_identity(state)
    mode = stat.S_IMODE(identity.store_identity_path(state).stat().st_mode)
    assert mode == 0o600


def test_reading_an_absent_store_identity_returns_none(tmp_path):
    assert identity.read_store_identity(tmp_path / "never-opened") is None


def test_a_corrupt_store_identity_reads_as_absent(tmp_path):
    state = tmp_path / "state"
    state.mkdir()
    identity.store_identity_path(state).write_text("{not json", encoding="utf-8")
    assert identity.read_store_identity(state) is None


def test_dial_in_actor_key_is_empty_when_the_client_is_unconfigured(monkeypatch):
    for suffix in ("_CONTROL_PLANE_URL", "_ACTOR_KEY", "_DIAL_TOKEN"):
        monkeypatch.delenv(identity.DIAL_IN_ENV_PREFIX + suffix, raising=False)
    assert identity.dial_in_actor_key() == ""


def test_dial_in_actor_key_reports_the_key_the_dial_in_client_uses(monkeypatch):
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_CONTROL_PLANE_URL", "http://cp:18080")
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_ACTOR_KEY", ACTOR_KEY)
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_DIAL_TOKEN", "dial-secret")
    assert identity.dial_in_actor_key() == ACTOR_KEY


def test_a_half_configured_dial_in_client_reports_no_key(monkeypatch):
    """`dialin.configured` refuses a partial configuration, so the client
    never starts — reporting the key anyway would tell a tracker this bridge
    receives work it will never be sent."""
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_ACTOR_KEY", ACTOR_KEY)
    monkeypatch.delenv(identity.DIAL_IN_ENV_PREFIX + "_CONTROL_PLANE_URL", raising=False)
    monkeypatch.delenv(identity.DIAL_IN_ENV_PREFIX + "_DIAL_TOKEN", raising=False)
    assert identity.dial_in_actor_key() == ""


# -- GET /identity ----------------------------------------------------------


def test_identity_reports_the_actor_the_store_and_the_dial_in_key(bridge, monkeypatch):
    base, cfg = bridge
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_CONTROL_PLANE_URL", "http://cp:18080")
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_ACTOR_KEY", ACTOR_KEY)
    monkeypatch.setenv(identity.DIAL_IN_ENV_PREFIX + "_DIAL_TOKEN", "dial-secret")

    status, body = _get(base, "/identity", headers={"Authorization": "Bearer s3cr3t"})

    assert status == 200
    assert body["actor_id"] == ACTOR_KEY
    assert body["store_id"] == identity.read_store_identity(cfg.state_dir).store_id
    assert body["dial_in"] == {"configured": True, "actor_key": ACTOR_KEY}


def test_identity_reports_no_address_of_any_kind(bridge):
    """The whole point of the cutover: this surface answers the identity
    question without an address, so nothing here may reintroduce one."""
    base, _cfg = bridge
    _status, body = _get(base, "/identity", headers={"Authorization": "Bearer s3cr3t"})
    rendered = json.dumps(body)
    for forbidden in ("endpoint", "host", "port", "url", "addr"):
        assert forbidden not in rendered.lower()


def test_identity_requires_the_bridge_token(bridge):
    """The store id is a co-location proof. It is not a secret that grants
    anything on its own, but an unauthenticated reader must not be handed
    the one value a wrong bridge would need to look right."""
    base, _cfg = bridge
    status, _body = _get(base, "/identity")
    assert status == 401


def test_identity_reports_an_unconfigured_dial_in_client_honestly(bridge, monkeypatch):
    base, _cfg = bridge
    for suffix in ("_CONTROL_PLANE_URL", "_ACTOR_KEY", "_DIAL_TOKEN"):
        monkeypatch.delenv(identity.DIAL_IN_ENV_PREFIX + suffix, raising=False)
    _status, body = _get(base, "/identity", headers={"Authorization": "Bearer s3cr3t"})
    assert body["dial_in"] == {"configured": False, "actor_key": ""}


def test_starting_the_bridge_mints_the_store_identity(tmp_path):
    """The tracker reads this file off the local filesystem, so the bridge
    must write it before anything asks — starting is what writes it."""
    cfg = _config(tmp_path)
    assert identity.read_store_identity(cfg.state_dir) is None
    srv, _thread = server.start_background(cfg)
    try:
        assert identity.read_store_identity(cfg.state_dir) is not None
    finally:
        srv.shutdown()
        srv.server_close()


def test_the_store_identity_file_does_not_look_like_a_task(tmp_path):
    """It lives beside `tasks/` and `idempotency/`, never inside them: the
    task store globs its directory, and a stray file there is a corrupt-task
    warning on every list."""
    state = tmp_path / "state"
    identity.ensure_store_identity(state)
    path = identity.store_identity_path(state)
    assert path.parent == state
    assert not (state / "tasks").exists() or not list((state / "tasks").glob("*.json"))
    assert os.path.basename(str(path)) == identity.STORE_IDENTITY_FILENAME
