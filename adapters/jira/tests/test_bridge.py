import json
from pathlib import Path

from jira_bridge import client, mapping
from jira_bridge.config import Config, ConfigError


class Response:
    status = 201
    def __init__(self):
        self.data = (Path(__file__).parent / "fixtures/comment-response.json").read_bytes()
    def read(self): return self.data
    def __enter__(self): return self
    def __exit__(self, *_): pass


def test_fixture_backed_comment_request_uses_only_comment_endpoint():
    seen = {}
    def open_(request, timeout):
        seen.update(url=request.full_url, method=request.method, body=json.loads(request.data), timeout=timeout)
        return Response()
    result = client.post_comment("team.example.com", "EX-123", "Shipped", "robot@example.com", "secret", opener=open_)
    assert result == client.PostResult(True, 201, comment_id="10042")
    assert seen["url"] == "https://team.example.com/rest/api/3/issue/EX-123/comment"
    assert seen["method"] == "POST"
    assert seen["body"]["body"]["content"][0]["content"][0]["text"] == "Shipped"


def test_exactly_one_verb_and_closed_input_shape():
    parsed, error = mapping.parse({"verb": "post_comment", "issue": "EX-123", "comment": "Done"})
    assert error is None and parsed.issue == "EX-123"
    for bad in [
        {"verb": "transition", "issue": "EX-123", "comment": "Done"},
        {"verb": "post_comment", "issue": "EX-123", "comment": "Done", "path": "/rest/api/3/issue/EX-123/transitions"},
    ]:
        assert mapping.parse(bad)[0] is None


def test_question_comment_carries_the_fixed_actor_marker_and_question_id():
    parsed, error = mapping.parse({
        "verb": "post_comment", "issue": "EX-123",
        "comment": "Which behavior is intended?", "question_id": "run-17.q1",
    })
    assert error is None
    assert parsed.marked_text.endswith("[culture-nodes:jira-actor question_id=run-17.q1]")


def test_question_id_is_closed_and_injection_safe():
    for question_id in ("", "run-17:q1", "bad id", "bad]\nanswer"):
        parsed, error = mapping.parse({
            "verb": "post_comment", "issue": "EX-123", "comment": "Question",
            "question_id": question_id,
        })
        if question_id in {"", "run-17:q1"}:
            assert error is None
        else:
            assert parsed is None and "question_id" in error


def test_credentials_cannot_be_loaded_from_config(tmp_path):
    path = tmp_path / "jira.json"
    path.write_text('{"jira_site":"team.example.com","JIRA_API_TOKEN":"committed"}')
    try:
        Config.load(str(path), env={})
    except ConfigError as exc:
        assert "unknown bridge config key" in str(exc)
    else:
        raise AssertionError("credential-bearing config was accepted")
