"""Bridge-side preserve-on-failure (task t25, issue #49): a failed node's
working-tree changes must not evaporate — but the bridge's `colleague`
subprocess runs in the operator's own live, shared checkout (see
`workspace.py`'s own docstring: no worktree/branch/commit/stash exists
anywhere in that module, by design), the very checkout a concurrently
running warm session (t5/t6's `continuation_ref` resume) may still be
parked in mid-turn. So preservation can never be a porcelain `git commit`:
that moves HEAD, rewrites the real index, and (via pre-commit/commit-msg
hooks a porcelain commit would run and plumbing never does) could mutate
files underneath a concurrent session. It is built entirely out of git
plumbing, and every index-touching step below runs against a private
SCRATCH index file (`GIT_INDEX_FILE`), never `.git/index`:

    git read-tree <head>      # scratch index := the tree at *head*
    git add -A                # scratch index += the working tree's diff
    git write-tree            # scratch index -> a tree object
    git commit-tree -p <head> -m "<failure reason + identifiers>"
    git update-ref refs/heads/<branch> <commit>

None of the five commands is `checkout`, `reset`, `stash`, or a plain
`commit` — HEAD is only ever read (as *head*, `workspace_measured`'s own
`head_after`), the real index is only ever read by `git add -A` scanning
the working tree (it stages into the SCRATCH index, never the real one),
and the working tree itself is only ever read, never written. `update-ref`
writes exactly one ref: the new preserve branch's own — never HEAD, never
an existing branch. This is what task t25's acceptance criterion means by
"byte-identical": `git status`, `HEAD`, and `.git/index` before and after a
preserve commit must be indistinguishable, because nothing here ever wrote
to any of the three.

Push is best-effort (task t25's honestly-recorded risk: bridge-host push
credentials for thor/orin are unverified — see the plan's risk register). A
missing or rejected credential, an unreachable remote, or push disabled by
configuration are all ordinary `local-only` outcomes here, never raised as
an error — the local plumbing commit already exists in the repo's object
database and on the minted branch ref either way, so nothing is lost. The
push subprocess disables any interactive credential prompt
(`GIT_TERMINAL_PROMPT=0`) so a missing credential fails fast and honestly
instead of hanging until `PUSH_TIMEOUT_SECONDS`.

Mirrored field-for-field by `claude_code_bridge.preserve` and
`codex_bridge.preserve` (all-backends rule): only the docstring's named
subprocess (`colleague` here) differs.

This module is never called on a domain outcome (PRD: domain outcome ≠
technical status) — only on a genuine technical failure (execution,
timeout, capacity_exhausted). `server.py`/`async_runner.py` call
`preserve_on_failure` after `mapping.sync_response`/`terminal_event` have
already classified the result, gated on the failure status THEY produced,
never on this module's own judgement of what colleague reported.
"""

from __future__ import annotations

import os
import re
import secrets
import subprocess
import tempfile
import time
from dataclasses import dataclass
from typing import Any

#: Bounds any single local (non-network) git plumbing subprocess. Same
#: rationale as `workspace.py`'s own `GIT_TIMEOUT_SECONDS`: local git talking
#: to a local repo is normally instant, but a bound still exists so a wedged
#: filesystem cannot hang the bridge forever preserving a workspace nobody is
#: waiting on synchronously.
GIT_TIMEOUT_SECONDS = 10.0

#: Bounds the one NETWORK step (`git push`) separately and more generously —
#: a slow-but-working remote should not be treated the same as a wedged local
#: filesystem, but this must still be bounded: an unresponsive remote must
#: never hang the bridge's failure-reporting path indefinitely.
PUSH_TIMEOUT_SECONDS = 20.0

#: Characters a minted branch name keeps verbatim; everything else collapses
#: to "-" so an arbitrary run/node-run/attempt id (expected to be a UUID or
#: similar in practice, but never trusted to be ref-safe) can never produce a
#: git ref git itself would refuse.
_UNSAFE_REF_CHARS = re.compile(r"[^A-Za-z0-9._-]+")


def _sanitize_ref_component(value: str) -> str:
    cleaned = _UNSAFE_REF_CHARS.sub("-", value).strip("-.")
    return cleaned or "x"


