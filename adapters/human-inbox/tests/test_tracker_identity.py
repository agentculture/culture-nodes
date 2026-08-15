"""Startup identity-guard tests for the human-inbox tracker.

These tests are separate from merge and reply polling because they exercise
the fail-closed deployment boundary before any polling cycle begins. Keeping
the network-locality matrix together makes that safety contract visible.
"""

from __future__ import annotations

import logging
import urllib.error

import pytest

from human_inbox_bridge import tracker

# --- startup identity check (task t8, issue #72) -----------------------
#
# The tracker submits through ONE bridge's authenticated surface, and that
# bridge's idempotency store is per-bridge and file-based (one JSON file per
# key under Config.state_dir). Two bridges serving one actor therefore
# cannot deduplicate each other's submissions — deployment convention is the
# only thing keeping "one logical human inbox" true, and this startup check
# is the only mechanism that can notice when it stops being true. These
# tests pin the refusal, not the arithmetic of the comparison.

ACTOR_KEY = "company/human-ops"


def _actor_row(endpoint: str, *, revision: int = 1, actor_key: str = ACTOR_KEY) -> dict:
    return {
        "id": f"actor_register_{revision}",
        "actor_key": actor_key,
        "revision": revision,
        "kind": "human",
        "protocol": "http",
        "endpoint_ref": endpoint,
    }


def _identity_config(**overrides) -> tracker.TrackerConfig:
    fields = {
        "state_dir": "/unused/state",
        "bridge_url": "http://127.0.0.1:8087",
        "actor_id": ACTOR_KEY,
        "control_plane_url": "http://192.168.1.5:18080",
        "github_token": "github-secret",
    }
    fields.update(overrides)
    return tracker.TrackerConfig(**fields)


def _fetch(*rows):
    def fetch(control_plane_url, **kwargs):
        assert control_plane_url == "http://192.168.1.5:18080"
        return list(rows)

    return fetch


def _local(*addresses):
    """A locality oracle for a host that answers on *addresses*."""

    def is_local(host: str) -> bool:
        return host in set(addresses) | {"127.0.0.1", "::1", "localhost"}

    return is_local


def test_bridge_serving_a_different_actor_is_refused_naming_both_endpoints():
    """The production split this exists for: the tracker runs on thor with a
    loopback bridge URL while company/human-ops is registered on spark."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
            is_local_address=_local("192.168.1.10"),  # this host is thor, not spark
        )
    message = str(excinfo.value)
    assert "http://192.168.1.157:8090" in message  # the actor's endpoint
    assert "http://127.0.0.1:8087" in message  # this tracker's own bridge
    assert ACTOR_KEY in message
    # The consequence, not just the mismatch: an operator reading this line
    # needs to know why it is fatal rather than cosmetic.
    assert "idempotenc" in message.lower()


def test_main_exits_non_zero_when_the_bridge_serves_a_different_actor(monkeypatch, capsys):
    """Acceptance: the refusal is an exit code, not only an exception."""
    monkeypatch.setattr(
        tracker, "fetch_actors", lambda url, **kwargs: [_actor_row("http://192.168.1.157:8090")]
    )
    monkeypatch.setattr(tracker, "_is_local_address", lambda host: host.startswith("127."))
    for name in ("HUMAN_INBOX_BRIDGE_CONFIG", "GITHUB_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("HUMAN_INBOX_BRIDGE_ACTOR_ID", ACTOR_KEY)
    monkeypatch.setenv("HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL", "http://192.168.1.5:18080")

    rc = tracker.main(["--once"])

    assert rc != 0
    err = capsys.readouterr().err
    assert "http://192.168.1.157:8090" in err
    assert "http://127.0.0.1:8087" in err


def test_co_located_bridge_passes_though_the_urls_differ_textually():
    """The tracker on the actor's own host addresses it as loopback while
    the actor row names the LAN address — the same bridge, so no refusal."""
    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://127.0.0.1:8090"),
        actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
        is_local_address=_local("192.168.1.157"),
    )
    assert resolved is not None
    assert resolved.endpoint_ref == "http://192.168.1.157:8090"


def test_identical_endpoints_need_no_locality_resolution():
    def never(host: str) -> bool:  # pragma: no cover - must not be called
        raise AssertionError(f"resolved locality for {host} on an exact match")

    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://192.168.1.157:8090"),
        actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
        is_local_address=never,
    )
    assert resolved is not None


def test_two_spellings_of_one_address_are_one_bridge():
    """A remote-but-correct pairing: the tracker points straight at the
    actor's endpoint, written differently. Same address, so no locality
    question arises and none is asked."""

    def never(host: str) -> bool:  # pragma: no cover - must not be called
        raise AssertionError(f"resolved locality for {host} on equal addresses")

    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://[0:0:0:0:0:0:0:1]:8090"),
        actor_fetch=_fetch(_actor_row("http://[::1]:8090")),
        is_local_address=never,
    )
    assert resolved is not None


def test_same_port_on_another_host_is_still_a_mismatch():
    """Matching ports must not be mistaken for the same bridge — the split
    deployment this guards against had both bridges on the same port."""
    with pytest.raises(tracker.BridgeIdentityError):
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8087")),
            is_local_address=_local("192.168.1.10"),
        )


def test_second_bridge_on_the_same_host_is_a_mismatch():
    """Same machine, different port: two bridge processes, two idempotency
    stores, and only one of them is the actor's."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(_actor_row("http://192.168.1.157:8090")),
            is_local_address=_local("192.168.1.157"),
        )
    assert "8090" in str(excinfo.value)


