"""Fake-host coverage for deploy/prod/cutover.sh (task t10, #298, spec c9/c35).

cutover.sh sequences the three automatable steps of #298's manual
harness-actor bring-up -- the engine account's bridge secret, the deploy of
its bridge, and the actor registration -- behind two preconditions it
refuses BY NAME (the account exists; both compose files declare the actor's
token key). The root bootstrap stays a hand-turn and this script never
performs it.

The harness is the same shape as tests/test_deploy_unix_user.py's: every
side-effecting tool is a shim on PATH that appends one line to a shared log,
``ssh`` maps ``culture-<engine>@<host>`` to a fake home under a per-host root
and runs the "remote" command there, and the fake hosts are named
``thor-fake`` / ``spark-fake`` so a test can never reach a real machine. It
is deliberately SMALLER than the account-bridges harness: cutover.sh runs no
deploy of its own (it invokes ``deploy.sh``, overridden here through
``CUTOVER_DEPLOY_CMD`` the way register-actor.sh lets a test override
``PSQL_CMD``), so the shim set is ssh, getent, curl, openssl, sudo, a
recording deploy and a stateful fake psql.

What is asserted, per t10's acceptance:

* ``--dry-run`` prints every step as would-run/would-skip and leaves the
  shim log EMPTY -- a dry run touches no host at all;
* a run whose token key is undeclared in the compose files exits non-zero
  naming the key; a run whose engine account does not open exits non-zero
  naming the account;
* a real run logs secrets, deploy and register in that order;
* a second identical run reports every step as skipped and exits 0;
* ``--yes`` is required, bad argv is a usage error, and the script invokes
  neither bootstrap-accounts.sh nor sudo -- checked both in the log and by
  grepping the script itself;
* install-secrets.sh is still exactly 999 lines (cutover.sh lifts its lane
  rather than adding to it) and cutover.sh is under the 1000-line limit.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
CUTOVER = ROOT / "deploy/prod/cutover.sh"
INSTALL_SECRETS = ROOT / "deploy/prod/install-secrets.sh"

THOR = "thor-fake"
ORIN = "orin-fake"
SPARK = "spark-fake"

# --- shims ----------------------------------------------------------------

# ssh: strips the `-o Option=value` pairs cutover.sh's BatchMode probes pass,
# logs the target and the command, then runs the command locally against the
# target's fake home. A target whose home does not exist fails with 255, the
# way a real ssh to an account that was never bootstrapped does -- which is
# how the "account not bootstrapped" refusal is exercised.
_SSH_SHIM = """#!/usr/bin/env bash
while [ "$1" = -o ]; do shift 2; done
target=$1; shift
printf 'ssh[%s] %s\\n' "$target" "$*" >> "$FAKE_LOG"
case "$target" in
  *@*)
    user=${target%@*}; host=${target#*@}
    [ "$host" = localhost ] && host=$FAKE_LOCAL_HOST
    home="$FAKE_HOSTS/$host/home/$user"
    ;;
  *)
    host=$target; user=${host%-fake}
    home="$FAKE_HOSTS/$host/home/$user"
    ;;
esac
[ -d "$home" ] || exit 255
cd "$home" || exit 255
exec env FAKE_HOST="$host" FAKE_USER="$user" HOME="$home" bash -c "$*"
"""

# getent answers the LAN address cutover.sh derives on the target (never
# hardcodes), the same shape `getent hosts thor` has on a real host.
_GETENT_SHIM = """#!/usr/bin/env bash
[ "$1" = hosts ] || exit 1
printf '192.168.1.5 %s\\n' "$2"
"""

# curl serves the bridge's /v1/capabilities deployment block, and only from a
# revision file a deploy actually wrote: before the first deploy the probe
# fails the way an unreachable bridge does (exit 22), so cutover.sh cannot
# skip a deploy that never happened.
_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl %s\\n' "$*" >> "$FAKE_LOG"
url=${@: -1}
case "$url" in
  */v1/capabilities)
    [ -f "$FAKE_DEPLOYED_REV" ] || exit 22
    printf '{"deployment": {"revision": "%s"}}' "$(cat "$FAKE_DEPLOYED_REV")" ;;
  *) exit 22 ;;
esac
"""

# The mint install-secrets.sh's lane performs. Deterministic so a re-run can
# be compared byte for byte.
_OPENSSL_SHIM = """#!/usr/bin/env bash
printf 'openssl %s\\n' "$*" >> "$FAKE_LOG"
printf 'fake-minted-token-value\\n'
"""

