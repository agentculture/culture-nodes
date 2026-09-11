"""examples/land/land_reply.py -- the land node's REPLY and RESOLVE steps
(loop-closure task t8; spec c3/c38/c40, honesty h12/h25/h27).

A fake GitHub on localhost stands in for api.github.com (REST + /graphql)
AND for the control plane's GET /v1alpha1/runs/{id} -- one HTTP server,
dispatched by path -- so every request the step makes is recorded and can be
asserted on: which threads were replied to, which were resolved, and that
nothing was posted twice. The landing context is either a stub (unit) or the
real land.py run end to end through its hook points (integration), so the
step records land in land.py's `land_step` shape both ways.
"""

from __future__ import annotations

import http.server
import importlib.util
import json
import re
import sys
import threading
from pathlib import Path
from types import SimpleNamespace

import pytest

from tests.test_land_node import (  # noqa: F401 - fixtures by name
    ACTOR_ID,
    LAND_RUN,
    PRODUCING_RUN,
    TARGET,
    WORK_ITEM,
    _quiet_env,
    actor_checkout,
    code_only,
    git,
    land,
    land_ws,
    mint_handover,
    origin,
    result,
    run_land,
    steps,
)

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "land"
REPLY_SCRIPT = EXAMPLE_DIR / "land_reply.py"
LAND_SCRIPT = EXAMPLE_DIR / "land.py"

REPO = "agentculture/culture-nodes"
PR = 307
SHA = "1" * 40
TOKEN = "land-pr-test-token"


