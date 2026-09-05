"""Fake-host coverage for the colleague engine in deploy/prod/lanes/unix-user.sh,
the spark host map in deploy/prod/bootstrap-accounts.sh, and the compose token
key the control plane authenticates its dispatches with (#298, task t5).

Same harness as tests/test_deploy_unix_user.py, imported rather than copied
(the way tests/test_deploy_unix_user_pi.py does), in its own module because
the unix-user file and both existing test modules sit near the repo's
1000-line hard limit (tests/lint/filelength_test.go). ``ColleagueHarness``
adds the two things colleague needs on top of the base harness:

- the account's own ``uv`` is no longer a stub that only echoes its name: the
  curl fake serves an installer that lays down a ``uv`` which *implements*
  ``uv tool install colleague==<version>`` the way the real one does (a
  console script in ``~/.local/bin`` answering ``--version``), because that
  install IS the colleague engine step -- there is no tarball and no npm;
- every login user carries a ``~/.colleague/config.json``, colleague's
  provider config, which the bootstrap copies the way it copies qwen's
  settings.json and pi's models.json.

NO bootstrap is run against a real host anywhere here, and none is implied:
creating ``culture-colleague`` on spark is an operator hand-turn recorded on
#298 (sudo asks for a password there), which is exactly why the spark deploy
lane treats the colleague half as opt-in.
"""

from __future__ import annotations

import json
import re
import subprocess
from pathlib import Path

from tests.test_deploy_account_bridges import _UV_BODY as _ACCOUNT_UV_BODY
from tests.test_deploy_account_bridges import DeployHarness
from tests.test_deploy_unix_user import (
    _CURL_SHIM,
    ORIN,
    PUBKEY,
    ROOT,
    SPARK,
    THOR,
    Harness,
    _block,
    _snapshot,
    _write_exec,
)

COLLEAGUE_VERSION = "1.76.0"

# The login user's colleague provider config: the `lobes` section that points
# a session at the gateway. Like qwen's settings.json it IS what makes the
# account able to run at all, so the bootstrap copies it.
COLLEAGUE_CONFIG = json.dumps(
    {
        "lobes": {"url": "http://thor:8000/v1", "model": "unsloth/Qwen3.8-27B-NVFP4"},
        "engine": "vllm-openai",
    }
)

# The uv the account installs for itself. The base harness's fake only echoes
# "uv-fake"; colleague is installed BY uv, so this one has to behave like the
# real `uv tool install <name>==<version>`: write a console script into
# ~/.local/bin that answers --version with the pin.
_UV_BODY = r"""#!/usr/bin/env bash
printf 'uv[%s] %s\n' "${FAKE_HOST:-}" "$*" >> "$FAKE_LOG"
if [ "$1" = tool ] && [ "$2" = install ]; then
  pkg=${@: -1}
  case "$pkg" in
    colleague==*)
      v=${pkg#colleague==}
      mkdir -p "$HOME/.local/bin"
      {
        printf '#!/usr/bin/env bash\n'
        printf 'case "${1:-}" in --version) echo "colleague %s" ;; esac\n' "$v"
        printf 'echo "fake colleague $*"\n'
      } > "$HOME/.local/bin/colleague"
      chmod +x "$HOME/.local/bin/colleague"
      ;;
    *) echo "uv fake: unexpected tool install $pkg" >&2; exit 1 ;;
  esac
fi
exit 0
"""

_UV_INSTALLER_BRANCH = """  *astral.sh/uv/install.sh*)
    printf '%s\\n' 'mkdir -p "$HOME/.local/bin"' \\
      'printf "#!/usr/bin/env bash\\necho uv-fake\\n" > "$HOME/.local/bin/uv"' \\
      'chmod +x "$HOME/.local/bin/uv"' ;;
"""
assert _UV_INSTALLER_BRANCH in _CURL_SHIM, "the base curl shim's uv installer branch moved"
_REAL_UV_BRANCH = """  *astral.sh/uv/install.sh*)
    printf 'mkdir -p "$HOME/.local/bin"; cat > "$HOME/.local/bin/uv" <<"UVEOF"\\n%s\\nUVEOF\\n' \\
      "$(cat "$FAKE_UV_BODY")"
    printf 'chmod +x "$HOME/.local/bin/uv"\\n' ;;
"""
_COLLEAGUE_CURL_SHIM = _CURL_SHIM.replace(_UV_INSTALLER_BRANCH, _REAL_UV_BRANCH)


