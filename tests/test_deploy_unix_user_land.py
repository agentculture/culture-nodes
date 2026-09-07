"""Fake-host coverage for the `land` engine in deploy/prod/lanes/unix-user.sh
(loop-closure t5, spec c13/c38, issue #315).

culture-land is the account the land node runs as: deterministic code the
runner executes to fetch a handover ref, rebase, gate, push and reply. It is
the one engine with NO engine binary and NO model credential -- what the
account holds is one checkout (``~/git/culture-nodes-land``) and the two git
credentials ``deploy/prod/install-secrets.sh`` delivers through
``lanes/land-secrets.sh`` (``bridge-push.env``, Contents write;
``land-pr.env``, pull-requests:write). tests/deploy/landcutover_test.go drives
``cutover.sh thor land`` end to end with a recorder standing in for
``deploy.sh``; this module is the half that recorder cannot reach -- the root
bootstrap and the account provision themselves -- run through the same
fake-host harness as tests/test_deploy_unix_user.py (imported, not copied).

NO bootstrap is run against a real host anywhere here, and none is implied:
creating ``culture-land`` on thor is the operator's counted hand-turn
(``deploy/prod/bootstrap-accounts.sh thor``), which is exactly what the
harness's fake ``sudo`` stands in for.
"""

from __future__ import annotations

from pathlib import Path

from tests.test_deploy_unix_user import (
    THOR,
    Harness,
    _block,
    _provisioned,
    _snapshot,
)


def test_engine_ok_accepts_land_and_its_role_is_land(tmp_path: Path):
    h = Harness(tmp_path)
    result = h.run("unix_user_engine_ok land && unix_user_roles land")
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == "land"
    # The engine list is stated in one place for the operator too: the usage
    # line of the hand-typed bootstrap form names land.
    assert "<codex|claude|qwen|pi|colleague|land>" in _block()


def test_bootstrap_land_creates_a_confined_account_with_no_model_credential(tmp_path: Path):
    """The root step for land is the account, its 750 home, linger and the
    operator key -- and nothing copied from the login user: there is no
    ~/.codex, ~/.claude, ~/.qwen, ~/.pi or ~/.colleague to copy, and the
    bootstrap says so instead of guessing a credential path."""
    h = Harness(tmp_path)
    result = h.run(f"unix_user_bootstrap {THOR} land")
    assert result.returncode == 0, result.stderr + result.stdout
    home = h.account_home(THOR, "land")
    assert oct(home.stat().st_mode & 0o777) == "0o750"
    assert (home / ".ssh/authorized_keys").exists()
    for cred_dir in (".codex", ".claude", ".qwen", ".pi", ".colleague"):
        assert not (home / cred_dir).exists(), cred_dir
    assert "credential: none for land" in result.stdout
    assert "bridge-push.env and land-pr.env" in result.stdout
    # A second bootstrap is a no-op per step: byte-identical account state.
    before = _snapshot(home)
    h.clear_log()
    again = h.run(f"unix_user_bootstrap {THOR} land")
    assert again.returncode == 0, again.stderr
    assert _snapshot(home) == before, "a second bootstrap changed the land account"


def test_provision_land_installs_no_engine_and_clones_the_land_checkout(tmp_path: Path):
    """After root: the guard, uv (the gate chain's pytest runs under it), the
    git identity, ONE checkout and the inventory. No engine installer runs
    and no ~/.local/bin/<engine> lands -- the land node is code, not a
    session."""
    h = Harness(tmp_path)
    result = _provisioned(h, THOR, "land")
    home = h.account_home(THOR, "land")
    assert (home / "git/culture-nodes-land/.git").is_dir()
    assert "no engine binary for land" in result.stdout
    assert (
        "culture-land provisioned on thor-fake: land (no engine binary), 1 checkout(s)"
        in result.stdout
    )
    for engine in ("codex", "claude", "qwen", "pi", "colleague", "land"):
        assert not (home / ".local/bin" / engine).exists(), engine
    # Only uv's installer was fetched; no engine release, no node tarball.
    installers = ("codex/releases", "claude.ai/install.sh", "install-qwen-standalone", "nodejs.org")
    for installer in installers:
        h.never("curl[", installer)
    # No --version probe of an engine that does not exist, no npm.
    h.never("npm[")


def test_provision_land_inventory_admits_exactly_the_two_credential_files(tmp_path: Path):
    """The inventory guard (h29) admits bridge-push.env AND land-pr.env, both
    mode 600, and still refuses anything else under ~/.culture-nodes -- a
    prod.env in a land account would be the operator secret bundle crossing
    the boundary the account exists to draw."""
    h = Harness(tmp_path)
    _provisioned(h, THOR, "land")
    home = h.account_home(THOR, "land")
    cn = home / ".culture-nodes"
    for name, line in (
        ("bridge-push.env", "GITHUB_TOKEN_WORKER=ghp-contents\n"),
        ("land-pr.env", "GITHUB_TOKEN_LAND_PR=ghp-pull-requests\n"),
    ):
        (cn / name).write_text(line)
        (cn / name).chmod(0o600)
    again = h.run(f"unix_user_provision {THOR} land")
    assert again.returncode == 0, again.stderr + again.stdout
    assert "inventory ok" in again.stdout

    # A mode-644 credential is refused: every env file is written umask 077.
    (cn / "land-pr.env").chmod(0o644)
    loose = h.run(f"unix_user_provision {THOR} land")
    assert loose.returncode != 0
    assert "land-pr.env is mode 644" in loose.stderr
    (cn / "land-pr.env").chmod(0o600)

    # Operator material is refused by name.
    (cn / "prod.env").write_text("NODES_DATABASE_URL=postgres://x\n")
    (cn / "prod.env").chmod(0o600)
    refused = h.run(f"unix_user_provision {THOR} land")
    assert refused.returncode != 0
    assert "prod.env" in refused.stderr
    # The refusal names the admitted inventory, land-pr.env included.
    assert "land-pr.env" in refused.stderr
