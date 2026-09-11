#!/usr/bin/env python3
"""examples/pr-upkeep/readiness.py — the merge-gate readiness collector.

Plan loop-closure-claude-codex task t18 (spec claim c10, honesty condition
h19, decision c14). A deterministic code node, run through the runner
boundary, that assembles ONE block a platform maintainer reads before
deciding the merge on `human-merges-pr`:

    ci        every CI check on the PR's head commit as {name, state}
    sonar     the SonarCloud quality gate, open-issue count and hotspot count
    threads   the PR's unresolved / total review-thread tally
    devague   the proposed devague records in the checkout under `.devague`
    evidence  the devague evidence records whose outcome is not `pass`

# Why this is a node and not a presentation field

`presentation` metadata is lifted out of the executable spec by the compiler
and never reaches a run, and `internal/engine/humantask.go` writes a task's
`context_refs` as POINTERS -- the node's input binding exactly as authored,
never a resolved payload. So "the approval presents readiness" cannot be a
field on the approval. It is this program's output, bound by the approval as
`readiness: /nodes/readiness/output`, with the approval reachable from this
node and nowhere else -- which is what makes "the task is not created until
the collector completed" a property of the EDGES rather than a habit.

# What it refuses to fabricate

An unreadable source is `null` plus an entry in `failures` naming it, and the
program still exits 0. That is the merge gate's doctrine applied one node
earlier (internal/worker/code.go's `measurement_incomplete`): folding "we
could not ask SonarCloud" into `open_issues: 0` is the false green this block
exists to prevent, and folding it into a failure would strand a merge
decision that is a human's to make (PRD 10.4). Exit is nonzero only when
there is no block to write at all -- a refused input.

**An unreadable source is not only a connection that failed.** A source that
answers HTTP 200 with a body missing the field the question was about is
equally unread, and it is the more dangerous half: a transport error raises,
while `body.get("total", 0)` returns a number that looks measured. GitHub
GraphQL in particular answers a partial failure with `data: null` and a 200,
which a chain of `or {}` reads as a PR with zero unresolved threads. So every
field this program derives is REQUIRED of the response
(`_require_object` / `_require_list` / `_require_count`), and a body that
lacks it raises `MalformedResponse` into the same `failures` entry a refused
connection produces.

# The shapes it reuses

The Sonar and thread halves are the same three SonarCloud queries and the
same GraphQL `reviewThreads`/`isResolved` read that
`.claude/skills/cicd/scripts/pr-status.sh` performs, so the number in this
block and the number an operator sees from the skill are the same number. The
CI half merges check runs AND commit statuses because `gh pr checks` -- which
the shell script shells out to -- reports both, and a collector reading only
check-runs would call a PR green whose SonarCloud commit status is red.

It calls those HTTP APIs DIRECTLY. `gh` and `devex` are operator tools on an
operator's PATH; this runs in a runner image, so there is no shell to reach
for and the program spawns no subprocess at all.

Stdlib only (the runtime constraint every example script honours). Exit
codes: 0 the block was written (an unread source is a `failures` entry, not a
nonzero exit); 1 the input was refused; 2 a granted environment value the
program cannot run without is missing.

Environment (all granted by the deployment; see workflow.yaml's block):

    NODES_INPUT_JSON                    the pr-upkeep.pr payload the run
                                        started from (or {"pr": ...}).
    PR_UPKEEP_READINESS_GITHUB_API      GitHub REST base; empty means
                                        https://api.github.com. The GraphQL
                                        endpoint is `<base>/graphql`.
    PR_UPKEEP_READINESS_SONAR_API       SonarCloud base; empty means
                                        https://sonarcloud.io
    PR_UPKEEP_READINESS_SONAR_COMPONENT the Sonar project key; empty means the
                                        `<owner>_<repo>` convention
                                        pr-status.sh derives.
    PR_UPKEEP_READINESS_DEVAGUE_ROOT    the checkout's `.devague` directory;
                                        empty means `.devague` under the
                                        working directory.
    PR_UPKEEP_READINESS_DEVAGUE_SLUG    optional: read only the frame/plan/
                                        delivery documents with this slug.
                                        Empty reads every document, and the
                                        block says which scope it used.
    GITHUB_TOKEN                        the read credential. The REST reads
                                        degrade to anonymous without it; the
                                        GraphQL thread read does not work at
                                        all without it, and says so.
    SONAR_TOKEN                         optional; SonarCloud takes it as the
                                        BASIC username with an empty password,
                                        never as a bearer (sweep.py records
                                        why that distinction bites).
    PR_UPKEEP_READINESS_RECORD_PATH     optional: a second path the block is
                                        written to, beside stdout. Not granted
                                        by the graph -- it exists for an
                                        operator running this program by hand.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.parse
import urllib.request
from base64 import b64encode
from pathlib import Path
from typing import Any

EXIT_OK = 0
EXIT_REFUSED = 1
EXIT_ENVIRONMENT = 2

DEFAULT_GITHUB_API = "https://api.github.com"
DEFAULT_SONAR_API = "https://sonarcloud.io"
HTTP_TIMEOUT_SECONDS = 30

#: `gh pr checks`'s own state vocabulary, which pr-status.sh's awk block
#: renders. Keeping it means the block and the skill say the same word about
#: the same check.
STATE_PASS = "pass"
STATE_FAIL = "fail"
STATE_PENDING = "pending"
STATE_SKIPPING = "skipping"
STATE_UNKNOWN = "unknown"

#: check-run conclusions -> the vocabulary above. A conclusion this table does
#: not name reads `unknown`, never `pass`.
_CHECK_CONCLUSIONS = {
    "success": STATE_PASS,
    "failure": STATE_FAIL,
    "timed_out": STATE_FAIL,
    "cancelled": STATE_FAIL,
    "action_required": STATE_FAIL,
    "startup_failure": STATE_FAIL,
    "stale": STATE_FAIL,
    "neutral": STATE_SKIPPING,
    "skipped": STATE_SKIPPING,
}

#: commit-status states -> the same vocabulary.
_STATUS_STATES = {
    "success": STATE_PASS,
    "failure": STATE_FAIL,
    "error": STATE_FAIL,
    "pending": STATE_PENDING,
}

#: The devague collections whose entries carry a `status`, and are therefore
#: what a bulk confirm would act on (decision c14). Honesty conditions are
#: nested inside claims and are handled beside them.
_STATUS_COLLECTIONS = ("claims", "tasks", "evidence", "deviations", "deltas", "obligations")

#: How many pages of review threads to walk before saying so. 100 threads a
#: page; ten pages is more threads than any PR this loop works has carried.
_MAX_THREAD_PAGES = 10

THREADS_QUERY = """
query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { id isResolved }
      }
    }
  }
}
"""


class Refusal(Exception):
    def __init__(self, message: str, hint: str, code: int = EXIT_REFUSED):
        super().__init__(message)
        self.hint = hint
        self.code = code


class MalformedResponse(RuntimeError):
    """A source answered, and what it said is not the shape its API promises.

    Raised so `attempt()` records the source as unread -- a `null` field and a
    named `failures` entry -- rather than letting a `.get(field, default)`
    turn an error-shaped 200 into a clean measurement. A body with no `total`
    is a source that did not answer the question; reporting it as `0` open
    issues is the false green this whole block exists to prevent.

    `pr-status.sh` does coerce (`jq '.total // 0'`), and that is the one place
    the reuse stops: the shell script prints a line for an operator who can
    see the whole terminal, while this block is the entire fact a merge
    decision turns on. The queries are the same; the fallbacks are not.
    """


def _require_object(payload: Any, source: str) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise MalformedResponse(f"{source}: expected a JSON object, got {type(payload).__name__}")
    return payload


def _require_list(payload: Any, field: str, source: str) -> list[Any]:
    """A required collection, absent-or-mistyped rather than empty.

    `[]` and "the key is missing" are different facts: the first is a commit
    with no check runs, the second is a body that is not a check-runs
    response at all.
    """
    body = _require_object(payload, source)
    if field not in body:
        raise MalformedResponse(f"{source}: response carries no {field!r}")
    value = body[field]
    if not isinstance(value, list):
        raise MalformedResponse(f"{source}: {field!r} is {type(value).__name__}, not a list")
    return value


def _require_count(value: Any, field: str, source: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise MalformedResponse(
            f"{source}: {field} is {type(value).__name__}, not an integer count"
        )
    return value


# ---------------------------------------------------------------------------
# input
# ---------------------------------------------------------------------------


def read_fact() -> dict[str, Any]:
    raw = os.environ.get("NODES_INPUT_JSON", "")
    if not raw.strip():
        raise Refusal(
            "NODES_INPUT_JSON is not set",
            "the engine wires the node's resolved input through NODES_INPUT_JSON; "
            "bind the run input to it",
            code=EXIT_ENVIRONMENT,
        )
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise Refusal(
            f"NODES_INPUT_JSON is not valid JSON: {exc}",
            "the input is the pr-upkeep.pr payload the run started from",
        ) from exc
    if isinstance(payload, dict) and "repository" not in payload:
        for key in ("pr", "fact", "finding"):
            nested = payload.get(key)
            if isinstance(nested, dict) and "repository" in nested:
                payload = nested
                break
    if not isinstance(payload, dict):
        raise Refusal(
            "NODES_INPUT_JSON is not a JSON object",
            "the input is the pr-upkeep.pr payload the run started from",
        )
    repository = payload.get("repository")
    number = payload.get("number")
    if not isinstance(repository, str) or "/" not in repository:
        raise Refusal(
            "the input carries no owner/repo `repository`",
            "a pr-upkeep.pr fact carries one; bind the run input, not one of its fields",
        )
    if not isinstance(number, int) or number < 1:
        raise Refusal(
            "the input carries no positive integer `number`",
            "a pr-upkeep.pr fact carries the pull request number",
        )
    return payload


# ---------------------------------------------------------------------------
# http
# ---------------------------------------------------------------------------


def _get_json(url: str, *, token: str | None = None, basic: tuple[str, str] | None = None) -> Any:
    request = urllib.request.Request(url)  # noqa: S310 - operator-granted base URL
    request.add_header("Accept", "application/json")
    request.add_header("User-Agent", "culture-nodes-readiness/1")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    if basic:
        encoded = b64encode(f"{basic[0]}:{basic[1]}".encode()).decode("ascii")
        request.add_header("Authorization", f"Basic {encoded}")
    with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT_SECONDS) as response:  # noqa: S310
        return json.load(response)


def _post_json(url: str, body: dict[str, Any], *, token: str | None = None) -> Any:
    data = json.dumps(body).encode("utf-8")
    request = urllib.request.Request(url, data=data, method="POST")  # noqa: S310
    request.add_header("Accept", "application/json")
    request.add_header("Content-Type", "application/json")
    request.add_header("User-Agent", "culture-nodes-readiness/1")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT_SECONDS) as response:  # noqa: S310
        return json.load(response)


def _github_api() -> str:
    return (os.environ.get("PR_UPKEEP_READINESS_GITHUB_API") or DEFAULT_GITHUB_API).rstrip("/")


def _sonar_api() -> str:
    return (os.environ.get("PR_UPKEEP_READINESS_SONAR_API") or DEFAULT_SONAR_API).rstrip("/")


def _github_token() -> str | None:
    return os.environ.get("GITHUB_TOKEN") or None


# ---------------------------------------------------------------------------
# ci
# ---------------------------------------------------------------------------


def check_state(check: dict[str, Any]) -> str:
    """One check run's state in `gh pr checks`'s vocabulary.

    A check that has not completed is `pending` -- it is not yet a pass and it
    is not yet a failure, which is the distinction a merge decision turns on.
    """
    if check.get("status") != "completed":
        return STATE_PENDING
    conclusion = (check.get("conclusion") or "").lower()
    return _CHECK_CONCLUSIONS.get(conclusion, STATE_UNKNOWN)


def status_state(status: dict[str, Any]) -> str:
    return _STATUS_STATES.get((status.get("state") or "").lower(), STATE_UNKNOWN)


def collect_ci(repository: str, head_sha: str) -> list[dict[str, str]]:
    """Every CI signal on the head commit, from BOTH surfaces `gh pr checks`
    merges: the check-runs API and the combined commit-status API. SonarCloud
    and Cloudflare report through the latter on this repository, so a
    collector reading only check runs would leave them out of the block a
    person merges on.

    Both collections are REQUIRED, not defaulted: a body with no `check_runs`
    key is an error-shaped response, and reading it as an empty list would
    report a commit with no CI as a commit whose CI is fine."""
    api, token = _github_api(), _github_token()
    checks = _get_json(
        f"{api}/repos/{repository}/commits/{urllib.parse.quote(head_sha)}/check-runs?per_page=100",
        token=token,
    )
    combined = _get_json(
        f"{api}/repos/{repository}/commits/{urllib.parse.quote(head_sha)}/status?per_page=100",
        token=token,
    )
    entries = [
        {
            "name": _require_object(check, "github check-runs").get("name") or "",
            "state": check_state(check),
        }
        for check in _require_list(checks, "check_runs", "github check-runs")
    ]
    entries += [
        {
            "name": _require_object(status, "github commit-status").get("context") or "",
            "state": status_state(status),
        }
        for status in _require_list(combined, "statuses", "github commit-status")
    ]
    return entries


# ---------------------------------------------------------------------------
# sonar
# ---------------------------------------------------------------------------


def sonar_component(repository: str) -> str:
    """pr-status.sh's precedence, minus the flag it has and a node does not:
    the granted component, else the `<owner>_<repo>` SonarCloud convention."""
    granted = os.environ.get("PR_UPKEEP_READINESS_SONAR_COMPONENT")
    if granted:
        return granted
    owner, _, name = repository.partition("/")
    return f"{owner}_{name}"


def _sonar_total(payload: Any, source: str) -> int:
    """A SonarCloud search total, from whichever of the two places that API
    puts it: top level on `issues/search`, under `paging` on
    `hotspots/search`. Neither present is a response that did not answer --
    `MalformedResponse`, never `0`."""
    body = _require_object(payload, source)
    if "total" in body:
        return _require_count(body["total"], "`total`", source)
    paging = body.get("paging")
    if isinstance(paging, dict) and "total" in paging:
        return _require_count(paging["total"], "`paging.total`", source)
    raise MalformedResponse(f"{source}: response carries neither `total` nor `paging.total`")


def collect_sonar(repository: str, number: int) -> dict[str, Any]:
    """The same three queries pr-status.sh issues, with the same filters:
    the PR's quality gate, its OPEN/CONFIRMED issue total, and its TO_REVIEW
    hotspot total.

    Where it deliberately parts company with the shell script: pr-status.sh
    falls back to `UNKNOWN` and `0` when a field is missing, and this raises.
    An operator reading a terminal can see the rest of the screen; a merge
    gate reading `open_issues: 0` cannot tell that from an answered query."""
    api = _sonar_api()
    component = sonar_component(repository)
    token = os.environ.get("SONAR_TOKEN") or None
    basic = (token, "") if token else None

    gate = _get_json(
        f"{api}/api/qualitygates/project_status?"
        + urllib.parse.urlencode({"projectKey": component, "pullRequest": number}),
        basic=basic,
    )
    issues = _get_json(
        f"{api}/api/issues/search?"
        + urllib.parse.urlencode(
            {
                "componentKeys": component,
                "pullRequest": number,
                "statuses": "OPEN,CONFIRMED",
                "ps": 1,
            }
        ),
        basic=basic,
    )
    hotspots = _get_json(
        f"{api}/api/hotspots/search?"
        + urllib.parse.urlencode(
            {"projectKey": component, "pullRequest": number, "status": "TO_REVIEW", "ps": 1}
        ),
        basic=basic,
    )
    project_status = _require_object(
        _require_object(gate, "sonar qualitygates/project_status").get("projectStatus"),
        "sonar qualitygates/project_status: `projectStatus`",
    )
    status = project_status.get("status")
    if not isinstance(status, str) or not status:
        raise MalformedResponse(
            "sonar qualitygates/project_status: `projectStatus.status` is not a gate status"
        )
    return {
        "gate": status,
        "open_issues": _sonar_total(issues, "sonar issues/search"),
        "hotspots": _sonar_total(hotspots, "sonar hotspots/search"),
    }


# ---------------------------------------------------------------------------
# review threads
# ---------------------------------------------------------------------------


def collect_threads(repository: str, number: int) -> dict[str, Any]:
    """The resolved-vs-unresolved tally, read exactly the way pr-status.sh
    reads it: `reviewThreads` and each thread's `isResolved`."""
    owner, _, name = repository.partition("/")
    url = f"{_github_api()}/graphql"
    token = _github_token()
    source = "github graphql reviewThreads"
    total = unresolved = 0
    cursor: str | None = None
    truncated = False
    for page in range(_MAX_THREAD_PAGES):
        body = {
            "query": THREADS_QUERY,
            "variables": {"owner": owner, "name": name, "number": number, "after": cursor},
        }
        payload = _require_object(_post_json(url, body, token=token), source)
        if payload.get("errors"):
            raise RuntimeError(f"graphql reviewThreads: {payload['errors']!r}"[:400])
        # Walked with a required step at every level. GraphQL answers a
        # partial failure with `data: null` and HTTP 200, and a chain of
        # `or {}` reads that as a PR with no review threads -- an unresolved
        # thread count of zero, which is the shape of a clean PR.
        threads: dict[str, Any] = payload
        for step in ("data", "repository", "pullRequest", "reviewThreads"):
            threads = _require_object(threads.get(step), f"{source}: `{step}`")
        nodes = _require_list(threads, "nodes", source)
        for node in nodes:
            resolved = _require_object(node, f"{source}: a thread").get("isResolved")
            if not isinstance(resolved, bool):
                raise MalformedResponse(f"{source}: a thread carries no boolean `isResolved`")
            total += 1
            unresolved += 0 if resolved else 1
        info = _require_object(threads.get("pageInfo"), f"{source}: `pageInfo`")
        if not info.get("hasNextPage"):
            break
        cursor = info.get("endCursor")
        if not isinstance(cursor, str) or not cursor:
            # Another page exists and the response did not say where it
            # starts. Re-sending `after: null` would re-read page one and
            # count it twice, which is a tally, not the tally.
            raise MalformedResponse(f"{source}: `hasNextPage` with no `endCursor` to follow")
        truncated = page == _MAX_THREAD_PAGES - 1
    tally: dict[str, Any] = {"unresolved": unresolved, "total": total}
    if truncated:
        # Said out loud rather than silently reported as the whole tally: a
        # count that stopped early is not the count.
        tally["truncated"] = True
    return tally


