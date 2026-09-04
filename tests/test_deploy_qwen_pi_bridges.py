"""Fake-host coverage for deploy.sh's qwen + pi engine-account bridge lanes
(task t7, #294).

Reuses tests/test_deploy_account_bridges.py's DeployHarness -- the same
per-account ssh axis and recording fakes that run the REAL deploy.sh end to
end against fake hosts -- and adds only what the two new lanes need on top of
it: a fake node-dist tarball (so the pi engine's node + npm install runs
offline), the pi provider config the bootstrap copies, the qwen/pi rows in the
actor registry, and the account bridge auth-token env files install-secrets.sh
writes. Everything else -- ssh, systemctl, docker, uv, the archive ship -- is
the parent harness's, unchanged.

This lives in its own file so neither it nor the parent crosses the 1000-line
hard limit (tests/lint/filelength_test.go).
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path

from tests.test_deploy_account_bridges import (
    _CURL_SHIM,
    _UV_BODY,
    DeployHarness,
)
from tests.test_deploy_unix_user import ORIN, ROOT, THOR, _write_exec

DEPLOY = ROOT / "deploy/prod/deploy.sh"
INSTALL_SECRETS = ROOT / "deploy/prod/install-secrets.sh"

# A fake node-dist tarball: bin/node prints the pinned node version, and a
# bin/npm whose `install -g --prefix DIR pkg@piver` drops DIR/bin/pi printing
# piver -- so unix-user.sh's pi engine install (node tarball + npm-into-prefix,
# task t3) runs offline exactly as it would live.
_NODE_CASE = r"""  *nodejs.org/dist/*)
    asset=${url##*/}; name=${asset%.tar.gz}
    ver=$(printf '%s' "$name" | sed -n 's/^node-v\([0-9.]*\)-.*/\1/p')
    tmp=$(mktemp -d)
    mkdir -p "$tmp/$name/bin"
    printf '#!/usr/bin/env bash\necho "v%s"\n' "$ver" > "$tmp/$name/bin/node"
    cat > "$tmp/$name/bin/npm" <<'NPMEOF'
