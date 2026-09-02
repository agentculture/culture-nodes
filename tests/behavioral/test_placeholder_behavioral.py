"""Placeholder pytest behavioral test.

Proves the convention's plumbing: the ``behavioral`` marker is registered,
``pytest -m behavioral tests/behavioral`` selects this module, and it
passes. Delete it once a real Python behavioral test exists. See
``tests/behavioral/README.md``.
"""

import pytest

pytestmark = pytest.mark.behavioral


def test_behavioral_convention_placeholder(request):
    marks = {mark.name for mark in request.node.iter_markers()}
    assert "behavioral" in marks
