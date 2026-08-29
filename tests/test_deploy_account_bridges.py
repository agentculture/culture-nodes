"""Fake-host coverage for deploy.sh's engine-account bridge lanes (task t7, #243).

Builds on tests/test_deploy_unix_user.py's harness -- the per-ACCOUNT ssh
axis (``culture-<engine>@<host>``, ``@localhost`` on spark) plus the
recording fakes -- and runs the REAL ``deploy/prod/deploy.sh`` end to end
against fake hosts, the way tests/deploy/runnerbinaryship_test.go does, so
the ORDER and the USER of every step are what is asserted:

* ``deploy.sh spark`` runs bridge lanes only: the five spark units land in
  the fake ``culture-claude`` / ``culture-qwen`` homes, the rendered configs
  carry no ``NODES_HUMAN_DECISION_TOKEN``, and neither compose nor the
  nodes-runner lane is ever invoked;
* ``deploy.sh orin`` still runs the runner and compose steps as the LOGIN
  user while the codex unit is installed into ``culture-codex``;
* the session-in-flight check refuses BEFORE any login-user unit is stopped;
* a host whose account was never bootstrapped is refused with the exact
  hand-typed command, and nothing on it is stopped.

Every side-effecting tool is a shim on PATH that appends one line to the
shared log; ``git``, ``tar``, ``sed`` and ``python3`` are real, which is
what makes the rendered files worth reading back.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
from pathlib import Path

from tests.test_deploy_unix_user import (
    ORIN,
    ROOT,
    SPARK,
    THOR,
    Harness,
    _git,
    _write_exec,
)

DEPLOY = ROOT / "deploy/prod/deploy.sh"
LANE = ROOT / "deploy/prod/lanes/account-bridges.sh"
MARKERS = ("# ACCOUNT_BRIDGES_LANE_START", "# ACCOUNT_BRIDGES_LANE_END")

CLAUDE_ROLES = ("developer", "planner", "verifier", "intake")
CLAUDE_PORTS = {"developer": 8088, "planner": 8087, "verifier": 8089, "intake": 8086}
SPARK_UNITS = tuple(f"culture-nodes-claude-{r}" for r in CLAUDE_ROLES) + (
    "culture-nodes-qwen-developer",
)

# The shim from the unix-user harness, plus two things the deploy needs that
# a lane test does not: `ssh -o Option=value target` (the reachability probe
# uses BatchMode) and the login user's own tools logging WHICH user ran them.
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
strip=()
for v in $(compgen -e UNIX_USER_) SKIP_SESSION_CHECK FIRST_DEPLOY SKIP_CODEX_PREFLIGHT; do
  strip+=(-u "$v")
done
exec env "${strip[@]}" FAKE_HOST="$host" FAKE_USER="$user" FAKE_UID=1000 HOME="$home" \\
  bash -c "$*"
"""

# systemctl logs host AND user, answers is-active/show so the health waits
# pass, and records every stop/disable/restart/enable in order.
_SYSTEMCTL_SHIM = """#!/usr/bin/env bash
printf 'systemctl[%s:%s] %s\\n' "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"
case "$*" in
  *is-active*) echo active ;;
  *NRestarts*) echo 0 ;;
esac
exit 0
"""

_GETENT_SHIM = """#!/usr/bin/env bash
if [ "$1" = hosts ]; then printf '192.168.1.5 %s\\n' "$2"; exit 0; fi
[ "$1" = passwd ] || exit 1
name=$2
login=${FAKE_HOST%-fake}
if [ "$name" = "$login" ]; then
  printf '%s:x:1000:1000::%s:/bin/bash\\n' "$name" "$FAKE_HOSTS/$FAKE_HOST/home/$login"
elif [ -d "$FAKE_HOSTS/$FAKE_HOST/home/$name" ]; then
  printf '%s:x:1500:1500::%s:/bin/bash\\n' "$name" "$FAKE_HOSTS/$FAKE_HOST/home/$name"
else
  exit 2
fi
"""

