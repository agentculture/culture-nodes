"""`scripts/open-issue.sh --disposition` and its helper `scripts/triage-rows.py`.

Plan loop-closure-claude-codex, task t15. Every issue the loop opens turns the
next PR's `triage` lint step red until someone hand-writes two CSV rows and
regenerates docs/triage/open-issues.md (memory: "lint triage step needs every
open issue dispositioned"). That is a hand-turn per issue, and it is the kind
that gets forgotten. So opening an issue and dispositioning it become ONE
command: `open-issue.sh --disposition "bucket|text|evidence"`.

The split of responsibilities is the load-bearing property:

  * `scripts/triage-rows.py` owns ALL CSV and report logic -- it appends the
    dispositions.csv and issue-types.csv rows, regenerates the report, and
    refuses malformed input by name with exit 2 having written nothing;
  * `scripts/open-issue.sh` only CALLS it. The wrapper is scheduled for
    deletion when agentculture/agtag#19 lands, so nothing may accrete there;
    the helper survives that deletion unchanged.

Everything here runs against temporary copies of docs/triage. `gh` and `agtag`
are stubbed on PATH for the wrapper tests. No issue is ever created and the
checked-in CSVs are never modified.
"""

from __future__ import annotations

import csv
import importlib.util
import json
import os
import shutil
import subprocess  # nosec B404 - runs in-repo scripts, no external input
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "scripts" / "triage-rows.py"
WRAPPER = ROOT / "scripts" / "open-issue.sh"
REPORT = ROOT / "scripts" / "triage-report.py"
TRIAGE = ROOT / "docs" / "triage"

DISPOSITION_HEADER = "issue,bucket,disposition,evidence_pointer"
TYPE_HEADER = "issue,type,evidence_pointer"

NEW = 9999
GOOD = "bug tail|fix the thing that broke|scripts/triage-rows.py"


def _load(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader
    spec.loader.exec_module(module)
    return module


REPORT_MODULE = _load(REPORT, "triage_report_for_rows")


# --- fixtures ---------------------------------------------------------------


def synthetic_triage(tmp_path: Path) -> tuple[Path, Path]:
    """A small triage dir with several buckets, plus a matching open-issue list."""
    triage = tmp_path / "triage"
    triage.mkdir()
    (triage / "dispositions.csv").write_text(
        DISPOSITION_HEADER + "\n"
        "5,finish work,deploy the collector,plan t13\n"
        "6,owner decisions,decide the OIDC lane,plan t27\n"
        "8,verify-then-close,verify registration tooling,plan t5\n"
        '9,bug tail,"fix the sweep, then the gate",issue #9\n'
        "10,operator-lane enablers,a pause verb,schedules.go\n"
        "11,large bets,per-revision measurement,#306\n",
        encoding="utf-8",
    )
    (triage / "issue-types.csv").write_text(
        TYPE_HEADER + "\n"
        "5,Task,plan t13\n"
        "6,Task,owner decision pending\n"
        '8,Feature,"exists, needs verification"\n'
        "9,Bug,reproduces on main\n"
        "10,Feature,wanted and does not exist\n"
        "11,Record,docs/decisions/x.md\n",
        encoding="utf-8",
    )
    table = REPORT_MODULE.render(
        [5, 6, 8, 9, 10, 11], REPORT_MODULE.dispositions(triage / "dispositions.csv")
    )
    (triage / "open-issues.md").write_text(
        table
        + "\n"
        + REPORT_MODULE.TYPES_HEADING
        + "\n\n| Type | Issues |\n|---|---:|\n| Bug | 1 |\n",
        encoding="utf-8",
    )
    issues = tmp_path / "issues.json"
    issues.write_text(json.dumps([{"number": n} for n in (5, 6, 8, 9, 10, 11)]))
    return triage, issues


def real_triage_copy(tmp_path: Path) -> tuple[Path, Path]:
    """A copy of the checked-in docs/triage, with its own issues as the open set."""
    triage = tmp_path / "triage"
    triage.mkdir()
    for name in ("dispositions.csv", "issue-types.csv", "open-issues.md"):
        shutil.copy(TRIAGE / name, triage / name)
    numbers = sorted(REPORT_MODULE.dispositions(triage / "dispositions.csv"))
    issues = tmp_path / "issues.json"
    issues.write_text(json.dumps([{"number": n} for n in numbers]))
    return triage, issues


def snapshot(triage: Path) -> dict[str, bytes]:
    return {p.name: p.read_bytes() for p in sorted(triage.iterdir()) if p.is_file()}


def run_helper(triage: Path, issues: Path, *args: str, number: int | None = NEW):
    argv = [sys.executable, str(HELPER)]
    if number is not None:
        argv.append(str(number))
    argv += ["--triage-dir", str(triage), "--issues-json", str(issues), *args]
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        argv, cwd=str(ROOT), text=True, capture_output=True, timeout=60
    )


