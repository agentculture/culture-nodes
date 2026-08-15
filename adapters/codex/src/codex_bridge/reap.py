"""Bridge-side worktree reaper (task t17): the other half of
`workspace.provision()`.

`provision()` mints one detached worktree per writer beneath a permitted
root. Something has to give those directories back, or a host accumulates
one checkout per node run forever. This module is that something — and it
is built around a single refusal, probed rather than argued:

    $ git worktree remove /path/to/dirty-worktree
    fatal: '/path/to/dirty-worktree' contains modified or untracked files,
    use --force to delete it

That refusal is the feature. `--force` appears nowhere in this module, and
`tests/lint/workspacereaper_test.go` fails the build if it ever does,
because a reaper that forces is issue #78's data loss arriving as
housekeeping instead of as a timeout — the same bytes gone, just from a
step nobody was watching.

The gate is POSITIVE EVIDENCE, never the absence of an error
(spec s42/c52). A worktree is reapable only when this module can name a
durable object that outlives the directory:

* `branch` — HEAD is contained by the worktree's own branch ref. Refs live
  in the SHARED `.git`, so the branch survives the directory (measured:
  `refs/heads/*` are common to every worktree of a repo).
* `preserve_ref` — HEAD is contained by a ref under `preserve.py`'s branch
  prefix. Same shared-ref argument; this is the timed-out writer's case.
* `merged` — HEAD is contained by a configured durable ref (`origin/main`).
* `reachable_ref` — HEAD is contained by some other ref. Detached but not
  lost.
* `artifact` — a published artifact handle the CALLER asserts. This is the
  one evidence kind this module cannot measure, and it is flagged
  `measured: false` on the record so a reader can tell a fact this process
  established from a claim it was handed.

No evidence and nothing to mint means REFUSE. Evidence is never inferred
from a command that happened not to fail.

## The four decisions

* `reap` — safe: remove the directory, keep the work. Never performed
  unless a caller explicitly passes `perform=True`; the default is a dry
  plan carrying the exact command an operator would run.
* `preserve_then_reap` — clean, real commits, and NO ref anywhere contains
  them (a detached worktree whose branch was deleted, or one
  `provision()` minted with `--detach` and nobody ever branched). Removing
  it strands the commits behind a reflog that expires. `secure()` mints
  one ref with `git update-ref` — preserve.py's doctrine exactly: one new
  ref written, HEAD/index/working tree never touched — which converts the
  case into the already-safe branch case. We mint rather than refuse
  because refusing is not neutral here: the work is degrading (unreferenced
  objects are what `git gc` collects), so "do nothing" quietly loses the
  same bytes `--force` would, only slower.
* `refuse` — would lose work, or is not ours to take. Domain outcome
  `retained`; the directory stays exactly as it was.
* `defer` — nothing wrong with it, but it may still be in use. Domain
  outcome `deferred`.

Plus `prune`, for a worktree git still has metadata for whose directory is
already gone: there are no bytes to lose, so there is nothing to gate.

## How we know a worktree is idle — and where we cannot know

We mostly cannot, and the policy is built to say so rather than guess:

1. `active_workspaces` — the caller's session registry. Authoritative for
   sessions THIS bridge started, and for nothing else. Passing `None`
   means "I do not know what is running" and every candidate DEFERS. An
   empty tuple is a positive statement ("nothing is registered"), and is
   the only way a sweep reaps anything.
2. git's own `locked` marker — a positive busy signal git itself honours.
3. A process on this host whose cwd is inside the worktree. This is a
   POSITIVE detector only: a hit proves busy, a miss proves nothing, since
   `/proc/<pid>/cwd` is unreadable for other users' processes. The count
   of pids we could not inspect rides along on every decision as
   `unreadable_pids` so the blind spot is visible instead of implied.
4. Age. The weakest signal and the only one that covers a session this
   host never started (another machine's checkout over NFS, a human's
   editor, a bridge that died). Default `min_idle_seconds` is 24h, because
   a work package legitimately pauses overnight.

Age is measured over the working tree's files AND the worktree's admin
directory (`<common>/.git/worktrees/<id>/`), but deliberately NOT over
that directory's `logs/` reflog: on the host this task was written for,
all three stale worktrees carried a `logs/HEAD` timestamped within the
same minute, hours after any human touched them — `git gc`/`reflog expire`
rewriting reflogs as background maintenance. Counting that as activity
would defer every worktree forever for a reason that has nothing to do
with anyone working.

None of 1–4 can see a session on ANOTHER host sharing this filesystem. So
idleness is only ever a reason to DEFER, never a reason to reap: a
worktree passes on evidence, and idleness merely stops us acting on
evidence too early.

## Failure is a domain outcome, never an exception

Nothing here raises. Every path — git missing, a repo that is not a repo,
a removal git refused — produces a decision record with a
`domain_outcome` of `reclaimed`, `retained` or `deferred` and leaves the
worktree in place. That is the PRD's domain-outcome-≠-technical-status
rule (and t17's own acceptance bullet): a cleanup that could not clean up
is a routable result, not an engine failure.

Everything that MUTATES a repository or the filesystem — `secure()`,
`execute()`, `sweep()` — lives in the sibling `reclaim.py`, so nothing
in this module can change anything. That split is what makes "the plan
does not touch your worktrees" checkable by reading a file name.

Mirrored byte-for-byte by `codex_bridge.reap` and `colleague_bridge.reap`
(all-backends rule), which is why the two sibling imports below are
RELATIVE — an absolute `from claude_code_bridge import ...`, the
convention everywhere else in this package, would make the three copies
differ by the one thing that must not differ. `preflight.py` avoids the
question by importing no siblings at all; this module cannot, because
running git through a second subprocess helper of its own is exactly the
duplication `workspace.py` exists to prevent.
"""

