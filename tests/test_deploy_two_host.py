"""Fake-host coverage for deploy.sh's two-host r4 sequence (task t2, #230).

Same harness style as tests/test_deploy_runner_env.py: the real script text
between marker comments is extracted and executed under ``set -euo pipefail``
with an ``ssh`` shim on PATH that runs the "remote" command locally against a
per-host temporary HOME. The shim never reaches a network. Every side-effecting
tool the extracted blocks invoke on a host (``docker``, ``curl``,
``systemd-run``, ``~/.local/bin/nodes``) is a recording fake that appends one
line to a shared log, so a test can assert ORDER — which is the whole point of
the r4 procedure — rather than just outcome.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "deploy/prod/deploy.sh"
PREFLIGHT_LANE = ROOT / "deploy/prod/lanes/preflight.sh"
TWO_HOST_LANE = ROOT / "deploy/prod/lanes/two-host.sh"

PREFLIGHT = ("# PREFLIGHT_START", "# PREFLIGHT_END")
TWO_HOST = ("# TWO_HOST_LANE_START", "# TWO_HOST_LANE_END")

# Named so the script's own thor*/orin* lane gates match them.
THOR = "thor-fake"
ORIN = "orin-fake"
REVISION = "0123456789abcdef0123456789abcdef01234567"
STALE = "fedcba9876543210fedcba9876543210fedcba98"

_SSH_SHIM = """#!/usr/bin/env bash
host=$1; shift
printf 'ssh[%s] %s\\n' "$host" "$*" >> "$FAKE_LOG"
# Like a real ssh: the remote command starts in that host's HOME.
cd "$FAKE_HOSTS/$host" || exit 255
exec env FAKE_HOST="$host" HOME="$FAKE_HOSTS/$host" bash -c "$*"
"""

_DOCKER_SHIM = """#!/usr/bin/env bash
printf 'docker[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
case "$*" in
  *pg_dump*)
    [ "${FAKE_DUMP_FAILS:-0}" = 1 ] && { echo "pg_dump: fake failure" >&2; exit 1; }
    name=$(printf '%s' "$*" | sed -n 's#.*-f /backups/\\([^ "'"'"']*\\).*#\\1#p')
    mkdir -p "$HOME/.culture-nodes/backups"
    printf 'PGDMP-fake' > "$HOME/.culture-nodes/backups/$name"
    ;;
  *"ps -q worker"*) echo "cid-worker-$FAKE_HOST" ;;
  *inspect*)
    var="FAKE_IMAGE_REVISION_${FAKE_HOST//-/_}"
    printf '%s\\n' "${!var:-$REVISION}"
    ;;
esac
exit 0
"""

_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
case "$*" in
  */v1alpha1/version*)
    printf '{"version":"fake","revision":"%s"}' "${FAKE_API_REVISION:-$REVISION}" ;;
  */v1alpha1/namespaces*) printf '[{"id":"ns-fake"}]' ;;
  */v1alpha1/readyz*) printf 'ok' ;;
esac
exit 0
"""

_SYSTEMD_RUN_SHIM = """#!/usr/bin/env bash
printf 'systemd-run[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
exit "${FAKE_CUTOVER_EXIT:-0}"
"""

_NODES_SHIM = """#!/usr/bin/env bash
printf 'nodes[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
exit "${FAKE_DOCTOR_EXIT:-0}"
"""


def _block(path: Path, markers: tuple[str, str]) -> str:
    script = path.read_text()
    start = script.index(markers[0])
    end = script.index(markers[1], start) + len(markers[1])
    return script[start:end]


def test_deploy_sources_the_real_two_host_lanes():
    script = DEPLOY.read_text()
    assert 'source "$SCRIPT_DIR/lanes/preflight.sh"' in script
    assert 'source "$SCRIPT_DIR/lanes/two-host.sh"' in script


def _write_exec(path: Path, body: str) -> None:
    path.write_text(body)
    path.chmod(0o755)


def _git(repo: Path, *args: str) -> None:
    subprocess.run(  # nosec B603 B607 - fixed git argv against a temp repo
        ["git", "-c", "user.name=t", "-c", "user.email=t@example.test", *args],
        cwd=repo,
        check=True,
        capture_output=True,
    )


def _agent_checkout(host_home: Path, state: str) -> Path:
    repo = host_home / "git/culture-nodes-agent"
    repo.mkdir(parents=True)
    _git(repo, "init", "-q", "-b", "main")
    (repo / "README.md").write_text("agent checkout\n")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-q", "-m", "one")
    if state == "dirty":
        (repo / "README.md").write_text("harvest me\n")
    elif state == "detached":
        _git(repo, "checkout", "-q", "--detach")
    elif state != "clean":
        raise ValueError(state)
    return repo


