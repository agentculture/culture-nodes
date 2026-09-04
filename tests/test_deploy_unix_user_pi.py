"""Fake-host coverage for the pi engine in deploy/prod/lanes/unix-user.sh and
the thor|orin host map in deploy/prod/bootstrap-accounts.sh (#294, task t3).

Same harness as tests/test_deploy_unix_user.py, imported rather than copied
(the way test_deploy_bootstrap_guards.py does), in its own module because the
unix-user file sits just under the repo's 1000-line hard limit
(tests/lint/filelength_test.go). ``PiHarness`` adds the three things pi
needs on top of the base harness:

- ``uname -m`` answers ``FAKE_ARCH`` (aarch64 by default, what thor and orin
  are) so the node tarball the lane picks is decided by the test, not by the
  machine running it;
- the curl fake serves a nodejs.org release tarball in the real layout
  (``bin/node``, ``bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js``) whose
  fake ``node`` runs whatever script it is handed through bash, and whose
  fake ``npm install -g --prefix`` lays ``@earendil-works/pi-coding-agent``
  down the way the real one does (``lib/node_modules/...`` plus a ``bin/pi``
  symlink to a ``#!/usr/bin/env node`` cli). Only THAT node can run that cli,
  so a ``~/.local/bin/pi`` that prints the pin from a shell with no fake node
  on PATH proves the wrapper prepends the tarball's node -- which is the
  whole reason the wrapper exists;
- every login user carries a ``~/.pi/agent/models.json``, pi's provider
  config, which the bootstrap copies the way it copies qwen's settings.json.
"""

from __future__ import annotations

import os
import re
import subprocess
from pathlib import Path

from tests.test_deploy_unix_user import (
    _CURL_SHIM,
    _LOG_ONLY_SHIM,
    LANE,
    LOCAL_HOST,
    ORIN,
    PUBKEY,
    QWEN_SETTINGS,
    ROOT,
    SPARK,
    THOR,
    Harness,
    _block,
    _provisioned,
    _snapshot,
    _write_exec,
)

# The login user's pi provider config (~/.pi/agent/models.json, pi docs
# models.md): an openai-completions provider for the keyless lobe on thor
# :8000. Like qwen's settings.json it IS the credential -- a pi account
# without its own copy has no provider at all -- so the bootstrap copies it.
PI_MODELS = (
    '{"providers": {"lobes-thor": {"api": "openai-completions", '
    '"baseUrl": "http://thor:8000/v1", "apiKey": "dummy", '
    '"models": [{"id": "unsloth/Qwen3.8-27B-NVFP4"}]}}}\n'
)
PI_VERSION = "0.85.0"
PI_NODE_VERSION = "22.23.2"
PI_NODE_DIR = f"node-v{PI_NODE_VERSION}-linux-arm64"

_UNAME_SHIM = """#!/usr/bin/env bash
if [ "${1:-}" = -m ]; then echo "${FAKE_ARCH:-aarch64}"; exit 0; fi
exec /usr/bin/uname "$@"
"""