from __future__ import annotations

import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Sequence

from .preserve import sanitize_ref_component

# `run_git` is imported here and re-exported for `reclaim.py`: the
# mutating half must go through the SAME bounded, never-raising git
# helper this module reads with, not a second one of its own.
from .workspace import git_stdout, is_within, run_git  # noqa: F401

#: How long a worktree must have gone untouched before age stops being a
#: reason to defer. A day: long enough that an overnight pause in a work
#: package does not get its checkout taken away, short enough that a host
#: does not accumulate a week of dead directories.
DEFAULT_MIN_IDLE_SECONDS = 86_400.0

#: Bounds the working-tree age walk. A worktree with a huge untracked tree
#: (node_modules, .venv, a dist/ build) must not turn a sweep into a
#: filesystem crawl. Hitting the cap is reported and DEFERS — a truncated
#: scan cannot support an idleness claim.
MAX_IDLE_SCAN_ENTRIES = 50_000

#: How many uncommitted paths a refusal names before it summarises. The
#: count is always exact; the listing is bounded so one refusal record
#: cannot be a thousand lines long.
MAX_NAMED_DIRTY_ENTRIES = 20

#: Files in `<common>/.git/worktrees/<id>/` that a USER git operation
#: writes. `logs/` is deliberately absent — see the module docstring's
#: reflog-maintenance measurement.
ADMIN_ACTIVITY_FILES = ("index", "HEAD", "ORIG_HEAD", "COMMIT_EDITMSG")

# --- decisions ---------------------------------------------------------
REAP = "reap"
PRESERVE_THEN_REAP = "preserve_then_reap"
PRUNE = "prune"
REFUSE = "refuse"
DEFER = "defer"

# --- domain outcomes (PRD: routable, never an engine failure) ----------
RECLAIMED = "reclaimed"
RETAINED = "retained"
DEFERRED = "deferred"

_OUTCOME_RANK = {RECLAIMED: 0, DEFERRED: 1, RETAINED: 2}


@dataclass(frozen=True)
class Evidence:
    """One durable thing that outlives the directory.

    `measured` is False for evidence this process was HANDED rather than
    established itself (today: a published artifact handle). A reader must
    be able to tell the two apart — the PRD's whole authority model turns
    on not laundering a claim into an observation.
    """

    kind: str
    detail: str
    measured: bool = True

    def to_dict(self) -> dict[str, Any]:
        return {"kind": self.kind, "detail": self.detail, "measured": self.measured}


@dataclass(frozen=True)
class Reason:
    """A blocker (refuse) or a hold (defer), with the specifics that make
    it actionable rather than a category name."""

    code: str
    detail: str

    def to_dict(self) -> dict[str, str]:
        return {"code": self.code, "detail": self.detail}


