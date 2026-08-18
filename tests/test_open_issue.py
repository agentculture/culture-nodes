"""`scripts/open-issue.sh` — a thin, deletable template+type wrapper.

Issue #157, task t6. A one-time backfill of issue types decays the moment the
next issue is opened, and nothing in this repo sets a type at creation: there
are no GitHub issue templates, and `agtag issue post` has no `--type` flag.
So the wrapper exists to close exactly two gaps at the moment of creation — a
rendered body template, and a type — and nothing else.

**Thin is a hard requirement, and these tests are how it stays thin.** An
upstream request (agentculture/agtag#19) asks agtag to absorb this; when it
lands, deleting this wrapper must be a one-file removal. So:

  * posting, signing and auth are DELEGATED to `agtag issue post` — the
    wrapper never calls `gh issue create`, never resolves a nick, never
    touches a token (`test_delegates_posting_to_agtag`);
  * the skills declared vendored by `docs/skill-sources.md` are untouched —
    the wrapper is a first-party script beside them, not an edit to them.
    The check delegates to the same guard CI's lint job runs
    (`scripts/check-vendored-skill-diff.py`), so this test and that guard
    cannot disagree about which skills are vendored; first-party skills
    authored in this repo (nodes-operator, jira-status) may change freely
    (`test_vendored_skill_tree_is_untouched`).

Validation order is the other load-bearing property. GitHub's `type:` search
qualifier FAILS OPEN — a bogus type name returns 0 results rather than an
error — so a name is only ever trustworthy if it was checked against
`organization.issueTypes`. And that check has to happen BEFORE the post, or a
typo leaves a real, untyped issue behind and the operator has to clean up
after a failed command.

Both `gh` and `agtag` are stubbed by prepending a temporary directory to
PATH. No issue is ever created by this suite.
"""

import importlib.util
import json
import os
import subprocess  # nosec B404 - runs an in-repo shell script, no external input
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "open-issue.sh"
TEMPLATE_DIR = ROOT / "docs" / "triage" / "issue-templates"
RECORD_TEMPLATE = TEMPLATE_DIR / "record.md"

# What the fake org menu returns. `Deviation` is present but disabled, so a
# disabled name is distinguishable from an absent one.
ORG_TYPES = [
    {"id": "IT_kwDOEI9FZ84B9t67", "name": "Task", "isEnabled": True},
    {"id": "IT_kwDOEI9FZ84B9t68", "name": "Bug", "isEnabled": True},
    {"id": "IT_kwDOEI9FZ84B9t69", "name": "Feature", "isEnabled": True},
    {"id": "IT_kwDOEI9FZ84B9t70", "name": "Record", "isEnabled": True},
    {"id": "IT_kwDOEI9FZ84B9t71", "name": "Deviation", "isEnabled": False},
]

GH_STUB = '''#!/usr/bin/env python3
"""Fake `gh` that answers the three GraphQL calls open-issue.sh makes."""
import json, os, sys

ORG_TYPES = json.loads("""%ORG_TYPES%""")
log = os.environ["GH_CALL_LOG"]
argv = sys.argv[1:]
query = ""
fields = {}
for i, a in enumerate(argv):
    if a in ("-f", "-F") and i + 1 < len(argv) and "=" in argv[i + 1]:
        k, v = argv[i + 1].split("=", 1)
        if k == "query":
            query = v
        else:
            fields[k] = v

with open(log, "a") as fh:
    fh.write(json.dumps({"argv": argv, "query": query, "fields": fields}) + "\\n")

if "gh issue create" in " ".join(argv) or (argv[:2] == ["issue", "create"]):
    sys.stderr.write("stub gh: the wrapper must not create issues itself\\n")
    sys.exit(9)

if "updateIssue" in query:
    print(json.dumps({"data": {"updateIssue": {"issue": {"number": 4242}}}}))
elif "issue(number" in query:
    print(json.dumps({"data": {"repository": {"issue": {"id": "I_node_4242"}}}}))
elif "issueTypes" in query:
    print(json.dumps({"data": {"organization": {"issueTypes": {"nodes": ORG_TYPES}}}}))
else:
    sys.stderr.write("stub gh: unexpected call: %s\\n" % argv)
    sys.exit(9)
'''