# The nodejs.org branch of the curl fake, spliced in front of the base
# shim's "no such URL" fallthrough so every other installer keeps its one
# definition. The fake npm logs as npm[<host>] so a test can count installs.
_NODEJS_BRANCH = """  *nodejs.org/dist/v*/node-v*-linux-*.tar.gz)
    asset=${url##*/}; name=${asset%.tar.gz}
    v=$(printf '%s' "$name" | sed -n 's#^node-v\\([0-9.]*\\)-linux-.*#\\1#p')
    case "$name" in *-linux-arm64|*-linux-x64) ;;
      *) echo "curl fake: nodejs.org has no $asset" >&2; exit 22 ;; esac
    tmp=$(mktemp -d)
    mkdir -p "$tmp/$name/bin" "$tmp/$name/lib/node_modules/npm/bin"
    printf '%s\\n' '#!/usr/bin/env bash' \\
      'case "${1:-}" in --version|-v) echo "v'"$v"'"; exit 0 ;; esac' \\
      'exec bash "$@"' > "$tmp/$name/bin/node"
    cat > "$tmp/$name/lib/node_modules/npm/bin/npm-cli.js" <<'NPM'
#!/usr/bin/env node
printf 'npm[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
prefix=; pkg=
while [ $# -gt 0 ]; do
  case "$1" in --prefix) prefix=$2; shift ;; install|-g) ;; -*) ;; *) pkg=$1 ;; esac
  shift
done
[ -n "$prefix" ] || { echo "npm fake: install -g without --prefix" >&2; exit 1; }
case "$pkg" in @earendil-works/pi-coding-agent@*) ;;
  *) echo "npm fake: unexpected package $pkg" >&2; exit 1 ;; esac
v=${pkg##*@}
mod=$prefix/lib/node_modules/@earendil-works/pi-coding-agent
mkdir -p "$mod/dist" "$prefix/bin"
printf '{"version": "%s"}\\n' "$v" > "$mod/package.json"
printf '%s\\n' '#!/usr/bin/env node' \\
  'case "${1:-}" in --version) echo "'"$v"'" ;; *) echo "fake pi $*" ;; esac' > "$mod/dist/cli.js"
chmod +x "$mod/dist/cli.js"
ln -sfn ../lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js "$prefix/bin/pi"
NPM
    chmod +x "$tmp/$name/bin/node" "$tmp/$name/lib/node_modules/npm/bin/npm-cli.js"
    ln -s ../lib/node_modules/npm/bin/npm-cli.js "$tmp/$name/bin/npm"
    tar -C "$tmp" -czf "$out" "$name"
    rm -rf "$tmp" ;;
"""
_FALLTHROUGH = '  *) echo "curl fake: no such URL $url" >&2; exit 22 ;;\n'
assert _FALLTHROUGH in _CURL_SHIM, "the base curl shim's fallthrough moved"
_PI_CURL_SHIM = _CURL_SHIM.replace(_FALLTHROUGH, _NODEJS_BRANCH + _FALLTHROUGH)


class PiHarness(Harness):
    def __init__(self, tmp_path: Path):
        super().__init__(tmp_path)
        _write_exec(self.bin / "uname", _UNAME_SHIM)
        _write_exec(self.bin / "curl", _PI_CURL_SHIM)
        for host in (THOR, ORIN, SPARK):
            home = self.login_home(host)
            (home / ".pi/agent").mkdir(parents=True)
            (home / ".pi/agent/models.json").write_text(PI_MODELS)

    def pi_node_dir(self, host: str) -> Path:
        return self.account_home(host, "pi") / ".local/share/pi-node" / PI_NODE_DIR


def _pi_version_from_a_bare_shell(wrapper: Path) -> subprocess.CompletedProcess:
    """Run the account's ~/.local/bin/pi from a PATH that holds NO fake node:
    the cli behind it is a bash body under a ``#!/usr/bin/env node`` shebang,
    so only the tarball's node can run it. If the wrapper does not prepend
    that node, env finds no node (or a real one, which chokes on bash)."""
    return subprocess.run(  # nosec B603 - the wrapper the lane just wrote
        [str(wrapper), "--version"],
        env={"PATH": os.environ["PATH"], "HOME": str(wrapper.parents[2])},
        capture_output=True,
        text=True,
        check=False,
    )


# --- pins and names -----------------------------------------------------------


def test_pi_and_qwen_pins_are_the_measured_versions():
    """#294: pi pins 0.85.0 (what the operator installed on thor/orin) and qwen
    stays at 0.22.0 (the version the ACP surface was measured on) even though
    orin's login user now carries 0.23.0. pi's shebang needs node >=22.19, and
    neither host has one, so the lane pins a node 22.x release tarball from
    nodejs.org -- the 22.23.2 the operator laid down by hand."""
    block = _block()
    assert f"UNIX_USER_PI_VERSION={PI_VERSION}\n" in block
    assert "UNIX_USER_QWEN_VERSION=0.22.0\n" in block
    assert f"UNIX_USER_PI_NODE_VERSION={PI_NODE_VERSION}\n" in block
    assert PI_NODE_VERSION.startswith("22.")
    assert "UNIX_USER_NODE_DIST_BASE=https://nodejs.org/dist" in block


def test_engine_ok_accepts_pi_and_its_role_is_pi_developer(tmp_path: Path):
    h = PiHarness(tmp_path)
    result = h.run("unix_user_engine_ok pi && unix_user_roles pi")
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == "pi-developer"
    # The engine list is stated in one place for the operator too: the usage
    # line of the hand-typed bootstrap form names pi.
    assert "<codex|claude|qwen|pi>" in _block()


# --- bootstrap ----------------------------------------------------------------