def run_check(triage: Path, issues: Path):
    """`triage-report.py --check` exactly as lint-all.sh runs it, on the temp copy."""
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [
            sys.executable,
            str(REPORT),
            "--check",
            "--issues-json",
            str(issues),
            "--table",
            str(triage / "dispositions.csv"),
            "--output",
            str(triage / "open-issues.md"),
        ],
        cwd=str(ROOT),
        text=True,
        capture_output=True,
        timeout=60,
    )


def rows(path: Path) -> list[dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        return list(csv.DictReader(handle))


def add_open_issue(issues: Path, number: int) -> None:
    data = json.loads(issues.read_text())
    data.append({"number": number})
    issues.write_text(json.dumps(data))


# --- AC1: one invocation appends both rows and leaves --check green ----------


def test_helper_appends_both_rows_and_check_passes_with_nothing_left_to_do(tmp_path):
    triage, issues = synthetic_triage(tmp_path)

    result = run_helper(triage, issues, "--type", "Bug", "--disposition", GOOD)
    assert result.returncode == 0, result.stderr

    dispositions = rows(triage / "dispositions.csv")
    assert list(dispositions[-1]) == DISPOSITION_HEADER.split(",")
    assert dispositions[-1] == {
        "issue": str(NEW),
        "bucket": "bug tail",
        "disposition": "fix the thing that broke",
        "evidence_pointer": "scripts/triage-rows.py",
    }
    types = rows(triage / "issue-types.csv")
    assert list(types[-1]) == TYPE_HEADER.split(",")
    assert types[-1]["issue"] == str(NEW)
    assert types[-1]["type"] == "Bug"
    assert types[-1]["evidence_pointer"].strip()

    report = (triage / "open-issues.md").read_text(encoding="utf-8")
    assert f"| #{NEW} | bug tail | fix the thing that broke | scripts/triage-rows.py |" in report
    assert "Open issues with dispositions: 7" in report

    # The issue is open now (it was just created), so --check sees it in the
    # open set -- and the regenerated report already carries it.
    add_open_issue(issues, NEW)
    check = run_check(triage, issues)
    assert check.returncode == 0, check.stderr + check.stdout


def test_the_open_set_is_unioned_with_the_new_number(tmp_path):
    """`gh issue list` can lag a creation; the issue is open by construction."""
    triage, issues = synthetic_triage(tmp_path)
    # issues.json deliberately does NOT list NEW.
    result = run_helper(triage, issues, "--type", "Bug", "--disposition", GOOD)
    assert result.returncode == 0, result.stderr
    assert f"| #{NEW} |" in (triage / "open-issues.md").read_text(encoding="utf-8")


def test_the_type_block_is_preserved_not_refreshed(tmp_path):
    """The report's type section needs an org read; the helper leaves it alone.

    --check ignores that section (deviation d1), so preserving it verbatim is
    what keeps a helper run from needing GitHub org access -- the same reason
    --check itself reads no types.
    """
    triage, issues = synthetic_triage(tmp_path)
    before = (triage / "open-issues.md").read_text(encoding="utf-8")
    result = run_helper(triage, issues, "--type", "Bug", "--disposition", GOOD)
    assert result.returncode == 0, result.stderr
    after = (triage / "open-issues.md").read_text(encoding="utf-8")
    heading = REPORT_MODULE.TYPES_HEADING
    assert after.split(heading, 1)[1] == before.split(heading, 1)[1]


def test_the_real_triage_tables_round_trip(tmp_path):
    """Against a copy of the checked-in CSVs, not a synthetic shape."""
    triage, issues = real_triage_copy(tmp_path)
    result = run_helper(
        triage,
        issues,
        "--type",
        "Record",
        "--disposition",
        "verify-then-close|a counted hand-turn, closes on read|docs/deliveries/x.md",
    )
    assert result.returncode == 0, result.stderr
    add_open_issue(issues, NEW)
    check = run_check(triage, issues)
    assert check.returncode == 0, check.stderr + check.stdout
    assert rows(triage / "dispositions.csv")[-1]["issue"] == str(NEW)
    assert rows(triage / "issue-types.csv")[-1]["type"] == "Record"


def test_a_comma_in_a_field_is_quoted_so_the_row_survives_the_reader(tmp_path):
    """#215: an unquoted comma shifts every later field; the writer must quote."""
    triage, issues = synthetic_triage(tmp_path)
    result = run_helper(
        triage,
        issues,
        "--type",
        "Task",
        "--disposition",
        "finish work|land it, then verify it|plan t15, wave 0",
    )
    assert result.returncode == 0, result.stderr
    last = rows(triage / "dispositions.csv")[-1]
    assert last["disposition"] == "land it, then verify it"
    assert last["evidence_pointer"] == "plan t15, wave 0"
    # And the strict reader triage-report.py uses accepts the file.
    REPORT_MODULE.dispositions(triage / "dispositions.csv")


def test_type_evidence_may_be_given_separately(tmp_path):
    triage, issues = synthetic_triage(tmp_path)
    result = run_helper(
        triage,
        issues,
        "--type",
        "Bug",
        "--disposition",
        GOOD,
        "--type-evidence",
        "reproduces on main at abc123",
    )
    assert result.returncode == 0, result.stderr
    assert (
        rows(triage / "issue-types.csv")[-1]["evidence_pointer"] == "reproduces on main at abc123"
    )


# --- AC2: malformed input is refused by name, exit 2, nothing written --------


@pytest.mark.parametrize(
    "disposition, named",
    [
        ("bug tail|fix it", "evidence"),
        ("bug tail||scripts/x.py", "disposition"),
        ("|fix it|scripts/x.py", "bucket"),
        ("fix it", "bucket|text|evidence"),
        ("bug tail|fix|it|scripts/x.py", "bucket|text|evidence"),
    ],
)
def test_a_missing_or_extra_field_is_refused_by_name(tmp_path, disposition, named):
    triage, issues = synthetic_triage(tmp_path)
    before = snapshot(triage)
    result = run_helper(triage, issues, "--type", "Bug", "--disposition", disposition)
    assert result.returncode == 2, (result.returncode, result.stderr)
    assert named in result.stderr
    assert snapshot(triage) == before, "a refusal writes nothing"


def test_an_unknown_bucket_is_refused_against_the_buckets_already_in_the_table(tmp_path):
    triage, issues = synthetic_triage(tmp_path)
    before = snapshot(triage)
    result = run_helper(
        triage, issues, "--type", "Bug", "--disposition", "misc|fix it|scripts/x.py"
    )
    assert result.returncode == 2
    assert "misc" in result.stderr, "the refused bucket is named"
    # The valid set is the one present in dispositions.csv, and it is offered.
    for bucket in ("bug tail", "finish work", "large bets"):
        assert bucket in result.stderr
    assert snapshot(triage) == before


def test_an_unknown_type_is_refused(tmp_path):
    triage, issues = synthetic_triage(tmp_path)
    before = snapshot(triage)
    result = run_helper(triage, issues, "--type", "Epic", "--disposition", GOOD)
    assert result.returncode == 2
    assert "Epic" in result.stderr
    assert snapshot(triage) == before


def test_an_issue_already_dispositioned_is_refused(tmp_path):
    triage, issues = synthetic_triage(tmp_path)
    before = snapshot(triage)
    result = run_helper(triage, issues, "--type", "Bug", "--disposition", GOOD, number=9)
    assert result.returncode == 2
    assert "#9" in result.stderr
    assert snapshot(triage) == before


def test_check_only_validates_without_a_number_and_writes_nothing(tmp_path):
    """The wrapper validates BEFORE it posts, the same way it validates the type."""
    triage, issues = synthetic_triage(tmp_path)
    before = snapshot(triage)
    ok = run_helper(
        triage, issues, "--check-only", "--type", "Bug", "--disposition", GOOD, number=None
    )
    assert ok.returncode == 0, ok.stderr
    bad = run_helper(
        triage,
        issues,
        "--check-only",
        "--type",
        "Bug",
        "--disposition",
        "misc|fix it|x",
        number=None,
    )
    assert bad.returncode == 2
    assert "misc" in bad.stderr
    assert snapshot(triage) == before


# --- AC3: the wrapper gains --disposition and stays thin ---------------------


ORG_TYPES = [
    {"id": "IT_task", "name": "Task", "isEnabled": True},
    {"id": "IT_bug", "name": "Bug", "isEnabled": True},
    {"id": "IT_record", "name": "Record", "isEnabled": True},
]

GH_STUB = '''#!/usr/bin/env python3
import json, os, sys
ORG_TYPES = json.loads("""%ORG_TYPES%""")
argv = sys.argv[1:]
query = ""
for i, a in enumerate(argv):
    if a in ("-f", "-F") and i + 1 < len(argv) and argv[i + 1].startswith("query="):
        query = argv[i + 1]
with open(os.environ["GH_CALL_LOG"], "a") as fh:
    fh.write(json.dumps({"argv": argv, "query": query}) + "\\n")
if argv[:2] == ["issue", "list"]:
    print(json.dumps([{"number": n} for n in json.loads("""%OPEN%""")]))
elif "updateIssue" in query:
    print(json.dumps({"data": {"updateIssue": {"issue": {"number": 4242}}}}))
elif "issue(number" in query:
    print(json.dumps({"data": {"repository": {"issue": {"id": "I_node_4242"}}}}))
elif "issueTypes" in query:
    print(json.dumps({"data": {"organization": {"issueTypes": {"nodes": ORG_TYPES}}}}))
else:
    sys.stderr.write("stub gh: unexpected call: %s\\n" % argv)
    sys.exit(9)
'''

AGTAG_STUB = """#!/usr/bin/env python3
import json, os, sys
with open(os.environ["AGTAG_CALL_LOG"], "a") as fh:
    fh.write(json.dumps({"argv": sys.argv[1:]}) + "\\n")
print(json.dumps({"url": "https://github.com/agentculture/culture-nodes/issues/4242",
                  "number": 4242, "signed_as": "culture-nodes"}))
"""


@pytest.fixture
def wrapper_repo(tmp_path):
    """A throwaway git repo holding copies of scripts/ and docs/triage/.

    The wrapper resolves `$root` with `git rev-parse` and calls
    `$root/scripts/triage-rows.py`, whose ROOT is its own parent -- so running
    the wrapper from a copied tree exercises the real call path against
    throwaway tables, with no test-only flag on the wrapper.
    """
    repo = tmp_path / "repo"
    shutil.copytree(ROOT / "scripts", repo / "scripts")
    shutil.copytree(TRIAGE, repo / "docs" / "triage")
    subprocess.run(["git", "init", "-q", str(repo)], check=True, timeout=60)  # nosec B603 B607

    # The stub's open set is the copied table's own issues, so the tree starts
    # in the state --check accepts.
    triage = repo / "docs" / "triage"
    dispositions = REPORT_MODULE.dispositions(triage / "dispositions.csv")
    numbers = sorted(dispositions)
    (triage / "open-issues.md").write_text(
        REPORT_MODULE.render(numbers, dispositions) + "\n", encoding="utf-8"
    )

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    gh_log = tmp_path / "gh.jsonl"
    agtag_log = tmp_path / "agtag.jsonl"
    gh_stub = GH_STUB.replace("%ORG_TYPES%", json.dumps(ORG_TYPES))
    (bin_dir / "gh").write_text(gh_stub.replace("%OPEN%", json.dumps(numbers)))
    (bin_dir / "gh").chmod(0o755)
    (bin_dir / "agtag").write_text(AGTAG_STUB)
    (bin_dir / "agtag").chmod(0o755)
    env = dict(
        os.environ,
        PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        GH_CALL_LOG=str(gh_log),
        AGTAG_CALL_LOG=str(agtag_log),
    )
    return repo, env, agtag_log


def run_wrapper(repo: Path, env: dict, *args: str):
    template = repo / "t.md"
    template.write_text("body\n")
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(repo / "scripts" / "open-issue.sh"), "--template", str(template), *args],
        cwd=str(repo),
        text=True,
        capture_output=True,
        env=env,
        timeout=120,
    )