@dataclass(frozen=True)
class ReapPolicy:
    """Who this reaper may reclaim from, and how patient it is.

    The two root lists are deliberately the SAME two lists `provision()`
    checks, read straight off the bridge config: a reaper that decided
    ownership differently from the provisioner would be a second, silently
    diverging ownership model.
    """

    #: Scoped roots this host mints into (`repo_allowlist_prefixes`). A
    #: candidate must be a STRICT child of one; the root itself is never a
    #: worktree we own.
    permitted_roots: tuple[str, ...] = ()
    #: Other writers' exact allowlisted roots (`repo_allowlist`). Anything
    #: strictly inside one of these belongs to whoever is dispatched
    #: there, and is refused however clean it looks.
    contained_by_roots: tuple[str, ...] = ()
    #: Workspaces this bridge currently has a live session in. `None`
    #: means UNKNOWN and defers everything; `()` is a positive "nothing is
    #: registered".
    active_workspaces: tuple[str, ...] | None = None
    #: Worktree path -> published artifact handle, asserted by the caller.
    published_artifacts: dict[str, str] = field(default_factory=dict)
    min_idle_seconds: float = DEFAULT_MIN_IDLE_SECONDS
    preserve_branch_prefix: str = "preserve/"
    #: Refs that count as "this work already landed" (e.g. origin/main).
    durable_refs: tuple[str, ...] = ()

    @classmethod
    def from_config(
        cls,
        cfg: Any,
        *,
        active_workspaces: tuple[str, ...] | None = None,
        published_artifacts: dict[str, str] | None = None,
        durable_refs: tuple[str, ...] = (),
    ) -> "ReapPolicy":
        """Build a policy from a bridge `Config`, reusing the provisioner's
        own allowlists rather than introducing a parallel one."""
        return cls(
            permitted_roots=tuple(getattr(cfg, "repo_allowlist_prefixes", ()) or ()),
            contained_by_roots=tuple(getattr(cfg, "repo_allowlist", ()) or ()),
            active_workspaces=active_workspaces,
            published_artifacts=dict(published_artifacts or {}),
            min_idle_seconds=float(
                getattr(cfg, "worktree_reap_min_idle_seconds", DEFAULT_MIN_IDLE_SECONDS)
            ),
            preserve_branch_prefix=str(getattr(cfg, "preserve_branch_prefix", "preserve/")),
            durable_refs=durable_refs,
        )


@dataclass(frozen=True)
class Decision:
    """What the reaper would do to one worktree, and every reason why.

    `blockers` and `holds` are both always reported, even when only one of
    them decided the outcome: an operator reading "refused: uncommitted
    work" also wants to know a session is live in there.
    """

    path: str
    decision: str
    domain_outcome: str
    evidence: tuple[Evidence, ...] = ()
    blockers: tuple[Reason, ...] = ()
    holds: tuple[Reason, ...] = ()
    facts: dict[str, Any] = field(default_factory=dict)
    #: The exact argv an operator (or `execute(perform=True)`) would run.
    #: `None` when there is nothing to run.
    command: tuple[str, ...] | None = None
    #: For `preserve_then_reap`: the ref `secure()` would mint.
    mint_ref: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "decision": self.decision,
            "domain_outcome": self.domain_outcome,
            "evidence": [e.to_dict() for e in self.evidence],
            "blockers": [r.to_dict() for r in self.blockers],
            "holds": [r.to_dict() for r in self.holds],
            "facts": dict(self.facts),
            "command": list(self.command) if self.command is not None else None,
            "mint_ref": self.mint_ref,
        }


# --- git surface -------------------------------------------------------


def _resolve(path: str) -> str:
    try:
        return str(Path(path).expanduser().resolve())
    except OSError:
        return str(Path(path).expanduser())


def parse_worktree_list(text: str) -> list[dict[str, Any]]:
    """Parse `git worktree list --porcelain`. The MAIN worktree is git's
    first entry — that ordering is the only way to identify it from this
    output, and it is what `is_main` below relies on."""
    entries: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    for line in text.splitlines():
        if not line.strip():
            if current is not None:
                entries.append(current)
                current = None
            continue
        key, _, value = line.partition(" ")
        if key == "worktree":
            if current is not None:
                entries.append(current)
            current = {
                "worktree": value,
                "head": None,
                "branch": None,
                "detached": False,
                "bare": False,
                "locked": None,
                "prunable": None,
            }
        elif current is None:
            continue
        elif key == "HEAD":
            current["head"] = value
        elif key == "branch":
            current["branch"] = value
        elif key == "detached":
            current["detached"] = True
        elif key == "bare":
            current["bare"] = True
        elif key == "locked":
            current["locked"] = value or "(no reason given)"
        elif key == "prunable":
            current["prunable"] = value or "(no reason given)"
    if current is not None:
        entries.append(current)
    return entries


