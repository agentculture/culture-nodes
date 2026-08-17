"""Tests for scripts/backfill-issue-types.py.

Every `gh` invocation in the script goes through one seam -- a callable with the
signature `(command) -> (returncode, stdout, stderr)`. These tests substitute a
fake for it, so "performs zero mutations" is an assertion about the process
boundary rather than about what the script says it did.
"""

from __future__ import annotations

import csv
import importlib.util
import json
from pathlib import Path

import pytest

SCRIPT = Path(__file__).parents[1] / "scripts" / "backfill-issue-types.py"
SPEC = importlib.util.spec_from_file_location("backfill_issue_types", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(MODULE)

ORG_TYPES = [
    {"id": "IT_task", "name": "Task", "isEnabled": True},
    {"id": "IT_bug", "name": "Bug", "isEnabled": True},
    {"id": "IT_feature", "name": "Feature", "isEnabled": True},
    {"id": "IT_record", "name": "Record", "isEnabled": True},
]


def node_id(number: int) -> str:
    return f"I_kwDO{number:08d}"


class FakeGh:
    """The `gh` seam. Answers reads from an in-memory repo, records writes."""

    def __init__(self, issues, org_types=None, fail_numbers=(), fail_reads=False):
        self.issues = dict(issues)
        self.org_types = ORG_TYPES if org_types is None else org_types
        self.fail_numbers = set(fail_numbers)
        self.fail_reads = fail_reads
        self.commands: list[list[str]] = []
        self.writes: list[tuple[int, str]] = []

    def __call__(self, command):
        self.commands.append(list(command))
        fields = {}
        for index, token in enumerate(command):
            if token == "-f" and index + 1 < len(command):
                key, _, value = command[index + 1].partition("=")
                fields[key] = value
        query = fields.get("query", "")
        if "updateIssue" in query:
            return self._write(fields)
        if self.fail_reads:
            return 1, "", "HTTP 503: no server is currently available"
        if "issueTypes" in query:
            payload = {"data": {"organization": {"issueTypes": {"nodes": self.org_types}}}}
            return 0, json.dumps(payload), ""
        nodes = [
            {
                "number": number,
                "id": node_id(number),
                "issueType": None if state is None else {"name": state},
            }
            for number, state in sorted(self.issues.items())
        ]
        payload = {
            "data": {
                "repository": {
                    "issues": {
                        "pageInfo": {"hasNextPage": False, "endCursor": None},
                        "nodes": nodes,
                    }
                }
            }
        }
        return 0, json.dumps(payload), ""

    def _write(self, fields):
        numbers = {node_id(number): number for number in self.issues}
        number = numbers[fields["id"]]
        names = {entry["id"]: entry["name"] for entry in self.org_types}
        name = names[fields["typeId"]]
        if number in self.fail_numbers:
            return 1, "", "HTTP 503: no server is currently available"
        self.writes.append((number, name))
        self.issues[number] = name
        return 0, json.dumps({"data": {"updateIssue": {"issue": {"number": number}}}}), ""


class NoGh:
    """A seam that fails the test if the script talks to GitHub at all."""

    def __call__(self, command):
        raise AssertionError(f"unexpected gh invocation: {command}")


def write_table(tmp_path, rows):
    path = tmp_path / "issue-types.csv"
    with path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["issue", "type", "evidence_pointer"])
        for row in rows:
            writer.writerow(row)
    return path


def issues_fixture(tmp_path, issues):
    path = tmp_path / "issues.json"
    nodes = [
        {
            "number": number,
            "id": node_id(number),
            "issueType": None if state is None else {"name": state},
        }
        for number, state in sorted(issues.items())
    ]
    path.write_text(json.dumps(nodes), encoding="utf-8")
    return path


def types_fixture(tmp_path, org_types=None):
    path = tmp_path / "org-types.json"
    path.write_text(json.dumps(ORG_TYPES if org_types is None else org_types), encoding="utf-8")
    return path


