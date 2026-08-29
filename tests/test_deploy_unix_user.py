"""Fake-host coverage for deploy/prod/lanes/unix-user.sh (task t1, #243).

Same harness style as tests/test_deploy_two_host.py: the real lane text
between its marker comments is extracted and executed under
``set -euo pipefail`` with an ``ssh`` shim on PATH that runs the "remote"
command locally against a per-host temporary HOME. The shim never reaches a
network.

What this file adds over the two-host harness is a per-ACCOUNT axis: the
lane reaches an engine account as ``culture-<engine>@<host>`` (``@localhost``
on spark), and the shim maps that target to its own fake home under the
host's fake root -- ``<host>/home/culture-<engine>`` -- exactly as a real
``useradd -m`` would lay it out beside the login user. A target whose home
does not exist fails the way a real ssh to an account that was never
bootstrapped does (exit 255), which is how "provision before bootstrap" is
refused.

Every side-effecting host tool the lane invokes (``sudo``, ``useradd``,
``loginctl``, ``chown``, ``curl``, ``pgrep``, ``systemctl``, ``id``,
``getent``) is a recording fake that appends one line to a shared log, so a
test can assert ORDER and ABSENCE -- useradd only from the bootstrap, no
systemctl after a session refusal -- rather than just outcome. ``git``,
``tar``, ``stat``, ``chmod`` and ``cmp`` are real: the state they produce is
what the idempotence snapshot compares.
"""

from __future__ import annotations

import hashlib
import os
import re
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
LANE = ROOT / "deploy/prod/lanes/unix-user.sh"
MARKERS = ("# UNIX_USER_LANE_START", "# UNIX_USER_LANE_END")

THOR = "thor-fake"
ORIN = "orin-fake"
SPARK = "spark-fake"
# ``spark`` is the host the lane cannot ssh to as itself; the lane reaches
# its engine accounts as culture-<engine>@localhost there.
LOCAL_HOST = SPARK

PUBKEY = "ssh-ed25519 AAAAfakeoperatorkey operator-key"
CODEX_AUTH = '{"tokens":{"access_token":"fake-codex"}}\n'
CLAUDE_CREDS = '{"claudeAiOauth":{"accessToken":"fake-claude"}}\n'
# The login user's qwen config, in the measured shape of ~/.qwen/settings.json
# (2026-08-26): the model provider names the env var that carries its API
# key (envKey), and the env block holds that variable's value. The account's
# session reads its OWN copy of this file, so the bootstrap copies it the way
# it copies codex's auth.json and claude's .credentials.json.
QWEN_KEY_NAME = "QWEN_CUSTOM_API_KEY_OPENAI_HTTP_LOCALHOST_8001_FAKE"
QWEN_SETTINGS = (
    '{"env": {"%s": "fake-qwen-key"}, "modelProviders": {"openai": [{"id": "m", '
    '"baseUrl": "http://localhost:8001/v1", "envKey": "%s"}]}, '
    '"model": {"name": "m", "baseUrl": "http://localhost:8001/v1"}}\n' % (QWEN_KEY_NAME, QWEN_KEY_NAME)
)

# The shim strips every lane variable from the remote environment, like a
# real sshd (AcceptEnv admits LANG/LC_* only): a lane that wants the host to
# see a version or a URL has to carry it inside the command.
_SSH_SHIM = """#!/usr/bin/env bash
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
# Like a real ssh to an account that does not exist (or a host that is
# down): no shell is ever started, and the caller sees 255.
[ -d "$home" ] || exit 255
cd "$home" || exit 255
strip=()
for v in $(compgen -e UNIX_USER_) SKIP_SESSION_CHECK FIRST_DEPLOY; do strip+=(-u "$v"); done
exec env "${strip[@]}" FAKE_HOST="$host" FAKE_USER="$user" FAKE_UID=1000 HOME="$home" \\
  bash -c "$*"
"""

_SUDO_SHIM = """#!/usr/bin/env bash
printf 'sudo[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
if [ "${FAKE_SUDO_NEEDS_PASSWORD:-0}" = 1 ]; then
  echo "sudo: a password is required" >&2
  exit 1
fi
[ "$1" = -n ] && shift
exec env FAKE_UID=0 SUDO_USER="$FAKE_USER" "$@"
"""

_USERADD_SHIM = """#!/usr/bin/env bash
printf 'useradd[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
name=${*: -1}
home="$FAKE_HOSTS/$FAKE_HOST/home/$name"
[ -e "$home" ] && { echo "useradd: user '$name' already exists" >&2; exit 9; }
mkdir -p "$home"
# login.defs HOME_MODE is 0750 on the real hosts; the fake creates 755 so a
# lane that ASSUMES the default instead of asserting it is caught.
chmod 755 "$home"
"""

