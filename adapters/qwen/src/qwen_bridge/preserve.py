"""Bridge-side preserve-on-failure (task t25, issue #49): a failed node's
working-tree changes must not evaporate — but the bridge's `qwen`
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

`handover_ref` (task t6, issue #74, spec decision q9) is the SECOND caller of
that same plumbing, and deliberately not a second implementation of it: it
calls `_commit_plumbing` and `_update_ref` exactly as `preserve_on_failure`
does, and differs only in why it runs, where the ref lands, and what it
refuses to do afterwards. Preserve is a rescue on a technical failure and
pushes best-effort; a handover is a SUCCESS path — the carrier a runner's
changes travel on when the next node is on another machine — and it never
pushes at all. That is policy, not capability: AGENTS.md lets an agent create
a handover commit and a ref under `refs/culture-nodes/<run-id>` and forbids it
to push or to commit onto a branch, so what this module produces is
unreachable from any branch until the operator or the control plane moves it,
and the handle says so in its own `publication` field rather than implying a
fetchability nobody here can deliver.

Mirrored field-for-field by `claude_code_bridge.preserve` and
`colleague_bridge.preserve` (all-backends rule): only the docstring's named
subprocess (`qwen` here) differs.

This module is never called on a domain outcome (PRD: domain outcome ≠
technical status) — only on a genuine technical failure (execution,
timeout, capacity_exhausted). `server.py`/`async_runner.py` call
`preserve_on_failure` after `mapping.sync_response`/`terminal_event` have
already classified the result, gated on the failure status THEY produced,
never on this module's own judgement of what qwen reported.
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


#: Public alias for `reap.py`, which names its rescue ref under this
#: module's own branch prefix and must sanitize it by the same rule — one
#: place decides what a ref-safe component is.
sanitize_ref_component = _sanitize_ref_component


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

    detail = _update_ref(repo, f"refs/heads/{branch}", commit_sha)
    if detail is not None:
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


def _update_ref(repo: str, ref: str, commit_sha: str) -> str | None:
    """`git update-ref <ref> <sha>` — the ONE ref-writing step in this module,
    shared by the preserve branch and the handover ref so neither grows its
    own. Returns `None` on success, or the failure detail. Never writes HEAD
    and never writes an existing branch: callers pass a freshly minted name."""
    proc = _run_git(repo, "update-ref", ref, commit_sha)
    if proc is None:
        return "git could not be run"
    if proc.returncode != 0:
        return (proc.stderr or "").strip() or f"git update-ref exited {proc.returncode}"
    return None


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


# ---------------------------------------------------------------------------
# the handover carrier (task t6, issue #74, spec decision q9)
# ---------------------------------------------------------------------------

#: Where a handover ref lives, and the only namespace this module will write
#: for one. AGENTS.md: an agent may create a handover commit and a ref under
#: `refs/culture-nodes/<run-id>`, may NEVER push, and may never commit onto a
#: branch. A ref here is reachable from no branch, so nothing this module
#: creates can be mistaken for work someone merged.
HANDOVER_REF_NAMESPACE = "refs/culture-nodes"

#: What a git-ref handle contains, reported as the handle's `media_type` so a
#: consuming node knows what it is opening before it opens it.
HANDOVER_MEDIA_TYPE = "application/vnd.culture-nodes.git-commit"

#: The `missing_capability` names this module can report, mirroring the CLOSED
#: enum in examples/pr-upkeep/workflow.yaml's `fix.handoff_unavailable`. A
#: capability the graph cannot name is one no dashboard can group by, so this
#: module never invents a third string.
MISSING_GIT_REF_PUBLISH = "git_ref_publish"
MISSING_WORKSPACE_EXPORT = "workspace_export"

#: `user@host:path`, git's scp-like remote syntax. Recognised so it can be
#: normalised to a real `ssh://` URL rather than refused: it is an ordinary
#: shared remote, just written in git's oldest spelling.
_SCP_LIKE_REMOTE = re.compile(r"^(?P<user>[^@/:]+@)?(?P<host>[^/:]+):(?P<path>[^/].*)$")


def mint_handover_ref(run_id: str, node_run_id: str | None, attempt_id: str | None) -> str:
    """A unique ref name under `HANDOVER_REF_NAMESPACE`, minted from the same
    sanitised identifiers and the same collision-proofing (`mint_branch_name`'s
    timestamp + random suffix) the preserve branch uses. Run-scoped by
    construction — `refs/culture-nodes/<run-id>/...` — so an operator sweeping
    a finished run's refs can select them by run without a database."""
    run = _sanitize_ref_component(run_id) if run_id else "unknown"
    tail = mint_branch_name("", "", node_run_id, attempt_id)
    return f"{HANDOVER_REF_NAMESPACE}/{run}/{tail}"