def base_argv(tmp_path, table, snapshot=None):
    return [
        "--table",
        str(table),
        "--snapshot",
        str(snapshot or tmp_path / "prestate.json"),
        "--backoff-seconds",
        "0",
    ]


def test_dry_run_prints_the_mapping_and_performs_zero_mutations(tmp_path, capsys):
    table = write_table(
        tmp_path,
        [(5, "Bug", "cli.py:20 still raises"), (6, "Record", "delivery doc")],
    )
    argv = base_argv(tmp_path, table) + [
        "--dry-run",
        "--issues-json",
        str(issues_fixture(tmp_path, {5: None, 6: None})),
        "--org-types-json",
        str(types_fixture(tmp_path)),
    ]

    # NoGh raises on any invocation, so a single write attempt fails this test.
    code = MODULE.main(argv, invoke=NoGh())

    out = capsys.readouterr().out
    assert code == 0
    assert "#5" in out
    assert "Bug" in out
    assert "#6" in out
    assert "Record" in out
    assert "dry run" in out.lower()


def test_dry_run_writes_nothing_to_github_even_with_a_live_seam(tmp_path):
    table = write_table(tmp_path, [(5, "Bug", "evidence")])
    gh = FakeGh({5: None})

    code = MODULE.main(base_argv(tmp_path, table) + ["--dry-run"], invoke=gh)

    assert code == 0
    assert gh.writes == []
    assert not any("updateIssue" in " ".join(command) for command in gh.commands)


def test_snapshot_is_written_before_the_first_mutation_and_its_path_printed(tmp_path, capsys):
    table = write_table(tmp_path, [(5, "Bug", "evidence"), (6, "Task", "evidence")])
    snapshot = tmp_path / "prestate.json"
    # Every mutation fails, so anything on disk was written before the first one.
    gh = FakeGh({5: "Task", 6: None}, fail_numbers={5, 6})

    code = MODULE.main(base_argv(tmp_path, table, snapshot), invoke=gh)

    out = capsys.readouterr().out
    assert code == 2
    assert gh.writes == []
    assert snapshot.exists()
    recorded = json.loads(snapshot.read_text(encoding="utf-8"))["issues"]
    assert recorded["5"]["type"] == "Task"
    assert recorded["6"]["type"] is None
    assert str(snapshot) in out


def test_unknown_type_name_fails_preflight_without_touching_any_issue(tmp_path, capsys):
    table = write_table(tmp_path, [(5, "Bogus", "evidence"), (6, "Task", "evidence")])
    gh = FakeGh({5: None, 6: None})

    code = MODULE.main(base_argv(tmp_path, table), invoke=gh)

    err = capsys.readouterr().err
    assert code == 1
    assert "Bogus" in err
    assert gh.writes == []


def test_missing_record_type_is_one_error_not_one_per_issue(tmp_path, capsys):
    without_record = [entry for entry in ORG_TYPES if entry["name"] != "Record"]
    table = write_table(
        tmp_path,
        [(number, "Record", "evidence") for number in (5, 6, 7, 8)],
    )
    gh = FakeGh({5: None, 6: None, 7: None, 8: None}, org_types=without_record)

    code = MODULE.main(base_argv(tmp_path, table), invoke=gh)

    err = capsys.readouterr().err.strip().splitlines()
    assert code == 1
    complaints = [line for line in err if "Record" in line]
    assert len(complaints) == 1, err
    assert gh.writes == []


def test_missing_record_type_stops_a_run_that_assigns_no_record_at_all(tmp_path, capsys):
    """The refusal is about the org's vocabulary, not about this CSV's rows.

    Without this case the check is indistinguishable from the unknown-name
    preflight next door, which would catch a CSV that happens to name Record.
    """
    without_record = [entry for entry in ORG_TYPES if entry["name"] != "Record"]
    table = write_table(tmp_path, [(5, "Task", "evidence"), (6, "Bug", "evidence")])
    gh = FakeGh({5: None, 6: None}, org_types=without_record)

    code = MODULE.main(base_argv(tmp_path, table), invoke=gh)

    err = capsys.readouterr().err
    assert code == 1
    assert "Record" in err
    assert gh.writes == []