_GETENT_SHIM = """#!/usr/bin/env bash
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

_ID_SHIM = """#!/usr/bin/env bash
case "$1" in
  -u) echo "${FAKE_UID:-1000}" ;;
  -un) echo "$FAKE_USER" ;;
  -nG) name=${2:-$FAKE_USER}; echo "$name ${FAKE_EXTRA_GROUPS:-}" ;;
  *) echo "uid=${FAKE_UID:-1000}($FAKE_USER)" ;;
esac
"""

# stat is real except for ownership (-c %U), which the fake filesystem
# cannot carry (every file is the test user's): the owner of a path under
# /home/<name> answers as <name>, the way useradd -m + chown -R leave a real
# account, and FAKE_FOREIGN_OWNER_PATH names one path some other user owns.
_STAT_SHIM = """#!/usr/bin/env bash
if [ "$1" = -c ] && [ "$2" = %U ]; then
  path=$3
  [ -n "${FAKE_FOREIGN_OWNER_PATH:-}" ] && [ "$path" = "$FAKE_FOREIGN_OWNER_PATH" ] && { echo intruder; exit 0; }
  rel=${path#"$FAKE_HOSTS"/*/home/}
  echo "${rel%%/*}"
  exit 0
fi
exec /usr/bin/stat "$@"
"""

_LOG_ONLY_SHIM = """#!/usr/bin/env bash
printf '%s[%s] %s\\n' "$(basename "$0")" "$FAKE_HOST" "$*" >> "$FAKE_LOG"
exit 0
"""

_PGREP_SHIM = """#!/usr/bin/env bash
printf 'pgrep[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
if [ "${FAKE_SESSION_RUNNING:-0}" = 1 ]; then echo 4242; exit 0; fi
exit 1
"""

# curl serves every installer the lane knows: a script on stdout for the
# pipe-to-bash installers, a tarball for the codex release download. Each
# fake engine prints the version it was installed at, which is what the
# lane's already-installed check reads.
_CURL_SHIM = """#!/usr/bin/env bash
printf 'curl[%s] %s\\n' "$FAKE_HOST" "$*" >> "$FAKE_LOG"
url=; out=
while [ $# -gt 0 ]; do
  case "$1" in -o) out=$2; shift ;; -*) ;; *) url=$1 ;; esac
  shift
done
case "$url" in
  *astral.sh/uv/install.sh*)
    printf '%s\\n' 'mkdir -p "$HOME/.local/bin"' \\
      'printf "#!/usr/bin/env bash\\necho uv-fake\\n" > "$HOME/.local/bin/uv"' \\
      'chmod +x "$HOME/.local/bin/uv"' ;;
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
  *) echo "curl fake: no such URL $url" >&2; exit 22 ;;