def mint_branch_name(
    prefix: str, run_id: str, node_run_id: str | None, attempt_id: str | None
) -> str:
    """A unique, code-minted preserve branch name (task t26 persists this on
    the attempt row; this module only mints it and creates the ref). The
    identifying part is the run/node-run/attempt id; the trailing
    timestamp+random suffix exist only so a SECOND preserve attempt for the
    same attempt id (e.g. a retried dispatch) never collides on one ref —
    they are never a substitute for the identifying part."""
    parts = [p for p in (run_id, node_run_id, attempt_id) if p]
    ident = "-".join(_sanitize_ref_component(p) for p in parts) or "unknown"
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    suffix = secrets.token_hex(3)
    return f"{prefix}{ident}-{stamp}-{suffix}"


def _run_git(
    repo: str,
    *args: str,
    env: dict[str, str] | None = None,
    timeout: float = GIT_TIMEOUT_SECONDS,
) -> subprocess.CompletedProcess[str] | None:
    """Run one `git` subprocess in *repo*. `None` on ANY failure to run it at
    all (git missing, timeout, OS error) — never raised: a preserve step
    that cannot even run must degrade to an honest recorded outcome, never
    crash the bridge's own failure-reporting path."""
    try:
        return subprocess.run(  # noqa: S603 # nosec B603,B607
            ["git", *args],
            cwd=repo,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=env,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None


@dataclass(frozen=True)
class PreserveResult:
    """What one `preserve_on_failure` call decided and did. Every field is
    the bridge's own direct observation — never a guess: `attempted=False`
    always carries a `reason` explaining exactly why nothing was tried;
    `committed=True` always carries a `branch` and `commit`; `pushed` is
    only ever True when a `git push` subprocess actually exited zero."""

    attempted: bool
    committed: bool
    pushed: bool
    local_only: bool
    branch: str | None = None
    commit: str | None = None
    remote: str | None = None
    reason: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "attempted": self.attempted,
            "committed": self.committed,
            "branch": self.branch,
            "commit": self.commit,
            "pushed": self.pushed,
            "local_only": self.local_only,
            "remote": self.remote,
            "reason": self.reason,
        }


def _skip(reason: str) -> PreserveResult:
    return PreserveResult(
        attempted=False, committed=False, pushed=False, local_only=False, reason=reason
    )


def _has_changes(measured: dict[str, Any]) -> bool:
    if measured.get("status_porcelain"):
        return True
    if measured.get("changed_files"):
        return True
    head_before = measured.get("head_before")
    head_after = measured.get("head_after")
    return bool(head_before) and bool(head_after) and head_before != head_after


def preserve_on_failure(
    repo: str,
    measured: dict[str, Any],
    *,
    enabled: bool,
    push: bool,
    remote: str,
    branch_prefix: str,
    run_id: str,
    node_run_id: str | None,
    attempt_id: str | None,
    reason: str,
) -> PreserveResult:
    """Preserve *repo*'s current working-tree state on a freshly minted
    branch, entirely via git plumbing against a scratch index (see module
    docstring). Called ONLY after a node has already been classified as a
    genuine technical failure — *reason* is that failure's own message, and
    becomes the preserve commit's message so the branch is self-explaining
    without a DB lookup.

    Degrades honestly at every step: disabled by configuration, an
    unmeasured/unavailable workspace, or a workspace with nothing to
    preserve all return `attempted=False` with a `reason` — never an
    exception, and never a fabricated commit.
    """
    if not enabled:
        return _skip("preserve-on-failure is disabled by bridge configuration")
    if not measured.get("measured"):
        return _skip(
            "workspace was not measurable "
            f"({measured.get('reason') or 'no reason recorded'}); nothing to preserve"
        )
    if not _has_changes(measured):
        return _skip("no workspace changes since the session started; nothing to preserve")

    head = measured.get("head_after") or measured.get("head_before")
    if not head:
        return _skip("no HEAD commit to build the preserve commit on top of")

    branch = mint_branch_name(branch_prefix, run_id, node_run_id, attempt_id)
    message = _preserve_message(reason, run_id, node_run_id, attempt_id)

    commit_sha, error = _commit_plumbing(repo, head, message)
    if commit_sha is None:
        return PreserveResult(
            attempted=True,
            committed=False,
            pushed=False,
            local_only=False,
            branch=branch,
            reason=error,
        )

    ref_proc = _run_git(repo, "update-ref", f"refs/heads/{branch}", commit_sha)
    if ref_proc is None or ref_proc.returncode != 0:
        detail = (ref_proc.stderr or "").strip() if ref_proc is not None else "git could not be run"
        return PreserveResult(
            attempted=True,
            committed=False,
            pushed=False,
            local_only=False,
            branch=branch,
            commit=commit_sha,
            reason=(
                f"the local preserve commit {commit_sha} exists but update-ref failed: {detail}"
            ),
        )

    if not push:
        return PreserveResult(
            attempted=True,
            committed=True,
            pushed=False,
            local_only=True,
            branch=branch,
            commit=commit_sha,
            remote=remote,
            reason="push disabled by bridge configuration; the commit is local-only",
        )

    pushed, push_reason = _push_best_effort(repo, remote, commit_sha, branch)
    return PreserveResult(
        attempted=True,
        committed=True,
        pushed=pushed,
        local_only=not pushed,
        branch=branch,
        commit=commit_sha,
        remote=remote,
        reason=push_reason,
    )


