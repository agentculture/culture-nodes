"""deploy/prod/lanes/land-toolchain.sh -- the land account's toolchain
DETECTOR (loop-closure task t7; spec c36, risk r2).

The land node's gate chain needs go, uv, node and markdownlint-cli2 on
culture-land's PATH on the runner host, and on 2026-09-07 `ssh thor
'command -v go'` found nothing (spec s26). land_gate.py refuses by name when
that is so; this lane is the deploy's half of the same fact -- it asks the
account, over the same ssh the other lanes use, which of the four binaries
answer, prints one line per binary, and warns by name for the missing ones.
It is a detector, not a gate: installing Go on thor is a counted hand-turn,
and a deploy that refused to finish until someone typed it would leave the
stack half-shipped for a fact it can only report.

Run under tests/test_deploy_unix_user.py's fake-host harness (the ssh shim
maps culture-land@thor-fake to a fake home and runs the remote command
there, inheriting the test's PATH), so which binaries are "present" is
exactly what this test puts on PATH -- never the developer's machine. The
harness runs the lane under `set -e`, which is how these tests can tell the
difference between a check that FAILS by name and a deploy that aborts: the
check returns 1, and the deploy's own call site guards it.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

# The deploy-shaped ssh shim, not the lane-shaped one: this lane's
# reachability probe passes `-o BatchMode=yes -o ConnectTimeout=15` ahead of
# the target, which only the account-bridges variant strips.
from tests.test_deploy_account_bridges import _SSH_SHIM
from tests.test_deploy_unix_user import THOR, _block, _write_exec

ROOT = Path(__file__).resolve().parents[1]
LANE = ROOT / "deploy/prod/lanes/land-toolchain.sh"
DEPLOY = ROOT / "deploy/prod/deploy.sh"
BINARIES = ("go", "uv", "node", "markdownlint-cli2")


class Fleet:
    """A fake thor with a bootstrapped culture-land, an ssh shim, and a PATH
    the test owns entirely: shim dir, tools dir, and symlinks to the few
    real programs the shim and the lane need (bash, env, tr, id)."""

    def __init__(self, tmp_path: Path):
        self.tmp = tmp_path
        self.log = tmp_path / "calls.log"
        self.log.touch()
        self.hosts = tmp_path / "hosts"
        self.bin = tmp_path / "bin"
        self.bin.mkdir()
        _write_exec(self.bin / "ssh", _SSH_SHIM)
        self.tools = tmp_path / "tools"
        self.tools.mkdir()
        self.system = tmp_path / "system"
        self.system.mkdir()
        for helper in ("bash", "sh", "env", "tr", "id", "basename", "cat"):
            real = shutil.which(helper)
            assert real, helper
            os.symlink(real, self.system / helper)

    def bootstrap_land(self, host: str = THOR) -> Path:
        home = self.hosts / host / "home" / "culture-land"
        home.mkdir(parents=True)
        return home

    def install(self, *names: str) -> None:
        for name in names:
            _write_exec(self.tools / name, f"#!/usr/bin/env bash\necho {name}-fake\n")

    def run(self, body: str, host: str = THOR, **extra_env: str) -> subprocess.CompletedProcess:
        script = (
            "set -euo pipefail\n"
            "say() { printf '==> %s\\n' \"$*\"; }\n"
            f"HOST={host}\nREMOTE_DIR=culture-nodes-prod\nSCRIPT_DIR={ROOT / 'deploy/prod'}\n"
            + _block()
            + f'\nsource "{LANE}"\n'
            + body
            + "\n"
        )
        env = {
            "PATH": os.pathsep.join(str(p) for p in (self.bin, self.tools, self.system)),
            "HOME": str(self.tmp / "operator"),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "FAKE_LOCAL_HOST": "spark-fake",
            **extra_env,
        }
        return subprocess.run(  # nosec B603 - fixed bash over repository lane text
            ["bash", "-c", script],
            env=env,
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )


def lines(proc: subprocess.CompletedProcess, needle: str) -> list[str]:
    return [line for line in proc.stdout.splitlines() if needle in line]


def test_the_check_prints_one_line_per_binary_and_names_the_missing_one(tmp_path):
    fleet = Fleet(tmp_path)
    fleet.bootstrap_land()
    fleet.install("uv", "node", "markdownlint-cli2")  # the thor shape: no go

    proc = fleet.run('land_toolchain_check "$HOST" || echo "rc=$?"')

    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert "rc=1" in proc.stdout, "a missing binary must make the CHECK fail, by name"
    for name in ("uv", "node", "markdownlint-cli2"):
        present = lines(proc, f"land toolchain: {name} present")
        assert len(present) == 1, (name, proc.stdout)
        assert "culture-land@thor-fake" in present[0]
    missing = lines(proc, "land toolchain: go MISSING")
    assert len(missing) == 1, proc.stdout
    warning = lines(proc, "WARNING")
    assert len(warning) == 1, proc.stdout
    assert "go" in warning[0], proc.stdout
    assert "hand-turn" in warning[0]
    assert "toolchain_missing" in warning[0], "the warning names the record the gate would write"
    # Probed AS the account, once per binary, over ssh -- never sudo, never a
    # login-user shell.
    calls = fleet.log.read_text().splitlines()
    assert len([c for c in calls if c.startswith("ssh[culture-land@thor-fake]")]) >= len(BINARIES)
    assert not [c for c in calls if "sudo" in c]


def test_every_binary_present_is_said_so_and_nothing_warns(tmp_path):
    fleet = Fleet(tmp_path)
    fleet.bootstrap_land()
    fleet.install(*BINARIES)

    proc = fleet.run('land_toolchain_check "$HOST"; echo "rc=$?"')

    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert "rc=0" in proc.stdout
    for name in BINARIES:
        assert len(lines(proc, f"land toolchain: {name} present")) == 1, proc.stdout
    assert not lines(proc, "MISSING")
    assert not lines(proc, "WARNING")
    assert lines(proc, "the gate chain can run")


def test_an_unbootstrapped_land_account_is_skipped_by_name(tmp_path):
    fleet = Fleet(tmp_path)  # no culture-land home: ssh answers 255
    fleet.install(*BINARIES)

    proc = fleet.run('land_toolchain_check "$HOST"; echo "rc=$?"')

    assert proc.returncode == 0, proc.stderr + proc.stdout
    assert "rc=0" in proc.stdout
    skipped = lines(proc, "not bootstrapped")
    assert len(skipped) == 1, proc.stdout
    assert "culture-land" in skipped[0], proc.stdout
    assert not lines(proc, "present"), proc.stdout
    assert not lines(proc, "MISSING"), proc.stdout


def test_the_binary_list_is_the_gate_declaration(tmp_path):
    """One list, stated in the lane and read by the gate's tests: a binary
    the gate starts needing shows up here or the detector goes quiet on it."""
    lane = LANE.read_text()
    assert (
        'LAND_TOOLCHAIN_BINARIES=${LAND_TOOLCHAIN_BINARIES:-"go uv node markdownlint-cli2"}' in lane
    )
    fleet = Fleet(tmp_path)
    fleet.bootstrap_land()
    fleet.install("uv")
    proc = fleet.run(
        'land_toolchain_check "$HOST" || echo "rc=$?"', LAND_TOOLCHAIN_BINARIES="uv shellcheck"
    )
    assert proc.returncode == 0, proc.stderr
    assert "rc=1" in proc.stdout
    assert lines(proc, "uv present"), proc.stdout
    assert lines(proc, "shellcheck MISSING"), proc.stdout
    assert not lines(proc, "go ")


def test_deploy_wires_the_detector_into_both_runner_arms_under_the_line_limit():
    subprocess.run(["bash", "-n", str(LANE)], check=True)  # nosec B603 B607
    subprocess.run(["bash", "-n", str(DEPLOY)], check=True)  # nosec B603 B607
    script = DEPLOY.read_text()
    assert 'source "$SCRIPT_DIR/lanes/land-toolchain.sh"' in script
    assert script.count('land_toolchain_check "$HOST"') == 2, "thor and orin arms, once each"
    assert (
        script.count('land_toolchain_check "$HOST" || true') == 2
    ), "the check may fail by name; the deploy may not abort on it"
    assert len(script.splitlines()) <= 1000
    # After the doctor, before the summary, in each arm: a detector that
    # speaks once the stack is up, like audit-credentials.sh. `thor*)` also
    # names arms inside helper functions, so the dispatch is read from the
    # one `case "$HOST" in` at the tail of the script.
    dispatch = script.split('case "$HOST" in')[-1]
    for arm in ("thor*)", "orin*)"):
        body = dispatch.split(arm, 1)[1].split(";;", 1)[0]
        assert (
            body.index("nodes doctor")
            < body.index("land_toolchain_check")
            < body.index("account_bridges_summary")
        ), arm
    # The spark arm runs bridge lanes only and has no land account to probe.
    spark = dispatch.split("spark*)", 1)[1].split(";;", 1)[0]
    assert "land_toolchain_check" not in spark


@pytest.mark.parametrize("marker", ["set -e", "exit 1"])
def test_the_lane_is_sourced_and_never_exits_the_caller(marker):
    """A sourced lane RETURNS -- `return 1` reports a failed check to its
    caller, while an `exit` would end the deploy the caller is still running."""
    lane = LANE.read_text()
    assert marker not in lane, f"a sourced detector must not carry `{marker}`"
    assert "return 0" in lane
    assert "return 1" in lane