def _load_reply():
    spec = importlib.util.spec_from_file_location("land_reply", REPLY_SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


land_reply = _load_reply()

#: The real control-plane probe, captured before the autouse fixture from
#: test_land_node replaces it with a seam; the end-to-end tests below point
#: it at the fake server instead.
REAL_ACTIVE_ATTEMPTS = land.active_attempts


# ---------------------------------------------------------------------------
# the fake GitHub (+ control plane) on localhost
# ---------------------------------------------------------------------------


class FakeGitHub:
    """State one test's fake owns: review threads, issue comments, the
    producing run's input, and every request seen."""

    def __init__(self) -> None:
        self.threads: list[dict] = []
        self.issue_comments: list[dict] = []
        self.run_input: dict | None = None
        self.node_runs: list[dict] = []
        self.actors: list[dict] = [{"id": ACTOR_ID, "actor_key": "codex/thor", "revision": 1}]
        self.requests: list[tuple[str, str, dict | None, dict]] = []
        self.next_comment_id = 9000

    def add_thread(self, thread_id: str, *comments: str, resolved: bool = False) -> None:
        nodes = []
        for body in comments:
            self.next_comment_id += 1
            nodes.append({"databaseId": self.next_comment_id, "body": body})
        self.threads.append({"id": thread_id, "isResolved": resolved, "comments": nodes})

    def thread_of_comment(self, comment_id: int) -> dict | None:
        for thread in self.threads:
            if any(c["databaseId"] == comment_id for c in thread["comments"]):
                return thread
        return None

    def posts(self) -> list[tuple[str, str]]:
        """Every WRITE: a REST POST, or a GraphQL mutation (a GraphQL read is
        a POST on the wire too, and is not counted here)."""
        return [
            (m, p)
            for m, p, b, _h in self.requests
            if m == "POST" and (p != "/graphql" or "mutation" in (b or {}).get("query", ""))
        ]

    def resolves(self) -> list[str]:
        return [
            b["variables"]["id"]
            for m, p, b, _h in self.requests
            if m == "POST" and p == "/graphql" and "resolveReviewThread" in b.get("query", "")
        ]


def _handler(state: FakeGitHub):
    class Handler(http.server.BaseHTTPRequestHandler):
        def _send(self, status: int, payload) -> None:
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _body(self) -> dict | None:
            length = int(self.headers.get("Content-Length") or 0)
            if not length:
                return None
            return json.loads(self.rfile.read(length))

        def _record(self, method: str, body: dict | None) -> None:
            state.requests.append((method, self.path, body, dict(self.headers)))

        def do_GET(self):  # noqa: N802 - http.server's name
            self._record("GET", None)
            path = self.path.split("?", 1)[0]
            if path == f"/v1alpha1/runs/{PRODUCING_RUN}":
                if state.run_input is None:
                    return self._send(404, {"error": "not found"})
                return self._send(200, {"run": {"id": PRODUCING_RUN, "input": state.run_input}})
            if path == "/v1alpha1/node-runs":
                return self._send(200, {"items": state.node_runs})
            # The checkout probe resolves the producing actor's KEY and every
            # registration revision that shares it before it counts anything.
            if path == "/v1alpha1/actors":
                return self._send(200, {"items": state.actors})
            if path.startswith("/v1alpha1/actors/"):
                wanted = path.rsplit("/", 1)[1]
                for actor in state.actors:
                    if actor["id"] == wanted:
                        return self._send(200, actor)
                return self._send(404, {"error": "no such actor"})
            if path == f"/repos/{REPO}/issues/{PR}/comments":
                return self._send(200, state.issue_comments)
            return self._send(404, {"message": "Not Found"})

        def do_POST(self):  # noqa: N802 - http.server's name
            body = self._body()
            self._record("POST", body)
            if self.headers.get("Authorization") != f"Bearer {TOKEN}":
                return self._send(401, {"message": "Bad credentials"})
            if not self.headers.get("User-Agent"):
                return self._send(403, {"message": "Request forbidden by administrative rules"})
            if self.path == "/graphql":
                return self._graphql(body or {})
            replies = re.fullmatch(rf"/repos/{REPO}/pulls/{PR}/comments/(\d+)/replies", self.path)
            if replies:
                thread = state.thread_of_comment(int(replies.group(1)))
                if thread is None:
                    return self._send(404, {"message": "Not Found"})
                state.next_comment_id += 1
                comment = {"databaseId": state.next_comment_id, "body": body["body"]}
                thread["comments"].append(comment)
                return self._send(201, {"id": comment["databaseId"], "html_url": "u"})
            if self.path == f"/repos/{REPO}/issues/{PR}/comments":
                state.next_comment_id += 1
                comment = {"id": state.next_comment_id, "body": body["body"], "html_url": "u"}
                state.issue_comments.append(comment)
                return self._send(201, comment)
            return self._send(404, {"message": "Not Found"})

        def _graphql(self, body: dict) -> None:
            query = body.get("query", "")
            variables = body.get("variables", {})
            if "resolveReviewThread" in query:
                for thread in state.threads:
                    if thread["id"] == variables["id"]:
                        thread["isResolved"] = True
                        payload = {"thread": {"id": thread["id"], "isResolved": True}}
                        return self._send(200, {"data": {"resolveReviewThread": payload}})
                return self._send(200, {"errors": [{"message": "no such thread"}]})
            nodes = [
                {
                    "id": t["id"],
                    "isResolved": t["isResolved"],
                    "comments": {"nodes": list(t["comments"])},
                }
                for t in state.threads
            ]
            threads = {"pageInfo": {"hasNextPage": False, "endCursor": None}, "nodes": nodes}
            data = {"repository": {"pullRequest": {"reviewThreads": threads}}}
            return self._send(200, {"data": data})

        def log_message(self, *_a):  # silence
            return

    return Handler


@pytest.fixture
def fake_github(monkeypatch):
    state = FakeGitHub()
    server = http.server.HTTPServer(("127.0.0.1", 0), _handler(state))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    state.url = f"http://127.0.0.1:{server.server_address[1]}"
    monkeypatch.setenv("LAND_GITHUB_API_URL", state.url)
    monkeypatch.setenv("NODES_API_URL", state.url)
    monkeypatch.setenv("GITHUB_TOKEN_LAND_PR", TOKEN)
    monkeypatch.setenv("LAND_REPLY_ENV_FILE", "/nonexistent/land-pr.env")
    try:
        yield state
    finally:
        server.shutdown()


# ---------------------------------------------------------------------------
# a landing context without git: the stub the unit tests hand the steps
# ---------------------------------------------------------------------------


class Recorder:
    def __init__(self) -> None:
        self.records: list[dict] = []

    def step(self, name: str, outcome: str, **fields) -> dict:
        record = {"record": "land_step", "step": name, "outcome": outcome, **fields}
        self.records.append(record)
        return record

    def of(self, name: str) -> list[dict]:
        return [r for r in self.records if r["step"] == name]


def landing_stub(sha: str = SHA) -> SimpleNamespace:
    inputs = SimpleNamespace(run_id=PRODUCING_RUN, work_item=WORK_ITEM, target_branch="pr/x")
    return SimpleNamespace(
        inputs=inputs, land=SimpleNamespace(run_id=LAND_RUN), landed_commit=sha, records=Recorder()
    )


class StubRefusal(Exception):
    def __init__(self, message: str, hint: str, code: int = 2) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


def upkeep_input(*findings: dict) -> dict:
    return {
        "source": "github_pr",
        "repository": REPO,
        "number": PR,
        "head_sha": "0" * 40,
        "findings": list(findings),
        "work_item": WORK_ITEM,
    }


QODO = {"source": "qodo", "id": f"pr{PR}-qodo-3", "pr": PR, "title": "unused import"}
SONAR = {"source": "sonarcloud", "id": "AZk9sonarKEY", "pr": PR, "rule": "python:S1481"}
CHECK = {"source": "github-check", "id": f"pr{PR}-check-77", "pr": PR}


def run_both(ctx) -> tuple[dict, dict]:
    reply = land_reply.reply_step(ctx, refusal=StubRefusal)
    resolve = land_reply.resolve_step(ctx, refusal=StubRefusal)
    return reply, resolve


# ---------------------------------------------------------------------------
# one reply + one resolve per landed finding, signed, naming sha and id
# ---------------------------------------------------------------------------


def test_each_landed_finding_gets_one_reply_and_one_resolve(fake_github):
    fake_github.run_input = upkeep_input(QODO, SONAR)
    # The finding id sits in the thread's opening comment, as a review bot
    # (or a person quoting the id) leaves it; the Sonar thread carries its key.
    fake_github.add_thread("T_qodo", f"Qodo finding {QODO['id']}: unused import")
    fake_github.add_thread("T_sonar", f"SonarCloud {SONAR['id']} python:S1481")
    ctx = landing_stub()

    reply, resolve = run_both(ctx)

    assert reply["outcome"] == "ok", reply
    assert reply["replies_posted"] == 2, reply
    assert resolve["outcome"] == "ok", resolve
    assert resolve["resolved"] == 2, resolve
    posted = [p for m, p in fake_github.posts() if p.endswith("/replies")]
    assert len(posted) == 2, fake_github.posts()
    assert sorted(fake_github.resolves()) == ["T_qodo", "T_sonar"]
    assert all(t["isResolved"] for t in fake_github.threads)

    for thread, finding in ((fake_github.threads[0], QODO), (fake_github.threads[1], SONAR)):
        body = thread["comments"][-1]["body"]
        assert SHA in body, body
        assert finding["id"] in body, body
        assert body.rstrip().endswith(land_reply.SIGNATURE), body
        assert "(Claude)" not in body, "the land node signs as itself, not as the cicd scripts"

    # One land_step record per action, in land.py's shape, naming the finding.
    replies = ctx.records.of("reply")
    assert [r["outcome"] for r in replies] == ["posted", "posted"]
    assert {r["finding"] for r in replies} == {QODO["id"], SONAR["id"]}
    assert all(r["record"] == "land_step" and r["commit"] == SHA for r in replies)
    resolves = ctx.records.of("resolve")
    assert [r["outcome"] for r in resolves] == ["resolved", "resolved"]
    assert {r["thread_id"] for r in resolves} == {"T_qodo", "T_sonar"}
    # No issue comment: every finding had a thread.
    assert fake_github.issue_comments == []


def test_a_finding_may_name_its_thread_or_comment_directly(fake_github):
    fake_github.add_thread("T_a", "a review comment that does not quote the id")
    fake_github.add_thread("T_b", "another one")
    comment_b = fake_github.threads[1]["comments"][0]["databaseId"]
    by_thread = {**QODO, "thread_id": "T_a"}
    by_comment = {**SONAR, "comment_id": comment_b}
    fake_github.run_input = upkeep_input(by_thread, by_comment)
    ctx = landing_stub()

    reply, resolve = run_both(ctx)

    assert reply["replies_posted"] == 2 and resolve["resolved"] == 2
    assert QODO["id"] in fake_github.threads[0]["comments"][-1]["body"]
    assert SONAR["id"] in fake_github.threads[1]["comments"][-1]["body"]


# ---------------------------------------------------------------------------
# idempotent: a re-run posts nothing new, resolves nothing again
# ---------------------------------------------------------------------------


def test_a_rerun_posts_nothing_new(fake_github):
    fake_github.run_input = upkeep_input(QODO, SONAR, CHECK)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    first = landing_stub()
    run_both(first)
    posts_after_first = list(fake_github.posts())
    assert len(posts_after_first) == 3, posts_after_first  # reply, comment, resolve
    fake_github.requests.clear()

    second = landing_stub()
    reply, resolve = run_both(second)

    assert fake_github.posts() == [], "the re-run posted or resolved something"
    assert reply["outcome"] == "ok" and reply["replies_posted"] == 0
    assert reply["replies_existing"] == 1 and reply["pr_comment"] == "existing"
    assert resolve["outcome"] == "ok" and resolve["resolved"] == 0
    assert resolve["already_resolved"] == 1
    assert [r["outcome"] for r in second.records.of("reply")] == [
        "skipped_existing",
        "skipped_existing",
    ]
    assert {r["reason"] for r in second.records.of("resolve")} == {
        "already_resolved",
        "no_review_thread",
    }
    # Exactly one land-node reply in the thread and one PR comment, still.
    signed = [c for c in fake_github.threads[0]["comments"] if land_reply.SIGNATURE in c["body"]]
    assert len(signed) == 1
    assert len(fake_github.issue_comments) == 1


def test_a_reply_for_another_landing_does_not_satisfy_this_one(fake_github):
    """Two landings on one PR reply to the same thread once each: the
    existing-reply test is keyed on the landed sha, not on the signature."""
    fake_github.run_input = upkeep_input(QODO)
    other = "2" * 40
    fake_github.add_thread(
        "T_qodo",
        f"finding {QODO['id']}",
        f"Landed {other} for `{QODO['id']}`.\n\n{land_reply.SIGNATURE}",
    )
    ctx = landing_stub()

    reply, _ = run_both(ctx)

    assert reply["replies_posted"] == 1
    assert SHA in fake_github.threads[0]["comments"][-1]["body"]


def test_an_already_resolved_thread_is_not_resolved_again(fake_github):
    fake_github.run_input = upkeep_input(QODO)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}", resolved=True)
    ctx = landing_stub()

    reply, resolve = run_both(ctx)

    assert reply["replies_posted"] == 1, "a resolved thread still gets its reply"
    assert fake_github.resolves() == []
    assert resolve["already_resolved"] == 1 and resolve["resolved"] == 0