# sudo exists only so its ABSENCE from the log is a fact rather than a gap in
# the harness: a shim that is on PATH and never called proves the point.
_SUDO_SHIM = """#!/usr/bin/env bash
printf 'sudo %s\\n' "$*" >> "$FAKE_LOG"
exit 0
"""

# The deploy step. cutover.sh reaches deploy_account_engine_bridge by running
# deploy.sh (it takes its host on argv and cannot be sourced); this recorder
# stands in for that whole script and stamps the revision the bridge would
# then report, which is what makes the second run's deploy skip real.
_DEPLOY_SHIM = """#!/usr/bin/env bash
printf 'deploy %s\\n' "$*" >> "$FAKE_LOG"
printf '%s' "$FAKE_WANT_REVISION" > "$FAKE_DEPLOYED_REV"
exit 0
"""

# A stateful psql for register-actor.sh: it stores what an INSERT wrote and
# answers the next SELECT from it, so the script's own append-only
# idempotency (unchanged endpoint + metadata -> no INSERT) is what the second
# run exercises, rather than a canned answer.
_PSQL_SHIM = """#!/usr/bin/env python3
import json
import os
import re
import sys

query = sys.argv[-1]
with open(os.environ["FAKE_LOG"], "a") as handle:
    handle.write("psql %s\\n" % query.split()[0])
state = os.environ["FAKE_ACTOR_STATE"]
overlay_match = re.search(r"'(\\{.*?\\})'::jsonb", query)
overlay = overlay_match.group(1) if overlay_match else ""
if "INSERT INTO actors" in query:
    endpoint = re.search(r"'(https?://[^']+)'", query)
    with open(state, "w") as handle:
        json.dump({"endpoint": endpoint.group(1) if endpoint else "", "overlay": overlay}, handle)
elif "FROM actors" in query and os.path.exists(state):
    with open(state) as handle:
        row = json.load(handle)
    sys.stdout.write("1|%s|%s" % (row["endpoint"], "t" if row["overlay"] == overlay else "f"))
"""


def _write_exec(path: Path, body: str) -> None:
    path.write_text(body)
    path.chmod(0o755)


