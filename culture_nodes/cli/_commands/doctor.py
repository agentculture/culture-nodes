"""``culture-nodes doctor`` — check the agent-identity invariants.

Mirrors the two invariants ``steward doctor`` verifies for a mesh agent:

* **prompt-file-present** — the repo declares an agent in ``culture.yaml`` and
  has the matching prompt file on disk;
* **backend-consistency** — the declared ``backend`` matches the prompt file
  (``claude`` → ``CLAUDE.md``, ``colleague`` → ``AGENTS.colleague.md``,
  ``acp`` → ``AGENTS.md``, ``gemini`` → ``GEMINI.md``).

Plus a **skills-present** check (the vendored ``.claude/skills/`` kit) and a
**nodes_api_reachable** check (a ``GET /v1alpha1/healthz`` probe against the
resolved API URL — see :mod:`culture_nodes.api_client`). Read-only.

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

from culture_nodes.api_client import add_api_url_argument, probe_health, resolve_base_url
from culture_nodes.cli._commands.whoami import find_culture_yaml, read_agent_fields
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
            "backend-consistency) and nodes API reachability."
        ),
    )
    p.add_argument("--json", action="store_true", help="Emit structured JSON.")
    add_api_url_argument(p)
    p.set_defaults(func=cmd_doctor)
