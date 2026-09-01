import hashlib
import json
import threading
import urllib.error
import urllib.request
from pathlib import Path

from jira_bridge import read_issue, server
from jira_bridge.config import Config, ConfigError


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


def test_read_parse_has_an_exact_closed_input_shape():
    for bad in [
        {"verb": "read_issue"},
        {"verb": "post_comment", "issue": "SCRUM-17"},
        {"verb": "read_issue", "issue": "SCRUM-17", "fields": ["summary"]},
        {"verb": "read_issue", "issue": "bad key"},
        "not an object",
    ]:
        parsed, error = read_issue.parse(bad, project_prefix="SCRUM-", comment_limit=2)
        assert parsed is None and error


def test_read_parse_refuses_prefix_and_unconfigured_limit_before_http():
    calls = []
    for issue, limit in [("OPS-17", 2), ("SCRUM-17", None), ("SCRUM-17", 0), ("SCRUM-17", "2")]:
        parsed, error = read_issue.parse(
            {"verb": "read_issue", "issue": issue},
            project_prefix="SCRUM-",
            comment_limit=limit,
        )
        assert parsed is None and error.startswith("policy:")
    assert calls == []


def test_read_comment_limit_is_env_only_and_has_no_default(tmp_path):
    assert Config.load(env={}).read_comment_limit is None
    assert Config.load(env={"JIRA_READ_COMMENT_LIMIT": "2"}).read_comment_limit == 2
    path = tmp_path / "config.json"
    path.write_text('{"read_comment_limit": 2}', encoding="utf-8")
    try:
        Config.load(str(path), env={})
    except ConfigError as exc:
        assert "unknown bridge config key" in str(exc)
    else:
        raise AssertionError("read_comment_limit became a permitted config-file key")


def test_read_gets_exact_issue_fields_flattens_adf_caps_comments_and_proposes_claim():
    parsed, error = read_issue.parse(
        {"verb": "read_issue", "issue": "SCRUM-17"},
        project_prefix="SCRUM-",
        comment_limit=1,
    )
    assert error is None
    seen = {}

    def open_(request, timeout):
        seen.update(url=request.full_url, method=request.method, timeout=timeout)
        return Response(
            200, (Path(__file__).parent / "fixtures/read-issue-response.json").read_bytes()
        )

    fetched = read_issue.read(
        "team.example.com", parsed, "robot@example.com", "secret", opener=open_
    )
    assert fetched.ok
    assert seen == {
        "url": (
            "https://team.example.com/rest/api/3/issue/SCRUM-17"
            "?fields=summary,description,status,comment,issuelinks&expand=renderedFields"
        ),
        "method": "GET",
        "timeout": 10,
    }
    assert fetched.output == {
        "issue": "SCRUM-17",
        "summary": "Read the current Jira context",
        "description": "Context\nFresh from Jira.",
        "description_truncated": False,
        "status": "In Progress",
        "comments": [
            {
                "id": "101",
                "author": "acct-1",
                "created": "2026-09-01T10:00:00.000+0000",
                "body": "First",
            }
        ],
        "links": [
            {"type": "Blocks", "direction": "outward", "linked_key": "SCRUM-18"},
            {"type": "Relates", "direction": "inward", "linked_key": "SCRUM-9"},
        ],
    }
    result = read_issue.result(fetched.output, "jira-actor")
    assert result["output"] == fetched.output
    assert result["ledger_records"] == [
        {
            "record_type": "claim",
            "authority": "proposed",
            "origin": {"kind": "agent", "actor_id": "jira-actor"},
            "payload": {"verb": "read_issue", "issue": "SCRUM-17"},
        }
    ]


def test_read_description_is_capped_at_4000_characters():
    output = read_issue.issue_output(
        {
            "key": "SCRUM-17",
            "fields": {"description": "x" * 4001, "comment": {"comments": []}},
        },
        comment_limit=1,
    )
    assert output["description"] == "x" * 4000
    assert output["description_truncated"] is True


def test_server_dispatches_read_and_advertises_read_custody(monkeypatch):
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
    monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
    expected = {"issue": "SCRUM-17", "summary": "Current"}

    def read_(*_args, **_kwargs):
        return read_issue.ReadResult(True, 200, output=expected)

    monkeypatch.setattr(read_issue, "read", read_)
    cfg = Config(
        jira_site="team.example.com",
        transition_project_prefix="SCRUM-",
        read_comment_limit=3,
        port=0,
    )
    srv = server.make_server(cfg)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        with urllib.request.urlopen(f"{base}/v1/capabilities", timeout=5) as response:
            advertised = json.loads(response.read())
        assert advertised["verbs"] == [
            "post_comment",
            "transition_issue",
            "create_issue",
            "read_issue",
        ]
        assert advertised["custody"]["read_project_prefix"] == "SCRUM-"
        assert advertised["custody"]["read_comment_limit"] == 3

        request = urllib.request.Request(
            f"{base}/v1/invocations",
            data=json.dumps({"input": {"verb": "read_issue", "issue": "SCRUM-17"}}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            body = json.loads(response.read())
        assert body["output"] == expected
        assert body["ledger_records"][0]["authority"] == "proposed"
    finally:
        srv.shutdown()
        srv.server_close()


def test_client_module_remains_byte_unchanged():
    client = Path(__file__).parents[1] / "src/jira_bridge/client.py"
    assert hashlib.sha256(client.read_bytes()).hexdigest() == (
        "2c7818b242fc685a8a8d5f3412e36391161bb0d60eb00bcd34c644acfd11e926"
    )
