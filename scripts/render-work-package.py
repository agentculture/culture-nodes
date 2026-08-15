#!/usr/bin/env python3
"""Render a work-package brief from devague plan data and actor capabilities.

The plan is the work contract.  The capability document is the bridge's own
advertisement (the value registered under ``capabilities`` or returned by
``GET /v1/capabilities``).  This renderer adds no remembered host facts.
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent.parent


class BriefError(ValueError):
    pass


def read_json(source: str) -> dict[str, Any]:
    try:
        if source.startswith(("http://", "https://")):
            with urllib.request.urlopen(source, timeout=10) as response:
                value = json.load(response)
        else:
            with Path(source).open(encoding="utf-8") as stream:
                value = json.load(stream)
    except (OSError, json.JSONDecodeError, urllib.error.URLError) as exc:
        raise BriefError(f"cannot read JSON from {source}: {exc}") from exc
    if not isinstance(value, dict):
        raise BriefError(f"JSON from {source} must be an object")
    return value


def load_task(
    plan_slug: str, task_id: str, plan_dir: Path = ROOT / ".devague" / "plans"
) -> dict[str, Any]:
    plan = read_json(str(plan_dir / f"{plan_slug}.json"))
    if plan.get("slug") != plan_slug:
        raise BriefError(f"plan file declares slug {plan.get('slug')!r}, not {plan_slug!r}")
    tasks = plan.get("tasks")
    if not isinstance(tasks, list):
        raise BriefError("plan has no tasks list")
    for task in tasks:
        if isinstance(task, dict) and task.get("id") == task_id:
            return task
    raise BriefError(f"task {task_id!r} is not in plan {plan_slug!r}")


def capability_host(
    document: dict[str, Any], target_actor: str | None = None
) -> dict[str, Any]:
    # Accept both the bridge response and an actor registry row.
    actor_key = document.get("actor_key")
    if target_actor and actor_key is not None and actor_key != target_actor:
        raise BriefError(
            f"capability row belongs to actor {actor_key!r}, not {target_actor!r}"
        )
    capabilities = document.get("capabilities", document)
    if not isinstance(capabilities, dict):
        raise BriefError("capabilities must be an object")
    preflight = capabilities.get("preflight", capabilities)
    if not isinstance(preflight, dict):
        raise BriefError("capabilities.preflight must be an object")
    host = preflight.get("host")
    if not isinstance(host, dict) or not host:
        raise BriefError("capability document has no non-empty preflight.host block")
    return host


def bullets(values: list[str]) -> str:
    return "\n".join(f"- {value}" for value in values)


def render(
    plan_slug: str,
    task_id: str,
    actor: str,
    task: dict[str, Any],
    host: dict[str, Any],
    *,
    sandbox: str,
    repo: str,
    branch: str | None,
    base: str | None,
) -> str:
    summary = task.get("summary")
    instruction = task.get("instruction")
    acceptance = task.get("acceptance_criteria")
    covers = task.get("covers")
    if (
        not isinstance(summary, str)
        or not summary
        or not isinstance(instruction, str)
        or not instruction
    ):
        raise BriefError("task must have non-empty summary and instruction")
    if not isinstance(acceptance, list) or not all(
        isinstance(v, str) for v in acceptance
    ):
        raise BriefError("task acceptance_criteria must be a string list")
    if not isinstance(covers, list) or not all(isinstance(v, str) for v in covers):
        raise BriefError("task covers must be a string list")

    grants = host.get("dispatch_grants", {})
    if sandbox not in grants:
        available = ", ".join(grants) if isinstance(grants, dict) else "none"
        raise BriefError(
            f"actor does not advertise sandbox {sandbox!r}; advertised: {available or 'none'}"
        )
    mode_grants = grants[sandbox]
    if not isinstance(mode_grants, list):
        raise BriefError(f"dispatch_grants.{sandbox} must be a list")

    lines = [
        f"WORK PACKAGE {task_id} — {summary}",
        "",
        f"Plan: {plan_slug}",
        f"Target actor: {actor}",
        f"Repository: {repo}",
    ]
    if branch:
        lines.append(f"Branch: {branch}" + (f" at {base}" if base else ""))
    lines.extend(
        [
            "",
            "Instruction:",
            instruction,
            "",
            "Acceptance criteria:",
            bullets(acceptance) or "- none",
            "",
            "Coverage targets:",
            bullets(covers) or "- none",
            "",
            "Actor capability surface (advertised by the target bridge):",
            f"- hostname: {host.get('hostname', 'not advertised')}",
            f"- sandbox: {sandbox}",
            f"- dispatch grants: {', '.join(mode_grants) or 'none'}",
        ]
    )
    for key in ("confinement", "commit_policy", "writable_paths", "artifact_publish"):
        if key in host:
            value = (
                json.dumps(host[key], sort_keys=True)
                if not isinstance(host[key], str)
                else host[key]
            )
            lines.append(f"- {key.replace('_', ' ')}: {value}")
    tools = host.get("toolchains", [])
    if tools:
        lines.append("- toolchains:")
        for tool in tools:
            if isinstance(tool, dict):
                detail = ", ".join(
                    f"{key}={tool[key]}"
                    for key in ("state", "path", "packaging", "version")
                    if key in tool
                )
                usability = tool.get("usable_in", [])
                lines.append(
                    f"  - {tool.get('name', 'unnamed')}: {detail}; "
                    f"usable_in={json.dumps(usability)}"
                )

    lines.extend(["", "Checkout preparation:"])
    if "network-egress" in mode_grants:
        lines.append(
            "- This posture advertises network egress, but the source remote and "
            "revision remain dispatch inputs; pull the named branch before editing."
        )
    else:
        lines.append(
            "- This posture advertises no network egress. The agent cannot pull a "
            "remote checkout; the named branch/base must already exist locally. Any "
            "operator-side seeding remains a manual predecessor to dispatch."
        )
    lines.extend(
        [
            "",
            "Contract:",
            "- Every completion claim must name the command that produced it and its exit code.",
            "- Distinguish commands run here from checks that could not run under this capability surface.",
            "- Report claims as proposed, never confirmed or observed.",
        ]
    )
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="render a plan-grounded work-package brief")
    parser.add_argument("plan_slug")
    parser.add_argument("task_id")
    parser.add_argument("target_actor")
    parser.add_argument(
        "--capabilities",
        required=True,
        help="actor registry row or bridge capability JSON file/URL",
    )
    parser.add_argument("--sandbox", required=True)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--branch")
    parser.add_argument("--base")
    args = parser.parse_args(argv)
    try:
        task = load_task(args.plan_slug, args.task_id)
        host = capability_host(read_json(args.capabilities), args.target_actor)
        sys.stdout.write(
            render(
                args.plan_slug,
                args.task_id,
                args.target_actor,
                task,
                host,
                sandbox=args.sandbox,
                repo=args.repo,
                branch=args.branch,
                base=args.base,
            )
        )
    except BriefError as exc:
        parser.error(str(exc))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
