"""`toolchain-baseline.sh capture` must not destroy what it could not replace.

Issue #146, task t7. The instrument that notices toolchain drift on the
dispatch hosts is a committed JSON baseline per host. `capture` used to write
each host's probe output with a plain redirect:

    measure "$host" >"$BASELINE_DIR/$host.json"

A redirect opens and TRUNCATES its target before the command on the left has
run, let alone succeeded. So a network problem -- an unrelated fact, nothing
to do with any toolchain -- left a committed baseline at zero bytes, and the
next `check` had nothing to compare against. Reproduced at HEAD: a 52-byte
baseline became 0 bytes after one capture against an unreachable host.

Two distinct failures reach that same truncation, and only testing one leaves
the other live:

  1. The probe does not complete (ssh cannot resolve the host, exit 255).
  2. The probe COMPLETES and hands back nothing. `python3 -` fed an empty
     stdin runs an empty program, prints nothing and exits 0 -- so an ssh
     that answers can still empty a baseline while the command reports
     success. This is the path that actually exits 0, and it is the one the
     status check alone does not catch; hence `is_toolchain_envelope`.

Every test here works in a tmp_path baseline directory against invented
hostnames. Nothing in docs/baselines/toolchains/ is ever at risk, which
matters more than usual for a suite about a script whose defect is deleting
exactly those files.
"""

import json
import os
import subprocess  # nosec B404 - runs an in-repo shell script, no external input
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "toolchain-baseline.sh"

# Invented hostnames. `.invalid` is reserved by RFC 2606 and can never resolve,
# so "unreachable" is a property of the name rather than of today's network.
UNREACHABLE = "wpc-nosuchhost.invalid"
UNREACHABLE_2 = "wpc-nosuchhost-2.invalid"

COMMITTED_BASELINES = sorted((ROOT / "docs" / "baselines" / "toolchains").glob("*.json"))


def run_capture(baseline_dir, *hosts, path_prefix=None):
    """Run `capture <hosts...>` against a throwaway baseline directory."""
    env = dict(os.environ, TOOLCHAIN_BASELINE_DIR=str(baseline_dir))
    if path_prefix is not None:
        env["PATH"] = f"{path_prefix}{os.pathsep}{env['PATH']}"
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(SCRIPT), "capture", *hosts],
        text=True,
        capture_output=True,
        env=env,
        timeout=300,
    )


def seed(baseline_dir, host, payload):
    """Write a stand-in committed baseline and return its exact bytes."""
    target = baseline_dir / f"{host}.json"
    target.write_text(payload)
    return target, target.read_bytes()


@pytest.fixture
def answering_ssh(tmp_path):
    """A stand-in `ssh` that succeeds and prints nothing.

    Standing in for the real shape of failure 2: the connection is fine, the
    remote program is empty, the exit status is 0 and stdout is empty. Without
    it this case is unreproducible offline, and it is the case that made
    `capture` report success over a destroyed baseline.
    """
    shim_dir = tmp_path / "shim"
    shim_dir.mkdir()
    shim = shim_dir / "ssh"
    shim.write_text("#!/usr/bin/env bash\nexit 0\n")
    shim.chmod(0o755)
    return shim_dir


def test_unreachable_host_leaves_its_baseline_byte_identical(tmp_path):
    """The defect itself: a failed probe must not touch the baseline."""
    target, before = seed(tmp_path, UNREACHABLE, '{"seeded": "a committed baseline"}\n')

    result = run_capture(tmp_path, UNREACHABLE)

    assert result.returncode != 0
    assert target.read_bytes() == before


def test_a_probe_that_answers_with_nothing_also_leaves_the_baseline_alone(tmp_path, answering_ssh):
    """Exit status is not enough -- the content has to parse as JSON.

    This is the exit-0 half of #146. At HEAD this run printed `wrote ...`,
    exited 0, and left the baseline at zero bytes.
    """
    target, before = seed(tmp_path, UNREACHABLE, '{"seeded": "still here"}\n')

    result = run_capture(tmp_path, UNREACHABLE, path_prefix=answering_ssh)

    assert result.returncode != 0
    assert target.read_bytes() == before
    assert "no usable JSON" in result.stdout


def test_capture_exits_non_zero_and_names_every_host_it_could_not_measure(tmp_path):
    """All of them, not just the first.

    `set -e` used to abort the loop at the first failure, so a three-host
    capture with the second host down never tried the third and could not say
    whether the third was fine.
    """
    _, before_one = seed(tmp_path, UNREACHABLE, '{"host": "one"}\n')
    _, before_two = seed(tmp_path, UNREACHABLE_2, '{"host": "two"}\n')

    result = run_capture(tmp_path, "spark", UNREACHABLE, UNREACHABLE_2)

    assert result.returncode != 0
    assert UNREACHABLE in result.stderr
    assert UNREACHABLE_2 in result.stderr
    assert (tmp_path / f"{UNREACHABLE}.json").read_bytes() == before_one
    assert (tmp_path / f"{UNREACHABLE_2}.json").read_bytes() == before_two


def test_output_names_what_was_captured_and_what_was_skipped(tmp_path):
    """An operator reading the tail must not have to diff the directory."""
    seed(tmp_path, UNREACHABLE, '{"host": "one"}\n')

    result = run_capture(tmp_path, "spark", UNREACHABLE)

    assert "captured: spark" in result.stdout
    assert f"skipped:  {UNREACHABLE}" in result.stderr


def test_a_successful_capture_still_writes_a_baseline_and_exits_zero(tmp_path):
    """The fix must not cost the working case.

    `spark` is measured in-process rather than over ssh, so this runs
    anywhere the suite runs.
    """
    result = run_capture(tmp_path, "spark")

    assert result.returncode == 0, result.stderr
    assert "skipped:  (none)" in result.stdout
    measured = json.loads((tmp_path / "spark.json").read_text())
    assert isinstance(measured, dict)
    assert "search_path" in measured


def test_no_temp_file_is_left_behind_by_either_outcome(tmp_path, answering_ssh):
    """`<target>.new` is scaffolding; a leftover would be mistaken for a baseline."""
    seed(tmp_path, UNREACHABLE, '{"host": "one"}\n')

    run_capture(tmp_path, "spark", UNREACHABLE)
    run_capture(tmp_path, UNREACHABLE, path_prefix=answering_ssh)

    assert list(tmp_path.glob("*.new")) == []


def test_the_committed_baselines_are_non_empty_json(tmp_path):
    """The state #146 destroyed, asserted directly.

    Zero bytes is what a truncated baseline looks like, and a truncated
    baseline makes `check` report a missing comparison rather than drift --
    which reads like a setup problem, not like an instrument that was
    silently disarmed.
    """
    assert COMMITTED_BASELINES, "no committed toolchain baselines found"
    for baseline in COMMITTED_BASELINES:
        assert baseline.stat().st_size > 0, f"{baseline} is empty"
        assert isinstance(json.loads(baseline.read_text()), dict)