def test_wrapper_with_disposition_writes_the_rows_for_the_created_issue(wrapper_repo):
    repo, env, agtag_log = wrapper_repo
    result = run_wrapper(repo, env, "--type", "Record", "--title", "T", "--disposition", GOOD)
    assert result.returncode == 0, result.stderr
    assert agtag_log.exists(), "the issue was posted"
    triage = repo / "docs" / "triage"
    assert rows(triage / "dispositions.csv")[-1]["issue"] == "4242"
    assert rows(triage / "dispositions.csv")[-1]["bucket"] == "bug tail"
    assert rows(triage / "issue-types.csv")[-1] == {
        "issue": "4242",
        "type": "Record",
        "evidence_pointer": rows(triage / "issue-types.csv")[-1]["evidence_pointer"],
    }
    assert "| #4242 |" in (triage / "open-issues.md").read_text(encoding="utf-8")
    assert "4242" in result.stdout


def test_wrapper_without_disposition_behaves_exactly_as_before(wrapper_repo):
    repo, env, agtag_log = wrapper_repo
    triage = repo / "docs" / "triage"
    before = snapshot(triage)
    result = run_wrapper(repo, env, "--type", "Task", "--title", "T")
    assert result.returncode == 0, result.stderr
    assert agtag_log.exists()
    assert snapshot(triage) == before, "no --disposition, no triage write"


