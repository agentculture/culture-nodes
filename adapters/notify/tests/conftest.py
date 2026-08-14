"""Hermetic environment: unset both webhook env vars before every test, so
the suite can never pick up an ambient production webhook URL from
whatever environment `pytest` happens to run in -- the Python equivalent
of `internal/notify/testmain_test.go`'s package-wide `TestMain`. Individual
tests set a specific value with `monkeypatch.setenv`, which restores the
unset baseline automatically when the test ends.
"""

from __future__ import annotations

import pytest

from notify_bridge.webhook import ENV_FALLBACK, ENV_PRIMARY


@pytest.fixture(autouse=True)
def _hermetic_webhook_env(monkeypatch):
    monkeypatch.delenv(ENV_PRIMARY, raising=False)
    monkeypatch.delenv(ENV_FALLBACK, raising=False)