@dataclass(frozen=True)
class HandoverResult:
    """What one `handover_ref` call decided and did. Same discipline as
    `PreserveResult`: every field is this module's own direct observation.
    `created=True` always carries `ref`, `commit` and a `handle`;
    `created=False` after an attempt always carries a `missing_capability`
    from the graph's closed enum, so the caller can report the domain outcome
    the workflow declares instead of inventing prose."""

    attempted: bool
    created: bool
    ref: str | None = None
    commit: str | None = None
    remote: str | None = None
    handle: dict[str, Any] | None = None
    missing_capability: str | None = None
    reason: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "attempted": self.attempted,
            "created": self.created,
            "ref": self.ref,
            "commit": self.commit,
            "remote": self.remote,
            "handle": self.handle,
            "missing_capability": self.missing_capability,
            "reason": self.reason,
        }


def _unavailable(capability: str, reason: str, *, attempted: bool = True) -> HandoverResult:
    return HandoverResult(
        attempted=attempted, created=False, missing_capability=capability, reason=reason
    )


def _strip_userinfo(authority: str, *, keep_user: bool) -> str:
    """Remove credentials from a remote's authority before it is published in
    a handle. A handle is reported to the control plane and lands in the run
    record, so an `https://x-access-token:<token>@github.com/...` remote would
    write a live credential into the ledger. The ssh `git@` form is an
    identity rather than a secret and is kept; anything carrying a password
    (`user:secret@`) is dropped under either scheme."""
    if "@" not in authority:
        return authority
    userinfo, _, hostport = authority.rpartition("@")
    if keep_user and ":" not in userinfo:
        return f"{userinfo}@{hostport}"
    return hostport


def _portable_remote_url(url: str) -> tuple[str | None, str | None]:
    """Normalise a configured remote into the `git+<https|ssh>://...` prefix a
    handle may carry, or explain why it cannot be one. Returns
    `(prefix, None)` or `(None, reason)`.

    The refusals are the point. A `file://` remote, a bare directory and a
    relative path are all filesystem paths, and a handle built on one would
    re-admit exactly the unportable value the whole handoff contract exists to
    refuse (issue #74). `http://` and `git://` are refused for a different
    reason: the handle contract admits https and ssh only, so producing one
    would produce a handle no consumer's schema would accept."""
    url = url.strip()
    if not url:
        return None, "the remote resolved to an empty url"
    if any(ch.isspace() for ch in url) or "#" in url:
        return None, (
            f"the remote url {url!r} contains whitespace or '#', which cannot be expressed "
            "in a handle (the ref is carried in the url's fragment)"
        )

    lowered = url.lower()
    if lowered.startswith("https://"):
        return "git+https://" + _strip_userinfo(url[len("https://") :], keep_user=False), None
    if lowered.startswith("ssh://"):
        return "git+ssh://" + _strip_userinfo(url[len("ssh://") :], keep_user=True), None
    if lowered.startswith("file://") or url.startswith(("/", ".", "~")):
        return None, (
            f"the remote {url!r} is a filesystem path, which is meaningful only on this "
            "host — the same unportable handle issue #74 exists to refuse"
        )
    if lowered.startswith(("http://", "git://")):
        return None, (
            f"the remote {url!r} uses a transport a handoff handle may not carry; the "
            "contract admits https and ssh only"
        )
    scp = _SCP_LIKE_REMOTE.match(url)
    if scp:
        authority = _strip_userinfo(f"{scp.group('user') or ''}{scp.group('host')}", keep_user=True)
        return f"git+ssh://{authority}/{scp.group('path')}", None
    return None, f"the remote {url!r} is not a url this module knows how to make portable"


def _remote_prefix(repo: str, remote: str) -> tuple[str | None, str | None]:
    """Read the configured url for *remote* (a read-only git query — nothing
    is fetched, nothing is written) and normalise it."""
    proc = _run_git(repo, "remote", "get-url", remote)
    if proc is None:
        return None, f"git remote get-url {remote!r} could not be run"
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip()
        return None, (
            f"no remote named {remote!r} is configured in this checkout"
            + (f": {detail}" if detail else "")
        )
    return _portable_remote_url(proc.stdout.strip())


