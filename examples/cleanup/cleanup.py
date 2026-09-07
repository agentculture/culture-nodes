#!/usr/bin/env python3
"""examples/cleanup/cleanup.py — the cleanup node (plan loop-closure t13).

A deterministic code node, run through the runner boundary, that closes a
work item's loose ends once its pull request is MERGED (`pr.merged`) or CLOSED
without merge (`pr.closed`). Given the fact payload it:

  1. lists the `review-fix/*` branches and `refs/culture-nodes/*` handover
     refs that belong to the item's runs on the remote;
  2. deletes ONLY the refs whose commits are reachable from the default branch
     (`git merge-base --is-ancestor`) and reports every other ref as
     `declined: unreachable` — never deleting it, because an unreachable ref
     is the only copy of that work;
  3. cancels the item's runs still parked at the merge approval node through
     `POST /v1alpha1/runs/{id}/cancel` WITH a machine-readable reason
     (`pr_merged` | `pr_closed`), so the run.cancelled event says which fact
     ended the run (internal/api/cancelreason.go);
  4. drops per-item sweep state — of which there is none outside the control
     plane's own signal watermark rows, and the record says so;
  5. writes ONE derived-shaped record, as a single JSON line on stdout (the
     runner stores the attempt's stdout as an artifact referenced from its
     observed evidence record), listing every deletion, every declined ref,
     every cancelled run and every failure.

Spec claims c6/c15/c37, honesty h15/h9/h24 (docs/specs/
2026-09-07-loop-closure-claude-codex.md). Operator worktrees are OUT of
scope: they live on a laptop, not a control-plane host.

Which refs are "the item's"? A preserve branch is minted from the run id
(adapters/*/preserve.py mint_branch_name) and a handover ref lives under
`refs/culture-nodes/<run-id>/…` (mint_handover_ref), so the join is the run
id: the item's runs come from `GET /v1alpha1/runs?work_item=KEY` (t1), and a
ref is attributed to the item when one of those ids is a path component of a
handover ref or appears in a review-fix branch name. A ref under either
namespace that matches none of the item's runs is listed as `unattributed`
and left alone — it may be another item's, or a run that predates the
`work_item` column.

Stdlib only (the runtime constraint every example script honours). Exit
codes: 0 every step succeeded (declined refs and skipped runs are successes —
they are the honest answer); 1 the input was refused; 2 an environment step
failed (a fetch, a deletion the remote refused, an API call) — the record is
still written and lists the failure by step.

Environment (all granted by the deployment; see workflow.yaml's block):

    NODES_INPUT_JSON        the pr.merged / pr.closed payload (or {"fact": …}).
    NODES_API_URL           the control plane.
    NODES_API_TOKEN         optional bearer for the API calls.
    NODES_API_COOKIE        optional Cookie header (an Access-fronted listener).
    CLEANUP_REMOTE          the git remote; default https://github.com/<repository>.git
    CLEANUP_DEFAULT_BRANCH  reachability root; default main.
    CLEANUP_PARKED_NODE     the approval node id; default human-merges-pr.
    CLEANUP_RECORD_PATH     optional path the record is also written to.
    GITHUB_TOKEN_WORKER     the push credential (bridge-push.env), read only
                            by a temporary GIT_ASKPASS script; required for an
                            http(s) remote, unused for a local one.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

EXIT_OK = 0
EXIT_REFUSED = 1
EXIT_ENVIRONMENT = 2

REASON_MERGED = "pr_merged"
REASON_CLOSED = "pr_closed"

REVIEW_FIX_PREFIX = "refs/heads/review-fix/"
HANDOVER_PREFIX = "refs/culture-nodes/"
TERMINAL_RUN_STATES = frozenset({"completed", "failed", "cancelled"})
TERMINAL_NODE_STATES = frozenset({"completed", "failed", "cancelled"})

WORKER_PUSH_CREDENTIAL = "GITHUB_TOKEN_WORKER"
#: Reset the configured credential helpers for the push: a configured helper
#: SILENTLY outranks GIT_ASKPASS on push (measured on this fleet; the same fix
#: scripts/combining-loop-node.py and internal/handover/merge.go carry).
RESET_ARGS = ("-c", "credential.helper=", "-c", "credential.https://github.com.helper=")
ASKPASS_SCRIPT = (
    "#!/bin/sh\n"
    'case "$1" in\n'
    "  *Username*) printf '%s\\n' x-access-token ;;\n"
    "  *) printf '%s\\n' \"$" + WORKER_PUSH_CREDENTIAL + '" ;;\n'
    "esac\n"
)
GIT_TIMEOUT_SECONDS = 120
HTTP_TIMEOUT_SECONDS = 30
_UNSAFE_REF_CHARS = re.compile(r"[^A-Za-z0-9._-]+")


class Refusal(Exception):
    def __init__(self, message: str, hint: str, code: int = EXIT_REFUSED):
        super().__init__(message)
        self.hint = hint
        self.code = code


@dataclass
class Record:
    """The one derived-shaped record this node emits."""

    work_item: str
    repository: str
    pull_request: int
    reason: str
    default_branch: str
    runs: list[str] = field(default_factory=list)
    deleted: list[dict[str, Any]] = field(default_factory=list)
    declined: list[dict[str, Any]] = field(default_factory=list)
    unattributed: list[str] = field(default_factory=list)
    cancelled_runs: list[dict[str, Any]] = field(default_factory=list)
    skipped_runs: list[dict[str, Any]] = field(default_factory=list)
    left_running: list[dict[str, Any]] = field(default_factory=list)
    sweep_state: dict[str, Any] = field(default_factory=dict)
    failures: list[dict[str, Any]] = field(default_factory=list)

    def fail(self, step: str, detail: str, **extra: Any) -> None:
        self.failures.append({"step": step, "detail": detail, **extra})

    def as_json(self) -> dict[str, Any]:
        return {
            "record": "cleanup",
            "authority_shape": "derived",
            "work_item": self.work_item,
            "repository": self.repository,
            "pull_request": self.pull_request,
            "reason": self.reason,
            "default_branch": self.default_branch,
            "runs": self.runs,
            "deleted": self.deleted,
            "declined": self.declined,
            "unattributed": self.unattributed,
            "cancelled_runs": self.cancelled_runs,
            "skipped_runs": self.skipped_runs,
            "left_running": self.left_running,
            "sweep_state": self.sweep_state,
            "failures": self.failures,
        }


# ---------------------------------------------------------------------------
# input
# ---------------------------------------------------------------------------


def read_fact() -> dict[str, Any]:
    raw = os.environ.get("NODES_INPUT_JSON", "")
    if not raw.strip():
        raise Refusal(
            "NODES_INPUT_JSON is not set",
            "the engine wires the triggering fact through NODES_INPUT_JSON; bind /run/input to it",
            code=EXIT_ENVIRONMENT,
        )
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise Refusal(
            f"NODES_INPUT_JSON is not valid JSON: {exc}",
            "the input is the pr.merged or pr.closed payload",
        ) from exc
    if (
        isinstance(payload, dict)
        and isinstance(payload.get("fact"), dict)
        and "repository" not in payload
    ):
        payload = payload["fact"]
    if not isinstance(payload, dict):
        raise Refusal(
            "NODES_INPUT_JSON is not a JSON object",
            "the input is the pr.merged or pr.closed payload",
        )
    repository = payload.get("repository")
    number = payload.get("number")
    if not isinstance(repository, str) or "/" not in repository:
        raise Refusal(
            "the fact carries no owner/repo `repository`", "pr.merged and pr.closed both carry one"
        )
    if not isinstance(number, int) or number < 1:
        raise Refusal(
            "the fact carries no positive integer `number`",
            "pr.merged and pr.closed both carry one",
        )
    return payload


def reason_for(fact: dict[str, Any]) -> str:
    """pr.merged carries `merged_at`, pr.closed carries `closed_at`; a merged
    PR is never closed-unmerged, so `merged_at` wins when both are present
    (the emitter's own rule, pr_upkeep_emit.closed_pull_event)."""
    if fact.get("merged_at"):
        return REASON_MERGED
    if fact.get("closed_at"):
        return REASON_CLOSED
    raise Refusal(
        "the fact carries neither `merged_at` nor `closed_at`",
        "only a pr.merged (merged_at) or pr.closed (closed_at) fact names a reason to clean up",
    )


def work_item_for(fact: dict[str, Any]) -> str:
    """pr.closed carries `work_item`; pr.merged carries the correlated Jira
    key as `issue_key` (its subject). Both are the work item."""
    key = fact.get("work_item") or fact.get("issue_key")
    if not isinstance(key, str) or not key:
        raise Refusal(
            "the fact names no work item (`work_item` or `issue_key`)",
            "a fact without a key joins no runs",
        )
    return key


# ---------------------------------------------------------------------------
# control plane
# ---------------------------------------------------------------------------


class Api:
    def __init__(self) -> None:
        base = os.environ.get("NODES_API_URL", "").rstrip("/")
        if not base:
            raise Refusal(
                "NODES_API_URL is not set",
                "grant the control plane URL to the operation",
                code=EXIT_ENVIRONMENT,
            )
        self.base = base

    def _request(
        self, method: str, path: str, body: dict[str, Any] | None = None
    ) -> tuple[int, Any]:
        data = json.dumps(body).encode("utf-8") if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        req.add_header("Accept", "application/json")
        req.add_header("User-Agent", "culture-nodes-cleanup/1")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        token = os.environ.get("NODES_API_TOKEN", "")
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        cookie = os.environ.get("NODES_API_COOKIE", "")
        if cookie:
            req.add_header("Cookie", cookie)
        try:
            with urllib.request.urlopen(
                req, timeout=HTTP_TIMEOUT_SECONDS
            ) as resp:  # noqa: S310 - operator-granted base URL
                raw = resp.read()
                return resp.status, (json.loads(raw) if raw else None)
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                decoded = json.loads(raw) if raw else None
            except json.JSONDecodeError:
                decoded = raw.decode("utf-8", "replace")
            return exc.code, decoded

    def list_runs(self, work_item: str) -> list[dict[str, Any]]:
        items: list[dict[str, Any]] = []
        cursor = ""
        for _ in range(100):  # bounded: 100 pages of 500 is more runs than one item can own
            query = {"work_item": work_item, "limit": "500"}
            if cursor:
                query["cursor"] = cursor
            status, body = self._request("GET", "/v1alpha1/runs?" + urllib.parse.urlencode(query))
            if status != 200 or not isinstance(body, dict):
                raise RuntimeError(
                    f"GET /v1alpha1/runs?work_item={work_item}: HTTP {status}: {body!r}"[:500]
                )
            items.extend(r for r in body.get("items", []) if isinstance(r, dict))
            cursor = body.get("next_cursor") or ""
            if not cursor:
                break
        return items

    def run_view(self, run_id: str) -> dict[str, Any]:
        status, body = self._request("GET", f"/v1alpha1/runs/{run_id}")
        if status != 200 or not isinstance(body, dict):
            raise RuntimeError(f"GET /v1alpha1/runs/{run_id}: HTTP {status}: {body!r}"[:500])
        return body

    def cancel(self, run_id: str, reason: str) -> tuple[int, Any]:
        return self._request("POST", f"/v1alpha1/runs/{run_id}/cancel", {"reason": reason})


# ---------------------------------------------------------------------------
# git
# ---------------------------------------------------------------------------


def _git(
    repo: Path, *args: str, env: dict[str, str] | None = None
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        text=True,
        check=False,
        timeout=GIT_TIMEOUT_SECONDS,
        env={**(env or os.environ), "GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never"},
    )


def _sanitize(value: str) -> str:
    return _UNSAFE_REF_CHARS.sub("-", value).strip("-.") or "x"


def remote_url_for(repository: str) -> str:
    return os.environ.get("CLEANUP_REMOTE") or f"https://github.com/{repository}.git"


def is_network_remote(url: str) -> bool:
    return url.startswith(("http://", "https://"))


def list_candidate_refs(repo: Path) -> dict[str, str]:
    proc = _git(
        repo, "ls-remote", "--refs", "origin", REVIEW_FIX_PREFIX + "*", HANDOVER_PREFIX + "*"
    )
    if proc.returncode != 0:
        raise RuntimeError(f"git ls-remote failed: {proc.stderr.strip()}"[:500])
    refs: dict[str, str] = {}
    for line in proc.stdout.splitlines():
        sha, _, ref = line.partition("\t")
        if ref:
            refs[ref] = sha
    return refs


def attribute(refs: dict[str, str], run_ids: list[str]) -> tuple[dict[str, str], list[str]]:
    """Split the namespace listing into the item's refs and everything else."""
    ids = {_sanitize(rid).lower() for rid in run_ids} | {rid.lower() for rid in run_ids}
    ours: dict[str, str] = {}
    others: list[str] = []
    for ref, sha in sorted(refs.items()):
        if ref.startswith(HANDOVER_PREFIX):
            components = {c.lower() for c in ref[len(HANDOVER_PREFIX) :].split("/")}
            mine = bool(components & ids)
        elif ref.startswith(REVIEW_FIX_PREFIX):
            name = ref[len(REVIEW_FIX_PREFIX) :].lower()
            mine = any(rid in name for rid in ids)
        else:
            mine = False
        (ours.__setitem__(ref, sha) if mine else others.append(ref))
    return ours, others


def fetch_for_reachability(repo: Path, default_branch: str, refs: dict[str, str]) -> dict[str, str]:
    """Fetch the default branch and every candidate ref into a private local
    namespace, returning candidate ref -> local ref."""
    local = {ref: f"refs/cleanup/candidates/{i}" for i, ref in enumerate(sorted(refs))}
    specs = [f"+refs/heads/{default_branch}:refs/cleanup/default"]
    specs.extend(f"+{ref}:{local_ref}" for ref, local_ref in local.items())
    proc = _git(repo, "fetch", "--no-tags", "--quiet", "origin", *specs)
    if proc.returncode != 0:
        raise RuntimeError(f"git fetch failed: {proc.stderr.strip()}"[:500])
    return local


def reachable_from_default(repo: Path, local_ref: str) -> bool:
    proc = _git(repo, "merge-base", "--is-ancestor", local_ref, "refs/cleanup/default")
    if proc.returncode not in (0, 1):
        raise RuntimeError(f"git merge-base --is-ancestor {local_ref}: {proc.stderr.strip()}"[:500])
    return proc.returncode == 0


def delete_remote_ref(repo: Path, ref: str, push_env: dict[str, str]) -> str | None:
    """Delete `ref` on origin and re-read the remote to confirm it is gone.
    Returns None on success, else the failure detail."""
    proc = _git(
        repo, *RESET_ARGS, "push", "--porcelain", "--quiet", "origin", "--delete", ref, env=push_env
    )
    if proc.returncode != 0:
        return (proc.stderr.strip() or proc.stdout.strip() or "push --delete rejected")[:500]
    check = _git(repo, "ls-remote", "--refs", "origin", ref)
    if check.returncode != 0:
        return f"post-delete ls-remote failed: {check.stderr.strip()}"[:500]
    if check.stdout.strip():
        return "the remote still lists the ref after an accepted delete"
    return None


# ---------------------------------------------------------------------------
# the node
# ---------------------------------------------------------------------------


def clean_refs(record: Record, run_ids: list[str], remote: str) -> None:
    workdir = Path(tempfile.mkdtemp(prefix="cleanup-node-"))
    try:
        init = _git(workdir, "init", "--quiet")
        if init.returncode != 0:
            raise RuntimeError(f"git init failed: {init.stderr.strip()}")
        add = _git(workdir, "remote", "add", "origin", remote)
        if add.returncode != 0:
            raise RuntimeError(f"git remote add failed: {add.stderr.strip()}")

        listing = list_candidate_refs(workdir)
        ours, record.unattributed = attribute(listing, run_ids)
        if not ours:
            return
        local = fetch_for_reachability(workdir, record.default_branch, ours)

        reachable: dict[str, str] = {}
        for ref, sha in ours.items():
            if reachable_from_default(workdir, local[ref]):
                reachable[ref] = sha
            else:
                record.declined.append({"ref": ref, "commit": sha, "why": "unreachable"})
        if not reachable:
            return

        token = os.environ.get(WORKER_PUSH_CREDENTIAL, "")
        if is_network_remote(remote) and not token:
            for ref, sha in reachable.items():
                record.declined.append({"ref": ref, "commit": sha, "why": "no_push_credential"})
            record.fail(
                "delete",
                f"{WORKER_PUSH_CREDENTIAL} is not granted; no deletion was attempted on {remote}",
            )
            return

        askpass = workdir / "askpass.sh"
        askpass.write_text(ASKPASS_SCRIPT, encoding="utf-8")
        askpass.chmod(0o700)
        push_env = {**os.environ, "GIT_ASKPASS": str(askpass)}
        for ref, sha in reachable.items():
            detail = delete_remote_ref(workdir, ref, push_env)
            if detail is None:
                record.deleted.append({"ref": ref, "commit": sha})
            else:
                record.fail("delete", detail, ref=ref, commit=sha)
    except (RuntimeError, subprocess.TimeoutExpired, OSError) as exc:
        record.fail("refs", str(exc)[:500])
    finally:
        shutil.rmtree(workdir, ignore_errors=True)


def cancel_parked_runs(
    record: Record, api: Api, runs: list[dict[str, Any]], parked_node: str
) -> None:
    for run in runs:
        run_id = str(run.get("id", ""))
        if not run_id:
            continue
        if str(run.get("state", "")) in TERMINAL_RUN_STATES:
            record.skipped_runs.append({"run_id": run_id, "why": "already_terminal"})
            continue
        try:
            view = api.run_view(run_id)
        except (RuntimeError, OSError) as exc:
            record.fail("cancel", str(exc)[:500], run_id=run_id)
            continue
        node_runs = [nr for nr in view.get("node_runs", []) if isinstance(nr, dict)]
        live = [nr for nr in node_runs if str(nr.get("state", "")) not in TERMINAL_NODE_STATES]
        parked = [nr for nr in live if nr.get("node_id") == parked_node]
        if not parked:
            record.left_running.append(
                {"run_id": run_id, "live_nodes": sorted(str(nr.get("node_id")) for nr in live)}
            )
            continue
        try:
            status, body = api.cancel(run_id, record.reason)
        except OSError as exc:
            record.fail("cancel", str(exc)[:500], run_id=run_id)
            continue
        if status == 200:
            record.cancelled_runs.append(
                {"run_id": run_id, "reason": record.reason, "node": parked_node}
            )
        elif status == 409:
            record.skipped_runs.append({"run_id": run_id, "why": "already_terminal"})
        else:
            record.fail(
                "cancel",
                f"POST /v1alpha1/runs/{run_id}/cancel: HTTP {status}: {body!r}"[:500],
                run_id=run_id,
            )


def sweep_state_note() -> dict[str, Any]:
    return {
        "dropped": [],
        "note": (
            "the pr-upkeep sweep keeps no per-item state outside the control plane: its "
            "idempotency is the signal watermark row per source_key "
            "(internal/store/postgres/signal.go) plus the run-input walk, and a watermark "
            "is an immutable fact, not state to drop"
        ),
    }


def emit(record: Record) -> None:
    line = json.dumps(record.as_json(), sort_keys=True)
    path = os.environ.get("CLEANUP_RECORD_PATH", "")
    if path:
        Path(path).write_text(line + "\n", encoding="utf-8")
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def main() -> int:
    try:
        fact = read_fact()
        reason = reason_for(fact)
        record = Record(
            work_item=work_item_for(fact),
            repository=fact["repository"],
            pull_request=fact["number"],
            reason=reason,
            default_branch=os.environ.get("CLEANUP_DEFAULT_BRANCH") or "main",
        )
        api = Api()
    except Refusal as exc:
        sys.stderr.write(f"error: {exc}\nhint: {exc.hint}\n")
        return exc.code

    runs: list[dict[str, Any]] = []
    try:
        runs = api.list_runs(record.work_item)
    except (RuntimeError, OSError) as exc:
        record.fail("list_runs", str(exc)[:500])
    record.runs = [str(r.get("id")) for r in runs if r.get("id")]

    clean_refs(record, record.runs, remote_url_for(record.repository))
    cancel_parked_runs(
        record, api, runs, os.environ.get("CLEANUP_PARKED_NODE") or "human-merges-pr"
    )
    record.sweep_state = sweep_state_note()

    emit(record)
    return EXIT_OK if not record.failures else EXIT_ENVIRONMENT


if __name__ == "__main__":
    sys.exit(main())
