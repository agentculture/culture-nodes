import json

from jira_bridge import transition_issue
from jira_bridge.config import Config


class Response:
    def __init__(self, status, data=b""):
        self.status = status
        self.data = data

    def read(self):
        return self.data

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass


def test_transition_config_is_loaded_from_the_environment_at_startup():
    cfg = Config.load(env={
        "JIRA_TRANSITION_PROJECT_PREFIX": "SCRUM-",
        "JIRA_TRANSITION_TARGET": "Done",
    })
    assert cfg.transition_project_prefix == "SCRUM-"
    assert cfg.transition_targets == ("Done",)
    # Single-string compat: a deployment that configured one target reads
    # exactly that string back (task t11).
    assert cfg.transition_target == "Done"


def test_transition_config_accepts_a_second_target_as_a_comma_list():
    """Task t11: the ticket moves to 'Pending' when culture-nodes raises a
    human decision and to 'Done' when the work finishes -- one bridge, two
    allowlisted targets."""
    cfg = Config.load(env={
        "JIRA_TRANSITION_PROJECT_PREFIX": "SCRUM-",
        "JIRA_TRANSITION_TARGET": "Done, Pending",
    })
    assert cfg.transition_targets == ("Done", "Pending")


def test_transition_parse_accepts_either_allowlisted_target():
    for target in ("Done", "Pending"):
        parsed, error = transition_issue.parse(
            {"verb": "transition_issue", "issue": "SCRUM-17", "target": target},
            project_prefix="SCRUM-",
            allowed_targets=("Done", "Pending"),
        )
        assert error is None
        assert parsed.target == target


def test_transition_parse_refuses_a_target_outside_a_multi_entry_allowlist():
    parsed, error = transition_issue.parse(
        {"verb": "transition_issue", "issue": "SCRUM-17", "target": "In Progress"},
        project_prefix="SCRUM-",
        allowed_targets=("Done", "Pending"),
    )
    assert parsed is None
    assert error == "policy: target must be one of the configured transitions: 'Done', 'Pending'"


def test_transition_parse_refuses_everything_when_no_target_is_configured():
    parsed, error = transition_issue.parse(
        {"verb": "transition_issue", "issue": "SCRUM-17", "target": "Done"},
        project_prefix="SCRUM-",
        allowed_targets=(),
    )
    assert parsed is None
    assert error == "policy: target must be one of the configured transitions: (none configured)"


def test_transition_parse_refuses_issue_outside_configured_project_prefix():
    parsed, error = transition_issue.parse(
        {"verb": "transition_issue", "issue": "OPS-17", "target": "Done"},
        project_prefix="SCRUM-",
        allowed_targets=("Done",),
    )
    assert parsed is None
    assert error == "policy: issue must match configured project prefix 'SCRUM-'"


def test_transition_parse_refuses_target_outside_the_configured_allowlist():
    parsed, error = transition_issue.parse(
        {"verb": "transition_issue", "issue": "SCRUM-17", "target": "In Progress"},
        project_prefix="SCRUM-",
        allowed_targets=("Done",),
    )
    assert parsed is None
    assert error == "policy: target must be one of the configured transitions: 'Done'"


def test_transition_happy_path_uses_named_transition_and_proposes_claim():
    parsed, error = transition_issue.parse(
        {"verb": "transition_issue", "issue": "SCRUM-17", "target": "Done"},
        project_prefix="SCRUM-",
        allowed_targets=("Done",),
    )
    assert error is None
    seen = []

    def open_(request, timeout):
        seen.append({
            "url": request.full_url,
            "method": request.method,
            "body": json.loads(request.data) if request.data else None,
            "timeout": timeout,
        })
        if request.method == "GET":
            return Response(200, b'{"transitions":[{"id":"31","name":"Done"}]}')
        return Response(204)

    posted = transition_issue.transition(
        "team.example.com", parsed.issue, parsed.target, "robot@example.com", "secret", opener=open_
    )
    assert posted == transition_issue.TransitionResult(True, 204)
    assert seen == [
        {
            "url": "https://team.example.com/rest/api/3/issue/SCRUM-17/transitions",
            "method": "GET",
            "body": None,
            "timeout": 10,
        },
        {
            "url": "https://team.example.com/rest/api/3/issue/SCRUM-17/transitions",
            "method": "POST",
            "body": {"transition": {"id": "31"}},
            "timeout": 10,
        },
    ]
    assert transition_issue.result(parsed.issue, parsed.target, "jira-actor") == {
        "status": "completed",
        "outcome": "issue_transitioned",
        "output": {"issue": "SCRUM-17", "target": "Done"},
        "ledger_records": [{
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": "jira-actor"},
            "payload": {"verb": "transition_issue", "issue": "SCRUM-17", "target": "Done"},
        }],
    }
