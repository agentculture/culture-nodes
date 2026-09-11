"""``culture-nodes doctor`` — check the agent-identity invariants.

Mirrors the two invariants ``steward doctor`` verifies for a mesh agent:

* **prompt-file-present** — the repo declares an agent in ``culture.yaml`` and
  has the matching prompt file on disk;
* **backend-consistency** — the declared ``backend`` matches the prompt file
  (``claude`` → ``CLAUDE.md``, ``colleague`` → ``AGENTS.colleague.md``,
  ``acp`` → ``AGENTS.md``, ``gemini`` → ``GEMINI.md``).

Plus a **skills-present** check (the vendored ``.claude/skills/`` kit), a
**nodes_api_reachable** check (a ``GET /v1alpha1/healthz`` probe against the
resolved API URL — see :mod:`culture_nodes.api_client`), an
**unprivileged_userns** check (whether a bwrap-backed actor sandbox can start
on this host at all), and a **lane_liveness** check (which registered actor
lanes the control plane currently measures as dead — a spent refresh token
behind a bridge that still answers ``/healthz`` 200; issue #308). Read-only.

Reports the rubric-shaped contract
``{healthy, checks: [{id, passed, severity, message, remediation}]}`` so the
agent-first rubric's bundle 7 passes. ``healthy`` is derived only from
``severity == "error"`` checks — ``warning``/``info`` checks can fail
without flipping the overall verdict or the exit code. This matters for
``nodes_api_reachable`` in particular: the CLI's identity verbs
(whoami/learn/explain/overview) work with no API running at all, so an
unreachable API is reported (with a remediation) but never fails ``doctor``.
"""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import NamedTuple

from culture_nodes.api_client import (
    API_PREFIX,
    ApiClient,
    add_api_url_argument,
    probe_health,
    resolve_base_url,
)
from culture_nodes.cli._commands.whoami import find_culture_yaml, read_agent_fields
from culture_nodes.cli._errors import CliError
from culture_nodes.cli._output import emit_result

# backend → required prompt file (the backend-consistency mapping).
_PROMPT_FILE = {
    "claude": "CLAUDE.md",
    "colleague": "AGENTS.colleague.md",
    "acp": "AGENTS.md",
    "gemini": "GEMINI.md",
}


def _identity_checks(cfg) -> list[dict[str, object]]:
    """The culture.yaml-derived checks (backend/prompt-file/skills), split out
    of _diagnose to keep each function's branching readable (S3776)."""
    checks: list[dict[str, object]] = []
    root = cfg.parent
    fields = read_agent_fields()
    backend = fields["backend"]

    # 1. backend-consistency: the prompt file for the declared backend exists.
    expected = _PROMPT_FILE.get(backend)
    if expected is None:
        checks.append(
            {
                "id": "backend_consistency",
                "passed": False,
                "severity": "error",
                "message": f"unknown backend '{backend}' in culture.yaml",
                "remediation": f"set backend to one of: {', '.join(sorted(_PROMPT_FILE))}",
            }
        )
    else:
        present = (root / expected).is_file()
        checks.append(
            {
                "id": "prompt_file_present",
                "passed": present,
                "severity": "error",
                "message": (
                    f"backend '{backend}' requires {expected} — "
                    + ("present" if present else "missing")
                ),
                "remediation": "" if present else f"create {expected} at the repo root",
            }
        )

    # 2. skills-present: the vendored skill kit is on disk.
    skills_dir = root / ".claude" / "skills"
    has_skills = skills_dir.is_dir() and any(skills_dir.iterdir())
    checks.append(
        {
            "id": "skills_present",
            "passed": has_skills,
            "severity": "warning",
            "message": (
                ".claude/skills/ vendored" if has_skills else ".claude/skills/ missing or empty"
            ),
            "remediation": (
                "" if has_skills else "vendor the skill kit (see docs/skill-sources.md)"
            ),
        }
    )

    return checks


