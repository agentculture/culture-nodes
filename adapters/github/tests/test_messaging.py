import io
import json

from github_bridge import client, mapping
from github_bridge.config import Config, ConfigError
from github_bridge.server import Handler


class Response:
    status = 201

    def __init__(self, payload):
        self.payload = json.dumps(payload).encode()

    def read(self):
        return self.payload

    def __enter__(self):
        return self

    def __exit__(self, *_):
        pass


def test_both_verbs_port_the_land_reply_endpoints():
    seen = []

    def open_(request, timeout):
        seen.append((request.full_url, request.method, json.loads(request.data), timeout))
        return Response(
            {"id": 42, "html_url": "https://github.com/acme/repo/pull/7#issuecomment-42"}
        )

    comment = client.post_comment("acme/repo", 7, "Done", "secret", opener=open_)
    reply = client.reply_to_review_thread("acme/repo", 7, 19, "Fixed", "secret", opener=open_)
    assert comment.ok and reply.ok
    assert comment.comment_id == reply.comment_id == "42"
    assert seen[0][:3] == (
        "https://api.github.com/repos/acme/repo/issues/7/comments",
        "POST",
        {"body": "Done"},
    )
    assert seen[1][:3] == (
        "https://api.github.com/repos/acme/repo/pulls/7/comments/19/replies",
        "POST",
        {"body": "Fixed"},
    )


def test_allowlist_and_closed_input_reject_before_network():
    allowed = ("acme/repo",)
    for verb in ("post_comment", "reply_to_review_thread"):
        raw = {"verb": verb, "repository": "other/repo", "number": 7, "comment": "Done"}
        if verb == "reply_to_review_thread":
            raw["comment_id"] = 19
        assert mapping.parse(raw, allowed)[0] is None
        raw["repository"] = "acme/repo"
        assert mapping.parse(raw, allowed)[1] is None
        assert mapping.parse({**raw, "path": "/graphql"}, allowed)[0] is None
    assert (
        mapping.parse(
            {"verb": "post_comment", "repository": "acme/repo", "number": True, "comment": "Done"},
            allowed,
        )[0]
        is None
    )
    assert (
        mapping.parse(
            {"verb": "post_comment", "repository": "acme/repo", "number": 7, "comment": "Done"}, ()
        )[0]
        is None
    )


def test_results_are_proposed_and_include_created_comment_id():
    for verb in ("post_comment", "reply_to_review_thread"):
        result = mapping.result(verb, "acme/repo", 7, "42", "actor-1")
        assert result["status"] == "completed"
        assert result["output"]["comment_id"] == "42"
        assert result["ledger_records"][0]["authority"] == "proposed"
        assert result["ledger_records"][0]["origin"]["actor_id"] == "actor-1"
        assert result["ledger_records"][0]["payload"]["verb"] == verb


def test_config_rejects_credentials_and_requires_allowlist(tmp_path):
    path = tmp_path / "config.json"
    path.write_text('{"repositories":["acme/repo"],"GITHUB_TOKEN":"secret"}')
    try:
        Config.load(str(path), env={})
    except ConfigError:
        pass
    else:
        raise AssertionError("credential accepted in config")
    assert Config.load(env={}).repositories == ()
    assert Config.load(env={"GITHUB_REPOSITORIES": "acme/repo"}).repositories == ("acme/repo",)


def test_http_actor_enforces_allowlist_and_emits_proposed_record(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "secret")
    monkeypatch.setattr(
        client, "post_comment", lambda *args, **kwargs: client.PostResult(True, 201, "42")
    )

    def invoke(repository):
        handler = Handler.__new__(Handler)
        handler.server = type("Server", (), {"cfg": Config(repositories=("acme/repo",))})()
        handler.path = "/v1/invocations"
        body = json.dumps(
            {
                "input": {
                    "verb": "post_comment",
                    "repository": repository,
                    "number": 7,
                    "comment": "Done",
                }
            }
        ).encode()
        handler.headers = {"Content-Length": str(len(body))}
        handler.rfile = io.BytesIO(body)
        answers = []
        handler._json = lambda status, payload: answers.append((status, payload))
        handler.do_POST()
        return answers[0]

    assert invoke("acme/repo")[1]["ledger_records"][0]["authority"] == "proposed"
    assert invoke("other/repo")[0] == 400


def test_marked_dispatch_stamps_both_verbs_and_returns_created_id(monkeypatch):
    from github_bridge import stamping

    marker = "cn1:firing-a:github.comment:" + "ab" * 24 + ":" + "cd" * 32
    monkeypatch.setenv("GITHUB_TOKEN", "secret")
    seen = []

    def post(*args):
        seen.append(args[-2])
        return client.PostResult(True, 201, "42")

    monkeypatch.setattr(client, "post_comment", post)
    monkeypatch.setattr(client, "reply_to_review_thread", post)

    def invoke(verb, marked):
        handler = Handler.__new__(Handler)
        handler.server = type("Server", (), {"cfg": Config(repositories=("acme/repo",))})()
        handler.path = "/v1/invocations"
        input_ = {"verb": verb, "repository": "acme/repo", "number": 7, "comment": "Done"}
        if verb == "reply_to_review_thread":
            input_["comment_id"] = 19
        if marked:
            input_["marker"] = marker
        body = json.dumps({"input": input_}).encode()
        handler.headers = {"Content-Length": str(len(body))}
        handler.rfile = io.BytesIO(body)
        answers = []
        handler._json = lambda status, payload: answers.append((status, payload))
        handler.do_POST()
        return answers[0]

    for verb in mapping.VERBS:
        status, body = invoke(verb, True)
        assert status == 200
        assert seen[-1] == stamping.stamp_text("Done", marker)
        assert body["output"]["artifact_id"] == "42"
        assert body["output"]["marker"] == marker
        status, body = invoke(verb, False)
        assert status == 200
        assert seen[-1] == "Done"
        assert "artifact_id" not in body["output"]
