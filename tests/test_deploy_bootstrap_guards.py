"""The root step's guards (#243): what `unix_user_bootstrap` refuses before
it writes anything as root, and the doctor `deploy/prod/bootstrap-accounts.sh`
runs before it reaches for sudo.

Split out of tests/test_deploy_unix_user.py (the 1000-line source limit);
same harness, same shims, same fake hosts.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

from tests.test_deploy_unix_user import (
    _LOG_ONLY_SHIM,
    LOCAL_HOST,
    PUBKEY,
    ROOT,
    SPARK,
    THOR,
    Harness,
    _bootstrapped_codex,
    _snapshot,
    _write_exec,
)

# --- the root bootstrap follows nothing the account planted ------------------


def _assert_refused_before_any_write(h: Harness, result, login_before: dict, path_name: str):
    assert result.returncode != 0
    assert path_name in result.stderr
    assert "symlink" in result.stderr or "owned" in result.stderr
    # Nothing was written as root: no linger, no chown, and the login
    # user's files -- where the planted link pointed -- are untouched.
    h.never("loginctl[")
    h.never("chown[")
    assert _snapshot(h.login_home(THOR)) == login_before


def test_bootstrap_refuses_a_credential_file_the_account_replaced_with_a_symlink(
    tmp_path: Path,
):
    """The account owns its home and could plant a symlink where root is
    about to cp/chmod/chown; a root bootstrap that followed it would write
    the login user's credential somewhere the account chose (#249 review,
    finding 2)."""
    h = Harness(tmp_path)
    home = _bootstrapped_codex(h)
    target = h.login_home(THOR) / ".codex/auth.json"
    (home / ".codex/auth.json").unlink()
    (home / ".codex/auth.json").symlink_to(target)
    before = _snapshot(h.login_home(THOR))
    result = h.run("unix_user_bootstrap thor-fake codex")
    _assert_refused_before_any_write(h, result, before, ".codex/auth.json")
    assert (home / ".codex/auth.json").is_symlink(), "the lane never deletes"


def test_bootstrap_refuses_a_ssh_directory_that_is_a_symlink(tmp_path: Path):
    h = Harness(tmp_path)
    home = _bootstrapped_codex(h)
    shutil.rmtree(home / ".ssh")
    (home / ".ssh").symlink_to(h.login_home(THOR) / ".ssh")
    before = _snapshot(h.login_home(THOR))
    result = h.run("unix_user_bootstrap thor-fake codex")
    _assert_refused_before_any_write(h, result, before, ".ssh")
    assert (h.login_home(THOR) / ".ssh/authorized_keys").read_text() == PUBKEY + "\n"


def test_bootstrap_refuses_a_credential_directory_another_user_owns(tmp_path: Path):
    h = Harness(tmp_path)
    home = _bootstrapped_codex(h)
    before = _snapshot(h.login_home(THOR))
    result = h.run(
        "unix_user_bootstrap thor-fake codex", FAKE_FOREIGN_OWNER_PATH=str(home / ".codex")
    )
    _assert_refused_before_any_write(h, result, before, ".codex")
    assert "intruder" in result.stderr


# --- the account is asked about its own sessions before its unit moves --------


def test_account_session_in_flight_refuses_before_any_systemctl(tmp_path: Path):
    """After the cutover the sessions run AS THE ACCOUNT, so a redeploy that
    only asked about the login user would restart the account's unit under
    a live session (#249 review, finding 3). The account is asked the same
    question, as itself, over its own ssh target."""
    h = Harness(tmp_path)
    _bootstrapped_codex(h)
    body = (
        "unix_user_account_session_check thor-fake codex\n"
        'ssh culture-codex@thor-fake "systemctl --user restart codex-bridge"'
    )
    result = h.run(body, FAKE_SESSION_USER="culture-codex")
    assert result.returncode != 0
    assert "culture-codex" in result.stderr
    assert "SKIP_SESSION_CHECK=1" in result.stderr
    h.first(f"ssh[culture-codex@{THOR}]", "pgrep -u culture-codex -f")
    h.first(f"pgrep[{THOR}] -u culture-codex -f", "[c]laude -p|[c]odex exec|qwen_bridge[.]qwen_cli")
    h.never("systemctl[")


def test_account_session_check_proceeds_when_only_the_login_user_is_busy(tmp_path: Path):
    """The two checks are independent: the account's answer is about the
    account, and a login-user session (the legacy unit still serving) is the
    login check's business."""
    h = Harness(tmp_path)
    _bootstrapped_codex(h)
    body = (
        "unix_user_account_session_check thor-fake codex\n"
        'ssh culture-codex@thor-fake "systemctl --user restart codex-bridge"'
    )
    result = h.run(body, FAKE_SESSION_USER="thor")
    assert result.returncode == 0, result.stderr
    h.first(f"systemctl[{THOR}] --user restart codex-bridge")
    skipped = h.run(body, FAKE_SESSION_USER="culture-codex", SKIP_SESSION_CHECK="1")
    assert skipped.returncode == 0, skipped.stderr
    assert "WARNING" in skipped.stdout


def test_account_session_check_refuses_an_account_it_cannot_reach(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_account_session_check thor-fake codex")
    assert result.returncode != 0
    assert "culture-codex@thor-fake" in result.stderr
    h.never("pgrep[")


# --- bootstrap-accounts.sh: the operator's one root step, doctored first ------

BOOTSTRAP_ACCOUNTS = ROOT / "deploy/prod/bootstrap-accounts.sh"

# bootstrap-accounts.sh's ssh branch talks to a host by its bare name and
# stages the lane with scp; this shim answers the doctor command with
# FAKE_REMOTE_DOCTOR_EXIT (42: no checkout on the host) and the sudo step
# with 0, logging both in order.
_BOOTSTRAP_SSH_SHIM = """#!/usr/bin/env bash
while [ "$1" = -t ] || [ "$1" = -o ]; do [ "$1" = -o ] && shift; shift; done
host=$1; shift
printf 'ssh[%s] %s\\n' "$host" "$*" >> "$FAKE_LOG"
case "$*" in
  *"nodes doctor"*) exit "${FAKE_REMOTE_DOCTOR_EXIT:-42}" ;;
esac
exit 0
"""


def _run_bootstrap_accounts(
    h: Harness, host_arg: str, **fake_env: str
) -> subprocess.CompletedProcess:
    """`deploy/prod/bootstrap-accounts.sh <host>` typed on spark, under the
    harness shims: hostname answers spark, so `spark` takes the local sudo
    branch and `orin` the ssh branch."""
    _write_exec(h.bin / "hostname", "#!/usr/bin/env bash\necho spark\n")
    _write_exec(h.bin / "scp", _LOG_ONLY_SHIM)
    if host_arg != "spark":
        _write_exec(h.bin / "ssh", _BOOTSTRAP_SSH_SHIM)
    env = {
        "PATH": f"{h.bin}{os.pathsep}{os.environ['PATH']}",
        "HOME": str(h.login_home(SPARK)),
        "FAKE_LOG": str(h.log),
        "FAKE_HOSTS": str(h.hosts),
        "FAKE_HOST": SPARK,
        "FAKE_USER": "spark",
        "FAKE_LOCAL_HOST": LOCAL_HOST,
        **fake_env,
    }
    return subprocess.run(  # nosec B603 - the wrapper itself, under the shims
        ["bash", str(BOOTSTRAP_ACCOUNTS), host_arg],
        env=env,
        cwd=h.login_home(SPARK),
        text=True,
        capture_output=True,
        check=False,
    )


def test_bootstrap_accounts_runs_the_doctor_before_the_local_sudo(tmp_path: Path):
    """The one root step used to go straight to sudo (#249 review, finding
    8): the operator checkout's `nodes doctor` now runs first, non-strict, and
    only its error-severity check (prompt file) refuses."""
    h = Harness(tmp_path)
    result = _run_bootstrap_accounts(h, "spark")
    assert result.returncode == 0, result.stderr + result.stdout
    assert h.first(f"uv[{SPARK}:spark] run nodes doctor") < h.first("sudo[")
    assert h.count("useradd[") == 2
    assert oct(h.account_home(SPARK, "claude").stat().st_mode & 0o777) == "0o750"


def test_bootstrap_accounts_refuses_the_local_sudo_when_the_doctor_fails(tmp_path: Path):
    h = Harness(tmp_path)
    result = _run_bootstrap_accounts(h, "spark", FAKE_DOCTOR_EXIT="1")
    assert result.returncode != 0
    assert "doctor" in result.stderr
    h.never("sudo[")
    h.never("useradd[")
    assert not h.account_home(SPARK, "claude").exists()


def test_bootstrap_accounts_doctors_the_remote_host_best_effort_before_its_sudo(tmp_path: Path):
    """Over ssh the doctor is best effort: a host with no culture-nodes-prod
    checkout (or no uv) is a WARNING and the bootstrap goes on; a doctor that
    ran and failed refuses before the sudo step."""
    h = Harness(tmp_path)
    absent = _run_bootstrap_accounts(h, "orin")
    assert absent.returncode == 0, absent.stderr + absent.stdout
    assert "WARNING" in absent.stdout
    doctor = h.first("ssh[orin]", "nodes doctor")
    assert "bash -lc" in h.calls()[doctor]
    assert doctor < h.first("ssh[orin] sudo bash")
    assert h.first("scp[") < h.first("ssh[orin] sudo bash")

    h.clear_log()
    healthy = _run_bootstrap_accounts(h, "orin", FAKE_REMOTE_DOCTOR_EXIT="0")
    assert healthy.returncode == 0, healthy.stderr
    assert "WARNING" not in healthy.stdout
    h.first("ssh[orin] sudo bash")

    h.clear_log()
    sick = _run_bootstrap_accounts(h, "orin", FAKE_REMOTE_DOCTOR_EXIT="1")
    assert sick.returncode != 0
    assert "doctor" in sick.stderr
    h.never("ssh[orin] sudo bash")