def test_bootstrap_pi_copies_models_json_beside_codex_and_qwen(tmp_path: Path):
    """thor|orin bootstrap three engines since #294. pi's credential is its
    provider config, ~/.pi/agent/models.json: copied 600 under a 700 .pi/agent
    the account owns all the way up (.pi included, or pi as the account could
    not create its own sessions dir beside it)."""
    h = PiHarness(tmp_path)
    result = h.run("unix_user_bootstrap thor-fake codex qwen pi")
    assert result.returncode == 0, result.stderr + result.stdout
    assert h.count("useradd[") == 3
    pi = h.account_home(THOR, "pi")
    assert oct(pi.stat().st_mode & 0o777) == "0o750"
    models = pi / ".pi/agent/models.json"
    assert models.read_text() == PI_MODELS
    assert oct(models.stat().st_mode & 0o777) == "0o600"
    assert oct((pi / ".pi/agent").stat().st_mode & 0o777) == "0o700"
    assert oct((pi / ".pi").stat().st_mode & 0o777) == "0o700"
    chown = h.calls()[h.first(f"chown[{THOR}]", "culture-pi:culture-pi", "/.pi")]
    assert chown.endswith("/culture-pi/.pi"), chown
    assert "credential .pi/agent/models.json: copied into culture-pi" in result.stdout
    # An account holds only what its bridge needs: pi's home carries no
    # codex, claude or qwen credential, and the other two hold no models.json.
    for other in (".codex", ".claude", ".qwen"):
        assert not (pi / other).exists(), other
    for engine in ("codex", "qwen"):
        assert not (h.account_home(THOR, engine) / ".pi").exists(), engine
    assert (h.account_home(THOR, "qwen") / ".qwen/settings.json").read_text() == QWEN_SETTINGS

    before = _snapshot(pi)
    h.clear_log()
    again = h.run("unix_user_bootstrap thor-fake codex qwen pi")
    assert again.returncode == 0, again.stderr
    assert _snapshot(pi) == before, "a second bootstrap changed the pi account"
    assert "credential .pi/agent/models.json: already in culture-pi" in again.stdout
    h.never("useradd[")


