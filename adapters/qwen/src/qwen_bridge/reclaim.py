"""The worktree reaper's MUTATING half (task t17).

`reap.py` decides and never changes anything: `plan()` and `assess()` run
git read-only and return records. Everything that can alter a repository or
a filesystem lives here, in one module, so "the reaper does not destroy
things by accident" is a claim you can check by reading which file a
function is in rather than by auditing every function in a long one.

Three entry points, in increasing order of how much they can do:

* `secure()` — one `git update-ref` on a ref that does not exist yet, for a
  `preserve_then_reap` decision. Writes no working tree, no index, no HEAD.
  This is the step that RESCUES work, and it is the only mutation the
  module performs that cannot lose anything.
* `execute()` — one decision, and only with an explicit `perform=True`.
  Re-derives the decision from live state first, because a plan is a
  snapshot and a writer can come back to life in the gap.
* `sweep()` — plan the whole repo, then (again only with `perform=True`)
  secure and reclaim everything the plan cleared.

`--force` is passed nowhere here either; `tests/lint/workspacereaper_test.go`
guards both modules. A removal git refuses becomes the domain outcome
`retained`, and the worktree stays exactly as it was.

Mirrored byte-for-byte by `codex_bridge.reclaim` and
`colleague_bridge.reclaim` (all-backends rule); the relative import below
is what keeps the three copies identical.
"""

from __future__ import annotations

from typing import Any

from .reap import (
    _OUTCOME_RANK,
    PRESERVE_THEN_REAP,
    PRUNE,
    REAP,
    RECLAIMED,
    RETAINED,
    Decision,
    Evidence,
    ReapPolicy,
    Reason,
    git_stdout,
    plan,
    run_git,
)


def secure(repo: str, decision: Decision) -> dict[str, Any]:
    """Mint `decision.mint_ref` at the worktree's HEAD.

    One `git update-ref` on a ref that does not exist yet: preserve.py's
    doctrine, minus even the commit — HEAD, the index and the working tree
    are never written. Verified by reading the ref back, because a
    `update-ref` that returned 0 is not the same fact as a ref that exists.
    """
    if decision.decision != PRESERVE_THEN_REAP or not decision.mint_ref:
        return {
            "secured": False,
            "domain_outcome": RETAINED,
            "reason": f"nothing to secure for a {decision.decision} decision",
        }
    head = decision.facts.get("head")
    ref = f"refs/heads/{decision.mint_ref}"
    if not head:
        return {"secured": False, "domain_outcome": RETAINED, "reason": "no HEAD to point a ref at"}
    if git_stdout(repo, "rev-parse", "--verify", "--quiet", ref) is not None:
        return {
            "secured": False,
            "domain_outcome": RETAINED,
            "reason": f"{ref} already exists; refusing to move an existing ref",
        }
    proc = run_git(repo, "update-ref", ref, head)
    if proc is None or proc.returncode != 0:
        detail = "git update-ref could not run" if proc is None else proc.stderr.strip()
        return {"secured": False, "domain_outcome": RETAINED, "reason": detail}
    readback = git_stdout(repo, "rev-parse", "--verify", "--quiet", ref)
    if readback is None or readback.strip() != head:
        return {
            "secured": False,
            "domain_outcome": RETAINED,
            "reason": f"{ref} did not read back as {head[:12]} after update-ref",
        }
    return {"secured": True, "domain_outcome": RECLAIMED, "ref": ref, "commit": head}