#: sysctl → the value that means "restricted". Ubuntu's AppArmor gate (24.04+)
#: and the older Debian-family knob; either set against us breaks bwrap.
_USERNS_SYSCTLS = (
    ("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1"),
    ("/proc/sys/kernel/unprivileged_userns_clone", "0"),
)


def _userns_check(probes: tuple[tuple[str, str], ...] = _USERNS_SYSCTLS) -> dict[str, object]:
    """Report whether unprivileged user namespaces are available.

    Not an identity invariant — an environment one, and it is here because a
    dispatched actor otherwise learns it the expensive way. Codex's
    ``--sandbox workspace-write`` confines file writes with a bubblewrap
    helper; where the kernel refuses unprivileged user namespaces that helper
    cannot start, so *every* ``apply_patch`` fails while shell commands still
    run unconfined. The actor reads fine, writes nothing, and burns a session
    retrying patches before anyone notices. Ubuntu 24.04 ships the restriction
    on by default, which is how this reached three hosts at once.

    Read-only and stdlib-only: the sysctls are the fact, so read them rather
    than shelling out to ``bwrap`` to find out. ``probes`` is injectable so
    tests can assert the logic on both kinds of kernel rather than on
    whichever one happens to be running the suite.
    """
    blockers = []
    for path, blocking_value in probes:
        try:
            value = Path(path).read_text().strip()
        except OSError:
            # Absent knob means this kernel does not restrict here.
            continue
        if value == blocking_value:
            blockers.append(f"{Path(path).name}={value}")

    available = not blockers
    return {
        "id": "unprivileged_userns",
        "passed": available,
        "severity": "warning",
        "message": (
            "unprivileged user namespaces available — bwrap-backed actor sandboxes work"
            if available
            else (
                "unprivileged user namespaces restricted (" + ", ".join(blockers) + ") — "
                "a bwrap-backed sandbox cannot start here, so codex "
                "--sandbox workspace-write silently loses ALL file writes "
                "while still running shell commands unconfined"
            )
        ),
        "remediation": (
            ""
            if available
            else (
                "dispatch codex actors on this host with --sandbox danger-full-access and "
                "isolate with a git worktree or container instead; or grant bwrap an AppArmor "
                "profile. Never assume --sandbox workspace-write is enforcing here"
            )
        ),
    }


def _newest_rows(items: list) -> dict[str, dict]:
    """The current revision of every actor_key: the listing carries every
    append-only revision, and only the newest one's fact is the lane's."""
    newest: dict[str, dict] = {}
    for row in items:
        if not isinstance(row, dict) or not isinstance(row.get("actor_key"), str):
            continue
        key = row["actor_key"]
        rev = row.get("revision") if isinstance(row.get("revision"), int) else 0
        prev = newest.get(key)
        if prev is None or rev >= prev.get("revision", 0):
            newest[key] = {**row, "revision": rev}
    return newest


def _fetch_actor_rows(base_url: str, timeout: float) -> tuple[list | None, str]:
    """GET the actors listing without raising: ``(items, detail)``; ``items``
    is ``None`` when the API gave no readable answer."""
    try:
        resp = ApiClient(base_url, timeout=timeout).request("GET", f"{API_PREFIX}/actors")
    except CliError as err:
        return None, err.message
    payload = resp.payload
    items = payload.get("items") if isinstance(payload, dict) else None
    if not isinstance(items, list):
        return None, f"GET {API_PREFIX}/actors returned no actor listing (HTTP {resp.status})"
    return items, ""


class _LaneTally(NamedTuple):
    """How the newest revision of each actor's liveness fact classified.

    Three buckets, never two (code-review finding 8): ``unsure`` holds a lane
    whose fact is present but whose ``session_ok`` is ``null``, which is
    neither live nor dead and must never be folded into an all-clear.
    """

    live: int
    unsure: list[str]
    dead: list[str]

    @property
    def measured(self) -> int:
        """Lanes that carried a liveness fact at all — every one lands in
        exactly one of the three buckets."""
        return self.live + len(self.unsure) + len(self.dead)


def _describe_lane(key: str, state: str, fact: dict) -> str:
    return (
        f"{key} ({state}, reason={fact.get('reason', 'unmeasured')}, "
        f"mode={fact.get('mode', '-')}, checked_at={fact.get('checked_at', '-')})"
    )