def _admin_dir(repo: str, worktree: str) -> Path | None:
    """`<common>/.git/worktrees/<id>/` for *worktree*, read from the
    worktree itself so we never guess the id from its basename."""
    common = git_stdout(repo, "rev-parse", "--git-common-dir")
    if common is None:
        return None
    base = Path(common.strip())
    if not base.is_absolute():
        base = Path(repo) / base
    candidate = base / "worktrees" / Path(worktree).name
    return candidate if candidate.is_dir() else None


def _refs_containing(repo: str, head: str) -> tuple[str, ...] | None:
    """Every ref that contains *head*. `None` if git could not answer —
    which is fail-closed input: unknown reachability is not evidence."""
    out = git_stdout(repo, "for-each-ref", "--contains", head, "--format=%(refname)")
    if out is None:
        return None
    return tuple(line.strip() for line in out.splitlines() if line.strip())


def _collect_evidence(
    repo: str, entry: dict[str, Any], policy: ReapPolicy
) -> tuple[tuple[Evidence, ...], Reason | None]:
    head = entry.get("head")
    found: list[Evidence] = []
    handle = policy.published_artifacts.get(_resolve(entry["worktree"]))
    if handle:
        found.append(Evidence("artifact", handle, measured=False))
    if not head:
        return tuple(found), None

    refs = _refs_containing(repo, head)
    if refs is None:
        return tuple(found), Reason(
            "reachability_unknown",
            f"git could not list the refs containing {head[:12]}; "
            "unknown reachability is never treated as evidence",
        )

    branch = entry.get("branch")
    prefix = "refs/heads/" + policy.preserve_branch_prefix.lstrip("/")
    for ref in refs:
        if branch and ref == branch:
            found.append(Evidence("branch", f"{ref} contains {head[:12]}"))
        elif ref.startswith(prefix):
            found.append(Evidence("preserve_ref", f"{ref} contains {head[:12]}"))
        elif ref in policy.durable_refs or ref.removeprefix("refs/remotes/") in policy.durable_refs:
            found.append(Evidence("merged", f"{ref} contains {head[:12]}"))
    if not any(e.kind in {"branch", "preserve_ref", "merged"} for e in found) and refs:
        found.append(Evidence("reachable_ref", f"{refs[0]} contains {head[:12]}"))
    return tuple(found), None


# --- liveness ----------------------------------------------------------


def _newest_mtime(root: Path, cap: int) -> tuple[float | None, str | None, bool]:
    """Newest file mtime under *root*, the path that carried it, and
    whether the walk hit *cap*. The path is reported so "still warm" is
    checkable rather than an assertion.

    A `.git` DIRECTORY is pruned: in a main checkout that directory is the
    repository's shared metadata, which every OTHER worktree writes into,
    so walking it makes one worktree look busy because a different one is.
    (A linked worktree's `.git` is a file, not a directory, so only the
    main checkout is affected — and the per-worktree admin files are
    probed explicitly by `_admin_activity_mtime` instead.)
    """
    newest: float | None = None
    newest_path: str | None = None
    seen = 0
    for base, dirs, files in os.walk(root, onerror=lambda _e: None):
        dirs[:] = [d for d in dirs if d != ".git"]
        for name in files:
            seen += 1
            if seen > cap:
                return newest, newest_path, True
            try:
                mtime = os.lstat(os.path.join(base, name)).st_mtime
            except OSError:
                continue
            if newest is None or mtime > newest:
                newest = mtime
                newest_path = os.path.join(base, name)
    return newest, newest_path, False


def _admin_activity_mtime(admin: Path | None) -> tuple[float | None, str | None]:
    if admin is None:
        return None, None
    newest: float | None = None
    newest_path: str | None = None
    for name in ADMIN_ACTIVITY_FILES:
        try:
            mtime = os.lstat(admin / name).st_mtime
        except OSError:
            continue
        if newest is None or mtime > newest:
            newest = mtime
            newest_path = str(admin / name)
    return newest, newest_path