def test_interrupted_run_resumes_without_rewriting_correct_issues(tmp_path):
    table = write_table(tmp_path, [(5, "Bug", "evidence"), (6, "Task", "evidence")])
    snapshot = tmp_path / "prestate.json"
    argv = base_argv(tmp_path, table, snapshot)

    first = FakeGh({5: None, 6: None}, fail_numbers={6})
    assert MODULE.main(argv, invoke=first) == 2
    assert first.writes == [(5, "Bug")]
    original = snapshot.read_text(encoding="utf-8")

    second = FakeGh(first.issues)
    assert MODULE.main(argv, invoke=second) == 0

    assert second.writes == [(6, "Task")]
    # The pre-state survives the resume: it is the only way back.
    assert snapshot.read_text(encoding="utf-8") == original
    assert json.loads(original)["issues"]["5"]["type"] is None


def test_already_correct_issues_are_never_rewritten(tmp_path):
    table = write_table(tmp_path, [(5, "Bug", "evidence")])
    gh = FakeGh({5: "Bug"})

    assert MODULE.main(base_argv(tmp_path, table), invoke=gh) == 0
    assert gh.writes == []


def test_undetermined_rows_are_left_untyped_and_counted(tmp_path, capsys):
    table = write_table(
        tmp_path,
        [(5, "UNDETERMINED", "cannot tell if it reproduces"), (6, "Task", "evidence")],
    )
    gh = FakeGh({5: None, 6: None})

    code = MODULE.main(base_argv(tmp_path, table), invoke=gh)

    out = capsys.readouterr().out
    assert code == 0
    assert gh.writes == [(6, "Task")]
    assert "1 left untyped" in out


def test_unreadable_github_exits_2_not_1(tmp_path, capsys):
    table = write_table(tmp_path, [(5, "Bug", "evidence")])
    gh = FakeGh({5: None}, fail_reads=True)

    code = MODULE.main(base_argv(tmp_path, table) + ["--backoff-seconds", "0"], invoke=gh)

    assert code == 2
    assert "could not" in capsys.readouterr().err.lower()
    assert gh.writes == []


def test_read_failure_is_retried_gh_attempts_times(tmp_path):
    table = write_table(tmp_path, [(5, "Bug", "evidence")])
    gh = FakeGh({5: None}, fail_reads=True)

    MODULE.main(base_argv(tmp_path, table) + ["--backoff-seconds", "0"], invoke=gh)

    assert len(gh.commands) == MODULE.GH_ATTEMPTS


def test_a_row_whose_issue_is_not_open_is_reported_not_written(tmp_path, capsys):
    table = write_table(tmp_path, [(5, "Bug", "evidence"), (99, "Task", "evidence")])
    gh = FakeGh({5: None})

    code = MODULE.main(base_argv(tmp_path, table), invoke=gh)

    captured = capsys.readouterr()
    assert code == 0
    assert gh.writes == [(5, "Bug")]
    assert "1 not open" in captured.out
    assert "#99" in captured.err


def test_duplicate_issue_rows_are_refused(tmp_path):
    table = write_table(tmp_path, [(5, "Bug", "evidence"), (5, "Task", "evidence")])

    with pytest.raises(ValueError):
        MODULE.load_table(table)


def test_the_script_imports_nothing_third_party():
    source = SCRIPT.read_text(encoding="utf-8")
    imported = {
        line.split()[1].split(".")[0]
        for line in source.splitlines()
        if line.startswith("import ") or line.startswith("from ")
    }
    allowed = {
        "__future__",
        "argparse",
        "csv",
        "dataclasses",
        "datetime",
        "json",
        "pathlib",
        "subprocess",
        "sys",
        "time",
        "typing",
    }
    assert imported <= allowed, sorted(imported - allowed)