def test_bootstrap_pi_says_absent_when_the_login_user_has_no_models_json(tmp_path: Path):
    """Spec s12: on 2026-09-04 neither thor nor orin had a ~/.pi/agent/models.json
    yet. The bootstrap still creates the account and says so, the way it does
    for a missing auth.json, instead of failing or inventing a provider."""
    h = PiHarness(tmp_path)
    (h.login_home(ORIN) / ".pi/agent/models.json").unlink()
    result = h.run("unix_user_bootstrap orin-fake pi", host=ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    pi = h.account_home(ORIN, "pi")
    assert pi.is_dir()
    assert "absent on this host" in result.stdout
    assert ".pi/agent/models.json" in result.stdout
    assert not (pi / ".pi").exists()
    assert (pi / ".ssh/authorized_keys").read_text() == PUBKEY + "\n"


# --- provision ----------------------------------------------------------------


def test_provision_pi_installs_a_node_tarball_npm_pi_and_a_path_wrapper(tmp_path: Path):
    """The pi install (spec c4): a pinned node 22 tarball from nodejs.org
    untarred into ~/.local/share/pi-node/<node>/, @earendil-works/pi-coding-agent
    at the pin npm-installed INTO that node's prefix, and ~/.local/bin/pi a
    small wrapper that prepends that node's bin to PATH and execs the real
    pi -- because pi's shebang is ``#!/usr/bin/env node`` and the account has
    no node anywhere else. Idempotent on the pin: a second provision says
    'already installed' and changes nothing."""
    h = PiHarness(tmp_path)
    first = _provisioned(h, THOR, "pi")
    home = h.account_home(THOR, "pi")
    node_dir = h.pi_node_dir(THOR)
    assert (node_dir / "bin/node").exists()
    assert (node_dir / "bin/npm").is_symlink()
    assert (node_dir / "lib/node_modules/@earendil-works/pi-coding-agent/package.json").exists()
    assert (node_dir / "bin/pi").is_symlink(), "npm's own bin link into the node prefix"
    # The tarball came from nodejs.org, once, named for the pin and the arch.
    assert h.count("curl[", f"nodejs.org/dist/v{PI_NODE_VERSION}/{PI_NODE_DIR}.tar.gz") == 1
    npm = h.calls()[h.first(f"npm[{THOR}]")]
    assert f"@earendil-works/pi-coding-agent@{PI_VERSION}" in npm
    assert f"--prefix {node_dir}" in npm
    assert h.count("npm[") == 1
    h.never("curl[", "claude.ai/install.sh")
    h.never("curl[", "install-qwen-standalone.sh")
    wrapper = home / ".local/bin/pi"
    assert wrapper.is_file(), "the wrapper is a real file at ~/.local/bin/pi"
    assert not wrapper.is_symlink(), "a wrapper script, not a symlink to npm's bin"
    assert os.access(wrapper, os.X_OK)
    text = wrapper.read_text()
    assert text.startswith("#!/usr/bin/env bash\n")
    assert f"{node_dir}/bin:" in text, "the wrapper prepends the tarball's node to PATH"
    assert f'exec "{node_dir}/bin/pi"' in text
    bare = _pi_version_from_a_bare_shell(wrapper)
    assert bare.returncode == 0, bare.stderr
    assert bare.stdout.strip() == PI_VERSION
    assert (home / "git/culture-nodes-pi-developer/.git").is_dir()
    assert not (home / "git/culture-nodes-agent").exists()
    assert f"pi: installed {PI_VERSION} at {wrapper}" in first.stdout
    h.first(f"ssh[culture-pi@{THOR}]")

    before = _snapshot(home)
    h.clear_log()
    again = h.run(f"unix_user_provision {THOR} pi")
    assert again.returncode == 0, again.stderr + again.stdout
    assert _snapshot(home) == before, "a second provision changed the pi account"
    assert f"pi: {PI_VERSION} already installed at {wrapper}" in again.stdout
    h.never("curl[")
    h.never("npm[")
    h.never("sudo[")


def test_provision_pi_reinstalls_when_the_wrapper_reports_another_pin(tmp_path: Path):
    """The pin is the fact this file states (c24): a wrapper answering some
    other version -- an earlier hand install, a bumped pin -- is re-installed
    from the tarball, not trusted."""
    h = PiHarness(tmp_path)
    _provisioned(h, ORIN, "pi")
    wrapper = h.account_home(ORIN, "pi") / ".local/bin/pi"
    wrapper.write_text("#!/usr/bin/env bash\necho 0.84.2\n")
    h.clear_log()
    result = h.run(f"unix_user_provision {ORIN} pi", host=ORIN)
    assert result.returncode == 0, result.stderr + result.stdout
    assert h.count("curl[", "nodejs.org/dist") == 1
    assert h.count("npm[") == 1
    assert _pi_version_from_a_bare_shell(wrapper).stdout.strip() == PI_VERSION


def test_provision_pi_refuses_an_arch_with_no_node_tarball_before_downloading(tmp_path: Path):
    h = PiHarness(tmp_path)
    first = h.run("unix_user_bootstrap thor-fake pi")
    assert first.returncode == 0, first.stderr
    h.clear_log()
    result = h.run(f"unix_user_provision {THOR} pi", FAKE_ARCH="riscv64")
    assert result.returncode != 0
    assert "riscv64" in result.stderr
    assert "node" in result.stderr
    h.never("curl[", "nodejs.org")
    h.never("npm[")
    assert not (h.account_home(THOR, "pi") / ".local/bin/pi").exists()


def test_provision_pi_refuses_without_models_json_and_names_the_bootstrap(tmp_path: Path):
    """The qwen rule (#249 finding 1) applied to pi: an account with no
    ~/.pi/agent/models.json has no provider, so its bridge would start and
    every session would fail on the first request. Refuse at provision and
    name the root step that copies the file."""
    h = PiHarness(tmp_path)
    _provisioned(h, THOR, "pi")
    (h.account_home(THOR, "pi") / ".pi/agent/models.json").unlink()
    h.clear_log()
    refused = h.run(f"unix_user_provision {THOR} pi")
    assert refused.returncode != 0
    assert ".pi/agent/models.json" in refused.stderr
    assert "bootstrap pi" in refused.stderr
    h.never("curl[")


# --- session check ------------------------------------------------------------


def test_account_session_check_sees_a_pi_bridge_session(tmp_path: Path):
    """A pi bridge session runs as pi_bridge.pi_cli under culture-pi; the
    account session check must refuse to restart its unit mid-session the way
    it does for codex and qwen. Bracket idiom, like every other alternative,
    so the pattern never matches its own ssh shell."""
    lane = LANE.read_text()
    m = re.search(r"UNIX_USER_SESSION_PATTERN='([^']+)'", lane)
    assert m
    assert "pi_bridge[.]pi_cli" in m.group(1).split("|")
    h = PiHarness(tmp_path)
    _provisioned(h, THOR, "pi")
    h.clear_log()
    result = h.run(
        "unix_user_account_session_check thor-fake pi\n"
        'ssh culture-pi@thor-fake "systemctl --user stop culture-nodes-pi-developer"',
        FAKE_SESSION_USER="culture-pi",
    )
    assert result.returncode != 0
    assert "culture-pi" in result.stderr
    h.first(f"pgrep[{THOR}] -u culture-pi -f", "pi_bridge[.]pi_cli")
    h.never("systemctl[")


# --- bootstrap-accounts.sh host map -------------------------------------------

BOOTSTRAP_ACCOUNTS = ROOT / "deploy/prod/bootstrap-accounts.sh"

# bootstrap-accounts.sh's ssh branch: the doctor answers 42 (no shipped
# checkout on the host, a WARNING) and the sudo step 0, both logged.
_BOOTSTRAP_SSH_SHIM = """#!/usr/bin/env bash
while [ "$1" = -t ] || [ "$1" = -o ]; do [ "$1" = -o ] && shift; shift; done
host=$1; shift
printf 'ssh[%s] %s\\n' "$host" "$*" >> "$FAKE_LOG"
case "$*" in *"nodes doctor"*) exit 42 ;; esac
exit 0
"""


def _bootstrap_accounts_over_ssh(h: Harness, host_arg: str) -> subprocess.CompletedProcess:
    """`deploy/prod/bootstrap-accounts.sh <host>` typed on spark for another
    host: hostname answers spark, so the ssh branch stages the lane and runs
    the root step there with the host's engine list."""
    _write_exec(h.bin / "hostname", "#!/usr/bin/env bash\necho spark\n")
    _write_exec(h.bin / "scp", _LOG_ONLY_SHIM)
    _write_exec(h.bin / "ssh", _BOOTSTRAP_SSH_SHIM)
    env = {
        "PATH": f"{h.bin}{os.pathsep}{os.environ['PATH']}",
        "HOME": str(h.login_home(SPARK)),
        "FAKE_LOG": str(h.log),
        "FAKE_HOSTS": str(h.hosts),
        "FAKE_HOST": SPARK,
        "FAKE_USER": "spark",
        "FAKE_LOCAL_HOST": LOCAL_HOST,
    }
    return subprocess.run(  # nosec B603 - the wrapper itself, under the shims
        ["bash", str(BOOTSTRAP_ACCOUNTS), host_arg],
        env=env,
        cwd=h.login_home(SPARK),
        text=True,
        capture_output=True,
        check=False,
    )


def test_bootstrap_accounts_maps_thor_and_orin_to_codex_qwen_pi_and_spark_stays(tmp_path: Path):
    """#294: thor and orin each gain culture-qwen and culture-pi beside
    culture-codex; spark keeps claude + qwen (its culture-qwen and the
    qwen-developer bridge stay as they are, and spark gets no pi account
    unless the operator asks). The header comment states the same map, so
    what the operator reads is what the script runs."""
    script = BOOTSTRAP_ACCOUNTS.read_text()
    assert re.search(r'^\s*spark\)\s+ENGINES="claude qwen"', script, re.M)
    assert re.search(r'^\s*orin\|thor\)\s+ENGINES="codex qwen pi"', script, re.M)
    header = script[: script.index("set -euo pipefail")]
    assert "bootstrap-accounts.sh orin" in header
    assert "codex qwen pi" in header
    assert "bootstrap-accounts.sh thor" in header
    assert "bootstrap-accounts.sh spark" in header
    assert "culture-claude + culture-qwen" in header
    for host in ("thor", "orin"):
        (tmp_path / host).mkdir()
        h = PiHarness(tmp_path / host)
        result = _bootstrap_accounts_over_ssh(h, host)
        assert result.returncode == 0, result.stderr + result.stdout
        h.first(f"ssh[{host}] sudo bash", "bootstrap codex qwen pi")
        assert f"bootstrapping codex qwen pi on {host}" in result.stdout
