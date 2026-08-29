"""`.claude/skills/jira/` — the operator-lane Jira comment/create/show skill.

Issue #197 (gaps 2 and 3) and #230: the t8 proof ticket SCRUM-5 was created
with an ad-hoc script scp'd to thor, a counted hand-turn. This first-party
skill is the declared custody for that work, and these tests pin what makes
the custody honest:

  * the SKILL.md carries `type: command` (the culture skill loader skips
    files without it, so a missing frontmatter key is a silently absent
    skill);
  * the script parses;
  * the credential pair never appears in the script as a literal assignment
    and is never passed as an argv option — it is read on thor from the
    granted env file, inside the quoted remote heredoc;
  * a malformed issue key is refused BEFORE any ssh happens, with the
    `error:` / `hint:` contract on stderr and nothing on stdout.

`ssh` is stubbed by prepending a temporary directory to PATH; no host is
ever contacted by this suite.
"""

import os
import re
import subprocess  # nosec B404 - runs an in-repo shell script, no external input
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SKILL_DIR = ROOT / ".claude" / "skills" / "jira"
SKILL_MD = SKILL_DIR / "SKILL.md"
SCRIPT = SKILL_DIR / "scripts" / "jira.sh"

LOUD_SSH = """#!/usr/bin/env bash
echo "ssh must not be called: $*" >&2
exit 99
"""


def _frontmatter(text: str) -> dict:
    match = re.match(r"\A---\n(.*?)\n---\n", text, re.S)
    assert match, "SKILL.md has no YAML frontmatter block"
    keys = {}
    for line in match.group(1).splitlines():
        found = re.match(r"^([A-Za-z_]+):\s*(.*)$", line)
        if found:
            keys[found.group(1)] = found.group(2).strip()
    return keys


def _run(args, tmp_path):
    shim = tmp_path / "bin"
    shim.mkdir(exist_ok=True)
    ssh = shim / "ssh"
    ssh.write_text(LOUD_SSH)
    ssh.chmod(0o755)
    env = dict(os.environ, PATH=f"{shim}{os.pathsep}{os.environ['PATH']}")
    return subprocess.run(  # nosec B603 - fixed in-repo script, stubbed ssh
        ["bash", str(SCRIPT), *args], capture_output=True, text=True, env=env
    )


def test_skill_frontmatter_declares_type_command_and_name():
    fm = _frontmatter(SKILL_MD.read_text())
    assert fm.get("type") == "command"
    assert fm.get("name") == "jira"


def test_skill_description_names_the_self_echo_and_the_bridge_lane():
    text = SKILL_MD.read_text()
    assert "jira_comment_is_self_echo" in text
    assert "jira-status" in text
    assert "#197" in text
    assert "#230" in text


def test_script_parses():
    proc = subprocess.run(  # nosec B603 B607 - bash -n on an in-repo file
        ["bash", "-n", str(SCRIPT)], capture_output=True, text=True
    )
    assert proc.returncode == 0, proc.stderr


def test_credential_is_never_a_literal_or_an_argv_option():
    body = SCRIPT.read_text()
    assert "JIRA_API_TOKEN=" not in body
    assert "JIRA_ACCOUNT_EMAIL=" not in body
    assert "--password" not in body
    assert "--token" not in body
    # The one line that builds the remote argv carries only site/verb/user
    # arguments — never anything named like a credential.
    argv_lines = [ln for ln in body.splitlines() if "printf '%q" in ln]
    assert argv_lines, "the remote argv is expected to be built with printf %q"
    for ln in argv_lines:
        assert not re.search(r"(?i)token|password|secret|email", ln), ln


@pytest.mark.parametrize(
    "args",
    [
        ["show", "scrum-5"],
        ["comment", "scrum-5", "hello"],
        ["show", "SCRUM-05"],
        ["show", "SCRUM-5; rm -rf /"],
        ["create", "--project", "scrum", "--summary", "x"],
    ],
)
def test_invalid_keys_are_refused_before_any_ssh(args, tmp_path):
    proc = _run(args, tmp_path)
    assert proc.returncode == 1, (proc.returncode, proc.stderr)
    assert "ssh must not be called" not in proc.stderr
    assert proc.stdout == ""
    assert "error:" in proc.stderr
    assert "hint:" in proc.stderr


def test_unknown_verb_and_missing_args_are_refused_before_any_ssh(tmp_path):
    for args in ([], ["move", "SCRUM-5"], ["comment", "SCRUM-5"], ["create", "--summary", "x"]):
        proc = _run(args, tmp_path)
        assert proc.returncode == 1, args
        assert "ssh must not be called" not in proc.stderr
        assert "hint:" in proc.stderr


def test_a_valid_key_reaches_ssh_without_the_credential(tmp_path):
    proc = _run(["show", "SCRUM-5"], tmp_path)
    assert proc.returncode == 99, proc.stderr
    assert "ssh must not be called: thor python3 - " in proc.stderr
    assert "SCRUM-5" in proc.stderr
    assert not re.search(r"(?i)token|password", proc.stderr)
