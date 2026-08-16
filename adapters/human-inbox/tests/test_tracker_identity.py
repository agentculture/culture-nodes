"""Startup identity-guard tests for the human-inbox tracker.

These tests are separate from merge and reply polling because they exercise
the fail-closed deployment boundary before any polling cycle begins.

The guard was rebuilt for task t7 (issues #121/#136). Its question is
unchanged — "is the bridge I submit to the bridge that serves the actor I
observe?" — but the evidence it uses is not. It used to compare the actor's
registered `endpoint_ref` against its own bridge URL, host and port; that
column goes away with migration 0036, and after the dial-in cutover the
control plane holds no participant address at all. The replacement asks the
BRIDGE, and proves the answer came from the process that owns the durable
store this tracker reads tasks out of. See tracker.verify_bridge_serves_actor
for the full argument, and test_bridge_identity.py for the bridge half.
"""

from __future__ import annotations

import json
import logging
import threading
import urllib.error
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from human_inbox_bridge import identity, server, tracker, tracker_identity
from human_inbox_bridge.config import Config

ACTOR_KEY = "company/human-ops"
OUR_STORE = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
OTHER_STORE = "ffffffffffffffffffffffffffffffff"


def _identity_config(**overrides) -> tracker.TrackerConfig:
    fields = {
        "state_dir": "/unused/state",
        "bridge_url": "http://127.0.0.1:8087",
        "bridge_token": "bridge-secret",
        "actor_id": ACTOR_KEY,
        "control_plane_url": "http://192.168.1.5:18080",
        "github_token": "github-secret",
    }
    fields.update(overrides)
    return tracker.TrackerConfig(**fields)


def _bridge_says(
    *, actor_id: str = ACTOR_KEY, store_id: str = OUR_STORE, dial_in_key: str | None = ACTOR_KEY
):
    """A bridge that answers GET /identity the way *this* deployment would."""

    def fetch(bridge_url, bridge_token, **kwargs):
        assert bridge_token == "bridge-secret"
        return {
            "actor_id": actor_id,
            "store_id": store_id,
            "dial_in": {
                "configured": dial_in_key is not None,
                "actor_key": dial_in_key or "",
            },
        }

    return fetch


#: The actors row id of ACTOR_KEY's current registration revision. Production
#: writes THIS into the bridge's HUMAN_INBOX_BRIDGE_ACTOR_ID (ledger claims
#: carry it as a foreign key into actors(id)) while the tracker's copy of the
#: same variable holds the actor KEY — see deploy/prod/deploy.sh.
OUR_ROW_ID = "actor_01M0HUMANOPS0000000000000"


def _presence(presence: str = "connected", *, actor_key: str = ACTOR_KEY, seconds: float = 3.0):
    def fetch(control_plane_url, **kwargs):
        assert control_plane_url == "http://192.168.1.5:18080"
        return [
            {
                "actor_key": "company/codex-thor",
                "actor_id": "actor_01M0CODEXTHOR000000000000",
                "presence": "never_dialled",
            },
            {
                "actor_key": "company/developer",
                "actor_id": "actor_01M0DEVELOPER00000000000",
                "presence": "connected",
            },
            {
                "actor_key": actor_key,
                "actor_id": OUR_ROW_ID,
                "presence": presence,
                "seconds_since_last_seen": None if presence == "never_dialled" else seconds,
            },
        ]

    return fetch


def _no_presence_rows():
    def fetch(control_plane_url, **kwargs):
        return [{"actor_key": "company/developer", "presence": "connected"}]

    return fetch


def _store(store_id: str | None = OUR_STORE):
    def read(state_dir):
        return store_id

    return read


def _verify(cfg=None, *, bridge=None, presence=None, store=None):
    return tracker.verify_bridge_serves_actor(
        cfg or _identity_config(),
        bridge_identity_fetch=bridge or _bridge_says(),
        presence_fetch=presence or _presence(),
        local_store_id=store or _store(),
    )


# -- the happy paths --------------------------------------------------------


