import json
from pathlib import Path

from jira_bridge import create_issue
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


def test_create_allowlist_is_loaded_from_the_environment_at_startup():
    cfg = Config.load(env={"JIRA_CREATE_PROJECTS": "SCRUM, OPS"})
    assert cfg.create_projects == ("SCRUM", "OPS")


def test_create_allowlist_defaults_to_empty_which_refuses_every_project():
    cfg = Config.load(env={})
    assert cfg.create_projects == ()
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket"},
        allowed_projects=cfg.create_projects,
    )
    assert parsed is None
    assert error == "policy: project must be one of the configured creation targets ()"


def test_create_parse_refuses_project_outside_configured_allowlist_by_name():
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "OPS", "summary": "A ticket"},
        allowed_projects=("SCRUM",),
    )
    assert parsed is None
    assert error == "policy: project must be one of the configured creation targets ('SCRUM',)"


def test_create_allowlist_is_exact_match_not_prefix_or_case_insensitive():
    for project in ("SCRUM2", "SCR", "scrum"):
        parsed, _error = create_issue.parse(
            {"verb": "create_issue", "project": project, "summary": "A ticket"},
            allowed_projects=("SCRUM",),
        )
        assert parsed is None, f"{project!r} must not match the exact allowlist entry 'SCRUM'"


def test_create_parse_has_a_closed_input_shape():
    for bad in [
        {"verb": "post_comment", "project": "SCRUM", "summary": "A ticket"},
        {"verb": "create_issue", "project": "SCRUM"},
        {"verb": "create_issue", "project": "SCRUM", "summary": ""},
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket", "assignee": "alice"},
        {"verb": "create_issue", "project": "bad key", "summary": "A ticket"},
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket", "issue_type": ""},
        "not an object",
    ]:
        parsed, error = create_issue.parse(bad, allowed_projects=("SCRUM",))
        assert parsed is None and error


def test_create_happy_path_posts_recorded_v3_shape_and_proposes_claim():
    parsed, error = create_issue.parse(
        {
            "verb": "create_issue",
            "project": "SCRUM",
            "summary": "Wire the new lane",
            "description": "Details of the request.",
            "issue_type": "Task",
        },
        allowed_projects=("SCRUM",),
    )
    assert error is None
    seen = {}

    def open_(request, timeout):
        seen.update(
            url=request.full_url,
            method=request.method,
            body=json.loads(request.data),
            timeout=timeout,
        )
        return Response(
            201, (Path(__file__).parent / "fixtures/create-issue-response.json").read_bytes()
        )

    posted = create_issue.create(
        "team.example.com", parsed, "robot@example.com", "secret", opener=open_
    )
    assert posted == create_issue.CreateResult(True, 201, key="SCRUM-24", issue_id="10023")
    assert seen["url"] == "https://team.example.com/rest/api/3/issue"
    assert seen["method"] == "POST"
    fields = seen["body"]["fields"]
    assert fields["project"] == {"key": "SCRUM"}
    assert fields["summary"] == "Wire the new lane"
    assert fields["issuetype"] == {"name": "Task"}
    assert fields["description"]["content"][0]["content"][0]["text"] == "Details of the request."
    assert create_issue.result("SCRUM-24", "10023", "jira-actor") == {
        "status": "completed",
        "outcome": "issue_created",
        "output": {"issue": "SCRUM-24", "id": "10023"},
        "ledger_records": [{
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": "jira-actor"},
            "payload": {"verb": "create_issue", "issue": "SCRUM-24", "id": "10023"},
        }],
    }


def test_create_without_description_omits_the_field_entirely():
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "Bare summary"},
        allowed_projects=("SCRUM",),
    )
    assert error is None
    assert parsed.issue_type == "Task"
    seen = {}

    def open_(request, timeout):
        seen["body"] = json.loads(request.data)
        return Response(201, b'{"id":"10024","key":"SCRUM-25"}')

    posted = create_issue.create(
        "team.example.com", parsed, "robot@example.com", "secret", opener=open_
    )
    assert posted.ok and posted.key == "SCRUM-25"
    assert "description" not in seen["body"]["fields"]


def test_create_reports_http_failure_without_raising():
    import urllib.error

    def open_(request, timeout):
        raise urllib.error.HTTPError(request.full_url, 400, "Bad Request", None, None)

    parsed, _ = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket"},
        allowed_projects=("SCRUM",),
    )
    posted = create_issue.create(
        "team.example.com", parsed, "robot@example.com", "secret", opener=open_
    )
    assert posted == create_issue.CreateResult(
        False, 400, error="Jira create request returned HTTP 400"
    )


def test_create_refuses_a_site_that_is_not_a_bare_host_name():
    parsed, _ = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket"},
        allowed_projects=("SCRUM",),
    )
    posted = create_issue.create(
        "team.example.com/evil", parsed, "robot@example.com", "secret", opener=None
    )
    assert not posted.ok and "host name" in posted.error


def test_server_dispatches_create_and_advertises_the_verb_surface(monkeypatch):
    import threading
    import urllib.error
    import urllib.request

    from jira_bridge import server
    from jira_bridge.config import Config

    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
    monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")

    cfg = Config(
        jira_site="team.example.com",
        transition_project_prefix="SCRUM-",
        transition_target="Done",
        create_projects=("SCRUM",),
        port=0,
    )
    srv = server.make_server(cfg)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        with urllib.request.urlopen(f"{base}/v1/capabilities", timeout=5) as response:
            advertised = json.loads(response.read())
        assert advertised["verbs"] == ["post_comment", "transition_issue", "create_issue"]
        assert advertised["custody"]["create_projects"] == ["SCRUM"]

        refused = urllib.request.Request(
            f"{base}/v1/invocations",
            data=json.dumps(
                {"input": {"verb": "create_issue", "project": "OPS", "summary": "No"}}
            ).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(refused, timeout=5)
        except urllib.error.HTTPError as exc:
            assert exc.code == 400
            body = json.loads(exc.read())
            assert body["class"] == "actor_rejected_input"
            assert "creation targets" in body["error"]
        else:
            raise AssertionError("disallowed project was accepted by the bridge")
    finally:
        srv.shutdown()
        srv.server_close()


def test_create_parse_refuses_issue_type_outside_configured_allowlist_by_name():
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket", "issue_type": "Epic"},
        allowed_projects=("SCRUM",),
        allowed_issue_types=("Task", "Bug"),
    )
    assert parsed is None
    assert error == "policy: issue_type must be one of the configured types ('Task', 'Bug')"


def test_create_parse_defaults_issue_type_to_first_configured_type():
    parsed, error = create_issue.parse(
        {"verb": "create_issue", "project": "SCRUM", "summary": "A ticket"},
        allowed_projects=("SCRUM",),
        allowed_issue_types=("Bug", "Task"),
    )
    assert error is None
    assert parsed is not None and parsed.issue_type == "Bug"
