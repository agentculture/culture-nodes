"""Bridge-measured workspace facts around one actor session (task t10):
HEAD before/after, `git status --porcelain`, a changed-file list, a
diffstat, and the current branch — captured by THIS PROCESS running `git`
directly against the dispatched repo, never read from or trusted from
colleague's own reported output.

This is the "measured-by-bridge" half of the distinction `mapping.py`'s own
docstring documents: the bridge is still an *actor* (its ledger claim stays
`proposed`, per the PRD's authority model), but the fields this module
produces are the bridge's own direct observation of the working tree, not a
completion claim relayed from the model. `server.py`/`async_runner.py` call
`begin()` immediately before dispatching the `colleague` subprocess and
`measure()` immediately after it finishes; the two calls bracket the
session so `head_before`/`head_after` and the diff between them describe
exactly what happened during it.

Degrades honestly, never fabricates: a repo with no `git` on PATH, that is
not a git working tree, or with no commits yet (an unborn HEAD) all produce
`measured: False` with a `reason` — mirrored field-for-field by
`claude_code_bridge.workspace` and `codex_bridge.workspace`.
"""

from __future__ import annotations

import shutil
import subprocess
from dataclasses import dataclass
from typing import Any

#: Bounds any single `git` subprocess this module runs. Git talking to a
#: local repo is normally instant; a bound still exists so a wedged
#: filesystem (network mount, lock contention) cannot hang the bridge
#: forever measuring a workspace nobody is waiting on synchronously.
GIT_TIMEOUT_SECONDS = 10.0


def _run_git(repo: str, *args: str) -> subprocess.CompletedProcess[str] | None:
    """Run one `git` subprocess in *repo*. `None` on ANY failure to run it
    at all (git missing, timeout, OS error) — never raised: a
    workspace-measurement failure must degrade the result honestly, never
    crash the bridge's own dispatch."""
    try:
        # "git" is deliberately a bare/partial executable name, resolved via
        # PATH — the same convention every other bridge uses for its own
        # claude_bin/codex_bin/colleague_bin.
        return subprocess.run(  # noqa: S603 # nosec B603,B607
            ["git", *args],
            cwd=repo,
            capture_output=True,
            text=True,
            timeout=GIT_TIMEOUT_SECONDS,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None


def _git_stdout(repo: str, *args: str) -> str | None:
    """The stdout of one git subprocess, or `None` if it was not runnable
    or exited non-zero. Not stripped — callers decide whether trailing
    whitespace is meaningful (`status --porcelain` is; a `rev-parse` value
    is not)."""
    proc = _run_git(repo, *args)
    if proc is None or proc.returncode != 0:
        return None
    return proc.stdout


@dataclass(frozen=True)
class WorkspaceHandle:
    """What `begin()` captures right before the actor subprocess is
    spawned: enough to measure a delta afterwards, or an honest reason why
    it could not."""

    repo: str
    available: bool
    head_before: str | None = None
    reason: str | None = None


def begin(repo: str) -> WorkspaceHandle:
    """Capture the workspace's starting point, as close as possible to the
    moment the actor subprocess is spawned.

    Degrades honestly: `git` absent from PATH, *repo* not a git working
    tree, or an unborn HEAD (no commits yet) all produce `available=False`
    with a `reason` — never a fabricated HEAD.
    """
    if shutil.which("git") is None:
        return WorkspaceHandle(
            repo=repo, available=False, reason="git is not installed on this bridge host"
        )

    inside = _git_stdout(repo, "rev-parse", "--is-inside-work-tree")
    if inside is None or inside.strip() != "true":
        return WorkspaceHandle(
            repo=repo, available=False, reason=f"{repo!r} is not a git working tree"
        )

    head = _git_stdout(repo, "rev-parse", "HEAD")
    if head is None:
        return WorkspaceHandle(
            repo=repo,
            available=False,
            reason="git HEAD could not be resolved (e.g. an unborn branch with no commits yet)",
        )

    return WorkspaceHandle(repo=repo, available=True, head_before=head.strip())


def unmeasured(repo: str | None, reason: str) -> dict[str, Any]:
    """The one honest "no measured facts" shape — used both for a
    `begin()` that could not establish a starting point and for
    `mapping.py`'s own default when a caller supplies no measurement at
    all. Identical shape to a successful `measure()`, with every fact null.
    """
    return {
        "measured": False,
        "repo": repo,
        "reason": reason,
        "branch": None,
        "head_before": None,
        "head_after": None,
        "status_porcelain": None,
        "changed_files": [],
        "diffstat": None,
    }


def measure(handle: WorkspaceHandle) -> dict[str, Any]:
    """Measure the workspace's ending point against *handle* and return the
    `workspace_measured` block (see `mapping.py`'s module docstring for the
    exact shape this bridge shares with `claude_code_bridge`/`codex_bridge`).

    Every field here comes from a `git` subprocess THIS bridge just ran —
    never from colleague's own reported output. `changed_files` is the union
    of tracked paths that differ from `handle.head_before` (`git diff
    --name-only`, so it captures commits made during the session too, not
    only uncommitted edits) and any new untracked paths `git status
    --porcelain` reports. `diffstat` is `git diff --stat` against the same
    starting point, for the same reason.
    """
    if not handle.available:
        return unmeasured(
            handle.repo, handle.reason or "workspace was not measurable at session start"
        )

    branch = _git_stdout(handle.repo, "rev-parse", "--abbrev-ref", "HEAD")
    head_after = _git_stdout(handle.repo, "rev-parse", "HEAD")
    status = _git_stdout(handle.repo, "status", "--porcelain")
    diffstat = _git_stdout(handle.repo, "diff", "--stat", handle.head_before)
    names = _git_stdout(handle.repo, "diff", "--name-only", handle.head_before)

    changed: list[str] = []
    if names is not None:
        changed.extend(line.strip() for line in names.splitlines() if line.strip())
    if status:
        for line in status.splitlines():
            if line.startswith("??"):
                untracked = line[3:].strip()
                if untracked and untracked not in changed:
                    changed.append(untracked)

    return {
        "measured": True,
        "repo": handle.repo,
        "reason": None,
        "branch": branch.strip() if branch is not None else None,
        "head_before": handle.head_before,
        "head_after": head_after.strip() if head_after is not None else None,
        "status_porcelain": status if status is not None else None,
        "changed_files": changed,
        "diffstat": diffstat.strip() if diffstat is not None else None,
    }