def _handover_message(
    reason: str, run_id: str, node_run_id: str | None, attempt_id: str | None
) -> str:
    lines = [
        "culture-nodes: handover",
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


def handover_success_reason(outcome: Any) -> str:
    """The reason line a SUCCESSFUL dispatch's handover commit carries.

    Shared by every call site (each bridge's synchronous and asynchronous
    success path) so the commit message a fetching node reads means the same
    thing whichever path produced it: it names the DOMAIN OUTCOME the session
    reported, which is the one fact that distinguishes two otherwise
    identical handover commits from the same run.
    """
    if outcome:
        return f"the session completed with outcome {outcome!r}; its changes travel as this ref"
    return "the session completed; its changes travel as this ref"


def handover_ref(
    repo: str,
    measured: dict[str, Any],
    *,
    enabled: bool,
    remote: str,
    run_id: str,
    node_run_id: str | None,
    attempt_id: str | None,
    reason: str,
) -> HandoverResult:
    """Publish this session's workspace changes as a `git_ref` HANDLE the next
    node can read from another machine (task t6, spec decision q9): a runner's
    CHANGES travel as a git ref, while context and data that is not naturally
    a git object travels as an `artifact://` reference through the artifact
    store. The rule itself is declared once, in
    `schemas/workflow/handoff.schema.json`.

    *enabled* is the per-dispatch opt-in. A package that hands nothing over
    never reaches the git plumbing here at all — which is the same boundary
    the sandbox side draws: a qwen dispatch that hands over no ref is not
    given `.git` write either (issue #91, deviation d6).

    What this deliberately does NOT do is push. Publication is a separate
    authority: AGENTS.md permits creating the commit and the ref, and forbids
    pushing and forbids committing onto a branch, so the handle is reported
    with `publication: "pending"` and the ref stays unreachable from any
    branch until the operator or the control plane moves it. A consuming node
    reading `pending` can say so by name instead of reporting a fetch failure
    whose cause it would have to guess.

    Degrades into the graph's own closed vocabulary rather than into prose:
    every unsuccessful attempt names a `missing_capability` the workflow
    already declares (`git_ref_publish`, `workspace_export`), so the caller
    can report `handoff_unavailable` — a domain outcome — instead of a
    technical failure at somebody else's node.
    """
    if not enabled:
        return HandoverResult(
            attempted=False,
            created=False,
            reason="this dispatch asked for no handover; nothing was created",
        )
    if not measured.get("measured"):
        return _unavailable(
            MISSING_WORKSPACE_EXPORT,
            "workspace was not measurable "
            f"({measured.get('reason') or 'no reason recorded'}); there is nothing to hand over",
        )
    if not _has_changes(measured):
        return _unavailable(
            MISSING_WORKSPACE_EXPORT,
            "no workspace changes since the session started; there is nothing to hand over",
        )

    head = measured.get("head_after") or measured.get("head_before")
    if not head:
        return _unavailable(
            MISSING_WORKSPACE_EXPORT,
            "no HEAD commit to build the handover commit on top of",
        )

    # Resolved BEFORE anything is written: a checkout with no remote another
    # host could ever fetch from cannot produce a portable handle, and finding
    # that out first keeps the honest failure free of a stray object.
    prefix, remote_error = _remote_prefix(repo, remote)
    if prefix is None:
        return _unavailable(
            MISSING_GIT_REF_PUBLISH,
            f"this host cannot name a remote a handover ref could reach: {remote_error}",
        )

    commit_sha, error = _commit_plumbing(
        repo, head, _handover_message(reason, run_id, node_run_id, attempt_id)
    )
    if commit_sha is None:
        return _unavailable(MISSING_WORKSPACE_EXPORT, error or "the handover commit failed")

    ref = mint_handover_ref(run_id, node_run_id, attempt_id)
    detail = _update_ref(repo, ref, commit_sha)
    if detail is not None:
        return _unavailable(
            MISSING_WORKSPACE_EXPORT,
            f"the local handover commit {commit_sha} exists but update-ref failed: {detail}",
        )

    return HandoverResult(
        attempted=True,
        created=True,
        ref=ref,
        commit=commit_sha,
        remote=remote,
        handle={
            "kind": "git_ref",
            "ref": f"{prefix}#{ref}",
            "commit": commit_sha,
            "publication": "pending",
            "media_type": HANDOVER_MEDIA_TYPE,
        },
    )