def _tally_lanes(newest: dict[str, dict]) -> _LaneTally:
    """Sort each actor's newest liveness fact into live / unmeasured / dead.

    A lane with no ``liveness`` field is not measured at all, so it counts
    towards none of the three — the caller reads that as ``unmeasured``.
    """
    live = 0
    unsure: list[str] = []
    dead: list[str] = []
    for key, row in sorted(newest.items()):
        fact = row.get("liveness")
        if not isinstance(fact, dict):
            continue
        if fact.get("locked") is True:
            dead.append(_describe_lane(key, "locked", fact))
        elif fact.get("session_ok") is False:
            dead.append(_describe_lane(key, "session_ok=false", fact))
        elif fact.get("session_ok") is True:
            live += 1
        else:
            unsure.append(_describe_lane(key, "session_ok=null", fact))
    return _LaneTally(live=live, unsure=unsure, dead=dead)


def _liveness_check(*, passed: bool, message: str, remediation: str) -> dict[str, object]:
    return {
        "id": "lane_liveness",
        "passed": passed,
        "severity": "warning",
        "message": message,
        "remediation": remediation,
    }


def _lane_liveness_check(base_url: str, *, timeout: float = 2.0) -> dict[str, object]:
    """Report which registered actor lanes the control plane measures as dead.

    Why this is a doctor check (issue #308, loop-closure t11): on 2026-09-07
    both codex bridges answered ``/healthz`` 200 and ``codex login status``
    printed "Logged in using ChatGPT", and minutes later every dispatch
    failed with "refresh token was revoked". Neither of those signals is the
    fact; the bridge's ``liveness`` probe is (t9), and the control plane
    exposes each actor row's copy of it on ``GET /v1alpha1/actors`` as
    ``liveness: {session_ok, reason, mode, checked_at, locked}`` (t10). An
    operator about to fan out reads it here, before a session is billed.

    Three honest answers, none of them an error (only ``prompt_file_present``
    decides ``healthy``): a lane with ``session_ok=false`` or ``locked=true``
    fails the check BY KEY AND REASON; an unreachable API or a listing that
    carries no ``liveness`` field yet is ``unmeasured`` — a stale or unmeasured
    lane is never a verdict (c26), so the field's absence passes and says so,
    while an unreachable API fails so the non-answer is visible next to
    ``nodes_api_reachable``.

    A lane whose fact IS present but whose ``session_ok`` is ``null`` is the
    third state, and it is neither of the other two: it is counted and named
    ``unmeasured``, never folded into the all-clear (code-review finding 8 —
    this used to tally "measured" against "dead" only, so one lane whose probe
    returned no verdict was reported as "all live (session_ok=true, none
    locked)", which is the very claim the three-valued fact exists to avoid).
    """
    items, detail = _fetch_actor_rows(base_url, timeout)
    if items is None:
        return _liveness_check(
            passed=False,
            message=f"lane liveness unmeasured: {detail}",
            remediation=(
                "see nodes_api_reachable above; the liveness fact is read off the control "
                "plane's actor listing, so nothing about the lanes can be said without it"
            ),
        )

    newest = _newest_rows(items)
    lanes = _tally_lanes(newest)
    unsure, dead = lanes.unsure, lanes.dead

    if lanes.measured == 0:
        return _liveness_check(
            passed=True,
            message=(
                f"lane liveness unmeasured: {len(newest)} actor(s) registered, "
                "none carries a liveness fact (no liveness field on the actor listing)"
            ),
            remediation="",
        )
    tally = (
        f"{lanes.measured} lane(s) measured: {lanes.live} live, "
        f"{len(unsure)} unmeasured, {len(dead)} dead"
    )
    if unsure:
        # Named in the MESSAGE, not only the remediation: the check passes
        # when nothing is dead, and the text renderer prints a hint only for a
        # failing check, so a remediation-only note would be invisible exactly
        # when the operator is about to fan out into the lane.
        tally += "; unmeasured: " + "; ".join(unsure)
    if dead:
        return _liveness_check(
            passed=False,
            message=f"{tally}; dead: " + "; ".join(dead),
            remediation=(
                "do not put these lanes in a split plan. Restore one with an interactive "
                "engine re-login on the bridge host as the engine account (codex: `codex login`; "
                "reason=not_logged_in means that lane never had a session on this host at all, "
                "so the same login bootstraps it), "
                "then re-copy the credential (deploy/prod/lanes/unix-user.sh bootstrap) and "
                "restart the bridge so its start-up probe clears the latch. Meanwhile route to "
                "the lane's registered fallback_actor (register-actor.sh --metadata "
                "fallback_actor=<actor_key>) or another live actor"
            ),
        )
    if unsure:
        return _liveness_check(
            passed=True,
            message=tally,
            remediation=(
                "these lanes have a liveness fact but no verdict, so nothing about their "
                "sessions is known — an unmeasured lane is never refused (c26) and never "
                "reported live. Set the lane's bridge to CHECK mode "
                "(CODEX_BRIDGE_LIVENESS_MODE=CHECK) so it probes before a dispatch, or read "
                "the reason: probe_timeout/probe_failed means the probe itself could not run"
            ),
        )
    return _liveness_check(
        passed=True,
        message=f"{tally} (session_ok=true, none locked)",
        remediation="",
    )


