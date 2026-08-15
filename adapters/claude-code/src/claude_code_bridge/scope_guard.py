"""The workflow-scope boundary, enforced against what a session ACTUALLY
changed rather than against what its brief said (issue #98).

The boundary itself is real and stays: this bridge's fine-grained push
credential deliberately excludes GitHub Actions workflow administration, so
an actor here may not modify `.github/workflows/`. What was wrong was the
enforcement point. The original guard `grep`ed the INSTRUCTION TEXT for the
path fragment before dispatch, which made it both over- and under-inclusive
at the same time:

* over-inclusive — a brief whose safest line is `Do NOT touch
  .github/workflows/**` was refused *for naming the boundary*. Hit live:
  Culture Nodes run `01M039KA0QQ73XM3WQCQEQF1CN` was answered 403 before any
  model was dispatched, and `01M029WMMF8DVHCZX1V4ECW8B6` before it. The
  workaround — stop mentioning CI in briefs — makes dispatches less safe,
  not more;
* under-inclusive — a session that was never told about CI and edits
  `.github/workflows/ci.yml` mid-run passed the check, because the string
  never appeared in the prompt. The guard fired on discussion and missed the
  act.

So the check now runs on paths THIS bridge measured with its own `git`
subprocesses, bracketing the session. Never `task_result`'s own
model-reported file list — that is a completion claim, and a guard that
trusts the guarded party is not a guard.

It reads two measurements, because neither alone is complete:

1. `workspace.measure()`'s `changed_files`, which covers everything tracked
   at `head_before` and everything the session COMMITTED during its turn;
2. one targeted `git status --porcelain --untracked-files=all -- <guarded
   prefixes>`, because `measure()` reports `git status`'s default collapsed
   form — a session that creates `.github/workflows/go.yml` in a repo with
   no `.github/` yet shows up there as the single entry `.github/`, which
   names no guarded path at all. Scoping the second probe to the guarded
   prefixes is what keeps this cheap: expanding every untracked directory
   would put a `.venv/` or a `node_modules/` into a payload the control
   plane has to store.

Enforcement shape, both dispatch paths:

* synchronous — `server.py` replaces the response with a 403
  `auth_or_policy` refusal *before* its preserve hook runs, so the refused
  work lands on a preserve branch instead of evaporating;
* asynchronous — `async_runner.py` replaces the terminal event with a
  `failed` one carrying the same class, which reaches the same preserve
  hook for the same reason.

Two honest limits, both deliberate:

1. The session already ran. This is a post-hoc refusal — it stops the change
   being reported as a success and preserves it for a human, but it does not
   un-write the file. Un-writing is the sandbox's job, not the boundary's.
2. A workspace that could not be measured (`measured: False` — no git, not a
   working tree, an unborn HEAD) yields NO violations. The bridge refuses to
   invent a verdict it did not measure; issue #98's option 2, a harvest/merge
   guard, is what covers that gap, and it is not this module.

Mirrored field-for-field by `codex_bridge.scope_guard` and
`colleague_bridge.scope_guard` (the all-backends rule in CLAUDE.md — the
original guard existed in this bridge alone, which was itself part of the
bug).
"""

from __future__ import annotations

from typing import Any, Iterable

from claude_code_bridge import workspace

#: Repo-relative path prefixes this actor may not modify. Directory
#: prefixes end in "/" and match everything beneath them; an entry without a
#: trailing "/" matches that one exact path.
GUARDED_PATH_PREFIXES: tuple[str, ...] = (".github/workflows/",)

#: The §13.2 error class for a boundary refusal — the same one the
#: pre-dispatch repo-allowlist refusal uses, because it is the same kind of
#: answer: this actor is not authorized for this work.
REFUSAL_CLASS = "auth_or_policy"


def _normalize(path: str) -> str:
    """One repo-relative path in the form the prefixes are written in.

    Git reports POSIX separators already; the backslash fold is for a
    caller (a Windows-side measurement, a hand-built payload in a test) that
    does not. A leading `./` is stripped iteratively rather than with
    `lstrip("./")`, which would eat the leading dot of `.github` itself.
    """
    normalized = path.replace("\\", "/").strip()
    while normalized.startswith("./"):
        normalized = normalized[2:]
    return normalized