AGTAG_STUB = '''#!/usr/bin/env python3
"""Fake `agtag` that records the post and returns agtag's real JSON shape."""
import json, os, sys

argv = sys.argv[1:]
body = ""
for i, a in enumerate(argv):
    if a == "--body-file" and i + 1 < len(argv):
        body = open(argv[i + 1]).read()

with open(os.environ["AGTAG_CALL_LOG"], "a") as fh:
    fh.write(json.dumps({"argv": argv, "body": body}) + "\\n")

print(json.dumps({
    "url": "https://github.com/agentculture/culture-nodes/issues/4242",
    "number": 4242,
    "signed_as": "culture-nodes",
}))
'''


@pytest.fixture
def stubs(tmp_path):
    """`gh` and `agtag` stubs on PATH, plus their call logs."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    gh_log = tmp_path / "gh-calls.jsonl"
    agtag_log = tmp_path / "agtag-calls.jsonl"

    gh = bin_dir / "gh"
    gh.write_text(GH_STUB.replace("%ORG_TYPES%", json.dumps(ORG_TYPES)))
    gh.chmod(0o755)

    agtag = bin_dir / "agtag"
    agtag.write_text(AGTAG_STUB)
    agtag.chmod(0o755)

    env = dict(
        os.environ,
        PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        GH_CALL_LOG=str(gh_log),
        AGTAG_CALL_LOG=str(agtag_log),
    )
    return env, gh_log, agtag_log


def read_calls(log):
    if not log.exists():
        return []
    return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]


def run_open(env, *args):
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(SCRIPT), *args],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        env=env,
        timeout=60,
    )


# --- AC1: render a template AND set the type, in one command -----------------


def test_renders_template_and_sets_type_in_one_command(stubs, tmp_path):
    env, gh_log, agtag_log = stubs
    template = tmp_path / "t.md"
    template.write_text(
        "## What happened\n\n{{SUMMARY}}\n\n## Committed artifact\n\n`{{ARTIFACT_PATH}}`\n"
    )

    result = run_open(
        env,
        "--type",
        "Record",
        "--title",
        "Operator hand-turn: created the Record type",
        "--template",
        str(template),
        "--set",
        "SUMMARY=the org owner ran createIssueType",
        "--set",
        "ARTIFACT_PATH=docs/decisions/issue-types.md",
    )
    assert result.returncode == 0, result.stderr

    posts = read_calls(agtag_log)
    assert len(posts) == 1, "exactly one agtag post"
    body = posts[0]["body"]
    assert "the org owner ran createIssueType" in body
    assert "docs/decisions/issue-types.md" in body
    assert "{{" not in body, "no placeholder may survive into a posted body"

    mutations = [c for c in read_calls(gh_log) if "updateIssue" in c["query"]]
    assert len(mutations) == 1, "the type is set on the created issue"
    assert mutations[0]["fields"]["typeId"] == "IT_kwDOEI9FZ84B9t70"
    assert mutations[0]["fields"]["id"] == "I_node_4242"
    assert "4242" in result.stdout


def test_template_may_be_named_rather_than_pathed(stubs):
    """`--template record` resolves inside docs/triage/issue-templates/."""
    env, _gh_log, agtag_log = stubs
    result = run_open(
        env,
        "--type",
        "Record",
        "--title",
        "A record",
        "--template",
        "record",
        "--set",
        "SUMMARY=x",
        "--set",
        "ARTIFACT_PATH=docs/adr/0001-example.md",
        "--set",
        "WHY_RECORD=complete when written",
        "--set",
        "CONTEXT=none",
    )
    assert result.returncode == 0, result.stderr
    assert read_calls(agtag_log), "the named template resolved and posted"


# --- AC2: the type is validated BEFORE anything is posted --------------------


def test_unknown_type_fails_before_any_issue_is_created(stubs, tmp_path):
    env, gh_log, agtag_log = stubs
    template = tmp_path / "t.md"
    template.write_text("body\n")

    result = run_open(env, "--type", "NotARealType", "--title", "T", "--template", str(template))
    assert result.returncode != 0
    assert read_calls(agtag_log) == [], "nothing may be posted for an unknown type"
    assert [c for c in read_calls(gh_log) if "issueTypes" in c["query"]], "the org menu was read"
    assert "NotARealType" in result.stderr


def test_disabled_type_is_refused(stubs, tmp_path):
    """Present in the menu but switched off is still not a usable type."""
    env, _gh_log, agtag_log = stubs
    template = tmp_path / "t.md"
    template.write_text("body\n")

    result = run_open(env, "--type", "Deviation", "--title", "T", "--template", str(template))
    assert result.returncode != 0
    assert read_calls(agtag_log) == []


def test_unsubstituted_placeholder_fails_before_posting(stubs, tmp_path):
    env, _gh_log, agtag_log = stubs
    template = tmp_path / "t.md"
    template.write_text("artifact: {{ARTIFACT_PATH}}\n")

    result = run_open(env, "--type", "Record", "--title", "T", "--template", str(template))
    assert result.returncode != 0
    assert read_calls(agtag_log) == []
    assert "ARTIFACT_PATH" in result.stderr


def test_missing_template_fails_before_posting(stubs):
    env, _gh_log, agtag_log = stubs
    result = run_open(env, "--type", "Record", "--title", "T", "--template", "docs/triage/nope.md")
    assert result.returncode != 0
    assert read_calls(agtag_log) == []


# --- AC3: the vendored skill tree is not touched -----------------------------


def _vendored_skill_prefixes():
    """The vendored set, parsed by the SAME code CI's lint guard runs.

    `.claude/skills/` is not all-vendored: first-party skills
    (nodes-operator, jira-status) are authored here and may change freely.
    Which skills are vendored is declared by `docs/skill-sources.md`, and
    `scripts/check-vendored-skill-diff.py` is the parser of record — loading
    it means this test cannot disagree with the lint job about the set.
    """
    guard = ROOT / "scripts" / "check-vendored-skill-diff.py"
    spec = importlib.util.spec_from_file_location("check_vendored_skill_diff", guard)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.vendored_paths()


def test_vendored_skill_tree_is_untouched():
    """The wrapper lives in scripts/, never inside a vendored skill."""
    assert SCRIPT.exists()
    assert not (ROOT / ".claude" / "skills" / "communicate" / "scripts" / "open-issue.sh").exists()

    prefixes = _vendored_skill_prefixes()
    dirty = subprocess.run(  # nosec B603 - fixed argv, no shell
        ["git", "status", "--porcelain", "--", ".claude/skills"],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        timeout=60,
    )
    touched = [
        line for line in dirty.stdout.splitlines() if any(prefix in line for prefix in prefixes)
    ]
    assert touched == [], f"vendored skill files modified: {touched}"

    base = subprocess.run(  # nosec B603 - fixed argv, no shell
        ["git", "merge-base", "HEAD", "origin/main"],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        timeout=60,
    )
    if base.returncode != 0:
        pytest.skip("origin/main not available to diff against")
    guard = subprocess.run(  # nosec B603 - fixed argv, no shell
        [
            sys.executable,
            str(ROOT / "scripts" / "check-vendored-skill-diff.py"),
            base.stdout.strip(),
            "HEAD",
        ],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        timeout=60,
    )
    assert guard.returncode == 0, f"vendored skill files changed on this branch: {guard.stderr}"


# --- AC4: posting, signing and auth are delegated, not reimplemented ---------


def test_delegates_posting_to_agtag(stubs, tmp_path):
    env, _gh_log, agtag_log = stubs
    template = tmp_path / "t.md"
    template.write_text("body\n")
    result = run_open(env, "--type", "Task", "--title", "T", "--template", str(template))
    assert result.returncode == 0, result.stderr

    argv = read_calls(agtag_log)[0]["argv"]
    assert argv[:2] == ["issue", "post"]
    assert "--body-file" in argv


def test_the_wrapper_never_creates_or_signs_an_issue_itself():
    body = SCRIPT.read_text()
    assert "gh issue create" not in body, "creation is agtag's job"
    assert "(Claude)" not in body, "signing is agtag's job — it resolves the nick itself"
    assert "GITHUB_TOKEN" not in body, "auth is agtag's/gh's job"
    assert "GH_TOKEN" not in body, "auth is agtag's/gh's job"

    posts = [
        (n, line)
        for n, line in enumerate(body.splitlines(), 1)
        if "agtag issue post" in line and not line.lstrip().startswith("#")
    ]
    assert len(posts) == 1, "exactly one delegation site, so a reviewer sees it at a glance"
    line = posts[0][1]
    assert "--repo" in line
    assert "--title" in line
    assert "--body-file" in line


def test_delegation_is_visible_in_under_twenty_lines():
    """The post-and-type step is one contiguous, readable block.

    "Thin" is only checkable if the delegation is somewhere a reviewer can
    point at. This pins the span from the `agtag issue post` call to the
    `updateIssue` mutation that types the result — everything this wrapper
    adds over agtag — to under twenty lines, and pins that no *other* GitHub
    write happens outside it.
    """
    lines = SCRIPT.read_text().splitlines()
    code = [
        (n, line)
        for n, line in enumerate(lines)
        if line.strip() and not line.lstrip().startswith("#")
    ]
    start = [n for n, line in code if "agtag issue post" in line]
    end = [n for n, line in code if "updateIssue" in line]
    assert start, "no `agtag issue post` call found"
    assert end, "no `updateIssue` mutation found"
    assert end[-1] >= start[0]
    span = end[-1] - start[0] + 1
    assert span < 20, f"delegation spans {span} lines"

    assert len(end) == 1, "exactly one GitHub write, and it is the type mutation"


# --- AC5: a Record template exists and names the committed artifact ----------


def test_record_template_exists_and_names_the_committed_artifact():
    assert RECORD_TEMPLATE.exists(), "the Record type needs a body template"
    text = RECORD_TEMPLATE.read_text()
    assert "{{ARTIFACT_PATH}}" in text, "the record must point at a committed path"
    assert "artifact" in text.lower()
    # The record's home is the tree; the issue is only the pointer.
    assert "docs/" in text


def test_record_template_uses_the_vendored_placeholder_syntax():
    """Same `{{NAME}}` convention as .claude/skills/communicate templates."""
    import re

    text = RECORD_TEMPLATE.read_text()
    found = re.findall(r"\{\{([A-Z0-9_]+)\}\}", text)
    assert found, "no placeholders found"
    assert "ARTIFACT_PATH" in found


# --- the window between creating and typing ---------------------------------


def test_a_failure_after_the_post_names_the_issue_and_the_repair(stubs, tmp_path):
    """Creating and typing are two calls, so the issue can exist untyped.

    Validating the type up front removes the *likely* cause of a failure here
    but not the window itself: `agtag issue post` creates, this wrapper types,
    and nothing makes the pair atomic — which is exactly what agentculture/agtag#19
    asks upstream to close.

    An untyped issue nobody knows is untyped is the decay this wrapper exists to
    prevent, arriving through a different door. So a failure past the post must
    be loud, must name the issue it left behind, and must print the command that
    finishes the job. Found by an independent review of this branch; this test is
    why the fix cannot silently regress.
    """
    env, _, _ = stubs
    # Break only the node-id lookup — the post has already succeeded by then.
    gh = Path(env["PATH"].split(os.pathsep)[0]) / "gh"
    gh.write_text(
        gh.read_text().replace(
            'elif "issue(number" in query:',
            'elif "issue(number" in query:\n'
            '    sys.stderr.write("stub gh: node lookup exploded\\n"); sys.exit(7)\n'
            "elif False:",
        )
    )

    result = run_open(
        env,
        "--type",
        "Record",
        "--title",
        "half-done",
        "--template",
        str(RECORD_TEMPLATE),
        "--set",
        "SUMMARY=s",
        "--set",
        "ARTIFACT_PATH=docs/triage/issue-types.md",
        "--set",
        "WHY_RECORD=w",
        "--set",
        "CONTEXT=c",
    )

    assert result.returncode != 0, "a half-done creation must not look like success"
    assert "4242" in result.stderr, "the abandoned issue is named"
    assert "NOT typed" in result.stderr
    # The repair is printed, not left as an exercise.
    assert "updateIssue" in result.stderr
    assert "IT_kwDOEI9FZ84B9t70" in result.stderr, "the resolved type id is carried"