def processes_inside(path: str, proc_root: str = "/proc") -> tuple[tuple[int, ...] | None, int]:
    """Pids on THIS host whose cwd is inside *path*, and how many pids we
    were not permitted to inspect.

    A positive detector only. `None` for the pid tuple means the probe is
    unavailable at all (no `/proc`), which is a hold in its own right —
    absence of a probe is not absence of a session.
    """
    root = Path(proc_root)
    if not root.is_dir():
        return None, 0
    target = Path(_resolve(path))
    hits: list[int] = []
    unreadable = 0
    try:
        names = os.listdir(root)
    except OSError:
        return None, 0
    for name in names:
        if not name.isdigit():
            continue
        try:
            cwd = os.readlink(root / name / "cwd")
        except PermissionError:
            unreadable += 1
            continue
        except OSError:
            continue
        try:
            if is_within(Path(cwd), target):
                hits.append(int(name))
        except (OSError, ValueError):
            continue
    return tuple(sorted(hits)), unreadable


def _liveness_holds(
    path: str, admin: Path | None, policy: ReapPolicy, now: float, facts: dict[str, Any]
) -> list[Reason]:
    holds: list[Reason] = []

    if policy.active_workspaces is None:
        holds.append(
            Reason(
                "session_liveness_unknown",
                "no session-registry snapshot was supplied, so this host cannot say whether a "
                "writer is live in this worktree; pass an empty tuple to state positively that "
                "nothing is registered",
            )
        )
        facts["registered_session"] = None
    else:
        registered = _resolve(path) in {_resolve(p) for p in policy.active_workspaces}
        facts["registered_session"] = registered
        if registered:
            holds.append(
                Reason("session_active", f"this bridge has a live session registered at {path}")
            )

    pids, unreadable = processes_inside(path)
    facts["unreadable_pids"] = unreadable
    facts["processes_inside"] = list(pids) if pids is not None else None
    if pids is None:
        holds.append(
            Reason(
                "liveness_probe_unavailable",
                "no readable /proc on this host, so a running process inside the worktree "
                "cannot be ruled out",
            )
        )
    elif pids:
        holds.append(
            Reason(
                "process_cwd_inside",
                f"pid(s) {', '.join(str(p) for p in pids)} have a working directory inside it",
            )
        )

    tree_mtime, tree_path, truncated = _newest_mtime(Path(path), MAX_IDLE_SCAN_ENTRIES)
    admin_mtime, admin_path = _admin_activity_mtime(admin)
    newest, newest_path = tree_mtime, tree_path
    if admin_mtime is not None and (newest is None or admin_mtime > newest):
        newest, newest_path = admin_mtime, admin_path
    facts["idle_scan_truncated"] = truncated
    facts["newest_touch_path"] = newest_path
    facts["idle_seconds"] = None if newest is None else round(now - newest, 1)
    facts["min_idle_seconds"] = policy.min_idle_seconds

    if truncated:
        holds.append(
            Reason(
                "idle_scan_truncated",
                f"the age walk stopped after {MAX_IDLE_SCAN_ENTRIES} files, so the newest "
                "touch is not known",
            )
        )
    elif newest is None:
        holds.append(
            Reason("idle_age_unknown", "no file under the worktree could be stat'ed for its age")
        )
    elif now - newest < policy.min_idle_seconds:
        holds.append(
            Reason(
                "too_recently_touched",
                f"{newest_path} was written {round((now - newest) / 3600, 1)}h ago, inside the "
                f"{round(policy.min_idle_seconds / 3600, 1)}h idle floor",
            )
        )
    return holds


# --- the decision ------------------------------------------------------


def _self_paths() -> tuple[str, ...]:
    """Paths that would make a candidate the reaper's OWN home. Removing
    the checkout you are running from is the one mistake no evidence gate
    catches, because the work is safe and the reaper still dies."""
    here = [str(Path(__file__).resolve().parent)]
    try:
        here.append(_resolve(os.getcwd()))
    except OSError:
        pass
    return tuple(here)