def test_a_co_located_dialled_in_bridge_is_confirmed():
    confirmed = _verify()
    assert confirmed is not None
    assert confirmed.actor_key == ACTOR_KEY
    assert confirmed.store_id == OUR_STORE
    assert confirmed.dials_in is True
    assert confirmed.presence == "connected"


def test_the_address_reading_helpers_are_gone():
    """Acceptance (t7/#121): the guard reads dial-in presence, which is keyed
    by actor_key and carries no address. The helpers that existed only to
    parse and compare `endpoint_ref` must not survive as a second, quieter
    path back to an address."""
    for gone in (
        "fetch_actors",
        "newest_actor_revision",
        "ActorEndpoint",
        "_is_local_address",
        "_numeric_addresses",
        "_host_port",
    ):
        assert not hasattr(tracker, gone), f"tracker.{gone} still reads an address"
        assert not hasattr(tracker_identity, gone), f"tracker_identity.{gone} reads an address"


def test_a_pre_cutover_bridge_is_confirmed_on_co_location_alone(caplog):
    """Mixed mode (transport-inversion.md): a bridge that has not been
    converted yet dials in as nobody, and nothing is dialled in as this
    actor either. Co-location is then the WHOLE guarantee, and the log says
    so rather than implying the presence half proved something."""
    with caplog.at_level(logging.WARNING, logger="human_inbox_bridge.tracker"):
        confirmed = _verify(
            bridge=_bridge_says(dial_in_key=None),
            presence=_presence("never_dialled"),
        )
    assert confirmed is not None
    assert confirmed.dials_in is False
    assert "dial" in caplog.text.lower()


# -- criterion 2: two bridges serving one actor are still refused -----------


def test_a_second_bridge_on_this_host_is_refused_because_it_owns_another_store():
    """The #72 hazard exactly: two bridge processes, two file-based
    idempotency stores, and a tracker reading one store while submitting to
    the other. No address is involved in noticing it any more."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=_bridge_says(store_id=OTHER_STORE))
    message = str(excinfo.value)
    assert "http://127.0.0.1:8087" in message  # the bridge this tracker submits to
    assert "/unused/state" in message  # the store it reads tasks from
    assert OUR_STORE[:8] in message and OTHER_STORE[:8] in message
    # The consequence, not just the mismatch: an operator reading this line
    # needs to know why it is fatal rather than cosmetic.
    assert "idempotenc" in message.lower()


def test_a_bridge_serving_another_actor_is_refused_naming_both_actors():
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=_bridge_says(actor_id="company/developer"))
    message = str(excinfo.value)
    assert ACTOR_KEY in message
    assert "company/developer" in message
    assert "http://127.0.0.1:8087" in message
    assert "idempotenc" in message.lower()


def test_the_production_shape_is_accepted_bridge_row_id_tracker_actor_key():
    """deploy/prod/deploy.sh writes HUMAN_INBOX_BRIDGE_ACTOR_ID with two
    DIFFERENT values on purpose — the actors row id for the bridge (its
    ledger claims are a foreign key into actors(id)), the actor key for the
    tracker. A plain string comparison of the two would refuse every correct
    production deployment; the presence row carries both names, so they are
    comparable."""
    confirmed = _verify(bridge=_bridge_says(actor_id=OUR_ROW_ID))
    assert confirmed is not None
    assert confirmed.actor_key == ACTOR_KEY


def test_an_unrecognised_bridge_actor_id_warns_rather_than_refusing(caplog):
    """A row id from a superseded registration names nothing the control
    plane knows. The store proof already established the tasks came out of
    this bridge, so submitting through it is still right — the stale ledger
    stamp is a log line, not a refusal."""
    with caplog.at_level(logging.WARNING, logger="human_inbox_bridge.tracker"):
        confirmed = _verify(bridge=_bridge_says(actor_id="actor_01M0SUPERSEDED0000000000"))
    assert confirmed is not None
    assert "actor_01M0SUPERSEDED0000000000" in caplog.text
    assert "redeploy" in caplog.text.lower()


def test_a_bridge_that_dials_in_as_another_actor_is_refused():
    """Same store, same configured actor_id, but its dial-in client presents
    a different key — so the control plane delivers this actor's work to a
    different process. HUMAN_INBOX_BRIDGE_ACTOR_ID and
    HUMAN_INBOX_BRIDGE_ACTOR_KEY are two variables, one letter apart."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=_bridge_says(dial_in_key="company/developer"))
    message = str(excinfo.value)
    assert "company/developer" in message
    assert ACTOR_KEY in message
    assert "HUMAN_INBOX_BRIDGE_ACTOR_KEY" in message