def test_newest_revision_decides_the_endpoint():
    """Actor identity is append-only: an endpoint move is a new revision, so
    an older matching row must not excuse a moved actor."""
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(bridge_url="http://127.0.0.1:8087"),
            actor_fetch=_fetch(
                _actor_row("http://192.168.1.157:8087", revision=1),
                _actor_row("http://192.168.1.99:8087", revision=2),
            ),
            is_local_address=_local("192.168.1.157"),
        )
    message = str(excinfo.value)
    assert "http://192.168.1.99:8087" in message
    assert "revision 2" in message


def test_other_actors_rows_are_ignored():
    resolved = tracker.verify_bridge_serves_actor(
        _identity_config(bridge_url="http://127.0.0.1:8090"),
        actor_fetch=_fetch(
            _actor_row("http://192.168.1.99:8086", revision=7, actor_key="company/codex-thor"),
            _actor_row("http://192.168.1.157:8090"),
        ),
        is_local_address=_local("192.168.1.157"),
    )
    assert resolved is not None and resolved.actor_key == ACTOR_KEY


def test_unregistered_actor_is_refused():
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(
                _actor_row("http://192.168.1.99:8086", revision=1, actor_key="company/developer")
            ),
            is_local_address=_local(),
        )
    message = str(excinfo.value)
    assert ACTOR_KEY in message
    assert "http://192.168.1.5:18080" in message
    assert "HUMAN_INBOX_BRIDGE_ACTOR_ID" in message


def test_unreachable_control_plane_fails_closed():
    """An unverifiable identity is not a verified one. The unit restarts, so
    a control plane that is merely restarting costs a retry, not a silent
    window in which a split deployment could double-submit."""

    def fetch(control_plane_url, **kwargs):
        raise urllib.error.URLError("connection refused")

    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(), actor_fetch=fetch, is_local_address=_local()
        )
    assert "http://192.168.1.5:18080" in str(excinfo.value)


def test_unconfigured_control_plane_warns_that_the_guard_is_inactive(caplog):
    with caplog.at_level(logging.WARNING, logger="human_inbox_bridge.tracker"):
        resolved = tracker.verify_bridge_serves_actor(
            _identity_config(control_plane_url=""),
            actor_fetch=_fetch(),
            is_local_address=_local(),
        )
    assert resolved is None
    assert "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL" in caplog.text
    assert "idempotenc" in caplog.text.lower()


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


@pytest.mark.parametrize("bad", ["", "not a url", "192.168.1.157:8090", "http://"])
def test_unusable_endpoint_ref_is_refused(bad):
    with pytest.raises(tracker.BridgeIdentityError) as excinfo:
        tracker.verify_bridge_serves_actor(
            _identity_config(),
            actor_fetch=_fetch(_actor_row(bad)),
            is_local_address=_local(),
        )
    assert ACTOR_KEY in str(excinfo.value)
