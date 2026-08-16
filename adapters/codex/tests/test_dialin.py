from codex_bridge import dialin


def test_dialin_disabled_when_all_settings_absent(monkeypatch):
    prefix = "TEST_BRIDGE"
    for suffix in ("_CONTROL_PLANE_URL", "_ACTOR_KEY", "_DIAL_TOKEN"):
        monkeypatch.delenv(prefix + suffix, raising=False)
    assert dialin.configured(prefix) is None


def test_dialin_requires_identity_url_and_credential_together(monkeypatch):
    prefix = "TEST_BRIDGE"
    monkeypatch.setenv(prefix + "_CONTROL_PLANE_URL", "http://control")
    monkeypatch.delenv(prefix + "_ACTOR_KEY", raising=False)
    monkeypatch.delenv(prefix + "_DIAL_TOKEN", raising=False)
    try:
        dialin.configured(prefix)
    except ValueError as exc:
        assert "_ACTOR_KEY" in str(exc)
    else:
        raise AssertionError("partial dial-in configuration was accepted")


def test_an_idle_poll_returning_204_is_not_treated_as_a_fault(monkeypatch):
    """The defect the live demonstration found (task t23, operator lane).

    An idle long poll answers 204 with an EMPTY body. 204 is a 2xx, so urllib
    does not raise HTTPError for it -- the ``except HTTPError ... code == 204``
    branch is unreachable on the shipped server. Before the fix json.loads("")
    raised, the loop logged "dial-in reconnecting", slept a second, and every
    bridge in the fleet spun forever without ever claiming work. The empty
    mailbox is the NORMAL case, so this was the steady state, not an edge.
    """
    prefix = "TEST_BRIDGE"
    monkeypatch.setenv(prefix + "_CONTROL_PLANE_URL", "http://control")
    monkeypatch.setenv(prefix + "_ACTOR_KEY", "company/x")
    monkeypatch.setenv(prefix + "_DIAL_TOKEN", "t")

    class Idle:
        status = 204

        def read(self):
            return b""

        def close(self):
            pass

    calls = {"polls": 0}

    def opener(request, timeout=None):
        calls["polls"] += 1
        if calls["polls"] > 3:
            raise SystemExit
        return Idle()

    naps = []
    try:
        dialin.run(prefix, 1, opener=opener, pause=naps.append)
    except SystemExit:
        pass

    # Four polls, and every nap the short idle pause -- never the one-second
    # reconnect backoff, which is what a fault would have produced.
    assert calls["polls"] == 4, calls
    assert naps == [0.25, 0.25, 0.25], naps
