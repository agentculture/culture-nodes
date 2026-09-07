#!/usr/bin/env python3
"""The land node's REPLY and RESOLVE steps (loop-closure task t8; spec
c3/c38/c40, honesty h12/h25/h27; issues #315 and #309 step 2).

A sibling of land.py, reached through its `reply_hook` / `resolve_hook`
bodies. After a successful push the node tells the PR what landed: for each
finding the producing run was dispatched on, a reply on that finding's review
thread naming the landed commit and the finding id, signed as the land node,
and then the thread is resolved. A finding with no review thread (a
SonarCloud issue or a CI check has none) is named in ONE PR comment per
landing, never one comment per finding.

# Where the findings come from

The land input carries the PRODUCING run's id (land.py INPUT_KEYS), and a
triggered run's input IS the event payload the sweep emitted
(pr_upkeep_emit.upkeep_pr_fact): {source, repository, number, head_sha,
findings:[{source, id, pr, ...}], work_item}. So the step reads
GET /v1alpha1/runs/<run_id> on the control plane and takes `input`. A run
whose input has no repository / number / findings (an assigned developer
package, say) is a recorded skip -- there is nothing to reply to, and a node
that guessed a thread would be inventing an addressee.

A finding reaches its thread three ways, most specific first: `thread_id`
(a GraphQL review-thread node id), `comment_id` (a REST review-comment
databaseId, the id pr-reply.sh takes), or -- the sweep's shapes today carry
neither -- the thread whose comments quote the finding id literally
(`pr307-qodo-3`, a SonarCloud key). No match: the finding goes in the PR
comment.

# Idempotent by reading before writing (c40, h27)

Before a reply is posted the thread's comments are read and a land-node
reply naming THIS landed sha is a skip; before a resolve the thread's
isResolved is read and true is a skip; before the PR comment the issue
comments are read and a land-node comment naming this sha is a skip. A
re-run after a kill therefore posts nothing new, and the ledger shows the
skips by name.

# What this never does (c38, h25)

The client has no merge endpoint: its GitHub operations are exactly read
review threads, reply to a review comment, resolveReviewThread, read issue
comments, create an issue comment. The token is GITHUB_TOKEN_LAND_PR
(pull-requests:write and nothing else) from the process environment or
~/.culture-nodes/land-pr.env (LAND_REPLY_ENV_FILE) -- a different token from
the Contents-write push credential on purpose (deploy/prod/README.md, "Two
tokens, two scopes, two files"). Never from the input, never in argv.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

REPLY_CREDENTIAL = "GITHUB_TOKEN_LAND_PR"
DEFAULT_REPLY_ENV_FILE = "~/.culture-nodes/land-pr.env"
DEFAULT_GITHUB_API_URL = "https://api.github.com"
#: The tunnel in front of nodes.culture.dev bans urllib's default UA (1010),
#: and GitHub requires one; land.py's control-plane read wears the same name.
USER_AGENT = "culture-nodes-land/1"
SIGNATURE = "- culture-nodes (land node)"
HTTP_TIMEOUT_SECONDS = 30
THREADS_PAGE = 100

REPLY_TEMPLATE = (
    "Landed in {sha} (`{short}`) for finding `{finding}` "
    "(work item {work_item}, land run {land_run}).\n\n{signature}"
)
COMMENT_TEMPLATE = (
    "Landed in {sha} (`{short}`) for {count} finding(s) with no review thread:\n"
    "{items}\n\n(work item {work_item}, land run {land_run})\n\n{signature}"
)

THREADS_QUERY = """
query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          comments(first: 100) { nodes { databaseId body } }
        }
      }
    }
  }
}
"""
RESOLVE_MUTATION = """
mutation($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}
"""

Refusal = Callable[..., Exception]


def short(sha: str) -> str:
    return sha[:12]


# -- the credential (mirrors land.push_token) --------------------------------


def reply_token() -> str | None:
    """GITHUB_TOKEN_LAND_PR from the process environment, else from
    land-pr.env (lanes/land-secrets.sh writes it for culture-land). Never
    from the input."""
    value = os.environ.get(REPLY_CREDENTIAL)
    if value:
        return value
    path = Path(os.environ.get("LAND_REPLY_ENV_FILE") or DEFAULT_REPLY_ENV_FILE).expanduser()
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return None
    for line in lines:
        if line.strip().startswith(f"{REPLY_CREDENTIAL}="):
            return line.strip().split("=", 1)[1].strip().strip('"').strip("'") or None
    return None


def redact(text: str, token: str | None) -> str:
    return text.replace(token, "<redacted>") if token else text


# -- HTTP ---------------------------------------------------------------------


class HttpFailure(Exception):
    def __init__(self, message: str, hint: str) -> None:
        super().__init__(message)
        self.hint = hint


def http_json(
    url: str, *, method: str = "GET", token: str | None = None, payload: Any = None
) -> Any:
    headers = {"Accept": "application/json", "User-Agent": USER_AGENT}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(
            request, timeout=HTTP_TIMEOUT_SECONDS
        ) as response:  # nosec B310
            raw = response.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        detail = redact(exc.read().decode("utf-8", "replace")[:300], token)
        raise HttpFailure(f"{method} {url} -> {exc.code}: {detail}", "see the response") from exc
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise HttpFailure(f"{method} {url} failed: {redact(str(exc), token)}", "check the host")
    return json.loads(raw) if raw.strip() else None


# -- the producing run: repository, PR number, findings ----------------------


@dataclass(frozen=True)
class Target:
    repository: str
    number: int
    findings: tuple[dict, ...]

    @property
    def owner(self) -> str:
        return self.repository.split("/", 1)[0]

    @property
    def name(self) -> str:
        return self.repository.split("/", 1)[1]


def target_from_input(run_input: Any) -> Target | None:
    """The pr-upkeep.pr payload -> Target, or None when the producing run
    was not dispatched on PR findings."""
    if not isinstance(run_input, dict):
        return None
    repository = run_input.get("repository")
    number = run_input.get("number")
    findings = run_input.get("findings")
    if not isinstance(repository, str) or repository.count("/") != 1:
        return None
    if not isinstance(number, int) or isinstance(number, bool) or number <= 0:
        return None
    if not isinstance(findings, list):
        return None
    kept = tuple(f for f in findings if isinstance(f, dict) and isinstance(f.get("id"), str))
    if not kept:
        return None
    return Target(repository, number, kept)


def producing_run_input(api_url: str, run_id: str) -> Any:
    url = f"{api_url.rstrip('/')}/v1alpha1/runs/{urllib.parse.quote(run_id, safe='')}"
    body = http_json(url)
    if isinstance(body, dict) and isinstance(body.get("run"), dict):
        return body["run"].get("input")
    return body.get("input") if isinstance(body, dict) else None


# -- GitHub: the five operations, and only these ------------------------------


@dataclass
class Thread:
    id: str
    resolved: bool
    comments: list[dict] = field(default_factory=list)

    @property
    def first_comment_id(self) -> int | None:
        for comment in self.comments:
            if isinstance(comment.get("databaseId"), int):
                return comment["databaseId"]
        return None

    def has_comment(self, comment_id: int) -> bool:
        return any(c.get("databaseId") == comment_id for c in self.comments)

    def quotes(self, text: str) -> bool:
        return any(text in (c.get("body") or "") for c in self.comments)


class GitHub:
    def __init__(self, token: str, api_url: str) -> None:
        self.token = token
        self.api_url = api_url.rstrip("/")

    def rest(self, method: str, path: str, payload: Any = None) -> Any:
        return http_json(f"{self.api_url}{path}", method=method, token=self.token, payload=payload)

    def graphql(self, query: str, variables: dict[str, Any]) -> dict[str, Any]:
        body = self.rest("POST", "/graphql", {"query": query, "variables": variables})
        if not isinstance(body, dict) or body.get("errors"):
            raise HttpFailure(f"GraphQL: {json.dumps(body)[:300]}", "see the GraphQL errors")
        return body.get("data") or {}

    def review_threads(self, target: Target) -> list[Thread]:
        threads: list[Thread] = []
        after = None
        while True:
            variables = {
                "owner": target.owner,
                "name": target.name,
                "number": target.number,
                "after": after,
            }
            data = self.graphql(THREADS_QUERY, variables)
            page = ((data.get("repository") or {}).get("pullRequest") or {}).get("reviewThreads")
            if not isinstance(page, dict):
                raise HttpFailure(f"PR #{target.number} has no reviewThreads", "check the PR")
            for node in page.get("nodes") or []:
                comments = list(((node.get("comments") or {}).get("nodes")) or [])
                threads.append(Thread(node["id"], bool(node.get("isResolved")), comments))
            info = page.get("pageInfo") or {}
            if not info.get("hasNextPage") or not info.get("endCursor"):
                return threads
            after = info["endCursor"]

    def reply(self, target: Target, comment_id: int, body: str) -> Any:
        path = f"/repos/{target.repository}/pulls/{target.number}/comments/{comment_id}/replies"
        return self.rest("POST", path, {"body": body})

    def resolve(self, thread_id: str) -> None:
        self.graphql(RESOLVE_MUTATION, {"id": thread_id})

    def issue_comments(self, target: Target) -> list[dict]:
        comments: list[dict] = []
        page = 1
        while True:
            path = f"/repos/{target.repository}/issues/{target.number}/comments"
            path += f"?per_page=100&page={page}"
            batch = self.rest("GET", path) or []
            comments.extend(c for c in batch if isinstance(c, dict))
            if len(batch) < 100:
                return comments
            page += 1

    def comment(self, target: Target, body: str) -> Any:
        path = f"/repos/{target.repository}/issues/{target.number}/comments"
        return self.rest("POST", path, {"body": body})


# -- matching findings to threads ----------------------------------------------


def match_thread(finding: dict, threads: list[Thread]) -> Thread | None:
    thread_id = finding.get("thread_id")
    if isinstance(thread_id, str) and thread_id:
        return next((t for t in threads if t.id == thread_id), None)
    comment_id = finding.get("comment_id")
    if isinstance(comment_id, int) and not isinstance(comment_id, bool):
        return next((t for t in threads if t.has_comment(comment_id)), None)
    finding_id = finding["id"]
    return next((t for t in threads if t.quotes(finding_id)), None)


def is_land_note(body: str, sha: str) -> bool:
    """A land-node post about THIS landing: signed as the land node and
    naming the full landed sha (a note about another landing on the same
    thread does not count)."""
    return SIGNATURE in body and sha in body


# -- the two steps -----------------------------------------------------------------


@dataclass
class Plan:
    target: Target
    github: GitHub
    threads: list[Thread]
    threaded: list[tuple[dict, Thread]]
    unthreaded: list[dict]


def _prepare(ctx: Any, step: str, refusal: Refusal) -> Plan | dict[str, Any]:
    """Everything both steps share: the producing run's findings, the
    credential, the threads. Returns the summary record instead when the
    step has nothing to do (a skip the hook writes as-is)."""
    api_url = os.environ.get("NODES_API_URL")
    if not api_url:
        note = "NODES_API_URL is unset: the producing run's findings cannot be read"
        return {"outcome": "skipped", "reason": "no_control_plane", "note": note}
    run_id = ctx.inputs.run_id
    try:
        run_input = producing_run_input(api_url, run_id)
    except HttpFailure as exc:
        raise refusal(f"could not read producing run {run_id}: {exc}", exc.hint) from exc
    target = target_from_input(run_input)
    if target is None:
        note = f"run {run_id} was not dispatched on PR findings; nothing to reply to"
        return {"outcome": "skipped", "reason": "no_pr_findings_on_producing_run", "note": note}
    token = reply_token()
    if not token:
        hint = "install culture-land's land-pr.env (deploy/prod/lanes/land-secrets.sh)"
        ctx.records.step(step, "refused", credential=REPLY_CREDENTIAL, hint=hint, posted=0)
        raise refusal(f"{REPLY_CREDENTIAL} is not available; nothing was posted", hint)
    github = GitHub(token, os.environ.get("LAND_GITHUB_API_URL") or DEFAULT_GITHUB_API_URL)
    try:
        threads = github.review_threads(target)
    except HttpFailure as exc:
        raise refusal(f"could not read PR #{target.number} threads: {exc}", exc.hint) from exc
    threaded: list[tuple[dict, Thread]] = []
    unthreaded: list[dict] = []
    for finding in target.findings:
        thread = match_thread(finding, threads)
        (threaded.append((finding, thread)) if thread else unthreaded.append(finding))
    return Plan(target, github, threads, threaded, unthreaded)


def _landing_facts(ctx: Any) -> dict[str, str]:
    return {
        "sha": ctx.landed_commit,
        "short": short(ctx.landed_commit),
        "work_item": ctx.inputs.work_item,
        "land_run": ctx.land.run_id or "<unknown>",
        "signature": SIGNATURE,
    }


def reply_step(ctx: Any, *, refusal: Refusal) -> dict[str, Any]:
    """One reply per landed finding with a thread, one PR comment for the
    rest; each action is a `reply` step record, and this returns the summary."""
    plan = _prepare(ctx, "reply", refusal)
    if isinstance(plan, dict):
        return plan
    sha = ctx.landed_commit
    facts = _landing_facts(ctx)
    common = {"repository": plan.target.repository, "pr": plan.target.number, "commit": sha}
    posted = existing = 0
    try:
        for finding, thread in plan.threaded:
            fid = finding["id"]
            if any(is_land_note(c.get("body") or "", sha) for c in thread.comments):
                existing += 1
                ctx.records.step(
                    "reply", "skipped_existing", finding=fid, thread_id=thread.id, **common
                )
                continue
            anchor = thread.first_comment_id
            if anchor is None:
                ctx.records.step(
                    "reply", "skipped", finding=fid, reason="thread_has_no_comment", **common
                )
                continue
            body = REPLY_TEMPLATE.format(finding=fid, **facts)
            made = plan.github.reply(plan.target, anchor, body) or {}
            posted += 1
            url = made.get("html_url")
            ctx.records.step("reply", "posted", finding=fid, thread_id=thread.id, url=url, **common)
        pr_comment = None
        if plan.unthreaded:
            ids = [f["id"] for f in plan.unthreaded]
            marked = [c for c in plan.github.issue_comments(plan.target) if isinstance(c, dict)]
            if any(is_land_note(c.get("body") or "", sha) for c in marked):
                pr_comment = "existing"
                ctx.records.step(
                    "reply", "skipped_existing", kind="pr_comment", findings=ids, **common
                )
            else:
                items = "\n".join(
                    f"- `{i}` ({f.get('source', '?')})" for i, f in zip(ids, plan.unthreaded)
                )
                body = COMMENT_TEMPLATE.format(count=len(ids), items=items, **facts)
                made = plan.github.comment(plan.target, body) or {}
                pr_comment = "posted"
                url = made.get("html_url")
                ctx.records.step(
                    "reply", "posted", kind="pr_comment", findings=ids, url=url, **common
                )
    except HttpFailure as exc:
        raise refusal(f"GitHub reply failed after {posted} post(s): {exc}", exc.hint) from exc
    return {
        "outcome": "ok",
        "replies_posted": posted,
        "replies_existing": existing,
        "pr_comment": pr_comment,
        "findings": len(plan.target.findings),
        **common,
    }


def resolve_step(ctx: Any, *, refusal: Refusal) -> dict[str, Any]:
    """resolveReviewThread for each landed finding's thread not already
    resolved; a finding with no thread is a recorded skip."""
    plan = _prepare(ctx, "resolve", refusal)
    if isinstance(plan, dict):
        return plan
    common = {"repository": plan.target.repository, "pr": plan.target.number}
    resolved = already = 0
    done: set[str] = set()
    try:
        for finding, thread in plan.threaded:
            fid = finding["id"]
            if thread.resolved or thread.id in done:
                already += 1
                reason = "already_resolved"
                ctx.records.step(
                    "resolve", "skipped", finding=fid, thread_id=thread.id, reason=reason, **common
                )
                continue
            plan.github.resolve(thread.id)
            done.add(thread.id)
            resolved += 1
            ctx.records.step("resolve", "resolved", finding=fid, thread_id=thread.id, **common)
    except HttpFailure as exc:
        raise refusal(f"resolving a thread failed after {resolved}: {exc}", exc.hint) from exc
    for finding in plan.unthreaded:
        reason = "no_review_thread"
        ctx.records.step("resolve", "skipped", finding=finding["id"], reason=reason, **common)
    return {
        "outcome": "ok",
        "resolved": resolved,
        "already_resolved": already,
        "no_thread": len(plan.unthreaded),
        **common,
    }