def _dirty_blocker(repo_path: str) -> Reason | None:
    """`git status --porcelain` in the candidate, named not counted.

    Fails CLOSED: an unreadable status is a refusal, because "we could not
    look" and "there is nothing there" must never produce the same answer.

    `--no-optional-locks` is load-bearing, not tidiness: a plain `git
    status` REFRESHES and rewrites the worktree's index, which bumps that
    file's mtime — so an inspecting reaper would make every worktree it
    looked at appear touched-just-now and then defer it for being warm.
    The age probe also runs before this call for the same reason (see
    `assess`). A reaper must not write into a worktree it is only reading.
    """
    status = git_stdout(repo_path, "--no-optional-locks", "status", "--porcelain")
    if status is None:
        return Reason(
            "workspace_unreadable",
            "git status could not be read here, so the worktree cannot be shown to be clean",
        )
    entries = [line.rstrip() for line in status.splitlines() if line.strip()]
    if not entries:
        return None
    shown = entries[:MAX_NAMED_DIRTY_ENTRIES]
    more = len(entries) - len(shown)
    listing = "; ".join(shown) + (f"; and {more} more" if more else "")
    return Reason(
        "uncommitted_work",
        f"{len(entries)} uncommitted path(s) that no ref holds: {listing}",
    )


def assess(
    repo: str,
    entry: dict[str, Any],
    policy: ReapPolicy,
    *,
    is_main: bool = False,
    main_worktree: str | None = None,
    now: float | None = None,
    self_paths: Sequence[str] | None = None,
) -> Decision:
    """Decide one worktree. Never raises, never mutates anything."""
    now = time.time() if now is None else now
    path = entry["worktree"]
    resolved = _resolve(path)
    facts: dict[str, Any] = {
        "head": entry.get("head"),
        "branch": entry.get("branch"),
        "detached": bool(entry.get("detached")),
        "bare": bool(entry.get("bare")),
        "locked": entry.get("locked"),
        "prunable": entry.get("prunable"),
        "is_main_worktree": is_main,
        "exists": Path(resolved).is_dir(),
    }

    # A worktree git remembers whose directory is already gone. No bytes,
    # so no gate: `prune` only rewrites git's own metadata.
    if entry.get("prunable") and not facts["exists"]:
        return Decision(
            path=path,
            decision=PRUNE,
            domain_outcome=RECLAIMED,
            facts=facts,
            command=("git", "worktree", "prune"),
            evidence=(Evidence("no_working_tree", f"the directory is gone: {entry['prunable']}"),),
        )

    blockers: list[Reason] = []
    if is_main:
        blockers.append(Reason("main_worktree", "this is the repository's main checkout"))
    if entry.get("bare"):
        blockers.append(Reason("bare_worktree", "a bare repository is not a writer's workspace"))

    for own in self_paths if self_paths is not None else _self_paths():
        if is_within(Path(own), Path(resolved)):
            blockers.append(
                Reason("reaper_own_worktree", f"the reaper is running from inside it ({own})")
            )
            break

    # The repo's own main checkout is an IMPLICIT containment root, not a
    # configured one. A per-lane allowlist (which is what spark's bridges
    # carry today) would leave the nesting hazard unnamed if this depended
    # on configuration — and the hazard is a property of the filesystem,
    # not of anyone's config: whoever is dispatched at the repo root can
    # already read and write anything nested under it.
    roots = [(_resolve(r), "another writer's allowlisted root") for r in policy.contained_by_roots]
    if main_worktree:
        roots.append((_resolve(main_worktree), "the repository's own main checkout"))
    containing = next(
        (
            (root, label)
            for root, label in roots
            if resolved != root and is_within(Path(resolved), Path(root))
        ),
        None,
    )
    facts["containing_allowlisted_root"] = None if containing is None else containing[0]
    if containing is not None:
        blockers.append(
            Reason(
                "nested_under_allowlisted_root",
                f"it is nested inside {containing[0]}, which is {containing[1]} — a writer "
                "dispatched there can already read and write this directory, so it is not ours "
                "to reclaim; relocate it to a sibling root (t16) and reap it there",
            )
        )

    owned = any(
        resolved != root and is_within(Path(resolved), Path(root))
        for root in (_resolve(r) for r in policy.permitted_roots)
    )
    facts["inside_permitted_root"] = owned
    if not owned:
        blockers.append(
            Reason(
                "outside_permitted_roots",
                "it is not a strict child of any root this host mints into "
                f"({', '.join(policy.permitted_roots) or 'none configured'})",
            )
        )

    if entry.get("locked"):
        blockers.append(Reason("locked_by_git", f"git holds a worktree lock: {entry['locked']}"))

    # Age BEFORE cleanliness, deliberately. Even with `--no-optional-locks`
    # the ordering is the durable guarantee: no probe below can make the
    # worktree look younger than the moment this reaper arrived.
    holds = _liveness_holds(resolved, _admin_dir(repo, resolved), policy, now, facts)

    if facts["exists"]:
        dirty = _dirty_blocker(resolved)
        if dirty is not None:
            blockers.append(dirty)
    else:
        blockers.append(
            Reason("worktree_missing", "the directory is gone but git does not report it prunable")
        )

    evidence, evidence_problem = _collect_evidence(repo, entry, policy)
    if evidence_problem is not None:
        blockers.append(evidence_problem)

    mint_ref: str | None = None
    candidate = REAP
    if not evidence and evidence_problem is None:
        head = entry.get("head")
        if head:
            candidate = PRESERVE_THEN_REAP
            mint_ref = mint_reap_ref(policy.preserve_branch_prefix, resolved, head)
        else:
            blockers.append(
                Reason(
                    "no_positive_evidence",
                    "no ref contains this worktree's work and it has no HEAD commit to mint one "
                    "from, so removal cannot be shown to be safe",
                )
            )

    if blockers:
        return Decision(
            path=path,
            decision=REFUSE,
            domain_outcome=RETAINED,
            evidence=evidence,
            blockers=tuple(blockers),
            holds=tuple(holds),
            facts=facts,
        )
    if holds:
        return Decision(
            path=path,
            decision=DEFER,
            domain_outcome=DEFERRED,
            evidence=evidence,
            holds=tuple(holds),
            facts=facts,
        )
    return Decision(
        path=path,
        decision=candidate,
        domain_outcome=RECLAIMED,
        evidence=evidence,
        facts=facts,
        command=("git", "worktree", "remove", resolved),
        mint_ref=mint_ref,
    )