# ---------------------------------------------------------------------------
# findings with no thread collapse into ONE PR comment
# ---------------------------------------------------------------------------


def test_findings_without_a_thread_collapse_into_one_pr_comment(fake_github):
    fake_github.run_input = upkeep_input(SONAR, CHECK, QODO)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    ctx = landing_stub()

    reply, resolve = run_both(ctx)

    comments = [p for m, p in fake_github.posts() if p == f"/repos/{REPO}/issues/{PR}/comments"]
    assert len(comments) == 1, fake_github.posts()
    assert len(fake_github.issue_comments) == 1
    body = fake_github.issue_comments[0]["body"]
    assert SHA in body and SONAR["id"] in body and CHECK["id"] in body
    assert QODO["id"] not in body, "a finding with a thread is answered on the thread"
    assert body.rstrip().endswith(land_reply.SIGNATURE)
    assert reply["pr_comment"] == "posted" and reply["replies_posted"] == 1
    comment_records = [r for r in ctx.records.of("reply") if r.get("kind") == "pr_comment"]
    assert len(comment_records) == 1
    assert sorted(comment_records[0]["findings"]) == sorted([SONAR["id"], CHECK["id"]])
    # Nothing to resolve for a finding with no thread, and the record says so.
    no_thread = [r for r in ctx.records.of("resolve") if r.get("reason") == "no_review_thread"]
    assert {r["finding"] for r in no_thread} == {SONAR["id"], CHECK["id"]}
    assert resolve["no_thread"] == 2 and resolve["resolved"] == 1