class Harness:
    """Fake hosts, fake accounts and a shared call log."""

    def __init__(self, tmp_path: Path):
        self.tmp = tmp_path
        self.log = tmp_path / "calls.log"
        self.log.touch()
        self.hosts = tmp_path / "hosts"
        self.deployed_rev = tmp_path / "deployed.rev"
        self.actor_state = tmp_path / "actor.state"
        self.bin = tmp_path / "bin"
        self.bin.mkdir()
        _write_exec(self.bin / "ssh", _SSH_SHIM)
        _write_exec(self.bin / "getent", _GETENT_SHIM)
        _write_exec(self.bin / "curl", _CURL_SHIM)
        _write_exec(self.bin / "openssl", _OPENSSL_SHIM)
        _write_exec(self.bin / "sudo", _SUDO_SHIM)
        _write_exec(self.bin / "fake-deploy", _DEPLOY_SHIM)
        _write_exec(self.bin / "fake-psql", _PSQL_SHIM)
        # The revision the checkout under test would ship: cutover.sh reads it
        # with `git rev-parse` against its own directory, so the fake deploy
        # has to stamp the same value for the skip to be honest.
        self.revision = subprocess.run(  # nosec B603 B607 - fixed git argv
            ["git", "rev-parse", "HEAD"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        for host in (THOR, ORIN, SPARK):
            (self.hosts / host / "home" / host.removesuffix("-fake")).mkdir(parents=True)

    def account_home(self, host: str, engine: str) -> Path:
        return self.hosts / host / "home" / f"culture-{engine}"

    def bootstrap(self, host: str, engine: str) -> Path:
        """What the ROOT hand-turn leaves behind: an account with a home."""
        home = self.account_home(host, engine)
        (home / ".culture-nodes").mkdir(parents=True)
        return home

    def env(self, **overrides: str) -> dict[str, str]:
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(self.tmp / "operator"),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "FAKE_LOCAL_HOST": SPARK,
            "FAKE_DEPLOYED_REV": str(self.deployed_rev),
            "FAKE_ACTOR_STATE": str(self.actor_state),
            "FAKE_WANT_REVISION": self.revision,
            "CUTOVER_DEPLOY_CMD": str(self.bin / "fake-deploy"),
            "PSQL_CMD": str(self.bin / "fake-psql"),
            "NODES_NAMESPACE_ID": "namespace_1",
        }
        env.update(overrides)
        (self.tmp / "operator").mkdir(exist_ok=True)
        return env

    def run(self, *args: str, **overrides: str) -> subprocess.CompletedProcess:
        return subprocess.run(  # nosec B603 - the real cutover.sh under the shims
            ["bash", str(CUTOVER), *args],
            env=self.env(**overrides),
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def calls(self) -> list[str]:
        return [line for line in self.log.read_text().splitlines() if line.strip()]

    def index_of(self, needle: str) -> int:
        for i, line in enumerate(self.calls()):
            if needle in line:
                return i
        raise AssertionError(f"{needle!r} not in log:\n" + "\n".join(self.calls()))


def _steps(stdout: str) -> dict[str, str]:
    """Map step name -> verdict from the `step <name>: <verdict> — ...` lines."""
    out: dict[str, str] = {}
    for line in stdout.splitlines():
        if not line.startswith("step "):
            continue
        name, _, rest = line[len("step ") :].partition(":")
        out[name.strip()] = rest.strip().split(" ", 1)[0]
    return out


STEP_ORDER = [
    "account-exists",
    "compose-declares-token-key",
    "secrets",
    "deploy",
    "register",
]


# --- argument handling ----------------------------------------------------


@pytest.mark.parametrize(
    "argv",
    [
        (),
        ("thor-fake",),
        ("thor-fake", "pi", "extra"),
        ("thor-fake", "codex"),
        ("nowhere", "pi"),
        ("thor-fake", "pi", "--nonsense"),
    ],
)
def test_bad_arguments_are_a_usage_error(tmp_path: Path, argv):
    h = Harness(tmp_path)
    result = h.run(*argv)
    assert result.returncode == 1, result.stdout + result.stderr
    assert "usage: cutover.sh" in result.stderr
    assert h.calls() == [], "a usage error must not touch a host"


def test_colleague_is_refused_off_spark(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run(ORIN, "colleague", "--yes")
    assert result.returncode == 1
    assert "spark" in result.stderr
    assert h.calls() == []


def test_a_real_run_refuses_without_yes(tmp_path: Path):
    h = Harness(tmp_path)
    h.bootstrap(THOR, "pi")
    result = h.run(THOR, "pi")
    assert result.returncode == 1, result.stdout + result.stderr
    assert "refusing to act without --yes" in result.stderr
    assert "hint:" in result.stderr
    assert h.calls() == [], "the --yes refusal must land before any side effect"


# --- dry run --------------------------------------------------------------


def test_dry_run_reports_every_step_and_touches_nothing(tmp_path: Path):
    h = Harness(tmp_path)
    h.bootstrap(THOR, "pi")
    result = h.run(THOR, "pi", "--dry-run")
    assert result.returncode == 0, result.stdout + result.stderr
    steps = _steps(result.stdout)
    assert list(steps) == STEP_ORDER
    assert all(v.startswith("would-") for v in steps.values()), steps
    # The exact commands, not a paraphrase: this output is what the operator
    # reads before the first real run (spec honesty condition).
    assert "install_bridge_account_env thor-fake pi" in result.stdout
    assert "fake-deploy thor-fake" in result.stdout
    assert "register-actor.sh company/pi-thor http://<thor-lan-ip>:8093" in result.stdout
    assert "NODES_ACTOR_PI_THOR_TOKEN" in result.stdout
    assert "--metadata harness=pi" in result.stdout
    assert "--metadata model=unsloth/Qwen3.8-27B-NVFP4" in result.stdout
    assert "--metadata model_endpoint=http://thor:8000/v1" in result.stdout
    assert "--metadata repository_identity=agentculture/culture-nodes" in result.stdout
    assert "--os-user culture-pi" in result.stdout
    assert h.calls() == [], f"a dry run left side effects: {h.calls()}"
    assert not h.deployed_rev.exists()
    assert not h.actor_state.exists()


def test_dry_run_model_overrides_reach_the_printed_registration(tmp_path: Path):
    h = Harness(tmp_path)
    h.bootstrap(THOR, "qwen")
    result = h.run(
        THOR,
        "qwen",
        "--dry-run",
        "--model",
        "some/other-model",
        "--model-endpoint",
        "http://x:1/v1",
    )
    assert result.returncode == 0, result.stderr
    assert "--metadata model=some/other-model" in result.stdout
    assert "--metadata model_endpoint=http://x:1/v1" in result.stdout
    assert "http://<thor-lan-ip>:8092" in result.stdout


def test_dry_run_names_an_undeclared_token_key_without_failing(tmp_path: Path):
    """A dry run reports; it never fails. The refusal it previews is real."""
    h = Harness(tmp_path)
    h.bootstrap(SPARK, "qwen")
    result = h.run(SPARK, "qwen", "--dry-run")
    assert result.returncode == 0, result.stdout + result.stderr
    assert "step compose-declares-token-key: would-refuse" in result.stdout
    assert "NODES_ACTOR_QWEN_SPARK_TOKEN" in result.stdout
    assert "a real run would refuse here" in result.stderr


# --- preconditions --------------------------------------------------------


def test_an_undeclared_compose_token_key_is_refused_by_name(tmp_path: Path):
    # spark runs a qwen bridge, but its actor is company/qwen-developer with
    # NODES_ACTOR_QWEN_TOKEN -- there is no per-host key of the harness shape
    # for it, so the compose files genuinely do not declare one.
    h = Harness(tmp_path)
    h.bootstrap(SPARK, "qwen")
    result = h.run(SPARK, "qwen", "--yes")
    assert result.returncode != 0, result.stdout
    assert "NODES_ACTOR_QWEN_SPARK_TOKEN" in result.stderr
    assert "step compose-declares-token-key: refuse" in result.stdout
    assert "compose.thor.yml's api block" in result.stdout
    assert "hint:" in result.stderr
    assert "cutover: step compose-declares-token-key failed" in result.stderr
    # It stopped there: no secret was minted, nothing was deployed.
    assert not any(line.startswith("deploy ") for line in h.calls())
    assert not any(line.startswith("openssl ") for line in h.calls())


def test_an_unbootstrapped_account_is_refused_by_name(tmp_path: Path):
    h = Harness(tmp_path)  # no culture-pi account on thor-fake
    result = h.run(THOR, "pi", "--yes")
    assert result.returncode != 0, result.stdout
    assert "step account-exists: refuse" in result.stdout
    assert "culture-pi@thor-fake" in result.stdout
    assert "sudo bash deploy/prod/lanes/unix-user.sh bootstrap pi" in result.stderr
    assert "cutover: step account-exists failed" in result.stderr
    assert not any(line.startswith("deploy ") for line in h.calls())


# --- the full sequence ----------------------------------------------------


def test_a_full_run_does_secrets_then_deploy_then_register(tmp_path: Path):
    h = Harness(tmp_path)
    home = h.bootstrap(THOR, "pi")
    result = h.run(THOR, "pi", "--yes")
    assert result.returncode == 0, result.stdout + result.stderr
    steps = _steps(result.stdout)
    assert list(steps) == STEP_ORDER
    assert steps["secrets"] == "run"
    assert steps["deploy"] == "run"
    assert steps["register"] == "run"

    # The order the acceptance names, read off the shared log.
    secret_write = h.index_of("ENGINE='pi'")
    deployed = h.index_of("deploy thor-fake")
    registered = h.index_of("psql INSERT")
    assert secret_write < deployed < registered, h.calls()

    # The secret really landed in the ACCOUNT, mode 600, from the lane
    # install-secrets.sh owns.
    env_file = home / ".culture-nodes/pi-bridge.env"
    assert env_file.read_text() == "PI_BRIDGE_AUTH_TOKEN=fake-minted-token-value\n"
    assert oct(env_file.stat().st_mode & 0o777) == "0o600"

    # The registration carried the numeric LAN IP derived on the target and
    # every comparison tag.
    row = json.loads(h.actor_state.read_text())
    assert row["endpoint"] == "http://192.168.1.5:8093"
    overlay = json.loads(row["overlay"])
    assert overlay["auth_token_env"] == "NODES_ACTOR_PI_THOR_TOKEN"
    assert overlay["harness"] == "pi"
    assert overlay["model"] == "unsloth/Qwen3.8-27B-NVFP4"
    assert overlay["model_endpoint"] == "http://thor:8000/v1"
    assert overlay["repository_identity"] == "agentculture/culture-nodes"
    assert overlay["os_user"] == "culture-pi"
    assert "company/pi-thor" in result.stdout


def test_a_second_identical_run_skips_every_step(tmp_path: Path):
    h = Harness(tmp_path)
    h.bootstrap(THOR, "pi")
    first = h.run(THOR, "pi", "--yes")
    assert first.returncode == 0, first.stdout + first.stderr
    h.log.write_text("")

    second = h.run(THOR, "pi", "--yes")
    assert second.returncode == 0, second.stdout + second.stderr
    steps = _steps(second.stdout)
    assert list(steps) == STEP_ORDER
    assert steps["secrets"] == "skip"
    assert steps["deploy"] == "skip"
    assert steps["register"] == "skip"
    assert "unchanged" in second.stdout
    # Skipped means NOT done: no re-mint, no re-deploy.
    assert not any(line.startswith("openssl ") for line in h.calls())
    assert not any(line.startswith("deploy ") for line in h.calls())


def test_force_rotates_the_kept_bridge_secret(tmp_path: Path):
    h = Harness(tmp_path)
    home = h.bootstrap(THOR, "qwen")
    assert h.run(THOR, "qwen", "--yes").returncode == 0
    (home / ".culture-nodes/qwen-bridge.env").write_text("QWEN_BRIDGE_AUTH_TOKEN=stale\n")
    h.log.write_text("")

    forced = h.run(THOR, "qwen", "--yes", FORCE_QWEN="1")
    assert forced.returncode == 0, forced.stdout + forced.stderr
    assert _steps(forced.stdout)["secrets"] == "run"
    assert (
        home / ".culture-nodes/qwen-bridge.env"
    ).read_text() == "QWEN_BRIDGE_AUTH_TOKEN=fake-minted-token-value\n"


def test_a_failing_deploy_stops_the_run_and_names_the_step(tmp_path: Path):
    h = Harness(tmp_path)
    h.bootstrap(THOR, "pi")
    _write_exec(
        h.bin / "fake-deploy",
        '#!/usr/bin/env bash\nprintf \'deploy %s\\n\' "$*" >> "$FAKE_LOG"\nexit 7\n',
    )
    result = h.run(THOR, "pi", "--yes")
    assert result.returncode != 0
    assert "cutover: step deploy failed" in result.stderr
    assert "psql" not in "\n".join(h.calls()), "register ran after a failed deploy"


# --- the hand-turn this script never performs -----------------------------


def test_the_script_never_reaches_for_root(tmp_path: Path):
    # Comments and the usage heredoc are PROSE: they name the hand-turn on
    # purpose. What is checked is the executable body.
    body: list[str] = []
    in_heredoc = False
    for line in CUTOVER.read_text().splitlines():
        if in_heredoc:
            in_heredoc = line.strip() != "EOF"
            continue
        if line.rstrip().endswith("<<'EOF'"):
            in_heredoc = True
            continue
        if not line.lstrip().startswith("#"):
            body.append(line)
    assert "bootstrap-accounts.sh" not in "\n".join(body)
    # `sudo` appears only inside the hand-typed command the refusals PRINT --
    # never as something this script executes, so no line may start with it
    # and no substitution may wrap it.
    for line in body:
        assert not line.lstrip().startswith("sudo "), line
        assert "$(sudo" not in line, line
        assert "`sudo" not in line, line
        assert "| sudo" not in line, line
        assert "&& sudo" not in line, line

    h = Harness(tmp_path)
    h.bootstrap(THOR, "pi")
    assert h.run(THOR, "pi", "--yes").returncode == 0
    assert not any(line.startswith("sudo ") for line in h.calls())


# --- the line budgets the design depends on -------------------------------


def test_install_secrets_did_not_grow(tmp_path: Path):
    """cutover.sh lifts install-secrets' lane; it must not have added to it.

    999 is one under the 1000-line hard limit tests/lint/filelength_test.go
    enforces, which is exactly why the lane is lifted rather than wrapped in
    a new subcommand there.
    """
    assert len(INSTALL_SECRETS.read_text().splitlines()) == 999


def test_cutover_stays_under_the_file_length_limit():
    assert len(CUTOVER.read_text().splitlines()) < 1000


def test_cutover_lifts_the_lane_that_install_secrets_still_fences():
    """The marker pair cutover.sh slices on is a shared contract, not a hint."""
    secrets = INSTALL_SECRETS.read_text()
    assert "# QWEN_PI_ACCOUNT_ENV_START" in secrets
    assert "# QWEN_PI_ACCOUNT_ENV_END" in secrets
    assert "QWEN_PI_ACCOUNT_ENV_START" in CUTOVER.read_text()