def guarded(paths: Iterable[str]) -> tuple[str, ...]:
    """The pure matcher: which of *paths* fall inside a guarded prefix,
    de-duplicated, in first-seen order."""
    hits: list[str] = []
    for entry in paths:
        if not isinstance(entry, str):
            continue
        candidate = _normalize(entry)
        if not candidate:
            continue
        for prefix in GUARDED_PATH_PREFIXES:
            matched = (
                candidate.startswith(prefix)
                if prefix.endswith("/")
                else candidate == prefix or candidate.startswith(prefix + "/")
            )
            if matched and candidate not in hits:
                hits.append(candidate)
                break
    return tuple(hits)


def _untracked_under_guarded_prefixes(repo: str) -> tuple[str, ...]:
    """Guarded paths `git status` would otherwise report only as a collapsed
    parent directory. Runs through `workspace`'s own bounded,
    never-raising git helper (the same one `reap.py` reuses) so a git that
    cannot run degrades to "nothing measured" instead of crashing a
    dispatch."""
    status = workspace.git_stdout(
        repo,
        "status",
        "--porcelain",
        "--untracked-files=all",
        "--",
        *GUARDED_PATH_PREFIXES,
    )
    if not status:
        return ()
    found: list[str] = []
    for line in status.splitlines():
        # Porcelain v1: two status columns, a space, then the path. A
        # rename's " -> " form cannot appear here (renames are tracked
        # changes, and those already come from `changed_files`), so the
        # tail is one path.
        entry = line[3:].strip()
        if entry:
            found.append(entry)
    return guarded(found)


def violations(repo: str | None, workspace_measured: dict[str, Any] | None) -> tuple[str, ...]:
    """The guarded paths this session actually changed.

    Empty means "no measured violation", which covers both the ordinary
    clean session and the unmeasurable workspace — see this module's
    docstring, limit 2, for why those two are deliberately not
    distinguished here.
    """
    if not isinstance(workspace_measured, dict) or not workspace_measured.get("measured"):
        return ()

    changed = workspace_measured.get("changed_files")
    hits = list(guarded(changed if isinstance(changed, list) else ()))
    if repo:
        for entry in _untracked_under_guarded_prefixes(repo):
            if entry not in hits:
                hits.append(entry)
    return tuple(hits)


def refusal_message(paths: tuple[str, ...]) -> str:
    """The operator-facing sentence. It names the paths that were MEASURED
    as changed, so nobody has to go looking for which substring of a
    60-line brief tripped an allowlist — the old refusal's third failure
    (#98) was being opaque about its own cause."""
    changed = ", ".join(paths)
    return (
        "workflow-scope boundary: this actor may not modify "
        + ", ".join(GUARDED_PATH_PREFIXES)
        + f"; this session changed {changed}. The change is preserved, not merged — "
        "split that work into a separately authorized package."
    )


def refusal_body(paths: tuple[str, ...], workspace_measured: dict[str, Any]) -> dict[str, Any]:
    """The synchronous 403 body. `workspace_measured` rides along for the
    same reason every other branch of `mapping.sync_response` carries it:
    the workspace changed, and a reader of the refusal needs to see what
    was measured, not just that something was."""
    return {
        "error": refusal_message(paths),
        "class": REFUSAL_CLASS,
        "scope_violations": list(paths),
        "workspace_measured": workspace_measured,
    }


def refusal_payload(paths: tuple[str, ...], workspace_measured: dict[str, Any]) -> dict[str, Any]:
    """The asynchronous `failed` terminal payload, shaped like every other
    failure `mapping.terminal_event` produces."""
    return {
        "class": REFUSAL_CLASS,
        "message": refusal_message(paths),
        "detail": "refused on the bridge-measured change set, not on the instruction text",
        "scope_violations": list(paths),
        "workspace_measured": workspace_measured,
    }
