"""Measured capability surface for the pi bridge."""

from __future__ import annotations

import getpass
import shutil
import sys
from pathlib import Path
from typing import Any, Callable

from pi_bridge import deployment, preflight
from pi_bridge.config import Config

TOOLCHAINS = (preflight.Toolchain("pi"), preflight.Toolchain("git"))


def _unix_user() -> str:
    return getpass.getuser()


def _confinement() -> str:
    return (
        f"unix-user:{_unix_user()}: pi has no sandbox and no tool-approval prompt; "
        "tools run with the bridge process user's full account authority"
    )


def host_facts(
    cfg: Config,
    *,
    probes=preflight.USERNS_SYSCTLS,
    capability_probe=None,
    locate: Callable[[str], tuple[str | None, bool]] | None = None,
    version=preflight.toolchain_version,
    git_probe: Callable[[Path], bool] = preflight.probe_git_metadata_write,
) -> dict[str, Any]:
    del probes, capability_probe
    if locate is None:

        def locate(name: str) -> tuple[str | None, bool]:
            if name == "pi":
                resolved = shutil.which(cfg.pi_bin)
                return (resolved or cfg.pi_bin, resolved is not None)
            return preflight.locate_toolchain(name)

    modes = ("read-only", "workspace-write")
    grants = preflight.dispatch_grants({mode: preflight.GRANTS for mode in modes})
    writable = list(cfg.repo_allowlist + cfg.repo_allowlist_prefixes)
    return preflight.host_block(
        hostname=preflight.hostname(),
        sandbox_modes=modes,
        sandbox_modes_unavailable={},
        default_sandbox_mode=cfg.default_sandbox,
        confinement=_confinement(),
        dispatch_grants=grants,
        toolchains=preflight.measure_toolchains(
            TOOLCHAINS, grants=grants, locate=locate, version=version
        ),
        commit_policy=preflight.harvest_commit_policy(
            preserve_on_failure=cfg.preserve_on_failure,
            branch_prefix=cfg.preserve_branch_prefix,
            push=cfg.preserve_push,
            remote=cfg.preserve_remote,
        ),
        writable_paths=writable,
        git_metadata_writable=preflight.measure_git_metadata_writable(writable, probe=git_probe),
        artifact_publish="unsupported-by-host",
        deployment=deployment.deployment_facts(sys.modules[__package__], "culture-nodes-pi-bridge"),
    )


def registration_capabilities(cfg: Config, **_: Any) -> dict[str, Any]:
    return {
        preflight.CAPABILITY_KEY: preflight.surface(host_facts(cfg)),
        "pi": {
            "binary": cfg.pi_bin,
            "provider": cfg.provider or None,
            "model": cfg.model or None,
            "mode": "json-print",
        },
    }