# ---------------------------------------------------------------------------
# the credential: named refusal, nothing posted
# ---------------------------------------------------------------------------


def test_a_missing_token_is_a_refusal_by_name_and_posts_nothing(fake_github, monkeypatch):
    fake_github.run_input = upkeep_input(QODO)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    monkeypatch.delenv("GITHUB_TOKEN_LAND_PR")
    ctx = landing_stub()

    with pytest.raises(StubRefusal) as excinfo:
        land_reply.reply_step(ctx, refusal=StubRefusal)

    assert "GITHUB_TOKEN_LAND_PR" in str(excinfo.value)
    assert "land-pr.env" in excinfo.value.hint
    assert fake_github.posts() == []
    refused = ctx.records.of("reply")
    assert len(refused) == 1 and refused[0]["outcome"] == "refused"
    assert refused[0]["credential"] == "GITHUB_TOKEN_LAND_PR"

    with pytest.raises(StubRefusal):
        land_reply.resolve_step(ctx, refusal=StubRefusal)
    assert fake_github.posts() == []


def test_the_token_is_read_from_land_pr_env_and_never_from_the_input(tmp_path, monkeypatch):
    monkeypatch.delenv("GITHUB_TOKEN_LAND_PR", raising=False)
    env_file = tmp_path / "land-pr.env"
    env_file.write_text('# culture-land\nGITHUB_TOKEN_LAND_PR="from-file"\n', encoding="utf-8")
    monkeypatch.setenv("LAND_REPLY_ENV_FILE", str(env_file))
    assert land_reply.reply_token() == "from-file"
    monkeypatch.setenv("GITHUB_TOKEN_LAND_PR", "from-env")
    assert land_reply.reply_token() == "from-env"
    monkeypatch.delenv("GITHUB_TOKEN_LAND_PR")
    monkeypatch.setenv("LAND_REPLY_ENV_FILE", str(tmp_path / "absent.env"))
    assert land_reply.reply_token() is None
    # The push token is a different credential (h25: two tokens, two scopes).
    assert land_reply.REPLY_CREDENTIAL != land.PUSH_CREDENTIAL


