"""Shared pytest fixtures for culture-nodes tests."""

from __future__ import annotations

import pytest

from tests.fake_api import FakeNodesAPI


@pytest.fixture
def fake_api():
    """A FakeNodesAPI instance, torn down after the test.

    Not started here — a test registers its routes first, then calls
    ``fake_api.start()`` once route setup is done.
    """
    server = FakeNodesAPI()
    try:
        yield server
    finally:
        server.stop()