# The fake uv the curl shim installs: `uv tool install` drops a console
# script into ~/.local/bin the way the real one does, so the units and the
# end-of-lane doctor find what they exec.
_UV_BODY = r"""#!/usr/bin/env bash
printf 'uv[%s:%s] %s\n' "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"
if [ "$1" = tool ] && [ "$2" = install ]; then
  mkdir -p "$HOME/.local/bin"
  case "${@: -1}" in
    *adapters/codex) bin=codex-bridge ;;
    *adapters/claude-code) bin=claude-code-bridge ;;
    *adapters/qwen) bin=qwen-bridge ;;
    culture-nodes) bin=nodes ;;
    *) bin=${@: -1} ;;
  esac
  {
    printf '#!/usr/bin/env bash\n'
    printf 'printf "%s[%%s:%%s] %%s\\n" "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"\n' "$bin"
    printf 'exit 0\n'
  } > "$HOME/.local/bin/$bin"
  chmod +x "$HOME/.local/bin/$bin"
fi
exit 0
"""

_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
url=; out=; config=0
while [ $# -gt 0 ]; do
  case "$1" in -o) out=$2; shift ;; -K) config=1; shift ;; -*) ;; *) url=$1 ;; esac
  shift
done
if [ "$config" = 1 ]; then
  # issue-dialin-credential.sh's mint: the whole request rides stdin.
  cfg=$(cat)
  party=$(printf '%s' "$cfg" | sed -n 's/.*party_key[^a-z]*\\([a-z][^"\\\\]*\\).*/\\1/p')
  cred="fake-dialin-for-$party"
  digest=$(printf '%s' "$cred" | sha256sum | cut -d' ' -f1)
  printf '{"party_key":"%s","credential":"%s","digest_sha256":"%s","issued_at":"now"}' \\
    "$party" "$cred" "$digest"
  exit 0
fi
case "$url" in
  *astral.sh/uv/install.sh*)
    printf 'mkdir -p "$HOME/.local/bin"; cat > "$HOME/.local/bin/uv" <<"UVEOF"\\n%s\\nUVEOF\\n' \\
      "$(cat "$FAKE_UV_BODY")"
    printf 'chmod +x "$HOME/.local/bin/uv"\\n' ;;
  *claude.ai/install.sh*)
    printf '%s\\n' 'v=${1:-latest}; mkdir -p "$HOME/.local/bin"' \\
      'bin="$HOME/.local/bin/claude"' \\
      'printf "#!/usr/bin/env bash\\necho \\"%s (Claude Code)\\"\\n" "$v" > "$bin"' \\
      'chmod +x "$HOME/.local/bin/claude"' ;;
  *install-qwen-standalone.sh*)
    printf '%s\\n' 'v=latest; while [ $# -gt 0 ]; do [ "$1" = --version ] && v=$2; shift; done' \\
      'mkdir -p "$HOME/.local/bin"' \\
      'printf "#!/usr/bin/env bash\\necho %s\\n" "$v" > "$HOME/.local/bin/qwen"' \\
      'chmod +x "$HOME/.local/bin/qwen"' ;;
  *github.com/openai/codex/releases/download/rust-v*)
    v=$(printf '%s' "$url" | sed -n 's#.*/rust-v\\([0-9.]*\\)/.*#\\1#p')
    asset=${url##*/}; name=${asset%.tar.gz}
    case "$asset" in codex-package-*) ;;
      *) echo "curl fake: expected the codex PACKAGE, got $asset" >&2; exit 22 ;; esac
    tmp=$(mktemp -d)
    mkdir -p "$tmp/$name/bin" "$tmp/$name/codex-path" "$tmp/$name/codex-resources"
    printf '#!/usr/bin/env bash\\necho "codex-cli %s"\\n' "$v" > "$tmp/$name/bin/codex"
    printf '#!/usr/bin/env bash\\necho code-mode-host\\n' > "$tmp/$name/bin/codex-code-mode-host"
    printf '#!/usr/bin/env bash\\necho rg\\n' > "$tmp/$name/codex-path/rg"
    chmod +x "$tmp/$name/bin/codex" "$tmp/$name/bin/codex-code-mode-host" "$tmp/$name/codex-path/rg"
    printf '{"layoutVersion": 1, "version": "%s"}\\n' "$v" > "$tmp/$name/codex-package.json"
    tar -C "$tmp" -czf "$out" "$name"
    rm -rf "$tmp" ;;
  */v1alpha1/version*) printf '{"version":"fake","revision":"%s"}' "$FAKE_REVISION" ;;
  */v1alpha1/namespaces*) printf '[{"id":"ns-fake"}]' ;;
  */v1alpha1/readyz*) printf 'ok' ;;
  */v1alpha1/dial-in-presence*) printf '{"items":[]}' ;;
  */v1alpha1/actors*) cat "$FAKE_ACTORS_JSON" ;;
  *) echo "curl fake: no such URL $url" >&2; exit 22 ;;