def test_a_non_dialling_bridge_is_refused_while_something_else_is_dialled_in():
    """The split deployment, positively detected rather than inferred: the
    actor IS connected, and it is not this bridge — so a second bridge with
    its own idempotency store is receiving this actor's work."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(
            bridge=_bridge_says(dial_in_key=None),
            presence=_presence("connected", seconds=4.0),
        )
    message = str(excinfo.value)
    assert ACTOR_KEY in message
    assert "http://127.0.0.1:8087" in message
    assert "second" in message.lower()
    assert "idempotenc" in message.lower()


def test_a_dialling_bridge_is_refused_while_the_actor_shows_no_presence():
    """This bridge says it dials in as the actor and the control plane has
    not heard from it. Whatever is wrong, the tracker cannot confirm its
    bridge is the one that will receive the actor's work."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(presence=_presence("disconnected", seconds=1800.0))
    message = str(excinfo.value)
    assert "disconnected" in message
    assert "1800" in message
    assert "http://192.168.1.5:18080" in message


def test_main_exits_non_zero_when_the_bridge_owns_a_different_store(monkeypatch, capsys):
    """Acceptance: the refusal is an exit code, not only an exception."""
    monkeypatch.setattr(
        tracker_identity,
        "fetch_bridge_identity",
        lambda url, token, **kwargs: {
            "actor_id": ACTOR_KEY,
            "store_id": OTHER_STORE,
            "dial_in": {"configured": True, "actor_key": ACTOR_KEY},
        },
    )
    monkeypatch.setattr(tracker_identity, "fetch_dial_in_presence", lambda url, **kwargs: [])
    monkeypatch.setattr(tracker_identity, "_local_store_id", lambda state_dir: OUR_STORE)
    for name in ("HUMAN_INBOX_BRIDGE_CONFIG", "GITHUB_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_ID", ACTOR_KEY)
    monkeypatch.setenv("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", "http://192.168.1.5:18080")

    rc = tracker.main(["--once"])

    assert rc != 0
    err = capsys.readouterr().err
    assert "http://127.0.0.1:8087" in err
    assert "idempotenc" in err.lower()


# -- the unhappy paths that are not a mismatch -----------------------------


