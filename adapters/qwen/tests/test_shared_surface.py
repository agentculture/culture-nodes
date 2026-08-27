"""The shared-surface diff test: the ported core must stay byte-identical
to the codex sibling where the siblings are.

Task t1 (plan qwen-bridge-acp) ports the 19-module shared core from
``adapters/codex`` into ``adapters/qwen``; the only sanctioned change is the
package rename (``codex_bridge`` -> ``qwen_bridge``) — the module logic must
not diverge, because a change to one bridge's copy of a shared module is a
second dialect of one protocol.

``tests/lint/preflightsurface_test.go`` guards the byte-identity of the
shared surface across ALL bridges from the Go side (guard 1: ``preflight.py``
is one module, not four inline copies; ``deployment.py`` joined the guarded
set when task t32 split the deployed-revision measurement out of
``preflight.py`` rather than let the two diverge). This file is the
committed, language-intrinsic leg of that same guard: it fails the moment a
future edit touches the qwen copy but not the codex sibling, even in a state
where the Go lint is not being run (a bridge-side PR on its own).

It asserts byte-identity of the two shared modules only — behaviour is the
sibling suites' concern, and the protocol-version agreement across the
language boundary is the Go guard's (guard 2).
"""

from __future__ import annotations

from pathlib import Path

import pytest

# This file lives at <root>/adapters/qwen/tests/, so the repo root is three
# levels up. Resolved from __file__, not the CWD, so the test is independent
# of the directory uv is invoked from.
_REPO_ROOT = Path(__file__).resolve().parents[3]

#: The files ``tests/lint/preflightsurface_test.go`` requires to be
#: byte-identical in every bridge that ships them (its ``sharedModules``
#: list). Keep the two lists in lockstep.
SHARED_MODULES = ("deployment.py", "preflight.py")


def _sibling_bytes(module: str) -> bytes:
    return (_REPO_ROOT / "adapters" / "codex" / "src" / "codex_bridge" / module).read_bytes()


@pytest.mark.parametrize("module", SHARED_MODULES)
def test_the_ported_shared_module_is_byte_identical_to_the_codex_sibling(
    module: str,
) -> None:
    ported = _REPO_ROOT / "adapters" / "qwen" / "src" / "qwen_bridge" / module
    assert ported.is_file(), (
        f"adapters/qwen/src/qwen_bridge/{module} is missing: task t1 ports "
        "the shared core from the codex sibling, and the shared surface must "
        "ride along with it (tests/lint/preflightsurface_test.go guard 1)"
    )
    assert ported.read_bytes() == _sibling_bytes(module), (
        f"qwen_bridge/{module} has diverged from the codex sibling: the "
        "shared surface is ONE module every bridge ships, so a change must "
        "land in every copy at once — or the module must be split, the way "
        "deployment.py was split out of preflight.py (task t32), and the "
        "Go guard's sharedModules list extended in the same change"
    )