esac
"""


def _block() -> str:
    script = LANE.read_text()
    start = script.index(MARKERS[0])
    end = script.index(MARKERS[1], start) + len(MARKERS[1])
    return script[start:end]


def _write_exec(path: Path, body: str) -> None:
    path.write_text(body)
    path.chmod(0o755)


def _git(repo: Path, *args: str) -> str:
    out = subprocess.run(  # nosec B603 B607 - fixed git argv against a temp repo
        ["git", "-c", "user.name=t", "-c", "user.email=t@example.test", *args],
        cwd=repo,
        check=True,
        capture_output=True,
        text=True,
    )
    return out.stdout.strip()


def _seed_repo(path: Path) -> Path:
    path.mkdir(parents=True)
    _git(path, "init", "-q", "-b", "main")
    (path / "README.md").write_text("seed\n")
    _git(path, "add", "README.md")
    _git(path, "commit", "-q", "-m", "one")
    return path


# git's own record that a fetch and a fast-forward were attempted: a
# re-provision fetches on purpose (that is the ff-only check), so these two
# bookkeeping files are the one thing allowed to appear between runs.
_GIT_BOOKKEEPING = {"FETCH_HEAD", "ORIG_HEAD"}


def _snapshot(root: Path) -> dict[str, str]:
    """Every path under root with its mode and content digest, mtimes excluded."""
    out: dict[str, str] = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in sorted(dirnames):
            p = Path(dirpath) / name
            rel = str(p.relative_to(root))
            out[rel] = f"dir {oct(p.lstat().st_mode & 0o777)}"
        for name in sorted(filenames):
            p = Path(dirpath) / name
            rel = str(p.relative_to(root))
            if name in _GIT_BOOKKEEPING and Path(dirpath).name == ".git":
                continue
            if p.is_symlink():
                out[rel] = f"link {os.readlink(p)}"
            else:
                digest = hashlib.sha256(p.read_bytes()).hexdigest()
                out[rel] = f"file {oct(p.lstat().st_mode & 0o777)} {digest}"
    return out


class Harness:
    def __init__(self, tmp_path: Path):
        self.tmp = tmp_path
        self.log = tmp_path / "calls.log"
        self.log.touch()
        self.hosts = tmp_path / "hosts"
        self.bin = tmp_path / "bin"
        self.bin.mkdir()
        _write_exec(self.bin / "ssh", _SSH_SHIM)
        _write_exec(self.bin / "sudo", _SUDO_SHIM)
        _write_exec(self.bin / "useradd", _USERADD_SHIM)
        _write_exec(self.bin / "getent", _GETENT_SHIM)
        _write_exec(self.bin / "id", _ID_SHIM)
        _write_exec(self.bin / "curl", _CURL_SHIM)
        _write_exec(self.bin / "pgrep", _PGREP_SHIM)
        _write_exec(self.bin / "stat", _STAT_SHIM)
        for tool in ("loginctl", "chown", "systemctl"):
            _write_exec(self.bin / tool, _LOG_ONLY_SHIM)
        # The origin every role clone comes from: a bare repo standing in for
        # https://github.com/agentculture/culture-nodes.
        seed = _seed_repo(tmp_path / "seed")
        self.origin = tmp_path / "origin.git"
        subprocess.run(  # nosec B603 B607
            ["git", "clone", "-q", "--bare", str(seed), str(self.origin)],
            check=True,
            capture_output=True,
        )
        for host in (THOR, ORIN, SPARK):
            # /home/<login> beside /home/culture-<engine>, as on a real host.
            home = self.login_home(host)
            (home / ".ssh").mkdir(parents=True)
            (home / ".ssh").chmod(0o700)
            if host == SPARK:
                # spark accepts no key for its own user: the operator's key
                # exists only as the identity file.
                (home / ".ssh/id_ed25519.pub").write_text(PUBKEY + "\n")
            else:
                (home / ".ssh/authorized_keys").write_text(PUBKEY + "\n")
            (home / ".codex").mkdir()
            (home / ".codex/auth.json").write_text(CODEX_AUTH)
            (home / ".codex/auth.json").chmod(0o600)
            (home / ".claude").mkdir()
            (home / ".claude/.credentials.json").write_text(CLAUDE_CREDS)
            (home / ".claude/.credentials.json").chmod(0o600)
            (home / ".qwen").mkdir()
            (home / ".qwen/settings.json").write_text(QWEN_SETTINGS)
            # The login user's own agent checkout, with an unpushed commit
            # and a dirty file: the lane must never write here (c30).
            repo = _seed_repo(home / "git/culture-nodes-agent")
            (repo / "WIP.md").write_text("unpushed work\n")
            _git(repo, "add", "WIP.md")
            _git(repo, "commit", "-q", "-m", "unpushed")
            (repo / "README.md").write_text("harvest me\n")
            (home / ".culture-nodes").mkdir()
            (home / ".culture-nodes/prod.env").write_text("NODES_DATABASE_URL=postgres://fake\n")

    def login_home(self, host: str) -> Path:
        return self.hosts / host / "home" / host.removesuffix("-fake")

    def account_home(self, host: str, engine: str) -> Path:
        return self.hosts / host / "home" / f"culture-{engine}"

    def run(self, body: str, host: str = THOR, **fake_env: str) -> subprocess.CompletedProcess:
        script = (
            "set -euo pipefail\n"
            "say() { printf '==> %s\\n' \"$*\"; }\n"
            f"HOST={host}\nREMOTE_DIR=culture-nodes-prod\n" + _block() + "\n" + body + "\n"
        )
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(self.tmp / "operator"),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "FAKE_LOCAL_HOST": LOCAL_HOST,
            "UNIX_USER_REPO_URL": str(self.origin),
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

    def run_local_bootstrap(self, *engines: str, **fake_env: str) -> subprocess.CompletedProcess:
        """The spark/orin form: the operator types `sudo bash lanes/unix-user.sh
        bootstrap <engine>...` on the host itself, so the lane is EXECUTED,
        not sourced, and there is no ssh in the path at all."""
        host = SPARK
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(self.login_home(host)),
            "FAKE_LOG": str(self.log),
            "FAKE_HOSTS": str(self.hosts),
            "FAKE_HOST": host,
            "FAKE_USER": host.removesuffix("-fake"),
            "FAKE_LOCAL_HOST": LOCAL_HOST,
            **fake_env,
        }
        return subprocess.run(  # nosec B603 - the lane itself, under the shims
            ["sudo", "bash", str(LANE), "bootstrap", *engines],
            env=env,
            cwd=self.login_home(host),
            text=True,
            capture_output=True,
            check=False,
        )

    def calls(self) -> list[str]:
        return self.log.read_text().splitlines()

    def first(self, *needles: str) -> int:
        for i, line in enumerate(self.calls()):
            if all(needle in line for needle in needles):
                return i
        raise AssertionError(f"{needles!r} never happened; log:\n" + "\n".join(self.calls()))

    def count(self, *needles: str) -> int:
        return sum(1 for line in self.calls() if all(n in line for n in needles))

    def never(self, *needles: str) -> None:
        hits = [line for line in self.calls() if all(needle in line for needle in needles)]
        assert not hits, f"{needles!r} happened: {hits}"

    def clear_log(self) -> None:
        self.log.write_text("")


# --- shape ------------------------------------------------------------------


def test_lane_parses_and_defines_the_four_verbs():
    subprocess.run(["bash", "-n", str(LANE)], check=True)  # nosec B603 B607
    for fn in (
        "unix_user_bootstrap",
        "unix_user_provision",
        "unix_user_session_check",
        "unix_user_rollback_pair",
    ):
        assert f"{fn}()" in _block(), f"{fn} is not defined inside the marker block"


def test_versions_are_pinned_in_variables_at_the_top():
    block = _block()
    for var in ("UNIX_USER_CODEX_VERSION=", "UNIX_USER_CLAUDE_VERSION=", "UNIX_USER_QWEN_VERSION="):
        assert var in block, f"{var} is not pinned in the lane"
        assert block.index(var) < block.index("unix_user_bootstrap()")


# --- bootstrap ----------------------------------------------------------------


def test_bootstrap_creates_the_account_and_is_a_no_op_the_second_time(tmp_path: Path):
    h = Harness(tmp_path)
    first = h.run("unix_user_bootstrap thor-fake codex")
    assert first.returncode == 0, first.stderr
    home = h.account_home(THOR, "codex")
    assert home.is_dir()
    assert oct(home.stat().st_mode & 0o777) == "0o750", "home mode is asserted, not assumed"
    h.first(f"useradd[{THOR}] -m -s /bin/bash culture-codex")
    h.first(f"loginctl[{THOR}] enable-linger culture-codex")
    # The only root step, run through sudo -n on a NOPASSWD host.
    h.first(f"sudo[{THOR}] -n")
    keys = (home / ".ssh/authorized_keys").read_text().splitlines()
    assert keys == [PUBKEY]
    assert oct((home / ".ssh/authorized_keys").stat().st_mode & 0o777) == "0o600"
    assert oct((home / ".ssh").stat().st_mode & 0o777) == "0o700"
    auth = home / ".codex/auth.json"
    assert auth.read_text() == CODEX_AUTH
    assert oct(auth.stat().st_mode & 0o777) == "0o600"
    h.first(f"chown[{THOR}]", "culture-codex:culture-codex", ".codex")
    # The account is a codex account: it holds codex's credential and not
    # claude's, because a home holds only what its bridge needs.
    assert not (home / ".claude").exists()
    assert "created" in first.stdout

    before = _snapshot(home)
    h.clear_log()
    second = h.run("unix_user_bootstrap thor-fake codex")
    assert second.returncode == 0, second.stderr
    assert _snapshot(home) == before, "a second bootstrap changed the account"
    h.never("useradd[")
    assert keys == (home / ".ssh/authorized_keys").read_text().splitlines()
    assert "present" in second.stdout


def test_bootstrap_takes_several_engines_and_copies_per_engine_credentials(tmp_path: Path):
    h = Harness(tmp_path)
    # spark's login user has no authorized_keys: the identity file is the key.
    result = h.run("unix_user_bootstrap spark-fake claude qwen", host=SPARK)
    assert result.returncode == 0, result.stderr
    claude = h.account_home(SPARK, "claude")
    qwen = h.account_home(SPARK, "qwen")
    assert (claude / ".claude/.credentials.json").read_text() == CLAUDE_CREDS
    assert oct((claude / ".claude/.credentials.json").stat().st_mode & 0o777) == "0o600"
    assert not (claude / ".codex").exists()
    assert (qwen / ".ssh/authorized_keys").read_text() == PUBKEY + "\n"
    assert not (qwen / ".codex").exists()
    assert not (qwen / ".claude").exists()
    # qwen's credential is its settings file (#249 review, finding 1): the
    # endpoint and the API-key variable the session authenticates with live
    # there, and a session under culture-qwen reads culture-qwen's copy.
    settings = qwen / ".qwen/settings.json"
    assert settings.read_text() == QWEN_SETTINGS
    assert oct(settings.stat().st_mode & 0o777) == "0o600"
    assert oct((qwen / ".qwen").stat().st_mode & 0o777) == "0o700"
    h.first(f"chown[{SPARK}]", "culture-qwen:culture-qwen", ".qwen")
    assert not (claude / ".qwen").exists()
    assert h.count("useradd[") == 2
    for engine in ("claude", "qwen"):
        assert oct(h.account_home(SPARK, engine).stat().st_mode & 0o777) == "0o750"


def test_bootstrap_never_touches_the_login_users_git_tree(tmp_path: Path):
    h = Harness(tmp_path)
    login_git = h.login_home(THOR) / "git"
    before = _snapshot(login_git)
    result = h.run("unix_user_bootstrap thor-fake codex")
    assert result.returncode == 0, result.stderr
    assert _snapshot(login_git) == before
    assert "/git/culture-nodes-agent" not in " ".join(
        line for line in h.calls() if not line.startswith("ssh[")
    )


def test_bootstrap_refuses_an_unknown_engine_before_useradd(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_bootstrap thor-fake gemini")
    assert result.returncode != 0
    assert "gemini" in result.stderr
    h.never("useradd[")


def test_bootstrap_without_passwordless_sudo_names_the_hand_turn(tmp_path: Path):
    """spark and orin need a typed password: the lane cannot bootstrap them
    over ssh, and says exactly what the operator has to type instead."""
    h = Harness(tmp_path)
    result = h.run("unix_user_bootstrap orin-fake codex", host=ORIN, FAKE_SUDO_NEEDS_PASSWORD="1")
    assert result.returncode != 0
    assert "sudo bash" in result.stderr
    assert "bootstrap codex" in result.stderr
    h.never("useradd[")
    assert not h.account_home(ORIN, "codex").exists()


def test_bootstrap_local_form_runs_the_lane_as_a_script_under_sudo(tmp_path: Path):
    """`sudo bash deploy/prod/lanes/unix-user.sh bootstrap claude qwen` on the
    host itself, with no ssh at all: the same root step, typed by hand."""
    h = Harness(tmp_path)
    result = h.run_local_bootstrap("claude", "qwen")
    assert result.returncode == 0, result.stderr + result.stdout
    h.never("ssh[")
    assert h.count("useradd[") == 2
    claude = h.account_home(SPARK, "claude")
    assert oct(claude.stat().st_mode & 0o777) == "0o750"
    assert (claude / ".ssh/authorized_keys").read_text() == PUBKEY + "\n"
    assert (claude / ".claude/.credentials.json").read_text() == CLAUDE_CREDS


def test_bootstrap_local_form_refuses_when_not_root(tmp_path: Path):
    h = Harness(tmp_path)
    env = {
        "PATH": f"{h.bin}{os.pathsep}{os.environ['PATH']}",
        "HOME": str(h.login_home(SPARK)),
        "FAKE_LOG": str(h.log),
        "FAKE_HOSTS": str(h.hosts),
        "FAKE_HOST": SPARK,
        "FAKE_USER": "spark",
        "FAKE_UID": "1000",
    }
    result = subprocess.run(  # nosec B603
        ["bash", str(LANE), "bootstrap", "claude"],
        env=env,
        cwd=h.login_home(SPARK),
        text=True,
        capture_output=True,
        check=False,
    )
    assert result.returncode != 0
    assert "root" in result.stderr
    h.never("useradd[")


def _bootstrapped_codex(h: Harness) -> Path:
    first = h.run("unix_user_bootstrap thor-fake codex")
    assert first.returncode == 0, first.stderr
    h.clear_log()
    return h.account_home(THOR, "codex")


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
    import shutil

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


def test_bootstrap_refuses_an_account_that_gained_sudo_or_docker(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_bootstrap thor-fake codex", FAKE_EXTRA_GROUPS="docker")
    assert result.returncode != 0
    assert "docker" in result.stderr
    h.never("loginctl[")


# --- provision ----------------------------------------------------------------


def _provisioned(h: Harness, host: str, engine: str) -> subprocess.CompletedProcess:
    result = h.run(
        f"unix_user_bootstrap {host} {engine}\nunix_user_provision {host} {engine}", host=host
    )
    assert result.returncode == 0, result.stderr + result.stdout
    return result


def test_provision_twice_yields_byte_identical_account_state(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    home = h.account_home(THOR, "codex")
    # The pinned engine, installed into the account's own ~/.local/bin from
    # the standalone release, reporting the pinned version.
    codex = home / ".local/bin/codex"
    assert codex.exists()
    version = subprocess.run(
        [str(codex), "--version"], capture_output=True, text=True
    )  # nosec B603
    assert "0.147.0" in version.stdout or "codex-cli" in version.stdout
    assert (home / ".local/bin/uv").exists()
    repo = home / "git/culture-nodes-agent"
    assert (repo / ".git").is_dir()
    assert _git(repo, "rev-parse", "--abbrev-ref", "HEAD") == "main"
    assert oct((home / ".culture-nodes").stat().st_mode & 0o777) == "0o700"
    h.first(f"ssh[culture-codex@{THOR}]")
    h.never("ssh[culture-codex@localhost]")
    # Engine and uv installers run exactly once for a fresh account.
    assert h.count("curl[", "openai/codex/releases") == 1
    assert h.count("curl[", "astral.sh/uv") == 1

    before = _snapshot(home)
    h.clear_log()
    again = h.run(f"unix_user_provision {THOR} codex")
    assert again.returncode == 0, again.stderr + again.stdout
    assert _snapshot(home) == before, "a second provision changed the account"
    # Nothing root-only runs from provision, and nothing is re-downloaded.
    h.never("useradd[")
    h.never("loginctl[")
    h.never("sudo[")
    h.never("curl[")


def test_provision_never_writes_the_login_users_git_tree(tmp_path: Path):
    h = Harness(tmp_path)
    login_git = h.login_home(ORIN) / "git"
    before = _snapshot(login_git)
    _provisioned(h, ORIN, "codex")
    assert _snapshot(login_git) == before
    # Every account-side ssh is addressed to the account, never to the login
    # user with a path into its home.
    for line in h.calls():
        if line.startswith(f"ssh[{ORIN}]"):
            assert "culture-nodes-agent" not in line, line


def test_provision_claude_on_spark_reaches_localhost_and_clones_the_four_roles(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, SPARK, "claude")
    home = h.account_home(SPARK, "claude")
    for role in ("developer", "planner", "verifier", "intake"):
        assert (home / f"git/culture-nodes-{role}/.git").is_dir(), role
    assert not (home / "git/culture-nodes-agent").exists()
    h.first("ssh[culture-claude@localhost]")
    h.never(f"ssh[culture-claude@{SPARK}]")
    claude = home / ".local/bin/claude"
    out = subprocess.run([str(claude), "--version"], capture_output=True, text=True)  # nosec B603
    assert "2.1.251" in out.stdout
    assert h.count("curl[", "claude.ai/install.sh") == 1
    assert "2.1.251" in " ".join(h.calls())


def test_provision_qwen_on_spark_uses_the_standalone_installer(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, SPARK, "qwen")
    home = h.account_home(SPARK, "qwen")
    assert (home / "git/culture-nodes-qwen-developer/.git").is_dir()
    assert (home / ".local/bin/qwen").exists()
    assert h.count("curl[", "install-qwen-standalone.sh") == 1
    h.never("npm")


def test_provision_qwen_refuses_without_its_settings_file_and_names_the_bootstrap(
    tmp_path: Path,
):
    """A culture-qwen with no ~/.qwen/settings.json has no endpoint and no
    API key: its bridge would start and every session would fail to
    authenticate. The provision refuses and names the root step that copies
    the file (#249 review, finding 1)."""
    h = Harness(tmp_path)
    _provisioned(h, SPARK, "qwen")
    (h.account_home(SPARK, "qwen") / ".qwen/settings.json").unlink()
    h.clear_log()
    refused = h.run(f"unix_user_provision {SPARK} qwen", host=SPARK)
    assert refused.returncode != 0
    assert ".qwen/settings.json" in refused.stderr
    assert "bootstrap qwen" in refused.stderr
    h.never("curl[")


def test_provision_fast_forwards_a_clean_clone_and_refuses_a_dirty_one(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    seed = h.tmp / "seed"
    (seed / "NEW.md").write_text("two\n")
    _git(seed, "add", "NEW.md")
    _git(seed, "commit", "-q", "-m", "two")
    _git(seed, "push", "-q", str(h.origin), "main")
    again = h.run(f"unix_user_provision {THOR} codex")
    assert again.returncode == 0, again.stderr
    repo = h.account_home(THOR, "codex") / "git/culture-nodes-agent"
    assert (repo / "NEW.md").exists()
    assert "fast-forwarded" in again.stdout

    (repo / "README.md").write_text("session diff\n")
    dirty = h.run(f"unix_user_provision {THOR} codex")
    assert dirty.returncode != 0
    assert "uncommitted" in dirty.stderr or "DIRTY" in dirty.stderr
    assert (repo / "README.md").read_text() == "session diff\n"


def test_provision_refuses_an_account_that_was_never_bootstrapped(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run(f"unix_user_provision {THOR} codex")
    assert result.returncode != 0
    assert "bootstrap" in result.stderr
    h.never("curl[")


def test_provision_refuses_foreign_files_in_the_account_inventory(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    cn = h.account_home(THOR, "codex") / ".culture-nodes"
    for allowed in ("codex-bridge.env", "bridge-push.env"):
        (cn / allowed).write_text("X=y\n")
        (cn / allowed).chmod(0o600)
    (cn / "codex-bridge.json").write_text("{}\n")
    (cn / "dialin").mkdir()
    (cn / "codex-bridge-state").mkdir()
    (cn / "bin").mkdir()
    ok = h.run(f"unix_user_provision {THOR} codex")
    assert ok.returncode == 0, ok.stderr
    assert "inventory" in ok.stdout

    (cn / "prod.env").write_text("NODES_DATABASE_URL=postgres://real\n")
    (cn / "prod.env").chmod(0o600)
    (cn / "runner.env").write_text("NODES_RUNNER_LISTEN=:17070\n")
    (cn / "runner.env").chmod(0o600)
    refused = h.run(f"unix_user_provision {THOR} codex")
    assert refused.returncode != 0
    assert "prod.env" in refused.stderr
    assert "runner.env" in refused.stderr


def test_provision_refuses_an_env_file_that_is_not_mode_600(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    env = h.account_home(THOR, "codex") / ".culture-nodes/codex-bridge.env"
    env.write_text("CODEX_BRIDGE_AUTH_TOKEN=fake\n")
    env.chmod(0o644)
    refused = h.run(f"unix_user_provision {THOR} codex")
    assert refused.returncode != 0
    assert "codex-bridge.env" in refused.stderr
    assert "600" in refused.stderr


def test_provision_refuses_a_home_that_is_not_mode_750(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    home = h.account_home(THOR, "codex")
    home.chmod(0o755)
    h.clear_log()
    refused = h.run(f"unix_user_provision {THOR} codex")
    assert refused.returncode != 0
    assert "750" in refused.stderr
    h.never("curl[")


def test_provision_refuses_an_account_in_the_docker_or_sudo_group(tmp_path: Path):
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    h.clear_log()
    refused = h.run(f"unix_user_provision {THOR} codex", FAKE_EXTRA_GROUPS="sudo")
    assert refused.returncode != 0
    assert "sudo" in refused.stderr
    h.never("curl[")


def test_provision_refuses_a_human_decision_token_under_the_account(tmp_path: Path):
    """q5 (#243): the developer account must hold no NODES_HUMAN_DECISION_TOKEN,
    the bearer that makes human decisions on the approval surface."""
    h = Harness(tmp_path)
    _provisioned(h, SPARK, "claude")
    cfg = h.account_home(SPARK, "claude") / ".config/culture-nodes-bridges"
    cfg.mkdir(parents=True)
    (cfg / "developer.json").write_text('{"claude_env":{"NODES_HUMAN_DECISION_TOKEN":"x"}}\n')
    refused = h.run(f"unix_user_provision {SPARK} claude", host=SPARK)
    assert refused.returncode != 0
    assert "NODES_HUMAN_DECISION_TOKEN" in refused.stderr
    assert "developer.json" in refused.stderr


# --- session check --------------------------------------------------------------


def test_session_in_flight_refuses_before_any_systemctl(tmp_path: Path):
    h = Harness(tmp_path)
    body = (
        "unix_user_session_check thor-fake thor\n"
        'ssh thor-fake "systemctl --user stop codex-bridge"'
    )
    result = h.run(body, FAKE_SESSION_RUNNING="1")
    assert result.returncode != 0
    assert "SKIP_SESSION_CHECK=1" in result.stderr
    h.first(f"pgrep[{THOR}] -u thor -f", "[c]laude -p|[c]odex exec|qwen_bridge[.]qwen_cli")
    h.never("systemctl[")


def test_skip_session_check_warns_and_proceeds(tmp_path: Path):
    h = Harness(tmp_path)
    body = (
        "unix_user_session_check thor-fake thor\n"
        'ssh thor-fake "systemctl --user stop codex-bridge"'
    )
    result = h.run(body, FAKE_SESSION_RUNNING="1", SKIP_SESSION_CHECK="1")
    assert result.returncode == 0, result.stderr
    assert "WARNING" in result.stdout
    h.first(f"systemctl[{THOR}] --user stop codex-bridge")


def test_no_session_in_flight_proceeds_quietly(tmp_path: Path):
    h = Harness(tmp_path)
    body = (
        "unix_user_session_check thor-fake thor\n"
        'ssh thor-fake "systemctl --user stop codex-bridge"'
    )
    result = h.run(body)
    assert result.returncode == 0, result.stderr
    assert "WARNING" not in result.stdout
    h.first(f"systemctl[{THOR}] --user stop codex-bridge")


def test_codex_is_installed_as_the_full_package_with_its_code_mode_host(
    tmp_path: Path,
):
    """Run 01M17DN04BTX9JAS3W3H9NJ7ZP (2026-08-29): a bare codex binary
    answers --version and then blocks every session, because codex 0.147
    spawns codex-code-mode-host from beside itself. The lane installs the
    standalone PACKAGE, and a bare binary left by an earlier install is
    re-installed rather than trusted for its --version alone."""
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    home = h.account_home(THOR, "codex")
    codex = home / ".local/bin/codex"
    assert codex.is_symlink()
    assert (codex.resolve().parent / "codex-code-mode-host").exists()
    assert (home / ".codex/packages/standalone/current/bin/codex").exists()
    assert h.count("curl[", "codex-package-") == 1
    # degrade to the first cutover's bare binary and provision again
    codex.unlink()
    codex.write_text('#!/usr/bin/env bash\necho "codex-cli 0.147.0"\n')
    codex.chmod(0o755)
    result = h.run("unix_user_provision thor-fake codex")
    assert result.returncode == 0, result.stderr
    assert (codex.resolve().parent / "codex-code-mode-host").exists()
    assert h.count("curl[", "codex-package-") == 2


def test_codex_account_gets_network_in_workspace_write_via_its_own_config(tmp_path: Path):
    """Deviation d2 (#243): the account is the fence, so the account's own
    ~/.codex/config.toml turns network on for workspace-write; codex's
    [projects.*] trust entries in the same file survive, and a second
    provision does not append the section twice."""
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    cfg = h.account_home(THOR, "codex") / ".codex/config.toml"
    text = cfg.read_text()
    assert "[sandbox_workspace_write]" in text
    assert "network_access = true" in text
    assert oct(cfg.stat().st_mode & 0o777) == "0o600"
    cfg.write_text(text + '\n[projects."/x"]\ntrust_level = "trusted"\n')
    result = h.run("unix_user_provision thor-fake codex")
    assert result.returncode == 0, result.stderr
    again = cfg.read_text()
    assert again.count("[sandbox_workspace_write]") == 1
    assert 'trust_level = "trusted"' in again


def test_account_gets_a_git_identity_so_handover_commits_can_be_made(tmp_path: Path):
    """Run 01M17NJ8W7B6NQ2RKT6B89SF94 (#243): without user.name/user.email the
    bridge's git commit-tree fails and the handover reports workspace_export
    missing. Provision sets the account as its own identity, idempotently."""
    h = Harness(tmp_path)
    _provisioned(h, THOR, "codex")
    home = h.account_home(THOR, "codex")
    cfg = (home / ".gitconfig").read_text()
    assert "name = culture-codex" in cfg
    assert "email = culture-codex@" in cfg
    result = h.run("unix_user_provision thor-fake codex")
    assert result.returncode == 0, result.stderr
    assert "git identity: culture-codex already set" in result.stdout
    assert (home / ".gitconfig").read_text().count("name = culture-codex") == 1


def test_session_pattern_does_not_match_its_own_ssh_shell():
    """Regression for the first thor cutover (2026-08-29): over ssh the
    pattern rides inside ``bash -c "pgrep ... '<pattern>'"``, so an
    unbracketed pattern matched its OWN wrapping shell and every deploy
    refused with a phantom session. The fake pgrep shim could not see it;
    only the real pgrep can. Proven with a nonce so nothing else running
    on this host (an operator's ``claude -p``, a colleague review whose
    instruction quotes the pattern) can turn the test red or green."""
    import shutil
    import uuid

    if shutil.which("pgrep") is None:  # pragma: no cover
        pytest.skip("no pgrep on this host")
    lane = LANE.read_text()
    m = re.search(r"pgrep -u \$login -f '([^']+)'", lane)
    assert m, "the lane's session pattern moved"
    # every alternative must carry the bracket idiom, or it self-matches
    for alt in m.group(1).split("|"):
        assert "[" in alt, f"session-check alternative {alt!r} would match its own ssh shell"
    nonce = "aou" + uuid.uuid4().hex[:12]
    me = subprocess.run(["id", "-un"], capture_output=True, text=True).stdout.strip()
    # exactly how the lane ships it: one bash -c whose cmdline carries the pattern
    bracketed = subprocess.run(
        ["bash", "-c", f"pgrep -u {me} -f '[{nonce[0]}]{nonce[1:]} -p' >/dev/null"],
        capture_output=True,
    )
    assert bracketed.returncode == 1, "the bracketed pattern matched its own shell"
    naive = subprocess.run(
        ["bash", "-c", f"pgrep -u {me} -f '{nonce} -p' >/dev/null"], capture_output=True
    )
    assert naive.returncode == 0, "the control should self-match; if not, this test proves nothing"


def test_unreachable_host_is_refused_not_treated_as_no_session(tmp_path: Path):
    """Three states, like preflight's orin probe: sessions (0), none (1), and
    ssh never reached the host (255) — the last must not read as 'none'."""
    h = Harness(tmp_path)
    body = (
        "unix_user_session_check no-such-host thor\n"
        'ssh thor-fake "systemctl --user stop codex-bridge"'
    )
    result = h.run(body)
    assert result.returncode != 0
    assert "no-such-host" in result.stderr
    h.never("systemctl[")


def test_session_check_validates_the_login_user_it_splices(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_session_check thor-fake 'thor; touch pwned'")
    assert result.returncode != 0
    h.never("pgrep[")
    assert not (h.login_home(THOR) / "pwned").exists()


# --- rollback pair ----------------------------------------------------------------


def test_rollback_pair_prints_stop_as_the_account_then_start_as_the_login_user(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_rollback_pair codex codex-bridge")
    assert result.returncode == 0, result.stderr
    out = result.stdout
    stop = out.index("systemctl --user stop codex-bridge")
    start = out.index("systemctl --user start codex-bridge")
    assert stop < start
    assert out.index("culture-codex@thor-fake") < stop
    assert "ssh thor-fake" in out[stop:start]
    # Printing is all it does: nothing is stopped or started.
    h.never("systemctl[")
    h.never("ssh[")


def test_rollback_pair_on_spark_addresses_localhost(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_rollback_pair claude culture-nodes-claude-developer", host=SPARK)
    assert result.returncode == 0, result.stderr
    assert "culture-claude@localhost" in result.stdout
    assert "systemctl --user start culture-nodes-claude-developer" in result.stdout