def _diagnose(base_url: str) -> dict[str, object]:
    cfg = find_culture_yaml()
    if cfg is None:
        checks: list[dict[str, object]] = [
            {
                "id": "source_checkout",
                "passed": True,
                "severity": "info",
                "message": "no culture.yaml found alongside the package; identity checks skipped",
                "remediation": "",
            }
        ]
    else:
        checks = _identity_checks(cfg)

    # 3. nodes_api_reachable: warn (never fail) when the API is unreachable —
    # this CLI is a thin client, not the API server, and identity verbs work
    # offline.
    reachable, detail = probe_health(base_url)
    checks.append(
        {
            "id": "nodes_api_reachable",
            "passed": reachable,
            "severity": "warning",
            "message": (
                f"nodes API reachable at {base_url}"
                if reachable
                else f"nodes API not reachable at {base_url} ({detail})"
            ),
            "remediation": (
                ""
                if reachable
                else "start it with 'nodes serve' (Go binary) or pass --api-url; "
                "identity verbs (whoami/learn/explain/overview) work with no API running"
            ),
        }
    )

    # 4. unprivileged_userns: an environment fact a dispatched actor needs
    # BEFORE it picks a sandbox mode, not after it has wasted a session.
    checks.append(_userns_check())

    # 5. lane_liveness: which actor lanes the control plane measures as dead,
    # read BEFORE a fan-out bills a session into one (issue #308).
    checks.append(_lane_liveness_check(base_url))

    healthy = all(c["passed"] for c in checks if c["severity"] == "error")
    return {"healthy": healthy, "checks": checks}


def cmd_doctor(args: argparse.Namespace) -> int:
    base_url = resolve_base_url(getattr(args, "api_url", None))
    report = _diagnose(base_url)
    json_mode = bool(getattr(args, "json", False))
    if json_mode:
        emit_result(report, json_mode=True)
    else:
        status = "healthy" if report["healthy"] else "unhealthy"
        lines = [f"culture-nodes doctor: {status}", ""]
        for check in report["checks"]:
            mark = "ok" if check["passed"] else "FAIL"
            lines.append(f"[{mark}] {check['id']}: {check['message']}")
            if not check["passed"] and check["remediation"]:
                lines.append(f"  hint: {check['remediation']}")
        emit_result("\n".join(lines), json_mode=False)
    return 0 if report["healthy"] else 1


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "doctor",
        help=(
            "Check the agent-identity invariants (prompt-file-present, "
            "backend-consistency), nodes API reachability, the userns sysctl, "
            "and which actor lanes are measured dead (lane_liveness)."
        ),
    )
    p.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(p)
    p.set_defaults(func=cmd_doctor)