def test_wrapper_refuses_a_malformed_disposition_before_posting(wrapper_repo):
    repo, env, agtag_log = wrapper_repo
    triage = repo / "docs" / "triage"
    before = snapshot(triage)
    result = run_wrapper(
        repo, env, "--type", "Task", "--title", "T", "--disposition", "misc|fix it|x"
    )
    assert result.returncode == 2, (result.returncode, result.stderr)
    assert "misc" in result.stderr
    assert not agtag_log.exists(), "a bad disposition must not leave an issue behind"
    assert snapshot(triage) == before


def test_wrapper_names_the_repair_when_the_rows_fail_after_the_post(wrapper_repo):
    """Past the post the issue exists; a helper failure must be loud and repairable."""
    repo, env, agtag_log = wrapper_repo
    # Make the helper's write path fail after validation passed: a read-only table.
    triage = repo / "docs" / "triage"
    (triage / "dispositions.csv").chmod(0o444)
    if os.access(triage / "dispositions.csv", os.W_OK):
        pytest.skip("running as a user that ignores file modes")
    result = run_wrapper(repo, env, "--type", "Task", "--title", "T", "--disposition", GOOD)
    assert result.returncode != 0
    assert agtag_log.exists(), "the post had already happened"
    assert "4242" in result.stderr, "the issue left without rows is named"
    assert "triage-rows.py" in result.stderr and "4242" in result.stderr, "the repair is printed"


