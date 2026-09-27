import json
import threading
import urllib.request

from jira_bridge import client, create_issue, server, transition_issue
from jira_bridge.config import Config

MARKER = "cn1:firing-a:jira.comment:" + "ab" * 24 + ":" + "cd" * 32


def test_marked_jira_verbs_embed_marker_and_return_artifact_id(monkeypatch):
    monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
    monkeypatch.setenv("JIRA_API_TOKEN", "secret")
    seen = []

    def comment(_site, _issue, text, *_args, **_kwargs):
        seen.append(("comment", text))
        return client.PostResult(True, 201, "42")

    def create(_site, issue, *_args, **_kwargs):
        seen.append(("create", issue.description))
        return create_issue.CreateResult(True, 201, "SCRUM-2", "43")

    def transition(_site, _issue, _target, *_args, **_kwargs):
        seen.append(("transition", ""))
        return transition_issue.TransitionResult(True, 204)

    monkeypatch.setattr(client, "post_comment", comment)
    monkeypatch.setattr(create_issue, "create", create)
    monkeypatch.setattr(transition_issue, "transition", transition)
    cfg = Config(
        jira_site="team.example.com",
        create_projects=("SCRUM",),
        transition_project_prefix="SCRUM-",
        transition_targets=("Done",),
        port=0,
    )
    srv = server.make_server(cfg)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        with urllib.request.urlopen(base + "/v1/capabilities") as response:
            assert json.load(response)["stamping"] == {"marker": "cn1", "version": 1}
        inputs = [
            ({"verb": "post_comment", "issue": "SCRUM-2", "comment": "Hello"}, "42"),
            ({"verb": "create_issue", "project": "SCRUM", "summary": "Hello"}, "43"),
            ({"verb": "transition_issue", "issue": "SCRUM-2", "target": "Done"}, "42"),
        ]
        for raw, expected_id in inputs:
            for marked in (True, False):
                payload = {**raw, **({"marker": MARKER} if marked else {})}
                req = urllib.request.Request(
                    base + "/v1/invocations",
                    data=json.dumps({"input": payload}).encode(),
                    method="POST",
                )
                with urllib.request.urlopen(req) as response:
                    body = json.load(response)
                if marked:
                    assert body["output"]["artifact_id"] == expected_id
                    assert body["output"]["marker"] == MARKER
                    assert MARKER in seen[-1][1]
                else:
                    assert "artifact_id" not in body["output"]
                    assert MARKER not in seen[-1][1]
    finally:
        srv.shutdown()
        srv.server_close()