def test_the_push_token_never_reaches_the_reply_client(fake_github, monkeypatch):
    monkeypatch.delenv("GITHUB_TOKEN_LAND_PR")
    monkeypatch.setenv("GITHUB_TOKEN_WORKER", "contents-write-token")
    fake_github.run_input = upkeep_input(QODO)
    ctx = landing_stub()
    with pytest.raises(StubRefusal):
        land_reply.reply_step(ctx, refusal=StubRefusal)
    assert fake_github.posts() == []


def test_every_request_carries_a_user_agent_and_the_bearer(fake_github):
    fake_github.run_input = upkeep_input(QODO)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    run_both(landing_stub())
    github_requests = [r for r in fake_github.requests if not r[1].startswith("/v1alpha1/")]
    assert github_requests
    for _m, path, _b, headers in github_requests:
        assert headers.get("User-Agent") == land_reply.USER_AGENT, path
        assert headers.get("Authorization") == f"Bearer {TOKEN}", path
        assert "Python-urllib" not in headers.get("User-Agent", "")


# ---------------------------------------------------------------------------
# the producing run: where the PR and its findings come from
# ---------------------------------------------------------------------------


def test_a_producing_run_without_pr_findings_is_a_recorded_skip(fake_github):
    """An assigned developer package has no pr-upkeep input: nothing to
    reply to, and the record says why rather than inventing a thread."""
    fake_github.run_input = {"instruction": "do the thing", "work_item": WORK_ITEM}
    ctx = landing_stub()
    reply, resolve = run_both(ctx)
    assert reply["outcome"] == "skipped" and reply["reason"] == "no_pr_findings_on_producing_run"
    assert (
        resolve["outcome"] == "skipped" and resolve["reason"] == "no_pr_findings_on_producing_run"
    )
    assert fake_github.posts() == []


def test_no_control_plane_is_a_recorded_skip_not_a_guess(monkeypatch):
    monkeypatch.delenv("NODES_API_URL", raising=False)
    ctx = landing_stub()
    reply = land_reply.reply_step(ctx, refusal=StubRefusal)
    assert reply["outcome"] == "skipped" and reply["reason"] == "no_control_plane"


def test_an_unreadable_control_plane_is_an_environment_refusal(fake_github):
    fake_github.run_input = None  # 404 from the control plane
    ctx = landing_stub()
    with pytest.raises(StubRefusal) as excinfo:
        land_reply.reply_step(ctx, refusal=StubRefusal)
    assert PRODUCING_RUN in str(excinfo.value)
    assert fake_github.posts() == []


# ---------------------------------------------------------------------------
# the boundary: no merge endpoint, in the code that executes
# ---------------------------------------------------------------------------


def test_the_client_has_no_merge_endpoint():
    code = code_only(REPLY_SCRIPT.read_text(encoding="utf-8"))
    assert not re.search(r"/pulls/[^\n\"']*/merge", code)
    assert not re.search(r"/merges\b", code)
    assert "mergePullRequest" not in code
    assert "enablePullRequestAutoMerge" not in code
    assert (
        "merge" not in code.lower()
    ), "the reply client never spells merge; a PR merge is a human's act"
    # Its declared GitHub operations are exactly: read threads, reply,
    # resolve, read comments, comment.
    assert "resolveReviewThread" in code
    assert "/replies" in code
    assert "/issues/" in code and "/comments" in code
    # And land.py's own guard still holds with the hooks wired in.
    land_code = code_only(LAND_SCRIPT.read_text(encoding="utf-8"))
    assert "api.github.com" not in land_code
    assert "merge" not in land_code.replace("merge-base", "").replace("git-common-dir", "")