def execute(
    repo: str,
    decision: Decision,
    policy: ReapPolicy,
    *,
    perform: bool = False,
    now: float | None = None,
) -> dict[str, Any]:
    """Perform one decision — and ONLY when explicitly asked.

    `perform=False` (the default, and what every plan/CLI path uses)
    returns the command and changes nothing. With `perform=True` the
    decision is re-derived from live state first: a plan is a snapshot, and
    a writer may have dirtied the worktree between the sweep and the
    removal.

    `git worktree remove` runs WITHOUT `--force`. If git refuses, that
    refusal becomes the domain outcome `retained` and the directory stays.
    """
    if decision.decision not in {REAP, PRUNE}:
        return {
            "performed": False,
            "domain_outcome": decision.domain_outcome,
            "reason": f"{decision.decision} is not a performable decision",
            "command": list(decision.command) if decision.command else None,
        }
    if not perform:
        return {
            "performed": False,
            "domain_outcome": decision.domain_outcome,
            "reason": "dry run: pass perform=True to remove",
            "command": list(decision.command) if decision.command else None,
        }

    fresh = plan(repo, policy, now=now, only=[decision.path])
    current = fresh["decisions"][0] if fresh["decisions"] else None
    if current is None or current["decision"] != decision.decision:
        return {
            "performed": False,
            "domain_outcome": RETAINED,
            "reason": (
                "the worktree changed between the plan and the removal; it now decides "
                f"{current['decision'] if current else 'unknown'}"
            ),
            "command": list(decision.command) if decision.command else None,
            "recheck": current,
        }
    if current["facts"].get("head") != decision.facts.get("head"):
        return {
            "performed": False,
            "domain_outcome": RETAINED,
            "reason": "HEAD moved between the plan and the removal",
            "command": list(decision.command) if decision.command else None,
            "recheck": current,
        }

    if decision.command is None:
        # Unreachable for a reap/prune decision built by `assess`, but this
        # module answers rather than raises even when it is handed one it
        # did not build.
        return {
            "performed": False,
            "domain_outcome": RETAINED,
            "reason": f"a {decision.decision} decision arrived with no command to run",
            "command": None,
        }
    proc = run_git(repo, *decision.command[1:])
    if proc is None or proc.returncode != 0:
        detail = "git could not run" if proc is None else proc.stderr.strip()
        return {
            "performed": False,
            "domain_outcome": RETAINED,
            "reason": detail or "git refused the removal",
            "command": list(decision.command),
        }
    return {
        "performed": True,
        "domain_outcome": RECLAIMED,
        "command": list(decision.command),
        "reason": None,
    }


def sweep(
    repo: str,
    policy: ReapPolicy,
    *,
    perform: bool = False,
    secure_unreferenced: bool = True,
    now: float | None = None,
) -> dict[str, Any]:
    """Plan, then (only with `perform=True`) reclaim what the plan cleared.

    `secure_unreferenced` mints the rescue ref for a `preserve_then_reap`
    candidate and reaps it once the ref reads back — the mint is what turns
    it into a `reap`, so a failure to mint leaves the worktree standing.
    """
    planned = plan(repo, policy, now=now)
    if not perform:
        return planned

    results: list[dict[str, Any]] = []
    for raw in planned["decisions"]:
        decision = _decision_from_dict(raw)
        if decision.decision == PRESERVE_THEN_REAP and secure_unreferenced:
            secured = secure(repo, decision)
            results.append({"path": decision.path, "step": "secure", **secured})
            if not secured.get("secured"):
                continue
            refreshed = plan(repo, policy, now=now, only=[decision.path])
            if not refreshed["decisions"]:
                continue
            decision = _decision_from_dict(refreshed["decisions"][0])
        outcome = execute(repo, decision, policy, perform=True, now=now)
        results.append({"path": decision.path, "step": "execute", **outcome})

    performed = [r for r in results if r.get("step") == "execute"]
    outcome = RECLAIMED
    for result in performed:
        rank = _OUTCOME_RANK[result["domain_outcome"]]
        if rank > _OUTCOME_RANK[outcome]:
            outcome = result["domain_outcome"]
    planned["performed"] = results
    planned["domain_outcome"] = outcome if performed else planned["domain_outcome"]
    return planned


def _decision_from_dict(raw: dict[str, Any]) -> Decision:
    return Decision(
        path=raw["path"],
        decision=raw["decision"],
        domain_outcome=raw["domain_outcome"],
        evidence=tuple(Evidence(**e) for e in raw.get("evidence", ())),
        blockers=tuple(Reason(**r) for r in raw.get("blockers", ())),
        holds=tuple(Reason(**r) for r in raw.get("holds", ())),
        facts=dict(raw.get("facts", {})),
        command=tuple(raw["command"]) if raw.get("command") else None,
        mint_ref=raw.get("mint_ref"),
    )