def test_an_unopened_state_directory_is_refused():
    """No bridge has ever run against this state dir, so there is nothing to
    compare — and a tracker watching a directory no bridge fills is the
    silent failure #72 was filed for."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(store=_store(None))
    message = str(excinfo.value)
    assert "/unused/state" in message
    assert "HUMAN_INBOX_TRACKER_STATE_DIR" in message


def test_an_unreachable_bridge_fails_closed():
    def fetch(bridge_url, bridge_token, **kwargs):
        raise urllib.error.URLError("connection refused")

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=fetch)
    assert "http://127.0.0.1:8087" in str(excinfo.value)


def test_a_bridge_without_an_identity_surface_fails_closed():
    """An older bridge answers 404. That is not permission to skip the
    check — it is an unverified identity, which is not a verified one."""

    def fetch(bridge_url, bridge_token, **kwargs):
        raise urllib.error.HTTPError(bridge_url, 404, "Not Found", {}, None)

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=fetch)
    assert "/identity" in str(excinfo.value)


def test_unreachable_control_plane_fails_closed():
    """An unverifiable identity is not a verified one. The unit restarts, so
    a control plane that is merely restarting costs a retry, not a silent
    window in which a split deployment could double-submit."""

    def fetch(control_plane_url, **kwargs):
        raise urllib.error.URLError("connection refused")

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(presence=fetch)
    assert "http://192.168.1.5:18080" in str(excinfo.value)


def test_unregistered_actor_is_refused():
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(presence=_no_presence_rows())
    message = str(excinfo.value)
    assert ACTOR_KEY in message
    assert "http://192.168.1.5:18080" in message
    assert "HUMAN_INBOX_BRIDGE_ACTOR_ID" in message


def test_unconfigured_control_plane_keeps_the_co_location_half(caplog):
    """Without HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL the presence half is
    inactive — but the co-location proof is local, so unlike the old check
    the guard is degraded rather than switched off."""
    with caplog.at_level(logging.WARNING, logger="human_inbox_bridge.tracker"):
        confirmed = _verify(_identity_config(control_plane_url=""))
    assert confirmed is not None
    assert confirmed.presence is None
    assert "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL" in caplog.text


def test_unconfigured_control_plane_still_refuses_the_wrong_store():
    with pytest.raises(tracker.BridgeIdentityError):
        _verify(_identity_config(control_plane_url=""), bridge=_bridge_says(store_id=OTHER_STORE))


def test_tracker_config_carries_the_actor_id_and_control_plane_url():
    cfg = tracker.TrackerConfig.from_env(
        {
            "HUMAN_INBOX_BRIDGE_ACTOR_ID": ACTOR_KEY,
            "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL": "http://192.168.1.5:18080/",
        }
    )
    assert cfg.actor_id == ACTOR_KEY
    # Trailing slash stripped, exactly as bridge_url is.
    assert cfg.control_plane_url == "http://192.168.1.5:18080"


def test_tracker_config_control_plane_url_is_unset_by_default():
    cfg = tracker.TrackerConfig.from_env({})
    assert cfg.control_plane_url == ""
    assert cfg.actor_id == "human-inbox-bridge"


# -- criterion 1, end to end: no endpoint_ref anywhere ---------------------
#
# Everything above uses seams. This one runs the real tracker entry point
# against a real bridge process over loopback and a control plane whose
# actor rows have NO endpoint_ref at all — the state migration 0036 leaves
# behind. It is the acceptance criterion in its literal form: the unit
# starts, and the identity it confirms was established without an address.


class _FakeControlPlane:
    """A control plane after migration 0036: actor rows carry no address."""

    def __init__(self, presence_rows):
        self.paths: list[str] = []
        plane = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):  # noqa: D401 - quiet test output
                pass

            def do_GET(self):  # noqa: N802 - stdlib naming
                plane.paths.append(self.path)
                if self.path.startswith("/v1alpha1/dial-in-presence"):
                    body = {"observed_at": "2026-08-16T00:00:00Z", "items": presence_rows}
                elif self.path.startswith("/v1alpha1/actors"):
                    # Post-0036 shape: the column is gone, so the key is
                    # simply absent from every row.
                    body = {
                        "items": [
                            {
                                "id": "actor_1",
                                "actor_key": ACTOR_KEY,
                                "revision": 3,
                                "kind": "human",
                                "protocol": "http",
                            }
                        ]
                    }
                else:
                    self.send_error(404)
                    return
                payload = json.dumps(body).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


def test_the_tracker_service_starts_with_endpoint_ref_absent(tmp_path, monkeypatch):
    """Acceptance criterion 1 (t7): culture-nodes-human-inbox-tracker.service
    starts and confirms its bridge identity with actors.endpoint_ref gone."""
    cfg = Config(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        actor_id=ACTOR_KEY,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    plane = _FakeControlPlane(
        [{"actor_key": ACTOR_KEY, "presence": "connected", "seconds_since_last_seen": 2.0}]
    )
    try:
        for name in ("HUMAN_INBOX_BRIDGE_CONFIG", "GITHUB_TOKEN"):
            monkeypatch.delenv(name, raising=False)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_ID", ACTOR_KEY)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_AUTH_TOKEN", "s3cr3t")
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_STATE_DIR", cfg.state_dir)
        monkeypatch.setenv("HUMAN_INBOX_TRACKER_BRIDGE_URL", f"http://{host}:{port}")
        monkeypatch.setenv("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", plane.url)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_CONTROL_PLANE_URL", plane.url)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_KEY", ACTOR_KEY)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_DIAL_TOKEN", "dial-secret")

        assert tracker.main(["--once"]) == 0
        # It never asked for the actors list — there is nothing there it needs.
        assert not [p for p in plane.paths if p.startswith("/v1alpha1/actors")]
        assert [p for p in plane.paths if p.startswith("/v1alpha1/dial-in-presence")]
    finally:
        plane.close()
        srv.shutdown()
        srv.server_close()


def test_the_live_guard_still_refuses_a_second_bridge(tmp_path, monkeypatch):
    """Acceptance criterion 2, over real HTTP: two bridges configured for one
    actor, each with its own state directory. The tracker reads one store and
    submits to the other, and is refused."""
    ours = Config(state_dir=str(tmp_path / "ours"), host="127.0.0.1", port=0, actor_id=ACTOR_KEY)
    theirs = Config(
        state_dir=str(tmp_path / "theirs"), host="127.0.0.1", port=0, actor_id=ACTOR_KEY
    )
    our_srv, _t1 = server.start_background(ours)
    their_srv, _t2 = server.start_background(theirs)
    their_host, their_port = their_srv.server_address
    plane = _FakeControlPlane([{"actor_key": ACTOR_KEY, "presence": "connected"}])
    try:
        for name in ("HUMAN_INBOX_BRIDGE_CONFIG", "GITHUB_TOKEN", "HUMAN_INBOX_BRIDGE_AUTH_TOKEN"):
            monkeypatch.delenv(name, raising=False)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_ID", ACTOR_KEY)
        monkeypatch.setenv("HUMAN_INBOX_BRIDGE_STATE_DIR", ours.state_dir)
        monkeypatch.setenv("HUMAN_INBOX_TRACKER_BRIDGE_URL", f"http://{their_host}:{their_port}")
        monkeypatch.setenv("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", plane.url)

        cfg = tracker.TrackerConfig.from_env()
        with pytest.raises(tracker.BridgeIdentityError) as excinfo:
            tracker.verify_bridge_serves_actor(cfg)
        assert "idempotenc" in str(excinfo.value).lower()
        assert identity.read_store_identity(ours.state_dir).store_id[:8] in str(excinfo.value)
    finally:
        plane.close()
        our_srv.shutdown()
        our_srv.server_close()
        their_srv.shutdown()
        their_srv.server_close()


# -- a malformed identity surface is a refusal, never a traceback -----------
#
# Reported by review on PR #154. `(raw.get("dial_in") or {}).get(...)` raises
# AttributeError on a truthy non-mapping, and that exception escapes past the
# handler that exists to turn a bad identity surface into an actionable
# refusal. The whole function's contract is "fail closed with a message an
# operator can act on"; a traceback naming neither endpoint is not that.
#
# Note what the RIGHT answer is: a bridge whose `dial_in` block is unreadable
# has, as far as this tracker can tell, no dial-in key -- so it reaches the
# ordinary "does not dial in at all" refusal, which names both endpoints and
# says what to do. The bug was never the refusal; it was the exception type.


@pytest.mark.parametrize("dial_in", [[], "yes", 7, [{"actor_key": ACTOR_KEY}]])
def test_a_non_object_dial_in_block_refuses_readably_instead_of_raising(dial_in):
    def bridge(bridge_url, bridge_token, **kwargs):
        return {"actor_id": ACTOR_KEY, "store_id": OUR_STORE, "dial_in": dial_in}

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=bridge)
    message = str(excinfo.value)
    assert "does not dial in at all" in message
    assert "http://127.0.0.1:8087" in message


@pytest.mark.parametrize("raw", [[], "identity", 42, None])
def test_an_identity_surface_that_is_not_an_object_is_refused_by_name(raw):
    def bridge(bridge_url, bridge_token, **kwargs):
        return raw

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        _verify(bridge=bridge)
    message = str(excinfo.value)
    assert "not an object" in message
    assert "http://127.0.0.1:8087" in message