class Harness:
    def __init__(self, tmp_path: Path):
        self.tmp = tmp_path
        self.log = tmp_path / "calls.log"
        self.log.touch()
        self.hosts = tmp_path / "hosts"
        self.bin = tmp_path / "bin"
        self.bin.mkdir()
        _write_exec(self.bin / "ssh", _SSH_SHIM)
        _write_exec(self.bin / "docker", _DOCKER_SHIM)
        _write_exec(self.bin / "curl", _CURL_SHIM)
        _write_exec(self.bin / "systemd-run", _SYSTEMD_RUN_SHIM)
        for host in (THOR, ORIN):
            home = self.hosts / host
            (home / ".local/bin").mkdir(parents=True)
            _write_exec(home / ".local/bin/nodes", _NODES_SHIM)
            (home / ".culture-nodes").mkdir()
            (home / ".culture-nodes/prod.env").write_text("NODES_DATABASE_URL=postgres://fake\n")
        self.thor_home = self.hosts / THOR
        self.orin_home = self.hosts / ORIN
        thor_compose = self.thor_home / "culture-nodes-prod/deploy/prod"
        thor_compose.mkdir(parents=True)
        (thor_compose / "compose.thor.yml").write_text("services: {scheduler: {}}\n")
        # orin has a deployed stack by default; a test deletes this to model
        # a first deploy where orin does not exist yet.
        orin_compose = self.orin_home / "culture-nodes-prod/deploy/prod"
        orin_compose.mkdir(parents=True)
        (orin_compose / "compose.orin.yml").write_text("services: {worker: {}}\n")

    def run(
        self, lane: str, checkout: str = "clean", **fake_env: str
    ) -> subprocess.CompletedProcess:
        # "absent" models a host that was never deployed (no agent checkout):
        # the preflight refuses it unless FIRST_DEPLOY=1 is declared.
        if checkout != "absent":
            _agent_checkout(self.thor_home, checkout)
            # orin is doctored by the thor lane too (Qodo 3 on #244).
            _agent_checkout(self.orin_home, "clean")
        if lane == "thor":
            body = "\n".join(
                [
                    _block(PREFLIGHT_LANE, PREFLIGHT),
                    _block(TWO_HOST_LANE, TWO_HOST),
                    "thor_two_host_lane",
                    'deploy_summary "thor"',
                ]
            )
            host = THOR
        elif lane == "orin":
            body = "\n".join(
                [
                    _block(TWO_HOST_LANE, TWO_HOST),
                    "NS=ns-fake",
                    "orin_two_host_lane",
                    'deploy_summary "orin"',
                ]
            )
            host = ORIN
        else:
            raise ValueError(lane)
        script = (
            "set -euo pipefail\n"
            "say() { printf '==> %s\\n' \"$*\"; }\n"
            f"HOST={host}\nREMOTE_DIR=culture-nodes-prod\nREVISION={REVISION}\nBRANCH=HEAD\n"
            + body
            + "\n"
        )
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(self.tmp / "operator"),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "REVISION": REVISION,
            "THOR_HOST": THOR,
            "ORIN_HOST": ORIN,
            **fake_env,
        }
        return subprocess.run(  # nosec B603 - fixed bash over extracted repository script
            ["bash", "-c", script],
            env=env,
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def calls(self) -> list[str]:
        return self.log.read_text().splitlines()

    def first(self, *needles: str) -> int:
        """Index of the first logged call containing every needle."""
        for i, line in enumerate(self.calls()):
            if all(needle in line for needle in needles):
                return i
        raise AssertionError(f"{needles!r} never happened; log:\n" + "\n".join(self.calls()))

    def never(self, *needles: str) -> None:
        hits = [line for line in self.calls() if all(needle in line for needle in needles)]
        assert not hits, f"{needles!r} happened: {hits}"


def _docker(host: str, tail: str) -> tuple[str, str]:
    """A compose call on ``host``: the host tag plus the compose file and verb.

    The remote bash expands ``~`` in ``--env-file`` (the fake host has a real
    HOME), so the match is on what is stable: which host, which compose file,
    which verb.
    """
    return f"docker[{host}] compose ", f"-f compose.{host.split('-')[0]}.yml {tail}"


def test_dirty_agent_checkout_refuses_before_anything_is_stopped(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", checkout="dirty")
    assert result.returncode != 0
    assert "DIRTY" in result.stderr
    h.never("docker[")
    h.never("systemd-run[")


def test_detached_agent_checkout_refuses_before_anything_is_stopped(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", checkout="detached")
    assert result.returncode != 0
    assert "DETACHED" in result.stderr
    h.never("docker[")


def test_preflight_doctor_failure_refuses_before_anything_is_stopped(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", FAKE_DOCTOR_EXIT="1")
    assert result.returncode != 0
    assert "BEFORE the deploy" in result.stderr
    h.first(f"nodes[{THOR}] doctor")
    h.never("docker[")


def test_thor_lane_records_the_r4_order_and_resumes_on_parity(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor")
    assert result.returncode == 0, result.stderr

    order = [
        f"nodes[{THOR}] doctor",
        _docker(THOR, "run --rm --no-deps -T backup"),
        _docker(THOR, "stop scheduler worker api"),
        _docker(ORIN, "stop worker"),
        _docker(THOR, "run --rm migrate"),
        f"systemd-run[{THOR}]",
        _docker(THOR, "up -d --scale scheduler=0"),
        _docker(ORIN, "up -d worker"),
        f"curl[{THOR}] -fsS http://localhost:18080/v1alpha1/version",
        f"docker[{THOR}] inspect",
        f"docker[{ORIN}] inspect",
        _docker(THOR, "up -d scheduler"),
    ]
    positions = [h.first(*step) if isinstance(step, tuple) else h.first(step) for step in order]
    assert positions == sorted(positions), "\n".join(h.calls())
    # The cutover one-shot is the real binary path, not a rebuilt cmd/nodes.
    assert "nodes-cutover" in h.calls()[h.first(f"systemd-run[{THOR}]")]
    # No compose rebuild anywhere: the label the parity check reads rides the
    # explicit `docker build`, and a `compose up --build` would re-tag it away.
    h.never("--build")

    dumps = list((h.thor_home / ".culture-nodes/backups").glob("predeploy-*.dump"))
    assert len(dumps) == 1 and dumps[0].stat().st_size > 0
    assert dumps[0].name in result.stdout, result.stdout
    assert "sweep schedule: resumed" in result.stdout


def test_dump_failure_refuses_before_anything_is_stopped(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", FAKE_DUMP_FAILS="1")
    assert result.returncode != 0
    assert "pg_dump" in result.stderr
    h.never("stop scheduler worker api")
    h.never(f"docker[{ORIN}]")


def test_parity_mismatch_leaves_the_scheduler_down_and_exits_nonzero(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", **{f"FAKE_IMAGE_REVISION_{ORIN.replace('-', '_')}": STALE})
    assert result.returncode == 3, result.stderr
    h.first(*_docker(ORIN, "up -d worker"))
    h.never("up -d scheduler")
    assert "sweep schedule: PAUSED" in result.stdout
    assert STALE[:12] in result.stdout + result.stderr
    assert "deploy.sh orin" in result.stdout + result.stderr


def test_api_revision_mismatch_refuses_resume(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("thor", FAKE_API_REVISION=STALE)
    assert result.returncode == 3
    h.never("up -d scheduler")


def test_first_deploy_without_an_orin_stack_skips_the_quiesce_and_resumes(tmp_path: Path):
    h = Harness(tmp_path)
    (h.orin_home / "culture-nodes-prod/deploy/prod/compose.orin.yml").unlink()
    result = h.run("thor")
    assert result.returncode == 0, result.stderr
    h.never(f"docker[{ORIN}]")
    h.first(*_docker(THOR, "up -d scheduler"))


def test_unreachable_orin_refuses_in_preflight(tmp_path: Path):
    h = Harness(tmp_path)
    # An ssh to a host the shim cannot serve: point ORIN_HOST at a name with
    # no fake home, and make the probe fail the way a dead ssh does.
    _write_exec(
        h.bin / "ssh",
        _SSH_SHIM.replace("exec env", '[ "$host" = no-such-host ] && exit 255\nexec env'),
    )
    result = h.run("thor", ORIN_HOST="no-such-host")
    assert result.returncode != 0
    assert "no-such-host" in result.stderr
    h.never("docker[")


def test_orin_lane_parity_check_resumes_thors_scheduler(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("orin")
    assert result.returncode == 0, result.stderr
    h.first(f"curl[{THOR}] -fsS http://localhost:18080/v1alpha1/version")
    h.first(f"docker[{ORIN}] inspect")
    h.first(*_docker(THOR, "up -d scheduler"))
    assert "sweep schedule: resumed" in result.stdout


def test_orin_lane_parity_mismatch_refuses_resume(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("orin", **{f"FAKE_IMAGE_REVISION_{THOR.replace('-', '_')}": STALE})
    assert result.returncode == 3
    h.never("up -d scheduler")
    assert "deploy.sh thor" in result.stdout + result.stderr


def test_missing_agent_checkout_refuses_unless_first_deploy_is_declared(tmp_path):
    """A host that cannot be doctored is refused before anything changes; the
    only exception is FIRST_DEPLOY=1, declared by the operator (Qodo 3, #244)."""
    refused_dir, declared_dir = tmp_path / "refused", tmp_path / "declared"
    refused_dir.mkdir()
    declared_dir.mkdir()
    refused = Harness(refused_dir).run("thor", checkout="absent")
    assert refused.returncode != 0, refused.stdout
    assert "FIRST_DEPLOY=1" in refused.stderr, refused.stderr
    declared = Harness(declared_dir).run("thor", checkout="absent", FIRST_DEPLOY="1")
    assert declared.returncode == 0, declared.stderr