def test_wrapper_delegates_all_csv_logic_to_the_helper():
    """The wrapper stays deletable: it calls the helper and nothing else grows."""
    body = WRAPPER.read_text()
    code = [
        line for line in body.splitlines() if line.strip() and not line.lstrip().startswith("#")
    ]
    calls = [line for line in code if "triage-rows.py" in line]
    assert calls, "the wrapper calls scripts/triage-rows.py"
    assert any("--type" in line and "--disposition" in line for line in calls)
    assert "dispositions.csv" not in "\n".join(code), "CSV knowledge lives in the helper"
    assert "issue-types.csv" not in "\n".join(code)
    assert "csv" not in "\n".join(line for line in code if "python3 -" in line)
    assert "agtag#19" in body, "the deletion plan is stated beside the flag"


def test_helper_is_stdlib_only_and_under_the_file_length_guard():
    text = HELPER.read_text()
    assert len(text.splitlines()) < 1000
    forbidden = ("import requests", "import yaml", "import click", "import typer")
    assert not any(token in text for token in forbidden)


# --- AC4: the tables describe ONE repository ---------------------------------
#
# `--repo` and `--disposition` interact. docs/triage/open-issues.md is
# regenerated from `gh issue list --repo <repo>` and re-read by
# `triage-report.py --check` against agentculture/culture-nodes, so an issue
# opened elsewhere is wrong in these tables twice over: the appended row names
# a number this repo will never see open, and the report around it would be
# rebuilt from a foreign open set -- turning the `triage` lint step red on a
# tree nobody touched by hand. The pair is refused where every other malformed
# input is: at --check-only time, BEFORE the issue is posted.

