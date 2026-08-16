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