def mint_reap_ref(prefix: str, worktree: str, head: str) -> str:
    """The ref `secure()` writes for an unreferenced detached worktree.
    Namespaced under the preserve prefix so the two mechanisms share one
    place an operator has to look for rescued work."""
    base = prefix if prefix.endswith("/") else prefix + "/"
    name = sanitize_ref_component(Path(worktree).name)
    return f"{base}reaped/{name}-{head[:12]}"


# --- the sweeper -------------------------------------------------------


def plan(
    repo: str,
    policy: ReapPolicy,
    *,
    now: float | None = None,
    only: Iterable[str] | None = None,
) -> dict[str, Any]:
    """The age-based orphan sweeper: assess every worktree of *repo*.

    *only* narrows it to specific paths — that is the follow-up-node shape,
    where one finished writer's own worktree is the single candidate.
    """
    listing = git_stdout(repo, "worktree", "list", "--porcelain")
    if listing is None:
        return {
            "repo": repo,
            "domain_outcome": RETAINED,
            "error": f"git worktree list could not be read in {repo}",
            "decisions": [],
            "counts": {},
        }
    entries = parse_worktree_list(listing)
    wanted = None if only is None else {_resolve(p) for p in only}
    self_paths = _self_paths()
    # git lists the MAIN worktree first; that ordering is the only signal
    # this output carries for which one it is.
    main_worktree = entries[0]["worktree"] if entries else None

    decisions: list[Decision] = []
    for index, entry in enumerate(entries):
        if wanted is not None and _resolve(entry["worktree"]) not in wanted:
            continue
        decisions.append(
            assess(
                repo,
                entry,
                policy,
                is_main=index == 0,
                main_worktree=main_worktree,
                now=now,
                self_paths=self_paths,
            )
        )

    counts: dict[str, int] = {}
    for decision in decisions:
        counts[decision.decision] = counts.get(decision.decision, 0) + 1
    outcome = RECLAIMED
    for decision in decisions:
        if _OUTCOME_RANK[decision.domain_outcome] > _OUTCOME_RANK[outcome]:
            outcome = decision.domain_outcome
    return {
        "repo": repo,
        "domain_outcome": outcome,
        "error": None,
        "decisions": [d.to_dict() for d in decisions],
        "counts": counts,
    }