def test_the_reply_client_never_forces_or_pushes():
    code = code_only(REPLY_SCRIPT.read_text(encoding="utf-8"))
    assert "subprocess" not in code and '"push"' not in code
    assert land_reply.REPLY_CREDENTIAL == "GITHUB_TOKEN_LAND_PR"
    assert land_reply.SIGNATURE == "- culture-nodes (land node)"


# ---------------------------------------------------------------------------
# end to end through land.py's hook points
# ---------------------------------------------------------------------------


def test_a_landing_replies_and_resolves_through_the_hooks(
    monkeypatch, capsys, origin, land_ws, actor_checkout, fake_github  # noqa: F811
):
    fake_github.run_input = upkeep_input(QODO, SONAR)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    monkeypatch.setattr(land, "active_attempts", REAL_ACTIVE_ATTEMPTS)
    ref, _sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t8: add a")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    landed = result(records)["landed_commit"]
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == landed
    by = steps(records)
    assert by["reply"]["outcome"] == "ok" and by["reply"]["replies_posted"] == 1
    assert by["reply"]["pr_comment"] == "posted"
    assert by["resolve"]["outcome"] == "ok" and by["resolve"]["resolved"] == 1
    assert by["reset"]["outcome"] == "ok"
    body = fake_github.threads[0]["comments"][-1]["body"]
    assert landed in body and QODO["id"] in body and land_reply.SIGNATURE in body
    assert SONAR["id"] in fake_github.issue_comments[0]["body"]
    per_finding = [r for r in records if r.get("step") == "reply" and r.get("finding")]
    assert per_finding and all(r["work_item"] == WORK_ITEM for r in per_finding)
    assert all(r["land_run_id"] == LAND_RUN for r in per_finding)

    # h27: the re-run skips the push AND posts nothing new.
    fake_github.requests.clear()
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED, records
    assert steps(records)["push"]["outcome"] == "skipped"
    assert fake_github.posts() == []
    assert len(fake_github.issue_comments) == 1
    signed = [c for c in fake_github.threads[0]["comments"] if land_reply.SIGNATURE in c["body"]]
    assert len(signed) == 1


def test_a_missing_reply_token_stops_the_landing_after_the_push(
    monkeypatch, capsys, origin, land_ws, actor_checkout, fake_github  # noqa: F811
):
    """Pushed but not replied is a partial landing the ledger shows (c40):
    the push stands, the reply step refuses by credential name, the run
    ends `environment`, and nothing was posted."""
    fake_github.run_input = upkeep_input(QODO)
    fake_github.add_thread("T_qodo", f"finding {QODO['id']}")
    monkeypatch.delenv("GITHUB_TOKEN_LAND_PR")
    ref, sha = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t8: add a")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ENVIRONMENT, records
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == sha
    by = steps(records)
    assert by["push"]["outcome"] == "ok"
    assert by["reply"]["outcome"] == "refused"
    assert by["reply"]["credential"] == "GITHUB_TOKEN_LAND_PR"
    assert "resolve" not in by and "reset" not in by
    assert result(records)["outcome"] == "environment"
    assert "GITHUB_TOKEN_LAND_PR" in result(records)["error"]
    assert fake_github.posts() == []


def test_workflow_grants_the_reply_credential_and_the_sibling_source():
    text = (EXAMPLE_DIR / "workflow.yaml").read_text(encoding="utf-8")
    yaml = pytest.importorskip("yaml")
    doc = yaml.safe_load(text)
    op = doc["spec"]["nodes"]["land"]["operation"]
    for ref in ("GITHUB_TOKEN_LAND_PR", "LAND_REPLY_SOURCE_URL", "LAND_REPLY_SOURCE_SHA256"):
        assert ref in op["environmentRefs"], ref
        assert ref in text.split("apiVersion:")[0], f"{ref} not named in the deployment prose"
    argv = " ".join(op["argv"])
    assert "LAND_REPLY_SOURCE_URL" in argv and "LAND_REPLY_SOURCE_SHA256" in argv
    assert "land_reply.py" in argv
