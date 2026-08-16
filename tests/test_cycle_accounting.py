import csv
import importlib.util
import json
from pathlib import Path


SCRIPT = Path(__file__).parents[1] / "scripts" / "cycle-accounting.py"
SPEC = importlib.util.spec_from_file_location("cycle_accounting", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(MODULE)


def write_fixture(tmp_path, issues, dispositions=()):
    issues_path = tmp_path / "issues.json"
    issues_path.write_text(json.dumps(issues), encoding="utf-8")
    dispositions_path = tmp_path / "dispositions.csv"
    with dispositions_path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["issue", "bucket", "disposition", "evidence_pointer"])
        for issue in dispositions:
            writer.writerow([issue, "finish work", "planned", "fixture"])
    return issues_path, dispositions_path


def issue(number, created, state="OPEN", closed=None):
    return {
        "number": number,
        "createdAt": created,
        "closedAt": closed,
        "state": state,
        "title": f"Issue {number}",
        "labels": [],
    }


def test_negative_delta_is_rendered_as_a_number_and_open_findings_are_cross_checked(tmp_path):
    issues_path, dispositions_path = write_fixture(
        tmp_path,
        [
            issue(1, "2026-08-14T12:00:00Z", "CLOSED", "2026-08-15T13:00:00Z"),
            issue(2, "2026-08-15T11:00:00Z"),
            issue(3, "2026-08-15T12:00:00Z"),
            issue(4, "2026-08-15T13:00:00Z", "CLOSED", "2026-08-15T14:00:00Z"),
        ],
        dispositions=[2, 3],
    )

    accounting = MODULE.account(
        MODULE.load_issues(issues_path),
        MODULE.load_dispositions(dispositions_path),
        MODULE.parse_timestamp("2026-08-15T10:00:00Z"),
    )

    assert accounting.opened == 3
    assert accounting.closed == 2
    assert accounting.delta == -1
    assert accounting.undispositioned == ()
    assert "Closed minus opened (delta): -1" in MODULE.render(accounting)
    assert "Opened-by-cycle issues undispositioned at cycle close: 0" in MODULE.render(accounting)


def test_reports_open_cycle_issue_missing_from_dispositions(tmp_path):
    issues_path, dispositions_path = write_fixture(
        tmp_path,
        [issue(7, "2026-08-15T11:00:00Z")],
    )

    accounting = MODULE.account(
        MODULE.load_issues(issues_path),
        MODULE.load_dispositions(dispositions_path),
        MODULE.parse_timestamp("2026-08-15T10:00:00Z"),
    )

    assert accounting.undispositioned == (7,)