class ColleagueHarness(Harness):
    def __init__(self, tmp_path: Path):
        super().__init__(tmp_path)
        self.uv_body = tmp_path / "uv-body.sh"
        self.uv_body.write_text(_UV_BODY)
        _write_exec(self.bin / "curl", _COLLEAGUE_CURL_SHIM)
        for host in (THOR, ORIN, SPARK):
            home = self.login_home(host)
            (home / ".colleague").mkdir()
            (home / ".colleague/config.json").write_text(COLLEAGUE_CONFIG)

    def run(self, body: str, host: str = SPARK, **fake_env: str):
        return super().run(body, host, FAKE_UV_BODY=str(self.uv_body), **fake_env)


def _provisioned(h: ColleagueHarness, host: str) -> subprocess.CompletedProcess:
    result = h.run(
        f"unix_user_bootstrap {host} colleague\nunix_user_provision {host} colleague", host=host
    )
    assert result.returncode == 0, result.stderr + result.stdout
    return result


# --- pins and names -----------------------------------------------------------


def test_colleague_version_is_pinned_in_a_variable_at_the_top():
    """The pin is a fact this file states, not one the installer picks (c24):
    the comparison holds the model constant, so the harness version has to be
    stated too."""
    block = _block()
    assert f"UNIX_USER_COLLEAGUE_VERSION={COLLEAGUE_VERSION}\n" in block
    assert block.index("UNIX_USER_COLLEAGUE_VERSION=") < block.index("unix_user_bootstrap()")


def test_engine_ok_accepts_colleague_and_its_role_is_colleague_developer(tmp_path: Path):
    h = ColleagueHarness(tmp_path)
    result = h.run("unix_user_engine_ok colleague && unix_user_roles colleague")
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == "colleague-developer"
    # The engine list is stated in one place for the operator too: the usage
    # line of the hand-typed bootstrap form names colleague.
    assert "<codex|claude|qwen|pi|colleague>" in _block()


def test_engine_ok_still_refuses_an_unknown_engine_and_names_all_five(tmp_path: Path):
    h = ColleagueHarness(tmp_path)
    result = h.run("unix_user_engine_ok gemini")
    assert result.returncode != 0
    assert "codex, claude, qwen, pi or colleague" in result.stderr


# --- bootstrap ----------------------------------------------------------------