# ---------------------------------------------------------------------------
# devague
# ---------------------------------------------------------------------------


def devague_root() -> Path:
    return Path(os.environ.get("PR_UPKEEP_READINESS_DEVAGUE_ROOT") or ".devague")


def _document_slug(path: Path, document: dict[str, Any]) -> str:
    return (
        document.get("slug") or document.get("plan_slug") or document.get("frame_slug") or path.stem
    )


def _record_id(slug: str, collection: str, record: dict[str, Any]) -> str:
    """A devague id is unique inside its document, not across the tree (`c1`
    exists in every frame), so the block qualifies it. A reader can take this
    string back to the file it came from."""
    return f"{slug}/{collection}/{record.get('id', '?')}"


def collect_devague(root: Path, slug_filter: str) -> dict[str, Any]:
    """Every proposed record in the checkout's devague documents, and every
    evidence record whose outcome is not a pass.

    A failing evidence record is reported whatever its status: a CONFIRMED
    failing measurement is the sharpest signal there is before a merge --
    somebody already agreed the thing does not work.
    """
    if not root.is_dir():
        raise RuntimeError(f"no devague directory at {root}")
    proposed: list[str] = []
    failing: list[str] = []
    scope: list[str] = []
    for path in sorted(root.rglob("*.json")):
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            continue
        if not isinstance(document, dict):
            continue
        slug = _document_slug(path, document)
        if slug_filter and slug != slug_filter:
            continue
        if slug not in scope:
            scope.append(slug)
        for collection in _STATUS_COLLECTIONS:
            for record in document.get(collection) or []:
                if not isinstance(record, dict):
                    continue
                if record.get("status") == "proposed":
                    proposed.append(_record_id(slug, collection, record))
                if collection == "evidence" and record.get("outcome") not in (None, "pass"):
                    failing.append(_record_id(slug, collection, record))
                for condition in record.get("honesty_conditions") or []:
                    if isinstance(condition, dict) and condition.get("status") == "proposed":
                        proposed.append(_record_id(slug, "honesty_conditions", condition))
    return {
        "devague": {"proposed": sorted(proposed), "scope": sorted(scope)},
        "evidence": {"failing": sorted(failing)},
    }


