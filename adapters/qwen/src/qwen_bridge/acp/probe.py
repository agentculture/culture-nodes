"""The qwen binary probe + boot refusal (the #113 contract leg).

Plan task t2 of qwen-bridge-acp (module split; the probe lived in the
monolithic `qwen_cli` before). `qwen` is located at its KNOWN INSTALL
PATHS, never via PATH - measured absent from the non-interactive ssh
PATH on thor/orin (spec s4, 2026-08-23), and the image entrypoint (t6)
must not rely on it either. A missing binary raises
errors.QwenAgentMissingError - a DISTINCT message naming the probe paths
and the refusal - in the BRIDGE process, before any Popen, so no invoke
is ever served. This is the boot refusal plan t3's h5 negative test
asserts on.
"""

from __future__ import annotations

import os

from qwen_bridge.acp import errors
from qwen_bridge.config import Config

#: The KNOWN INSTALL PATHS the probe checks, in order. Measured, not
#: guessed (spec s4, 2026-08-23): the fleet layout is
#: ~/.local/lib/qwen-code (all three hosts) and the user-local bin
#: ~/.local/bin (spark's PATH entry). The probe deliberately NEVER
#: consults the non-interactive PATH: an operator who installs qwen
#: elsewhere sets QWEN_BRIDGE_QWEN_BIN explicitly, and that is honored
#: as-is.
QWEN_PROBE_PATHS = ("~/.local/lib/qwen-code/bin/qwen", "~/.local/bin/qwen")


def _usable_binary(path: str) -> bool:
    return os.path.isfile(path) and os.access(path, os.X_OK)


def locate_qwen_bin(cfg: Config) -> str:
    """Locate the qwen binary by PROBING, not by PATH, and refuse
    distinctly when the contract leg is missing.

    Precedence: an explicit `cfg.qwen_bin` that names a PATH (contains a
    path separator) is honored as-is - the operator chose it - but must
    exist and be executable, or the same distinct refusal fires. The
    default bare `qwen` (the Config default) means "probe the known
    install paths". Raises `QwenAgentMissingError` in every missing case
    with a message that names exactly what was probed.
    """
    explicit = cfg.qwen_bin
    if explicit and os.sep in explicit:
        if _usable_binary(explicit):
            return explicit
        raise errors.QwenAgentMissingError(
            f"qwen-agent-missing: configured qwen_bin {explicit!r} is not an executable file — "
            "contract leg missing, refusing to serve invokes"
        )
    probes = [os.path.expanduser(p) for p in QWEN_PROBE_PATHS]
    for probe in probes:
        if _usable_binary(probe):
            return probe
    raise errors.QwenAgentMissingError(
        "qwen-agent-missing: qwen binary not found at "
        + " or ".join(f"'{p}'" for p in probes)
        + " — contract leg missing, refusing to serve invokes"
    )