def test_bootstrap_colleague_copies_config_json_the_way_qwen_and_pi_are(tmp_path: Path):
    """colleague's credential is its provider config, ~/.colleague/config.json:
    copied 600 into a 700 ~/.colleague the account owns, and nothing else of
    the login user's crosses."""
    h = ColleagueHarness(tmp_path)
    result = h.run("unix_user_bootstrap spark-fake colleague", host=SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    home = h.account_home(SPARK, "colleague")
    assert oct(home.stat().st_mode & 0o777) == "0o750"
    config = home / ".colleague/config.json"
    assert config.read_text() == COLLEAGUE_CONFIG
    assert oct(config.stat().st_mode & 0o777) == "0o600"
    assert oct((home / ".colleague").stat().st_mode & 0o777) == "0o700"
    h.first(f"chown[{SPARK}]", "culture-colleague:culture-colleague", "/.colleague")
    assert "credential .colleague/config.json: copied into culture-colleague" in result.stdout
    # An account holds only what its bridge needs.
    for other in (".codex", ".claude", ".qwen", ".pi"):
        assert not (home / other).exists(), other

    before = _snapshot(home)
    h.clear_log()
    again = h.run("unix_user_bootstrap spark-fake colleague", host=SPARK)
    assert again.returncode == 0, again.stderr
    assert _snapshot(home) == before, "a second bootstrap changed the colleague account"
    assert "credential .colleague/config.json: already in culture-colleague" in again.stdout
    h.never("useradd[")


def test_bootstrap_colleague_says_absent_when_the_login_user_has_no_config(tmp_path: Path):
    """As for a missing auth.json or models.json: the account is still created
    and the absence is reported, rather than a provider being invented."""
    h = ColleagueHarness(tmp_path)
    (h.login_home(SPARK) / ".colleague/config.json").unlink()
    result = h.run("unix_user_bootstrap spark-fake colleague", host=SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    home = h.account_home(SPARK, "colleague")
    assert home.is_dir()
    assert "absent on this host" in result.stdout
    assert ".colleague/config.json" in result.stdout
    assert not (home / ".colleague").exists()
    assert (home / ".ssh/authorized_keys").read_text() == PUBKEY + "\n"


# --- provision ----------------------------------------------------------------


def test_provision_colleague_installs_the_pin_with_the_accounts_own_uv(tmp_path: Path):
    """The colleague engine step is one `uv tool install colleague==<pin>` run
    by the account's OWN uv -- no system package, no node, no tarball --
    landing a console script in the account's ~/.local/bin. Idempotent on the
    pin: a second provision says 'already installed' and changes nothing."""
    h = ColleagueHarness(tmp_path)
    first = _provisioned(h, SPARK)
    home = h.account_home(SPARK, "colleague")
    binary = home / ".local/bin/colleague"
    assert binary.is_file()
    version = subprocess.run(  # nosec B603 - the console script the lane installed
        [str(binary), "--version"], capture_output=True, text=True, check=False
    )
    assert COLLEAGUE_VERSION in version.stdout
    install = h.calls()[h.first("uv[", "tool install")]
    assert f"colleague=={COLLEAGUE_VERSION}" in install
    assert h.count("uv[", "tool install") == 1
    # Nothing system-wide, and no other engine's installer. The one sudo in
    # the log is the bootstrap that ran first: the provision half never
    # reaches root at all.
    assert h.count("sudo[") == 1
    h.never("curl[", "claude.ai/install.sh")
    h.never("curl[", "install-qwen-standalone.sh")
    h.never("curl[", "openai/codex/releases")
    h.never("curl[", "nodejs.org")
    assert (home / "git/culture-nodes-colleague-developer/.git").is_dir()
    assert f"colleague: installed {COLLEAGUE_VERSION} at {binary}" in first.stdout
    h.first("ssh[culture-colleague@localhost]")

    before = _snapshot(home)
    h.clear_log()
    again = h.run(f"unix_user_provision {SPARK} colleague", host=SPARK)
    assert again.returncode == 0, again.stderr + again.stdout
    assert _snapshot(home) == before, "a second provision changed the colleague account"
    assert f"colleague: {COLLEAGUE_VERSION} already installed at {binary}" in again.stdout
    h.never("uv[", "tool install")
    h.never("curl[")


def test_provision_colleague_reinstalls_when_the_binary_reports_another_pin(tmp_path: Path):
    h = ColleagueHarness(tmp_path)
    _provisioned(h, SPARK)
    binary = h.account_home(SPARK, "colleague") / ".local/bin/colleague"
    binary.write_text("#!/usr/bin/env bash\necho 'colleague 1.70.0'\n")
    h.clear_log()
    result = h.run(f"unix_user_provision {SPARK} colleague", host=SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    assert h.count("uv[", f"tool install --force colleague=={COLLEAGUE_VERSION}") == 1
    check = subprocess.run(  # nosec B603 - the console script the lane installed
        [str(binary), "--version"], capture_output=True, text=True, check=False
    )
    assert COLLEAGUE_VERSION in check.stdout


def test_provision_colleague_refuses_without_config_json_and_names_the_bootstrap(tmp_path: Path):
    """The qwen/pi rule applied to colleague: an account with no
    ~/.colleague/config.json has no provider, so its bridge would start and
    every session would fail on the first request. Refuse at provision and
    name the root step that copies the file."""
    h = ColleagueHarness(tmp_path)
    _provisioned(h, SPARK)
    (h.account_home(SPARK, "colleague") / ".colleague/config.json").unlink()
    h.clear_log()
    refused = h.run(f"unix_user_provision {SPARK} colleague", host=SPARK)
    assert refused.returncode != 0
    assert "~/.colleague/config.json" in refused.stderr
    assert "bootstrap colleague" in refused.stderr


# --- session check ------------------------------------------------------------


def test_account_session_check_sees_a_colleague_session(tmp_path: Path):
    """A colleague session is the `colleague work` subprocess the bridge
    spawns; restarting the account's unit under one would kill the run and
    leave it `running` in the ledger. Bracket idiom, like every other
    alternative, so the pattern never matches its own ssh shell."""
    lane = (ROOT / "deploy/prod/lanes/unix-user.sh").read_text()
    m = re.search(r"UNIX_USER_SESSION_PATTERN='([^']+)'", lane)
    assert m
    assert "[c]olleague work" in m.group(1).split("|")
    h = ColleagueHarness(tmp_path)
    _provisioned(h, SPARK)
    h.clear_log()
    result = h.run(
        "unix_user_account_session_check spark-fake colleague\n"
        'ssh culture-colleague@localhost "systemctl --user stop'
        ' culture-nodes-colleague-developer"',
        host=SPARK,
        FAKE_SESSION_USER="culture-colleague",
    )
    assert result.returncode != 0
    assert "culture-colleague" in result.stderr
    h.first(f"pgrep[{SPARK}] -u culture-colleague -f", "[c]olleague work")
    h.never("systemctl[")


# --- bootstrap-accounts.sh host map -------------------------------------------


def test_bootstrap_accounts_maps_spark_to_claude_qwen_colleague():
    """spark's engine set gains colleague (#298 t5); thor and orin are
    untouched. The header comment states the same map, so what the operator
    reads is what the script runs."""
    script = (ROOT / "deploy/prod/bootstrap-accounts.sh").read_text()
    assert re.search(r'^\s*spark\)\s+ENGINES="claude qwen colleague"', script, re.M)
    assert re.search(r'^\s*orin\|thor\)\s+ENGINES="codex qwen pi"', script, re.M)
    header = script[: script.index("set -euo pipefail")]
    assert "culture-colleague" in header
    assert "hand-turn" in header.lower()


# --- the unit, the template and the compose token key -------------------------


def test_the_colleague_unit_is_versioned_and_execs_the_account_copy():
    unit = (ROOT / "deploy/prod/culture-nodes-colleague-developer.service").read_text()
    directives = [line for line in unit.splitlines() if line and not line.startswith("#")]
    assert "ExecStart=%h/.local/bin/colleague-bridge" in directives
    assert not any("uv run" in line or "/home/spark" in line for line in directives)
    assert (
        "COLLEAGUE_BRIDGE_CONFIG=%h/.config/culture-nodes-bridges/colleague-developer.json" in unit
    )
    # No preflight is claimed: there is no colleague-preflight.sh, and naming
    # one that does not exist would make systemd refuse to start the unit.
    assert not any(line.startswith("ExecStartPre") for line in directives)
    assert not (ROOT / "deploy/prod/colleague-preflight.sh").exists()


def test_the_colleague_template_is_loadable_by_the_bridges_own_config_loader():
    """The colleague bridge REFUSES an unknown config key (config.py's
    `_coerce_file_fields`), so a template modelled key-for-key on the pi one
    would fail to start the bridge: no `config_repo`, no `default_sandbox`.
    Asserted against the adapter's own field table, not a copy of it."""
    text = (ROOT / "deploy/prod/colleague-developer.json.template").read_text()
    assert "NODES_HUMAN_DECISION_TOKEN" not in text
    assert "/home/spark" not in text
    assert "__HOME__" in text
    cfg = json.loads(text.replace("__HOME__", "/h").replace("__NODES_API_URL__", "http://x"))
    assert cfg["port"] == 8094, "8094 sits beside qwen's 8092 and pi's 8093"
    assert cfg["host"] == "0.0.0.0"
    assert cfg["always_async"] is True
    assert cfg["colleague_bin"] == "/h/.local/bin/colleague"
    assert cfg["colleague_env"]["COLLEAGUE_ENGINE"] == "vllm-openai"
    assert cfg["repo_allowlist"] == ["/h/git/culture-nodes-colleague-developer"]
    fields = _colleague_file_fields()
    unknown = sorted(set(cfg) - fields)
    assert not unknown, f"the colleague bridge would refuse these keys: {unknown}"
    # The two keys the qwen/pi templates carry that this bridge does not know.
    for key in ("config_repo", "default_sandbox"):
        assert key not in fields, f"{key} is now a colleague config key; revisit the template"


def _colleague_file_fields() -> set[str]:
    """The adapter's `_FILE_FIELDS` keys, read out of its source rather than
    imported (adapters are separate distributions, not importable from here)."""
    src = (ROOT / "adapters/colleague/src/colleague_bridge/config.py").read_text()
    table = src[src.index("_FILE_FIELDS = {") : src.index("\n}\n", src.index("_FILE_FIELDS = {"))]
    return set(re.findall(r'^\s+"([a-z_]+)":', table, re.M))


def test_both_compose_files_declare_the_colleague_token_in_api_and_worker():
    """register-actor.sh accepts ANY auth_token_env name and nothing checks it
    against the compose files, so an actor registered without a matching line
    here dispatches, 401s, and points at nothing. thor and orin poll the same
    namespace (#224), which is why the key is declared on both hosts."""
    key = "NODES_ACTOR_COLLEAGUE_SPARK_TOKEN"
    expected = {
        "compose.thor.yml": {"api", "scheduler", "worker"},
        "compose.orin.yml": {"worker"},
    }
    for name, services in expected.items():
        body = (ROOT / "deploy/prod" / name).read_text()
        assert f"{key}: ${{{key}:-}}" in body, name
        for service in services:
            block = _service_block(body, service)
            assert key in block, f"{name}: {service} does not declare {key}"
            # Beside the family it belongs to, in the same ${...:-} shape.
            assert "NODES_ACTOR_PI_ORIN_TOKEN" in block, f"{name}: {service}"


def _service_block(body: str, service: str) -> str:
    lines = body.splitlines()
    start = next(i for i, line in enumerate(lines) if line == f"  {service}:")
    end = next(
        (
            i
            for i, line in enumerate(lines[start + 1 :], start + 1)
            if line.startswith("  ") and not line.startswith("   ") and line.rstrip().endswith(":")
        ),
        len(lines),
    )
    return "\n".join(lines[start:end])


# --- deploy.sh spark: the colleague half is opt-in ----------------------------


class ColleagueDeployHarness(DeployHarness):
    """The deploy harness with the colleague lane's three prerequisites: the
    login user's provider config (the bootstrap's source), the login user's
    colleague-developer.json (the custody point of the externally issued
    auth_token the renderer carries), and a registered company/colleague-spark
    row for the os_user merge."""

    def __init__(self, tmp_path: Path):
        super().__init__(tmp_path)
        self.uv_body.write_text(_DEPLOY_UV_BODY)
        for host in (THOR, ORIN, SPARK):
            home = self.login_home(host)
            (home / ".colleague").mkdir()
            (home / ".colleague/config.json").write_text(COLLEAGUE_CONFIG)
        spark = self.login_home(SPARK)
        (spark / ".config/culture-nodes-bridges/colleague-developer.json").write_text(
            json.dumps(
                {
                    "actor_id": "actor_colleague_row",
                    "auth_token": "login-token-colleague",
                    "port": 8094,
                }
            )
        )
        actors = json.loads(self.actors_json.read_text())
        actors["items"].append(
            {
                "id": "row-colleague-spark",
                "actor_key": "company/colleague-spark",
                "revision": 1,
                "endpoint_ref": "http://192.168.1.5:8094",
                "metadata": {"auth_token_env": "NODES_ACTOR_COLLEAGUE_SPARK_TOKEN"},
            }
        )
        self.actors_json.write_text(json.dumps(actors))


# The deploy harness's uv fake maps an adapter path to the console script it
# installs, and its scripts only log. colleague needs two more branches: the
# adapter -> colleague-bridge, and the ENGINE (`uv tool install
# colleague==<pin>`), whose script must answer --version or the lane's
# already-installed check can never be satisfied.
_QWEN_ADAPTER_BRANCH = "    *adapters/qwen) bin=qwen-bridge ;;\n"
assert _QWEN_ADAPTER_BRANCH in _ACCOUNT_UV_BODY, "the deploy uv fake's adapter table moved"
_ENGINE_ANCHOR = '  mkdir -p "$HOME/.local/bin"\n'
assert _ENGINE_ANCHOR in _ACCOUNT_UV_BODY, "the deploy uv fake's install branch moved"
_ENGINE_BRANCH = r"""  case "${@: -1}" in
    colleague==*)
      v=${@: -1}; v=${v#colleague==}
      {
        printf '#!/usr/bin/env bash\n'
        printf 'printf "colleague[%%s:%%s] %%s\n" "$FAKE_HOST" "$FAKE_USER" "$*" >> "$FAKE_LOG"\n'
        printf 'case "${1:-}" in --version) echo "colleague %s" ;; esac\n' "$v"
        printf 'exit 0\n'
      } > "$HOME/.local/bin/colleague"
      chmod +x "$HOME/.local/bin/colleague"
      exit 0 ;;
  esac
"""
_DEPLOY_UV_BODY = _ACCOUNT_UV_BODY.replace(
    _QWEN_ADAPTER_BRANCH,
    _QWEN_ADAPTER_BRANCH + "    *adapters/colleague) bin=colleague-bridge ;;\n",
).replace(_ENGINE_ANCHOR, _ENGINE_ANCHOR + _ENGINE_BRANCH)


def test_spark_deploys_the_five_bridges_and_skips_colleague_until_it_is_bootstrapped(
    tmp_path: Path,
):
    """Creating culture-colleague needs a typed sudo on spark (#298), so a
    deploy must not start refusing the five bridges it already serves because
    the sixth account does not exist yet. The skip is LOUD and names the root
    step; nothing colleague-shaped is installed or registered."""
    h = ColleagueDeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen")
    result = h.deploy(SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    assert "culture-colleague is not bootstrapped" in result.stdout
    assert "lanes/unix-user.sh bootstrap colleague" in result.stdout
    # The reachability probe is the only thing addressed to the account.
    assert h.count("ssh[culture-colleague@localhost]") == 1
    h.first("ssh[culture-colleague@localhost] id -un")
    h.never("systemctl[", "culture-nodes-colleague-developer")
    h.never("docker[", "company/colleague-spark")
    # The five that were already there are deployed exactly as before.
    h.first(f"systemctl[{SPARK}:culture-qwen] --user restart culture-nodes-qwen-developer")
    h.first(f"systemctl[{SPARK}:culture-claude] --user restart culture-nodes-claude-developer")


def test_spark_deploys_the_colleague_bridge_once_the_account_exists(tmp_path: Path):
    """With culture-colleague bootstrapped the lane does for it what it does
    for culture-qwen: provision, ship the archive, stamp + `uv tool install`
    the adapter, render the config from the template, install the unit and
    merge os_user onto company/colleague-spark. There is no login-user
    cutover -- spark never ran a colleague bridge as its login user."""
    h = ColleagueDeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen", "colleague")
    result = h.deploy(SPARK)
    assert result.returncode == 0, result.stderr + result.stdout
    home = h.account_home(SPARK, "colleague")
    unit = home / ".config/systemd/user/culture-nodes-colleague-developer.service"
    assert (
        unit.read_text() == (ROOT / "deploy/prod" / unit.name).read_text()
    ), "unit files are byte-for-byte"
    account = f"systemctl[{SPARK}:culture-colleague] --user"
    h.first(f"{account} restart culture-nodes-colleague-developer")
    h.first(f"{account} enable culture-nodes-colleague-developer")
    # The account's own pinned engine and its own archive copy.
    assert (home / ".local/bin/colleague").exists()
    assert (home / ".local/bin/colleague-bridge").exists()
    assert (home / "culture-nodes-prod/deploy/prod/deploy.sh").exists()
    stamp = json.loads(
        (
            home / "culture-nodes-prod/adapters/colleague/src/colleague_bridge/_revision.json"
        ).read_text()
    )
    assert stamp["revision"] == h.revision
    h.first(
        "uv[", "culture-colleague", "tool install --force ./culture-nodes-prod/adapters/colleague"
    )
    # The rendered config: the template's keys, the login user's two carried
    # ones, and no operator bearer.
    rendered = json.loads(
        (home / ".config/culture-nodes-bridges/colleague-developer.json").read_text()
    )
    assert rendered["actor_id"] == "actor_colleague_row"
    assert rendered["auth_token"] == "login-token-colleague"
    assert rendered["port"] == 8094
    assert rendered["colleague_bin"] == f"{home}/.local/bin/colleague"
    assert rendered["repo_allowlist"] == [f"{home}/git/culture-nodes-colleague-developer"]
    assert "NODES_HUMAN_DECISION_TOKEN" not in json.dumps(rendered)
    assert "qwen_env" not in rendered, "the qwen API-key carry is qwen's, not colleague's"
    h.first(
        f"docker[{THOR}:thor]",
        "psql",
        "INSERT INTO actors",
        "company/colleague-spark",
        '"os_user": "culture-colleague"',
    )
    # No login-user unit is stopped or disabled for colleague; the other five
    # are cut over exactly as before.
    h.never("systemctl[", ":spark]", "stop culture-nodes-colleague-developer")
    h.never("systemctl[", ":spark]", "disable culture-nodes-colleague-developer")
    h.first(f"systemctl[{SPARK}:spark] --user stop culture-nodes-qwen-developer")


def test_spark_refuses_the_colleague_render_when_the_provider_config_is_gone(tmp_path: Path):
    """The account reads its OWN copy of ~/.colleague/config.json, but a login
    user who has deleted the file the bootstrap copies has no way to refresh
    it -- and a deploy that rendered a config anyway would leave a bridge that
    starts and fails every session. Refuse, before any unit is stopped."""
    h = ColleagueDeployHarness(tmp_path)
    h.bootstrap(SPARK, "claude", "qwen", "colleague")
    (h.login_home(SPARK) / ".colleague/config.json").unlink()
    result = h.deploy(SPARK)
    assert result.returncode != 0
    assert ".colleague/config.json is missing" in result.stderr
    assert "lobes" in result.stderr
    h.never("systemctl[", "stop")
    h.never("systemctl[", "restart")