esac
"""

_DOCKER_SHIM = """#!/usr/bin/env bash
printf 'docker[%s:%s] %s\\n' "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"
case "$*" in
  *"FROM namespaces"*) echo ns-fake ;;
  *"SELECT revision"*) echo "1|http://192.168.1.5:8086|f" ;;
  *"SELECT id FROM actors"*) echo "actor-row-fake" ;;
  *"ps -q worker"*) echo "cid-worker-$FAKE_HOST" ;;
  *inspect*) printf '%s\\n' "$FAKE_REVISION" ;;
esac
exit 0
"""

_GO_SHIM = """#!/usr/bin/env bash
printf 'go[%s:%s] %s\\n' "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"
out=
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; *) shift ;; esac; done
[ -n "$out" ] && { mkdir -p "$(dirname "$out")"; : > "$out"; }
exit 0
"""

_LOG_USER_SHIM = """#!/usr/bin/env bash
printf '%s[%s:%s] %s\\n' "$(basename "$0")" "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"
exit 0
"""


def _block() -> str:
    script = LANE.read_text()
    start = script.index(MARKERS[0])
    end = script.index(MARKERS[1], start) + len(MARKERS[1])
    return script[start:end]


def _compose_keys() -> list[str]:
    keys: list[str] = []
    for name in ("compose.thor.yml", "compose.orin.yml"):
        body = (ROOT / "deploy/prod" / name).read_text().replace("$$", "@@")
        for key in re.findall(r"\$\{([A-Za-z_][A-Za-z0-9_]*)", body):
            if key not in keys:
                keys.append(key)
    return keys


def _login_config(role: str, port: int, home: Path) -> dict:
    cfg = {
        "actor_id": f"actor_{role}_row",
        "port": port,
        "host": "0.0.0.0",
        "auth_token": f"login-token-{role}",
        "repo_allowlist": [str(home / f"git/.worktrees.culture-nodes/owe-{role}")],
        "always_async": True,
        "state_dir": str(home / f".local/state/culture-nodes-bridges/{role}"),
    }
    if role == "developer":
        cfg["claude_env"] = {
            "NODES_API_URL": "http://192.168.1.146:18080",
            "NODES_HUMAN_DECISION_TOKEN": "human-bearer-must-not-cross",
        }
    return cfg


class DeployHarness(Harness):
    """The unix-user harness with the deploy's other tools faked too."""

    def __init__(self, tmp_path: Path):
        super().__init__(tmp_path)
        # Ship the WORKING TREE, not HEAD: the deploy archives `$BRANCH`, and a
        # tree object written from a scratch index is what makes this test
        # exercise the lane files as they are on disk, committed or not.
        index = tmp_path / "scratch-index"
        subprocess.run(  # nosec B603 B607 - fixed git argv, scratch index
            ["git", "add", "-A", "--", "deploy", "tests", "adapters", "examples"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            env={**os.environ, "GIT_INDEX_FILE": str(index)},
        )
        self.revision = subprocess.run(  # nosec B603 B607
            ["git", "write-tree"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
            env={**os.environ, "GIT_INDEX_FILE": str(index)},
        ).stdout.strip()
        _write_exec(self.bin / "ssh", _SSH_SHIM)
        _write_exec(self.bin / "systemctl", _SYSTEMCTL_SHIM)
        _write_exec(self.bin / "getent", _GETENT_SHIM)
        _write_exec(self.bin / "curl", _CURL_SHIM)
        _write_exec(self.bin / "docker", _DOCKER_SHIM)
        _write_exec(self.bin / "go", _GO_SHIM)
        for tool in ("headspace", "sleep", "loginctl", "chown"):
            _write_exec(self.bin / tool, _LOG_USER_SHIM)
        self.uv_body = tmp_path / "uv-body.sh"
        self.uv_body.write_text(_UV_BODY)
        self.actors_json = tmp_path / "actors.json"
        self.actors_json.write_text(
            json.dumps(
                {
                    "items": [
                        {
                            "id": f"row-{key.split('/')[1]}",
                            "actor_key": key,
                            "revision": 1,
                            "endpoint_ref": f"http://192.168.1.5:{port}",
                            "metadata": {"auth_token_env": "X"},
                        }
                        for key, port in (
                            ("company/codex-thor-fake", 8086),
                            ("company/codex-orin-fake", 8086),
                            ("company/developer", 8088),
                            ("company/planner", 8087),
                            ("company/verifier", 8089),
                            ("company/intake", 8086),
                            ("company/qwen-developer", 8092),
                        )
                    ]
                }
            )
        )
        for host in (THOR, ORIN, SPARK):
            home = self.login_home(host)
            # The deploy's preflight wants a CLEAN login checkout and a nodes
            # CLI; the base harness leaves the checkout dirty on purpose.
            _git(home / "git/culture-nodes-agent", "checkout", "--", "README.md")
            (home / ".local/bin").mkdir(parents=True, exist_ok=True)
            _write_exec(home / ".local/bin/nodes", _LOG_USER_SHIM)
            # Every compose-declared key, so audit-credentials.sh is satisfied.
            lines = [f"{k}=placeholder" for k in _compose_keys()]
            lines.append("NODES_INBOUND_ISSUANCE_TOKEN_SECRET=issuance-fake")
            (home / ".culture-nodes/prod.env").write_text("\n".join(lines) + "\n")
            (home / ".culture-nodes/codex-bridge.env").write_text(
                "CODEX_BRIDGE_AUTH_TOKEN=login-codex-token\n"
            )
            (home / ".culture-nodes/codex-bridge.env").chmod(0o600)
            (home / ".config/systemd/user").mkdir(parents=True)
            compose = home / "culture-nodes-prod/deploy/prod"
            compose.mkdir(parents=True)
            (compose / "compose.thor.yml").write_text("services: {scheduler: {}}\n")
            (compose / "compose.orin.yml").write_text("services: {worker: {}}\n")
        spark = self.login_home(SPARK)
        cfg_dir = spark / ".config/culture-nodes-bridges"
        cfg_dir.mkdir(parents=True)
        for role in CLAUDE_ROLES:
            (cfg_dir / f"{role}.json").write_text(
                json.dumps(_login_config(role, CLAUDE_PORTS[role], spark))
            )
        qwen = _login_config("qwen-developer", 8092, spark)
        qwen["default_sandbox"] = "workspace-write"
        (cfg_dir / "qwen-developer.json").write_text(json.dumps(qwen))
        for unit in SPARK_UNITS:
            (spark / f".config/systemd/user/{unit}.service").write_text("[Unit]\n")

    def env(self, host: str, **fake_env: str) -> dict[str, str]:
        operator_home = self.login_home(SPARK) if host == SPARK else self.tmp / "operator"
        operator_home.mkdir(exist_ok=True)
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(operator_home),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "FAKE_LOCAL_HOST": SPARK,
            "FAKE_REVISION": self.revision,
            "BRANCH": self.revision,
            "FAKE_UV_BODY": str(self.uv_body),
            "FAKE_ACTORS_JSON": str(self.actors_json),
            "UNIX_USER_REPO_URL": str(self.origin),
            "NODES_API_URL": f"http://{THOR}:18080",
            "NODES_CONTROL_HOST": THOR,
            "THOR_HOST": THOR,
            "ORIN_HOST": ORIN,
            "SKIP_CODEX_PREFLIGHT": "1",
            # The spark arm runs its login-user half LOCALLY (spark cannot ssh
            # itself), so the local shims log as spark's own user.
            "FAKE_HOST": host,
            "FAKE_USER": host.removesuffix("-fake"),
            **fake_env,
        }
        return env

    def deploy(self, host: str, **fake_env: str) -> subprocess.CompletedProcess:
        return subprocess.run(  # nosec B603 - the real deploy.sh under the shims
            ["bash", str(DEPLOY), host],
            env=self.env(host, **fake_env),
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def bootstrap(self, host: str, *engines: str) -> None:
        result = self.run(f"unix_user_bootstrap {host} {' '.join(engines)}", host=host)
        assert result.returncode == 0, result.stderr + result.stdout
        # install-secrets.sh's account step (mirrors the login user's bridge
        # token) is what puts codex-bridge.env into the account; seeded here.
        for engine in engines:
            if engine == "codex":
                cn = self.account_home(host, "codex") / ".culture-nodes"
                cn.mkdir(exist_ok=True)
                (cn / "codex-bridge.env").write_text("CODEX_BRIDGE_AUTH_TOKEN=login-codex-token\n")
                (cn / "codex-bridge.env").chmod(0o600)
        self.clear_log()


# --- shape --------------------------------------------------------------------


def test_lane_parses_and_deploy_sources_it():
    subprocess.run(["bash", "-n", str(LANE)], check=True)  # nosec B603 B607
    subprocess.run(["bash", "-n", str(DEPLOY)], check=True)  # nosec B603 B607
    script = DEPLOY.read_text()
    assert 'source "$SCRIPT_DIR/lanes/unix-user.sh"' in script
    assert 'source "$SCRIPT_DIR/lanes/account-bridges.sh"' in script
    assert len(script.splitlines()) <= 1000
    for fn in ("account_bridges_spark_lane", "account_bridges_summary"):
        assert f"{fn}()" in _block(), f"{fn} is not defined inside the marker block"


def test_marker_blocks_of_the_other_lanes_are_intact():
    for lane, markers in (
        ("preflight.sh", ("# PREFLIGHT_START", "# PREFLIGHT_END")),
        ("two-host.sh", ("# TWO_HOST_LANE_START", "# TWO_HOST_LANE_END")),
        ("runner-env-write.sh", ("# RUNNER_ENV_WRITE_START", "# RUNNER_ENV_WRITE_END")),
        ("unix-user.sh", ("# UNIX_USER_LANE_START", "# UNIX_USER_LANE_END")),
    ):
        text = (ROOT / "deploy/prod/lanes" / lane).read_text()
        assert text.index(markers[0]) < text.index(markers[1]), lane


def test_the_qwen_unit_is_versioned_and_execs_the_account_copy():
    unit = (ROOT / "deploy/prod/culture-nodes-qwen-developer.service").read_text()
    directives = [line for line in unit.splitlines() if line and not line.startswith("#")]
    assert "ExecStart=%h/.local/bin/qwen-bridge" in directives
    assert not any("uv run" in line or "/home/spark" in line for line in directives)
    assert "QWEN_BRIDGE_CONFIG=%h/.config/culture-nodes-bridges/qwen-developer.json" in unit


def test_the_five_spark_templates_carry_no_decision_token_and_no_operator_path():
    for name in (
        "claude-developer",
        "claude-planner",
        "claude-verifier",
        "claude-intake",
        "qwen-developer",
    ):
        text = (ROOT / "deploy/prod" / f"{name}.json.template").read_text()
        assert "NODES_HUMAN_DECISION_TOKEN" not in text, name
        assert "/home/spark" not in text and "upkeep-lane" not in text, name
        assert "__HOME__" in text, name
        json.loads(text.replace("__HOME__", "/h").replace("__NODES_API_URL__", "http://x"))


# --- deploy.sh spark ---------------------------------------------------------------


def test_spark_installs_the_five_units_under_the_accounts_and_never_touches_compose_or_runner(
    tmp_path: Path,
):
    h = DeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    result = h.deploy(SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    claude = h.account_home(SPARK, "claude")
    qwen = h.account_home(SPARK, "qwen")
    for role in CLAUDE_ROLES:
        unit = claude / f".config/systemd/user/culture-nodes-claude-{role}.service"
        assert (
            unit.read_text() == (ROOT / "deploy/prod" / unit.name).read_text()
        ), "unit files are byte-for-byte"
        h.first(f"systemctl[{SPARK}:culture-claude] --user restart culture-nodes-claude-{role}")
        h.first(f"systemctl[{SPARK}:culture-claude] --user enable culture-nodes-claude-{role}")
    assert (qwen / ".config/systemd/user/culture-nodes-qwen-developer.service").exists()
    h.first(f"systemctl[{SPARK}:culture-qwen] --user restart culture-nodes-qwen-developer")
    # Bridge lanes only: no image, no compose lifecycle, no runner, no
    # cutover. (thor's psql, reached through `docker compose exec` as thor's
    # login user, is the registry write and is the one docker call allowed.)
    h.never(f"docker[{SPARK}")
    h.never("docker[", "build")
    h.never("docker[", "compose", " up ")
    h.never("docker[", "compose", " stop ")
    h.never("docker[", "compose", " run ")
    h.never("go[")
    h.never("nodes-runner")
    h.never("systemd-run[")
    h.never("headspace")
    # The login user's five units are stopped AND disabled, never removed.
    login = h.login_home(SPARK)
    for unit in SPARK_UNITS:
        h.first(f"systemctl[{SPARK}:spark] --user stop {unit}")
        h.first(f"systemctl[{SPARK}:spark] --user disable {unit}")
        assert (login / f".config/systemd/user/{unit}.service").exists(), "disable, never rm"
    # The archive was shipped INTO each account (the login user's copy is
    # unreadable from there) and the bridges installed from it as copies.
    for home in (claude, qwen):
        assert (home / "culture-nodes-prod/deploy/prod/deploy.sh").exists()
    stamp = json.loads(
        (
            claude / "culture-nodes-prod/adapters/claude-code/src/claude_code_bridge/_revision.json"
        ).read_text()
    )
    assert stamp["revision"] == h.revision
    assert (
        json.loads(
            (qwen / "culture-nodes-prod/adapters/qwen/src/qwen_bridge/_revision.json").read_text()
        )["revision"]
        == h.revision
    )
    assert h.first(
        "uv[", "culture-claude", "tool install --force ./culture-nodes-prod/adapters/claude-code"
    ) > h.first("ssh[culture-claude@localhost]", "_revision.json")
    h.first("uv[", "culture-qwen", "tool install --force ./culture-nodes-prod/adapters/qwen")
    assert (claude / ".local/bin/claude-code-bridge").exists()
    assert (qwen / ".local/bin/qwen-bridge").exists()
    # The rollback pairs are printed, one per unit, as account-then-login.
    for unit in SPARK_UNITS:
        assert f"systemctl --user stop {unit}" in result.stdout
        assert f"systemctl --user start {unit}" in result.stdout
    assert "culture-claude@localhost" in result.stdout and "culture-qwen@localhost" in result.stdout


def test_spark_renders_the_configs_into_the_accounts_without_the_decision_token(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    result = h.deploy(SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    claude = h.account_home(SPARK, "claude")
    for role in CLAUDE_ROLES:
        path = claude / f".config/culture-nodes-bridges/{role}.json"
        cfg = json.loads(path.read_text())
        assert oct(path.stat().st_mode & 0o777) == "0o600"
        assert "NODES_HUMAN_DECISION_TOKEN" not in path.read_text()
        assert cfg["port"] == CLAUDE_PORTS[role], role
        assert cfg["repo_allowlist"] == [str(claude / f"git/culture-nodes-{role}")], role
        assert cfg["state_dir"].startswith(str(claude)), role
        assert cfg["claude_bin"] == str(claude / ".local/bin/claude")
        # Carried from the login user's config: the externally issued token
        # and the registered row id -- and nothing else from that file.
        assert cfg["auth_token"] == f"login-token-{role}"
        assert cfg["actor_id"] == f"actor_{role}_row"
        assert "/home/spark" not in path.read_text() and "upkeep-lane" not in path.read_text()
    developer = json.loads((claude / ".config/culture-nodes-bridges/developer.json").read_text())
    assert developer["claude_env"] == {"NODES_API_URL": f"http://{THOR}:18080"}
    assert developer["custody"]["checkout"] == str(claude / "git/culture-nodes-developer")
    qwen_cfg = json.loads(
        (
            h.account_home(SPARK, "qwen") / ".config/culture-nodes-bridges/qwen-developer.json"
        ).read_text()
    )
    assert qwen_cfg["port"] == 8092
    assert qwen_cfg["default_sandbox"] == "workspace-write"
    assert qwen_cfg["qwen_bin"] == str(h.account_home(SPARK, "qwen") / ".local/bin/qwen")
    assert qwen_cfg["auth_token"] == "login-token-qwen-developer"
    # grep the whole account the way spec h32 does -- minus the shipped
    # source archive, whose code and tests NAME the variable (this file
    # included); what must be clean is everything the account runs from.
    hits = subprocess.run(  # nosec B603 B607
        [
            "grep",
            "-rl",
            "--exclude-dir=culture-nodes-prod",
            "NODES_HUMAN_DECISION_TOKEN",
            str(claude),
        ],
        capture_output=True,
        text=True,
    )
    assert hits.stdout == "", hits.stdout


def test_spark_reissues_the_developer_dialin_into_the_account_and_registers_os_user(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    result = h.deploy(SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    dialin = h.account_home(SPARK, "claude") / ".culture-nodes/dialin/developer.env"
    assert dialin.exists(), "issue-dialin-credential.sh developer culture-claude@localhost"
    text = dialin.read_text()
    assert "CLAUDE_CODE_BRIDGE_DIAL_TOKEN=fake-dialin-for-company/developer" in text
    assert hashlib.sha256(b"fake-dialin-for-company/developer").hexdigest() in text
    assert oct(dialin.stat().st_mode & 0o777) == "0o600"
    # The mint ran on the control host as the operator, the delivery as the
    # account (one pipeline, so the two ssh calls start together: presence,
    # not order, is what the log can say).
    h.first(f"ssh[{THOR}]", "PARTY='company/developer'", "ROUTE_SUFFIX")
    h.first(
        "ssh[culture-claude@localhost]",
        "PARTY='company/developer'",
        "DEST_REL='.culture-nodes/dialin/developer.env'",
    )
    # register-actor.sh --os-user for each of the five, through thor's psql.
    for key, account in (
        ("company/developer", "culture-claude"),
        ("company/planner", "culture-claude"),
        ("company/verifier", "culture-claude"),
        ("company/intake", "culture-claude"),
        ("company/qwen-developer", "culture-qwen"),
    ):
        h.first(
            f"docker[{THOR}:thor]", "psql", "INSERT INTO actors", key, f'"os_user": "{account}"'
        )
    assert "register-actor: registered company/developer" in result.stdout


def test_spark_refuses_when_the_accounts_are_not_bootstrapped_and_names_the_sudo(tmp_path: Path):
    h = DeployHarness(tmp_path)
    result = h.deploy(SPARK)
    assert result.returncode != 0
    assert (
        "sudo bash" in result.stderr and "lanes/unix-user.sh bootstrap claude qwen" in result.stderr
    )
    h.never("systemctl[")
    h.never("useradd[")
    assert not h.account_home(SPARK, "claude").exists()


def test_spark_session_in_flight_refuses_before_any_stop(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    result = h.deploy(SPARK, FAKE_SESSION_RUNNING="1")
    assert result.returncode != 0
    assert "SKIP_SESSION_CHECK=1" in result.stderr
    h.first(f"pgrep[{SPARK}] -u spark -f")
    h.never("systemctl[", "stop")
    h.never("systemctl[", "disable")
    h.never("systemctl[", "restart")


def test_spark_refuses_a_role_whose_login_config_has_no_auth_token(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    planner = h.login_home(SPARK) / ".config/culture-nodes-bridges/planner.json"
    cfg = json.loads(planner.read_text())
    del cfg["auth_token"]
    planner.write_text(json.dumps(cfg))
    result = h.deploy(SPARK)
    assert result.returncode != 0
    assert "planner" in result.stderr and "auth_token" in result.stderr
    assert not (
        h.account_home(SPARK, "claude") / ".config/culture-nodes-bridges/planner.json"
    ).exists()
    h.never("systemctl[", "stop")


# --- deploy.sh orin ----------------------------------------------------------------


def test_orin_runs_runner_and_compose_as_the_login_user_and_the_codex_unit_as_the_account(
    tmp_path: Path,
):
    h = DeployHarness(tmp_path)
    h.bootstrap(ORIN, "codex")
    result = h.deploy(ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    login = h.login_home(ORIN)
    account = h.account_home(ORIN, "codex")
    # Login-user lanes, untouched: runner binary + unit, compose, prod.env.
    assert (login / ".culture-nodes/bin/nodes-runner").exists()
    assert (login / ".config/systemd/user/nodes-runner.service").exists()
    assert (login / ".culture-nodes/runner.env").exists()
    h.first(f"systemctl[{ORIN}:orin] --user restart nodes-runner")
    h.first(f"docker[{ORIN}:orin] compose", "-f compose.orin.yml up -d")
    h.first(f"docker[{ORIN}:orin] build")
    assert not (account / ".culture-nodes/bin/nodes-runner").exists()
    assert not (account / ".config/systemd/user/nodes-runner.service").exists()
    h.never(f"docker[{ORIN}:culture-codex]")
    # The codex bridge: archive shipped into the account, stamped, installed
    # and started THERE; stopped and disabled under the login user.
    assert (account / ".config/systemd/user/codex-bridge.service").read_text() == (
        ROOT / "deploy/prod/codex-bridge.service"
    ).read_text()
    stamp = json.loads(
        (account / "culture-nodes-prod/adapters/codex/src/codex_bridge/_revision.json").read_text()
    )
    assert stamp["revision"] == h.revision
    assert (account / ".local/bin/codex-bridge").exists()
    assert (account / ".local/bin/nodes").exists()
    cfg = json.loads((account / ".culture-nodes/codex-bridge.json").read_text())
    assert cfg["repo_allowlist"] == [str(account / "git/culture-nodes-agent")]
    assert cfg["codex_bin"] == str(account / ".local/bin/codex")
    assert cfg["actor_id"] == "actor-row-fake"
    assert (account / ".culture-nodes/bin/codex-preflight.sh").exists()
    assert h.first(f"pgrep[{ORIN}] -u orin -f") < h.first(
        f"systemctl[{ORIN}:orin] --user stop codex-bridge"
    )
    assert h.first(f"systemctl[{ORIN}:orin] --user disable codex-bridge") < h.first(
        f"systemctl[{ORIN}:culture-codex] --user restart codex-bridge"
    )
    h.first(f"systemctl[{ORIN}:culture-codex] --user enable codex-bridge")
    h.never(f"systemctl[{ORIN}:orin] --user restart codex-bridge")
    # register-actor.sh --os-user culture-codex, and the rollback pair.
    h.first(
        f"docker[{THOR}:thor]",
        "psql",
        "INSERT INTO actors",
        "company/codex-orin-fake",
        '"os_user": "culture-codex"',
    )
    assert (
        "culture-codex@orin-fake" in result.stdout
        and "systemctl --user start codex-bridge" in result.stdout
    )
    # The end-of-lane doctor runs as the account, in its own checkout.
    h.first(f"nodes[{ORIN}:culture-codex] doctor")


def test_orin_refuses_a_missing_account_naming_the_hand_turn_before_anything_is_stopped(
    tmp_path: Path,
):
    h = DeployHarness(tmp_path)
    result = h.deploy(ORIN, FAKE_SUDO_NEEDS_PASSWORD="1")
    assert result.returncode != 0
    assert "sudo bash" in result.stderr and "bootstrap codex" in result.stderr
    h.never("systemctl[", "stop codex-bridge")
    h.never("useradd[")
    assert not h.account_home(ORIN, "codex").exists()


def test_orin_session_in_flight_refuses_before_any_stop(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(ORIN, "codex")
    result = h.deploy(ORIN, FAKE_SESSION_RUNNING="1")
    assert result.returncode != 0
    assert "SKIP_SESSION_CHECK=1" in result.stderr
    h.never("systemctl[", "stop codex-bridge")
    h.never("systemctl[", "disable codex-bridge")
    h.never("systemctl[", "culture-codex")


def test_orin_refuses_when_the_account_has_no_bridge_env_and_names_install_secrets(tmp_path: Path):
    h = DeployHarness(tmp_path)
    h.bootstrap(ORIN, "codex")
    (h.account_home(ORIN, "codex") / ".culture-nodes/codex-bridge.env").unlink()
    result = h.deploy(ORIN)
    assert result.returncode != 0
    assert "install-secrets.sh" in result.stderr and "culture-codex" in result.stderr
    h.never("systemctl[", "stop codex-bridge")


def test_thor_bootstraps_inline_over_nopasswd_sudo(tmp_path: Path):
    """thor is NOPASSWD: a missing account is created by the lane itself,
    then the deploy carries on into the account."""
    h = DeployHarness(tmp_path)
    result = h.deploy(THOR, SKIP_ORIN_QUIESCE="1")
    h.first(f"sudo[{THOR}] -n")
    h.first(f"useradd[{THOR}] -m -s /bin/bash culture-codex")
    assert (
        h.account_home(THOR, "codex") / "git/culture-nodes-agent/.git"
    ).is_dir(), "provisioned right after"
    # A first cutover is two runs by design: the account now exists but holds
    # no codex-bridge.env yet (install-secrets.sh's account step writes it),
    # so the deploy stops there, names the script, and has stopped nothing.
    assert result.returncode != 0
    assert "install-secrets.sh" in result.stderr
    h.never("systemctl[", "stop codex-bridge")