OTHER_REPO = "agentculture/some-other-repo"


def run_helper_in(repo: Path, *args: str):
    """The copied tree's helper on ITS OWN default triage dir (no --triage-dir)."""
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [sys.executable, str(repo / "scripts" / "triage-rows.py"), *args],
        cwd=str(repo),
        text=True,
        capture_output=True,
        timeout=60,
    )


def test_helper_refuses_a_foreign_repo_against_this_checkouts_tables(wrapper_repo):
    repo, _env, _agtag_log = wrapper_repo
    triage = repo / "docs" / "triage"
    before = snapshot(triage)
    result = run_helper_in(
        repo, "--check-only", "--repo", OTHER_REPO, "--type", "Task", "--disposition", GOOD
    )
    assert result.returncode == 2, (result.returncode, result.stderr)
    assert OTHER_REPO in result.stderr, "the repository that does not own the tables is named"
    assert "agentculture/culture-nodes" in result.stderr, "the one that does is named too"
    assert snapshot(triage) == before, "a refusal writes nothing"


def test_helper_allows_a_foreign_repo_that_brings_its_own_tables(tmp_path):
    """Another tree's tables are that tree's business; only the default pair is refused."""
    triage, issues = synthetic_triage(tmp_path)
    result = run_helper(
        triage, issues, "--repo", OTHER_REPO, "--type", "Bug", "--disposition", GOOD
    )
    assert result.returncode == 0, result.stderr
    assert rows(triage / "dispositions.csv")[-1]["issue"] == str(NEW)


def test_wrapper_refuses_to_disposition_an_issue_opened_in_another_repo(wrapper_repo):
    repo, env, agtag_log = wrapper_repo
    triage = repo / "docs" / "triage"
    before = snapshot(triage)
    result = run_wrapper(
        repo, env, "--repo", OTHER_REPO, "--type", "Task", "--title", "T", "--disposition", GOOD
    )
    assert result.returncode == 2, (result.returncode, result.stdout, result.stderr)
    assert OTHER_REPO in result.stderr
    assert not agtag_log.exists(), "the refusal lands before the post, so no issue is left behind"
    assert snapshot(triage) == before


def test_wrapper_still_opens_in_another_repo_without_a_disposition(wrapper_repo):
    """The guard is about the triage rows only; --repo on its own is untouched."""
    repo, env, agtag_log = wrapper_repo
    triage = repo / "docs" / "triage"
    before = snapshot(triage)
    result = run_wrapper(repo, env, "--repo", OTHER_REPO, "--type", "Task", "--title", "T")
    assert result.returncode == 0, result.stderr
    posted = json.loads(agtag_log.read_text().splitlines()[0])["argv"]
    assert OTHER_REPO in posted, posted
    assert snapshot(triage) == before