# ---------------------------------------------------------------------------
# the block
# ---------------------------------------------------------------------------


def collect(fact: dict[str, Any]) -> dict[str, Any]:
    repository = fact["repository"]
    number = fact["number"]
    head_sha = fact.get("head_sha") or ""
    failures: list[dict[str, str]] = []

    def attempt(source: str, call):
        try:
            return call()
        except (OSError, RuntimeError, ValueError, KeyError, TypeError, AttributeError) as exc:
            # urllib.error.URLError (and HTTPError under it) is an OSError.
            # MalformedResponse is the RuntimeError the collectors raise when
            # a source answered 200 with a body that is not the shape its API
            # promises. The rest are the belt to that braces. Either way a
            # source that answered nonsense is an unread source, which is a
            # `null` field and a named failure -- never a zero.
            failures.append({"source": source, "detail": f"{type(exc).__name__}: {exc}"[:400]})
            return None

    if head_sha:
        ci = attempt("github-checks", lambda: collect_ci(repository, head_sha))
    else:
        ci = None
        failures.append(
            {"source": "github-checks", "detail": "the input carries no head_sha to read checks at"}
        )
    threads = attempt("github-threads", lambda: collect_threads(repository, number))
    sonar = attempt("sonar", lambda: collect_sonar(repository, number))
    records = attempt(
        "devague",
        lambda: collect_devague(
            devague_root(), os.environ.get("PR_UPKEEP_READINESS_DEVAGUE_SLUG") or ""
        ),
    )

    return {
        "record": "readiness",
        "authority_shape": "derived",
        "repository": repository,
        "pull_request": number,
        "head_sha": head_sha,
        "work_item": fact.get("work_item") or "",
        "ci": ci,
        "sonar": sonar,
        "threads": threads,
        "devague": records["devague"] if records else None,
        "evidence": records["evidence"] if records else None,
        "failures": failures,
    }


def main() -> int:
    try:
        fact = read_fact()
    except Refusal as refusal:
        print(f"error: {refusal}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code
    block = collect(fact)
    print(json.dumps(block, sort_keys=True))
    record_path = os.environ.get("PR_UPKEEP_READINESS_RECORD_PATH")
    if record_path:
        Path(record_path).write_text(json.dumps(block, sort_keys=True) + "\n", encoding="utf-8")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