def _preserve_message(
    reason: str, run_id: str, node_run_id: str | None, attempt_id: str | None
) -> str:
    lines = [
        "culture-nodes: preserve-on-failure",
        "",
        reason,
        "",
        f"run_id: {run_id}",
    ]
    if node_run_id:
        lines.append(f"node_run_id: {node_run_id}")
    if attempt_id:
        lines.append(f"attempt_id: {attempt_id}")
    return "\n".join(lines) + "\n"


def _commit_plumbing(repo: str, head: str, message: str) -> tuple[str | None, str | None]:
    """`git read-tree` + `git add -A` + `git write-tree` against a private
    scratch index file, then `git commit-tree` — see the module docstring
    for why every index-touching step below is pinned to that scratch file
    via `GIT_INDEX_FILE` rather than the repo's real `.git/index`."""
    with tempfile.TemporaryDirectory(prefix="culture-nodes-preserve-") as tmp:
        scratch_index = os.path.join(tmp, "index")
        env = dict(os.environ)
        env["GIT_INDEX_FILE"] = scratch_index

        seed = _run_git(repo, "read-tree", head, env=env)
        if seed is None or seed.returncode != 0:
            detail = (seed.stderr or "").strip() if seed is not None else "git could not be run"
            return None, f"git read-tree into a scratch index failed: {detail}"

        staged = _run_git(repo, "add", "-A", env=env)
        if staged is None or staged.returncode != 0:
            detail = (staged.stderr or "").strip() if staged is not None else "git could not be run"
            return None, f"git add -A into the scratch index failed: {detail}"

        tree_proc = _run_git(repo, "write-tree", env=env)
        if tree_proc is None or tree_proc.returncode != 0:
            detail = (
                (tree_proc.stderr or "").strip()
                if tree_proc is not None
                else "git could not be run"
            )
            return None, f"git write-tree failed: {detail}"
        tree_sha = tree_proc.stdout.strip()

    # commit-tree touches no index at all (scratch or real) — deliberately
    # run outside the `with` block, after the scratch index is already gone.
    commit_proc = _run_git(repo, "commit-tree", tree_sha, "-p", head, "-m", message)
    if commit_proc is None or commit_proc.returncode != 0:
        detail = (
            (commit_proc.stderr or "").strip()
            if commit_proc is not None
            else "git could not be run"
        )
        return None, f"git commit-tree failed: {detail}"
    return commit_proc.stdout.strip(), None


def _push_best_effort(
    repo: str, remote: str, commit_sha: str, branch: str
) -> tuple[bool, str | None]:
    """Best-effort `git push`, bounded by `PUSH_TIMEOUT_SECONDS`. Pushes the
    commit sha directly onto the named branch ref on *remote* — never
    requires a local checkout/tracking branch, so this cannot disturb
    whatever branch the live checkout currently has checked out. Never
    raises: a missing credential, an unreachable remote, or a rejected push
    are all ordinary `pushed=False` outcomes."""
    env = dict(os.environ)
    # Fail fast and honestly on a missing credential instead of hanging on
    # an interactive prompt nobody is present to answer.
    env["GIT_TERMINAL_PROMPT"] = "0"
    env.setdefault("GIT_ASKPASS", "true")
    refspec = f"{commit_sha}:refs/heads/{branch}"
    proc = _run_git(repo, "push", remote, refspec, env=env, timeout=PUSH_TIMEOUT_SECONDS)
    if proc is None:
        return False, (
            f"git push to remote {remote!r} could not be run (missing git, timeout, or OS "
            "error) — the local preserve commit still exists"
        )
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip()
        return False, (
            f"git push to remote {remote!r} failed"
            + (f": {detail}" if detail else " (no credential, unreachable remote, or rejected)")
            + " — the local preserve commit still exists"
        )
    return True, None