#!/usr/bin/env bash
prefix=; last=
while [ $# -gt 0 ]; do case "$1" in --prefix) prefix=$2; shift ;; esac; last=$1; shift; done
mkdir -p "$prefix/bin"
printf '#!/usr/bin/env bash\necho "%s"\n' "${last##*@}" > "$prefix/bin/pi"
chmod +x "$prefix/bin/pi"
NPMEOF
    chmod +x "$tmp/$name/bin/node" "$tmp/$name/bin/npm"
    tar -C "$tmp" -czf "$out" "$name"
    rm -rf "$tmp" ;;
"""

_CURL_WITH_NODE = _CURL_SHIM.replace(
    "  */v1alpha1/version*)", _NODE_CASE + "  */v1alpha1/version*)", 1
)
_UV_WITH_PI = _UV_BODY.replace(
    "    *adapters/qwen) bin=qwen-bridge ;;",
    "    *adapters/qwen) bin=qwen-bridge ;;\n    *adapters/pi) bin=pi-bridge ;;",
    1,
)

_PI_MODELS = json.dumps({"providers": {"lobes": {"models": [{"id": "unsloth/Qwen3.8-27B-NVFP4"}]}}})

_ENGINE_TOKENS = {
    "qwen": ("qwen-bridge.env", "QWEN_BRIDGE_AUTH_TOKEN=login-qwen-token\n"),
    "pi": ("pi-bridge.env", "PI_BRIDGE_AUTH_TOKEN=login-pi-token\n"),
}


class QwenPiHarness(DeployHarness):
    """DeployHarness plus the node-dist fake, the pi provider config, the
    qwen/pi registry rows, and the account bridge auth-token env files."""

    def __init__(self, tmp_path: Path):
        super().__init__(tmp_path)
        _write_exec(self.bin / "curl", _CURL_WITH_NODE)
        self.uv_body.write_text(_UV_WITH_PI)
        rows = json.loads(self.actors_json.read_text())
        for key, port in (
            ("company/qwen-thor-fake", 8092),
            ("company/qwen-orin-fake", 8092),
            ("company/pi-thor-fake", 8093),
            ("company/pi-orin-fake", 8093),
        ):
            rows["items"].append(
                {
                    "id": f"row-{key.split('/')[1]}",
                    "actor_key": key,
                    "revision": 1,
                    "endpoint_ref": f"http://192.168.1.5:{port}",
                    "metadata": {"auth_token_env": "X"},
                }
            )
        self.actors_json.write_text(json.dumps(rows))
        # pi's provider config: unix-user.sh's bootstrap copies
        # ~/.pi/agent/models.json into the culture-pi account the way it copies
        # codex auth.json, and provision refuses a pi account without it.
        for host in (THOR, ORIN):
            agent = self.login_home(host) / ".pi/agent"
            agent.mkdir(parents=True, exist_ok=True)
            (agent / "models.json").write_text(_PI_MODELS)

    def bootstrap(self, host: str, *engines: str) -> None:
        super().bootstrap(host, *engines)
        # install-secrets.sh's install_{qwen,pi}_account_env writes the bridge
        # auth-token env file; seeded here for the deploy tests.
        for engine in engines:
            if engine in _ENGINE_TOKENS:
                name, body = _ENGINE_TOKENS[engine]
                cn = self.account_home(host, engine) / ".culture-nodes"
                cn.mkdir(exist_ok=True)
                (cn / name).write_text(body)
                (cn / name).chmod(0o600)


# --- the pi unit + template, and the ports ------------------------------------


def test_the_pi_unit_mirrors_the_qwen_unit_and_gates_on_the_preflight():
    unit = (ROOT / "deploy/prod/culture-nodes-pi-developer.service").read_text()
    directives = [line for line in unit.splitlines() if line and not line.startswith("#")]
    assert "ExecStart=%h/.local/bin/pi-bridge" in directives
    assert "EnvironmentFile=-%h/.culture-nodes/bridge-push.env" in directives
    assert "Environment=PI_BRIDGE_CONFIG=%h/.config/culture-nodes-bridges/pi-developer.json" in unit
    pre = [d for d in directives if d.startswith("ExecStartPre=")]
    assert len(pre) == 1, "pi unit must gate startup on exactly one preflight"
    assert "pi-preflight.sh" in pre[0]
    assert "pi-developer.json" in pre[0]
    assert not any("uv run" in d or "/home/spark" in d for d in directives)


def test_the_pi_template_carries_the_preflight_and_bridge_keys():
    text = (ROOT / "deploy/prod/pi-developer.json.template").read_text()
    assert "__HOME__" in text
    assert "NODES_HUMAN_DECISION_TOKEN" not in text
    assert "/home/spark" not in text
    cfg = json.loads(text.replace("__HOME__", "/h"))
    assert cfg["pi_bin"] == "/h/.local/bin/pi"
    assert cfg["model_endpoint"] == "http://thor:8000/v1"
    assert cfg["repo_allowlist"] == ["/h/git/culture-nodes-pi-developer"]
    assert cfg["repo_identities"] == {
        "agentculture/culture-nodes": "/h/git/culture-nodes-pi-developer"
    }
    assert cfg["config_repo"] == "agentculture/culture-nodes"
    assert cfg["always_async"] is True
    assert cfg["default_sandbox"] == "workspace-write"
    assert cfg["state_dir"] == "/h/.local/state/culture-nodes-bridges/pi-developer"
    assert cfg["provider"]
    assert cfg["model"]
    assert "auth_token" not in cfg
    assert cfg["port"] == 8093


def test_actor_placement_knows_the_qwen_and_pi_ports():
    placement = ROOT / "deploy/prod/actor-placement.sh"
    body = (
        placement.read_text()
        + '\nfor e in qwen pi codex; do echo "$e=$(actor_bridge_port $e)"; done\n'
        + "actor_bridge_port nonsense && echo LEAK || echo refused\n"
    )
    out = subprocess.run(  # nosec B603 B607 - the placement library under bash
        ["bash", "-c", body], capture_output=True, text=True, check=True
    ).stdout
    assert "qwen=8092" in out
    assert "pi=8093" in out
    assert "codex=8086" in out
    assert "refused" in out
    assert "LEAK" not in out
    qwen_tmpl = json.loads(
        (ROOT / "deploy/prod/qwen-developer.json.template").read_text().replace("__HOME__", "/h")
    )
    pi_tmpl = json.loads(
        (ROOT / "deploy/prod/pi-developer.json.template").read_text().replace("__HOME__", "/h")
    )
    assert qwen_tmpl["port"] == 8092
    assert pi_tmpl["port"] == 8093


def test_deploy_defines_the_qwen_and_pi_lanes_after_the_codex_lane():
    script = DEPLOY.read_text()
    for fn in ("deploy_qwen_bridge", "deploy_pi_bridge", "deploy_account_engine_bridge"):
        assert f"{fn}(" in script, fn
    gate = script.index('if [[ "$HOST" != spark* ]]; then\n  deploy_codex_bridge')
    assert 'deploy_qwen_bridge "$HOST"' in script[gate:]
    assert 'deploy_pi_bridge "$HOST"' in script[gate:]
    assert script.index('deploy_codex_bridge "$HOST"', gate) < script.index(
        'deploy_qwen_bridge "$HOST"', gate
    )


# --- deploy.sh orin: the qwen + pi lanes render into their accounts -----------


def _assert_engine_bridge_installed(h, host, engine, role, port, token):
    account = h.account_home(host, engine)
    adapter = "qwen" if engine == "qwen" else "pi"
    unit_name = f"culture-nodes-{role}.service"
    assert (account / f".config/systemd/user/{unit_name}").read_text() == (
        ROOT / "deploy/prod" / unit_name
    ).read_text(), f"{engine} unit is byte-for-byte"
    stamp = json.loads(
        (
            account / f"culture-nodes-prod/adapters/{adapter}/src/{engine}_bridge/_revision.json"
        ).read_text()
    )
    assert stamp["revision"] == h.revision
    assert (account / f".local/bin/{engine}-bridge").exists()
    cfg_path = account / f".config/culture-nodes-bridges/{role}.json"
    cfg = json.loads(cfg_path.read_text())
    assert cfg["port"] == port, engine
    assert cfg["auth_token"] == token, engine
    assert cfg["actor_id"] == "actor-row-fake", engine
    assert "__HOME__" not in json.dumps(cfg), engine
    assert cfg["state_dir"].startswith(str(account)), engine
    assert oct(cfg_path.stat().st_mode & 0o777) == "0o600", engine


def test_orin_installs_the_qwen_and_pi_bridges_into_their_accounts(tmp_path: Path):
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "codex", "qwen", "pi")
    result = h.deploy(ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    for engine, role, port, token in (
        ("qwen", "qwen-developer", 8092, "login-qwen-token"),
        ("pi", "pi-developer", 8093, "login-pi-token"),
    ):
        _assert_engine_bridge_installed(h, ORIN, engine, role, port, token)
        adapter = "qwen" if engine == "qwen" else "pi"
        h.first(
            "uv[",
            f"culture-{engine}",
            f"tool install --force ./culture-nodes-prod/adapters/{adapter}",
        )
        h.first(f"systemctl[{ORIN}:culture-{engine}] --user restart culture-nodes-{role}")
        h.first(f"systemctl[{ORIN}:culture-{engine}] --user enable culture-nodes-{role}")
        h.first(
            f"docker[{THOR}:thor]",
            "psql",
            "INSERT INTO actors",
            f"company/{engine}-orin-fake",
            f'"os_user": "culture-{engine}"',
        )
        h.never(f"systemctl[{ORIN}:orin] --user restart culture-nodes-{role}")
    pi = h.account_home(ORIN, "pi")
    assert (pi / ".culture-nodes/bin/pi-preflight.sh").exists()
    # Only the pinned-version stub travels beside the preflight (not the whole
    # lane, whose body would trip the account inventory's operator-material grep).
    stub = (pi / ".culture-nodes/bin/lanes/unix-user.sh").read_text()
    assert stub.strip().startswith("UNIX_USER_PI_VERSION=")
    assert "NODES_DATABASE_URL" not in stub
    assert not (h.account_home(ORIN, "qwen") / ".culture-nodes/bin/pi-preflight.sh").exists()


def test_the_qwen_and_pi_lanes_are_a_no_op_the_second_time(tmp_path: Path):
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "codex", "qwen", "pi")
    first = h.deploy(ORIN)
    assert first.returncode == 0, first.stderr + first.stdout
    _assert_engine_bridge_installed(h, ORIN, "qwen", "qwen-developer", 8092, "login-qwen-token")
    _assert_engine_bridge_installed(h, ORIN, "pi", "pi-developer", 8093, "login-pi-token")
    h.clear_log()
    second = h.deploy(ORIN)
    assert second.returncode == 0, second.stderr + second.stdout
    _assert_engine_bridge_installed(h, ORIN, "qwen", "qwen-developer", 8092, "login-qwen-token")
    _assert_engine_bridge_installed(h, ORIN, "pi", "pi-developer", 8093, "login-pi-token")
    # The account's pinned engine was NOT re-downloaded on the second run.
    assert h.count("curl[", "nodejs.org/dist") == 0
    assert h.count("curl[", "install-qwen-standalone") == 0


def test_the_qwen_and_pi_lanes_skip_when_the_account_is_not_bootstrapped(tmp_path: Path):
    """Additive (#294): a codex-only deploy is still valid. An unbootstrapped
    qwen/pi account is skipped by name, not a hard failure."""
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "codex")
    result = h.deploy(ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    assert "culture-qwen on orin-fake is not bootstrapped" in result.stdout
    assert "culture-pi on orin-fake is not bootstrapped" in result.stdout
    assert not h.account_home(ORIN, "qwen").exists()
    assert not h.account_home(ORIN, "pi").exists()
    h.first(f"systemctl[{ORIN}:culture-codex] --user restart codex-bridge")
    h.never("systemctl[", "restart culture-nodes-qwen-developer")
    h.never("systemctl[", "restart culture-nodes-pi-developer")


def test_the_qwen_lane_skips_when_the_bridge_secret_is_missing(tmp_path: Path):
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "codex", "qwen", "pi")
    (h.account_home(ORIN, "qwen") / ".culture-nodes/qwen-bridge.env").unlink()
    result = h.deploy(ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    assert "qwen-bridge.env missing" in result.stdout
    assert "install-secrets.sh" in result.stdout
    assert not (
        h.account_home(ORIN, "qwen") / ".config/culture-nodes-bridges/qwen-developer.json"
    ).exists()
    h.never("systemctl[", "restart culture-nodes-qwen-developer")
    h.first(f"systemctl[{ORIN}:culture-pi] --user restart culture-nodes-pi-developer")


# --- install-secrets.sh: the qwen/pi account bridge auth-token env ------------


def _qwen_pi_account_env_block() -> str:
    script = INSTALL_SECRETS.read_text()
    start = script.index("# QWEN_PI_ACCOUNT_ENV_START")
    end = script.index("# QWEN_PI_ACCOUNT_ENV_END")
    return script[start:end]


def _install_engine_account_env(h, host, engine, **fake_env):
    body = _qwen_pi_account_env_block() + f"\ninstall_{engine}_account_env {host}\n"
    return h.run(body, host=host, **fake_env)


def test_install_secrets_mints_and_keeps_the_qwen_and_pi_account_env(tmp_path: Path):
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "qwen", "pi")
    for engine, var in (("qwen", "QWEN_BRIDGE_AUTH_TOKEN"), ("pi", "PI_BRIDGE_AUTH_TOKEN")):
        env = h.account_home(ORIN, engine) / f".culture-nodes/{engine}-bridge.env"
        env.unlink()  # bootstrap seeded a stand-in; test the real mint
        first = _install_engine_account_env(h, ORIN, engine)
        assert first.returncode == 0, first.stderr
        assert env.exists()
        assert oct(env.stat().st_mode & 0o777) == "0o600"
        assert f"{var}=" in env.read_text()
        minted = env.read_text()
        second = _install_engine_account_env(h, ORIN, engine)
        assert second.returncode == 0, second.stderr
        assert env.read_text() == minted, f"{engine} token rotated on a re-run"
        assert "keeping existing" in second.stderr


def test_install_secrets_relays_bridge_push_env_into_the_qwen_pi_accounts(tmp_path: Path):
    h = QwenPiHarness(tmp_path)
    h.bootstrap(ORIN, "qwen")
    result = _install_engine_account_env(h, ORIN, "qwen", GITHUB_TOKEN_WORKER="ghp-fake-worker")
    assert result.returncode == 0, result.stderr
    push = h.account_home(ORIN, "qwen") / ".culture-nodes/bridge-push.env"
    assert push.read_text() == "GITHUB_TOKEN_WORKER=ghp-fake-worker\n"
    assert oct(push.stat().st_mode & 0o777) == "0o600"


def test_install_secrets_skips_the_engine_env_when_the_account_is_absent(tmp_path: Path):
    h = QwenPiHarness(tmp_path)  # no qwen account bootstrapped
    result = _install_engine_account_env(h, ORIN, "qwen")
    assert result.returncode == 0
    assert "not bootstrapped or not reachable" in result.stderr
    assert not h.account_home(ORIN, "qwen").exists()
