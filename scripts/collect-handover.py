#!/usr/bin/env python3
"""Turn a RUN ID into a reviewable diff, and gate a merge on a real suite.

    scripts/collect-handover.py <run-id>            # fetch and show what changed
    scripts/collect-handover.py <run-id> --json
    scripts/collect-handover.py <run-id> --gate --suite 'go test ./...' -- go test ./...

# What this replaces

A hand-typed ``git fetch ssh://<host>/<path> <branch>``, used fifteen times
this cycle. Every one of those needed three facts the operator had to already
know: which machine the session ran on, where its checkout lives, and what the
ref or branch was called. The first is in the control plane, the second is the
operator's own configuration, and the third does not have to be known at all —
a handover ref is minted as
``refs/culture-nodes/<run-id>/<node-run-id>-<attempt-id>-<UTC>-<short-sha>``
(``codex_bridge.preserve.mint_handover_ref``), so the run id plus a wildcard
finds every attempt's handover. **A run id is the only thing an operator
should have to type.**

# Two fences, taken verbatim from internal/handover/doc.go

1. **The remote is the CONTROL PLANE's configuration and never the agent's
   report.** This script reads the run to learn WHICH ACTOR ran it, and the
   actor registry to learn where that actor is; it never reads the run's
   output, its handle, or any remote a session named for itself. A session
   that could point the fetch at a repository it prepared would make the
   measurement real and the subject forged.
2. **Only ``refs/culture-nodes/`` is ever fetched.** The refspec is built from
   constants plus the validated run id, so there is no input that turns this
   into a fetch of a release branch.

The remote itself comes from one of two places, both configuration:
``actor.metadata.handover_remote`` in the registry (the control plane's own
record, per actor, and the recommended one), or the operator's
``NODES_HANDOVER_REMOTE_TEMPLATE`` with ``{host}`` substituted from the
actor's registered ``endpoint_ref``. Neither present is a refusal, never a
guess.

# No ref is ambiguous, and is reported as ambiguous

Before issue #120 was fixed, the deployed bridges predated the code that mints
handover refs, so a session could succeed, commit, and leave no ref —
indistinguishable from a session that simply had nothing to hand over. Both
readings are still live for any run dispatched to a host whose bridge has not
been reinstalled. This script therefore exits non-zero and names BOTH
possibilities rather than picking one. Guessing here is how a lost handover
becomes "the agent did nothing".

# The gate half (task t11, issue #101)

``--gate`` runs a suite against the collected commit, in a detached worktree
at that exact commit, and records what happened as a ``derived`` ledger record
(PRD §10.4: a test suite is a deterministic validator; an operator reading a
green tick is not evidence of anything). The record names the suite, the exit
code, and the commit sha — and the sha is READ BACK from the worktree the
suite actually ran in rather than assumed, because a verdict that does not
name what it tested is not evidence.

# The routing half (task t32, issue #102)

A failing gate no longer ends in the operator's session. The control plane
routes it — to a bounded repair attempt on a lane whose advertised capability
surface shows it can actually run the suite that failed, or to a human — and
this prints the answer with its bound: **two repair attempts per run, over a
24-hour window from the run's first gate rejection, and a human node at either
ceiling**. Nine packages in the own-the-work-end-to-end batch failed their gate
and were repaired by hand, with no record of how many rounds any of them took;
after this that count is a ledger query.

Two refusals are worth knowing before running it. A failure implicating
`.github/` routes to a human, because a repair attempt is a dispatch and a
dispatch may not modify CI configuration — pass `--implicates <path>` to
declare one the control plane could not measure. And a lane that cannot run
the failing suite is never sent a repair, because a fix it cannot verify is
another unverified claim; `--requires-grant network-egress` is how a
database-backed suite says so.

The routing is a DECISION, not an execution. Nothing is dispatched, the record
says so in its own payload, and this prints that line every time. Unattended
repair is deliberately not enabled — see `internal/repair`'s package doc for
why, and #18/#119 for the measurements behind it.

Exit codes follow culture_nodes/cli/_errors.py: 0 success, 1 user/domain
outcome (no ref collected; the suite failed), 2 environment (no control plane,
no configured remote, no validator identity). A routed failure still exits 1:
the gate rejected, and the routing is what happens next, not a pass.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess  # noqa: S404 # nosec B404 - fixed `git` binary, constant argv, no shell
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit

DEFAULT_API = "http://192.168.1.146:18080"

#: The only namespace fetched, matching internal/handover's RefNamespace and
#: the bridges' preserve.HANDOVER_REF_NAMESPACE. Both halves of the fence are
#: needed: t9 stops a session pushing outside it, this stops the operator
#: looking outside it.
REF_NAMESPACE = "refs/culture-nodes"

#: Where collected refs land in the operator's repository. Deliberately NOT
#: under refs/heads or refs/remotes: nothing here is a branch, nothing is
#: checked out, and `git log --all` still shows it.
LOCAL_NAMESPACE = "refs/handover"

#: A run id reaches a git refspec, so it is validated as a ref component
#: before it does. ULIDs pass; `..`, a leading `-`, and a `/` do not.
_RUN_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")

GIT_TIMEOUT_SECONDS = 30.0
FETCH_TIMEOUT_SECONDS = 180.0
HTTP_TIMEOUT_SECONDS = 30.0


class Refusal(Exception):
    """A refusal to proceed, carrying the exit code it should produce.

    `code` is 1 for a user/domain outcome and 2 for an environment one — the
    same split culture_nodes/cli/_errors.py draws, so a caller can tell "the
    gate ran and says no" from "the gate could not run".
    """

    def __init__(self, message: str, hint: str, code: int = 1) -> None:
        super().__init__(message)
        self.message = message
        self.hint = hint
        self.code = code


# ---------------------------------------------------------------------------
# operator configuration
# ---------------------------------------------------------------------------


def operator_env_path() -> Path:
    """The operator env file, overridable so a test or a CI job never reads a
    developer's real ~/.culture-nodes/operator.env."""
    override = os.environ.get("NODES_OPERATOR_ENV")
    if override:
        return Path(override)
    return Path.home() / ".culture-nodes" / "operator.env"


def operator_env(key: str) -> str | None:
    env = operator_env_path()
    if not env.is_file():
        return None
    for line in env.read_text(encoding="utf-8").splitlines():
        name, _, value = line.partition("=")
        if name.strip() == key:
            return value.strip().strip("\"'")
    return None


def config(key: str) -> str | None:
    """Process environment first, then the operator env file."""
    value = os.environ.get(key)
    if value:
        return value
    return operator_env(key)


def api_base() -> str:
    return (config("NODES_API_URL") or DEFAULT_API).rstrip("/")


def decision_token() -> str | None:
    for key in ("NODES_HUMAN_DECISION_TOKEN", "NODES_HUMAN_DECISION_TOKEN_SECRET"):
        value = config(key)
        if value:
            return value
    return None


# ---------------------------------------------------------------------------
# the control plane
# ---------------------------------------------------------------------------


def request(url: str, payload: dict | None = None, token: str | None = None) -> dict:
    data = None if payload is None else json.dumps(payload).encode()
    headers = {}
    if data:
        headers["content-type"] = "application/json"
    if token:
        headers["authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers)  # noqa: S310
    try:
        with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT_SECONDS) as response:  # noqa: S310
            body = response.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")[:400]
        raise Refusal(
            f"HTTP {exc.code} from {url}: {detail}",
            "check the run id and that this control plane carries the endpoint",
            code=1,
        ) from exc
    except json.JSONDecodeError as exc:
        # The SPA catch-all answers 200 with index.html for any unknown
        # /v1alpha1/ path (issue #8), so an absent endpoint looks like an
        # empty one by status code alone.
        raise Refusal(
            f"{url} did not answer with JSON",
            "this control plane predates the endpoint (the SPA catch-all answered instead); deploy a newer binary",
            code=2,
        ) from exc
    except OSError as exc:
        raise Refusal(
            f"cannot reach the control plane at {url}: {exc}",
            "set NODES_API_URL, or check that the control plane is up — this is not a passing gate, it is no gate",
            code=2,
        ) from exc


def actor_ids_for_run(run_view: dict) -> list[str]:
    """Every distinct actor that ran an attempt on this run, in order.

    Read from the ATTEMPTS, which is where the engine records who it actually
    dispatched to — never from the run's input or its output, which are the
    request and the report rather than the fact.
    """
    seen: list[str] = []
    for node_run in run_view.get("node_runs", []) or []:
        for attempt in node_run.get("attempts", []) or []:
            actor_id = attempt.get("actor_id")
            if actor_id and actor_id not in seen:
                seen.append(actor_id)
    return seen


def resolve_remote(actor: dict) -> str:
    """Where the control plane says this actor's handover refs are fetchable.

    Two configured sources, in order, and a refusal if neither is present:

      1. ``metadata.handover_remote`` on the registered actor — the control
         plane's own per-actor record, and the one that scales to a fleet of
         unlike hosts.
      2. ``NODES_HANDOVER_REMOTE_TEMPLATE``, an operator-side template with
         ``{host}``, ``{actor_key}`` and ``{actor_name}`` substituted. ``host``
         comes from the actor's registered ``endpoint_ref`` — the control
         plane, not a table in this file.

    There is deliberately no third source and no default. A guessed remote
    that happened to resolve would fetch a real commit from the wrong
    repository, which is worse than fetching nothing.
    """
    metadata = actor.get("metadata") or {}
    configured = metadata.get("handover_remote")
    if isinstance(configured, str) and configured.strip():
        return _reject_option_like(configured.strip())

    template = config("NODES_HANDOVER_REMOTE_TEMPLATE")
    if not template:
        raise Refusal(
            f"no handover remote is configured for actor {actor.get('actor_key') or actor.get('id')}",
            "either register the actor with metadata.handover_remote (the control plane's own record of "
            "where its refs are fetchable), or set NODES_HANDOVER_REMOTE_TEMPLATE — e.g. "
            "'ssh://thor@{host}/home/thor/git/culture-nodes-agent', where {host} is substituted from the "
            "actor's registered endpoint_ref. The remote is configuration, never something to guess",
            code=2,
        )

    host = urlsplit(actor.get("endpoint_ref") or "").hostname
    if not host and "{host}" in template:
        raise Refusal(
            f"actor {actor.get('actor_key') or actor.get('id')} has no endpoint_ref this script can read a host from",
            "register the actor with an endpoint_ref (or with metadata.handover_remote), so the host comes "
            "from the control plane rather than from an operator's memory",
            code=2,
        )
    remote = template.format(
        host=host or "",
        actor_key=actor.get("actor_key", ""),
        actor_name=str(actor.get("actor_key", "")).rpartition("/")[2],
    )
    return _reject_option_like(remote)


def _reject_option_like(remote: str) -> str:
    if remote.startswith("-"):
        raise Refusal(
            f"the configured remote {remote!r} would be read by git as an option, not a repository",
            "fix the configured remote",
            code=2,
        )
    return remote


# ---------------------------------------------------------------------------
# git
# ---------------------------------------------------------------------------


def git(
    repo: Path, *args: str, timeout: float = GIT_TIMEOUT_SECONDS
) -> subprocess.CompletedProcess[str]:
    """One bounded, shell-free `git` subprocess.

    Prompting is disabled so a remote wanting a credential this process does
    not have fails inside the deadline rather than hanging on a terminal
    nobody is watching — the same discipline internal/handover/git.go applies
    to the control plane's own fetch.
    """
    env = {
        **os.environ,
        "GIT_TERMINAL_PROMPT": "0",
        "GIT_ASKPASS": "",
        "GCM_INTERACTIVE": "never",
    }
    return subprocess.run(  # noqa: S603 # nosec B603 - fixed binary, constant argv, no shell
        ["git", *args],  # noqa: S607 # nosec B607
        cwd=str(repo),
        capture_output=True,
        text=True,
        timeout=timeout,
        env=env,
    )


def git_or_refuse(repo: Path, *args: str, timeout: float = GIT_TIMEOUT_SECONDS) -> str:
    proc = git(repo, *args, timeout=timeout)
    if proc.returncode != 0:
        raise Refusal(
            f"git {' '.join(args[:2])} failed: {(proc.stderr or '').strip()}",
            "check the repository and the configured remote",
            code=2,
        )
    return proc.stdout.strip()


def operator_repo(explicit: str | None) -> Path:
    if explicit:
        return Path(explicit).resolve()
    proc = git(Path.cwd(), "rev-parse", "--show-toplevel")
    if proc.returncode != 0:
        raise Refusal(
            "not inside a git repository, and no --repo was given",
            "run this from the checkout you want the handover fetched into, or pass --repo <dir>",
            code=2,
        )
    return Path(proc.stdout.strip())


def validate_run_id(run_id: str) -> str:
    """A run id becomes part of a refspec, so it is checked as one first."""
    if not _RUN_ID.match(run_id) or ".." in run_id:
        raise Refusal(
            f"{run_id!r} is not a run id this script will put in a refspec",
            "give the run's id as the control plane reports it (a ULID); this script never interpolates an "
            "arbitrary string into a git ref",
            code=1,
        )
    return run_id


def fetch_handovers(repo: Path, run_id: str, remote: str) -> list[dict]:
    """Fetch every handover ref for this run from one remote, and report what
    each contains.

    The refspec is a wildcard over the run's own namespace only, and the
    destination is a namespace of its own — no branch, no remote-tracking ref,
    nothing checked out. `--no-tags` keeps the fetch to exactly what was
    asked for, and `--` stops the remote or the refspec being re-read as an
    option however they were configured.
    """
    source = f"{REF_NAMESPACE}/{run_id}/*"
    destination = f"{LOCAL_NAMESPACE}/{run_id}/*"
    proc = git(
        repo,
        "fetch",
        "--no-tags",
        "--quiet",
        "--force",
        "--",
        remote,
        f"{source}:{destination}",
        timeout=FETCH_TIMEOUT_SECONDS,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip()
        # A wildcard refspec matching nothing is NOT an error to git; a
        # failure here means the remote itself could not be reached or read.
        raise Refusal(
            f"could not fetch {source} from the configured remote: {detail}",
            "check that the operator can reach the remote the control plane names for this actor "
            "(ssh reachability, credentials, path); an unreachable remote is not an empty one",
            code=2,
        )

    listing = git_or_refuse(
        repo,
        "for-each-ref",
        f"{LOCAL_NAMESPACE}/{run_id}/",
        "--format=%(refname)%09%(objectname)",
    )
    collected = []
    for line in listing.splitlines():
        if not line.strip():
            continue
        local_ref, _, sha = line.partition("\t")
        suffix = local_ref[len(f"{LOCAL_NAMESPACE}/{run_id}/") :]
        collected.append(
            {
                "ref": f"{REF_NAMESPACE}/{run_id}/{suffix}",
                "local_ref": local_ref,
                "commit_sha": sha,
                "source_remote": remote,
                "subject": git_or_refuse(repo, "log", "-1", "--format=%s", sha),
                "parent_sha": git_or_refuse(repo, "rev-list", "--max-count=1", "--skip=1", sha),
                "changed_paths": changed_paths(repo, sha),
            }
        )
    return collected


def changed_paths(repo: Path, sha: str) -> list[str]:
    """The paths the handover commit changed against its first parent.

    ``--root`` makes a root commit report its whole tree instead of nothing;
    ``-z`` is what keeps a path containing a newline or a quote intact.
    """
    raw = git_or_refuse(
        repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", sha
    )
    return sorted(part for part in raw.split("\0") if part)


# ---------------------------------------------------------------------------
# collection
# ---------------------------------------------------------------------------


def collect(run_id: str, repo: Path, base: str) -> dict:
    run_view = request(f"{base}/v1alpha1/runs/{run_id}")
    actor_ids = actor_ids_for_run(run_view)
    if not actor_ids:
        raise Refusal(
            f"run {run_id} has no attempt with an actor, so there is nowhere to fetch a handover from",
            "check the run: a run that was never dispatched has nothing to hand over "
            f"(scripts/decide-claims.py {run_id} --dry-run shows what it does have)",
            code=1,
        )

    handovers: list[dict] = []
    remotes: list[dict] = []
    for actor_id in actor_ids:
        actor = request(f"{base}/v1alpha1/actors/{actor_id}")
        remote = resolve_remote(actor)
        remotes.append(
            {"actor_id": actor_id, "actor_key": actor.get("actor_key"), "remote": remote}
        )
        for entry in fetch_handovers(repo, run_id, remote):
            entry["actor_id"] = actor_id
            entry["actor_key"] = actor.get("actor_key")
            handovers.append(entry)

    return {
        "run_id": run_id,
        "repo": str(repo),
        "remotes": remotes,
        "collected": bool(handovers),
        "handovers": handovers,
    }


#: The two readings of "no ref", named rather than chosen between. See this
#: module's docstring and issue #120.
POSSIBILITIES = [
    "the session handed over nothing — it made no commit, or the dispatch asked for no handover "
    "(the run's input carries `handover`), so no ref was ever minted",
    "the bridge on that host cannot hand over — a bridge deployed before the ref-minting code creates no "
    "ref even on a successful session (issue #120); reinstalling it is what fixed thor and orin",
]


def no_ref_refusal(result: dict) -> Refusal:
    remotes = ", ".join(entry["remote"] for entry in result["remotes"]) or "(no remote resolved)"
    return Refusal(
        f"run {result['run_id']} has no ref under {REF_NAMESPACE}/{result['run_id']}/ on {remotes}",
        "this state is AMBIGUOUS and must not be guessed at. Either: (1) "
        + POSSIBILITIES[0]
        + "; or (2) "
        + POSSIBILITIES[1]
        + ". Neither the run's own report nor the empty fetch distinguishes them — check the bridge "
        "version on that host before concluding the session did nothing",
        code=1,
    )


def render_collection(result: dict) -> None:
    """Render what was collected. Callers reach this only after the empty case
    has already been refused, but it renders an empty collection honestly
    rather than indexing into it — a rendering function that crashes on the
    one state the caller is meant to have handled hides which of the two bugs
    happened."""
    print(f"run {result['run_id']}")
    for entry in result["remotes"]:
        print(f"  fetched from {entry['remote']}  ({entry['actor_key'] or entry['actor_id']})")
    for handover in result["handovers"]:
        print()
        print(f"  ref    {handover['ref']}")
        print(f"  commit {handover['commit_sha']}  {handover['subject']}")
        print(f"  local  {handover['local_ref']}")
        if handover["changed_paths"]:
            print(f"  changed {len(handover['changed_paths'])} path(s):")
            for path in handover["changed_paths"]:
                print(f"    {path}")
        else:
            # Measured live on run 01M04CJT84WD20GDQEN266J9J6: a handover
            # commit has an EMPTY first-parent diff whenever the session
            # committed its own work before the bridge minted the ref. The
            # bridge builds the handover commit from the working tree on top
            # of head_after, so a clean tree gives an identical one. That is
            # not "the session did nothing" — the work is in the parent — and
            # saying so here is the difference between a useful report and a
            # misleading one. The same measurement semantics as
            # internal/handover's Measurement.ChangedPaths are kept
            # deliberately: this reports what the commit changed, and points
            # at where to look further rather than guessing a wider base.
            print("  changed 0 paths against its first parent")
            print(
                f"    the handover commit's tree is identical to {handover['parent_sha'][:12]}, which"
            )
            print("    normally means the session committed its own work before the bridge")
            print(f"    minted the ref. Review the parent: git show {handover['parent_sha']}")
    if not result["handovers"]:
        print("  no ref under this run's handover namespace")
        return
    print()
    print(f"review it with: git show {result['handovers'][0]['commit_sha']}")


# ---------------------------------------------------------------------------
# the gate
# ---------------------------------------------------------------------------


def run_suite(repo: Path, sha: str, command: list[str]) -> tuple[int, str]:
    """Run the suite in a detached worktree at exactly `sha`, and report the
    exit code together with the commit the worktree was actually ON when the
    suite finished.

    The second half is the point. The verdict's commit sha must be MEASURED,
    not assumed: a suite that switched branches, or a worktree that was never
    at the commit the operator thought, would otherwise produce a verdict
    naming a commit it never tested. That is the failure mode t11 exists to
    close, and it is the same shape as a `go test` that prints `ok` for a
    package where every test skipped.
    """
    with tempfile.TemporaryDirectory(prefix="culture-nodes-gate-") as tmp:
        worktree = Path(tmp) / "tree"
        git_or_refuse(repo, "worktree", "add", "--detach", "--quiet", str(worktree), sha)
        try:
            print(f"gate: running {' '.join(command)} at {sha}", file=sys.stderr)
            proc = subprocess.run(  # noqa: S603 # nosec B603 - operator-supplied argv, no shell
                command,
                cwd=str(worktree),
                stdout=sys.stderr,
                stderr=sys.stderr,
                check=False,
            )
            tested = git_or_refuse(worktree, "rev-parse", "HEAD")
            return proc.returncode, tested
        finally:
            git(repo, "worktree", "remove", "--force", str(worktree))


def record_verdict(
    base: str,
    result: dict,
    handover: dict,
    suite: str,
    command: list[str],
    exit_code: int,
    tested_sha: str,
    validator: str,
    requires_grants: list[str] | None = None,
    implicates: list[str] | None = None,
) -> dict:
    """POST the verdict and return the whole SuiteVerdictResult — the verdict
    record and, for a rejecting gate, where the control plane routed it.

    ``requires_grants`` and ``implicates`` are ROUTING inputs and never reach
    the verdict record: they say what a repair of this failure would need, not
    what the suite did. The gate is the only party that can supply the first —
    it is the only one that knows its suite talks to a database — and #119 is
    what that fact decides: a repair lane whose posture grants no network
    egress cannot verify a PostgreSQL-backed suite no matter how cleanly `go`
    runs there.
    """
    return request(
        f"{base}/v1alpha1/runs/{result['run_id']}/suite-verdicts",
        {
            "suite": suite,
            "command": command,
            "exit_code": exit_code,
            "commit_sha": tested_sha,
            "ref": handover["ref"],
            "validator_actor_id": validator,
            "requires_grants": requires_grants or [],
            "implicated_paths": implicates or [],
        },
        token=decision_token(),
    )


def gate(base: str, repo: Path, result: dict, args: argparse.Namespace) -> tuple[int, dict]:
    if not args.suite:
        raise Refusal(
            "--gate needs --suite naming what ran",
            "pass --suite 'go test ./...' (or the CI job's name); a verdict that does not name its suite "
            "cannot be re-run by whoever reads it",
            code=1,
        )
    if not args.command:
        raise Refusal(
            "--gate needs a command to run, after a `--` separator",
            "e.g. scripts/collect-handover.py <run-id> --gate --suite 'go test ./...' -- go test ./...",
            code=1,
        )

    handovers = result["handovers"]
    if len(handovers) > 1:
        raise Refusal(
            f"run {result['run_id']} has {len(handovers)} handover refs and the gate must test exactly one",
            "pass --ref <full-ref-name> to choose which attempt's handover to gate; testing one and "
            "recording the verdict against the run as a whole would misattribute it",
            code=1,
        )
    handover = handovers[0]

    validator = args.validator or config("NODES_VALIDATOR_ACTOR_ID")
    if not validator:
        raise Refusal(
            "no validator identity is configured, so the suite's result cannot be attributed",
            "register a non-human actor for the gate and set NODES_VALIDATOR_ACTOR_ID (or pass "
            "--validator <actor-id>). PRD §10.4 admits a derived record only from an IDENTIFIED "
            "deterministic producer; an anonymous verdict attests to nothing",
            code=2,
        )

    exit_code, tested_sha = run_suite(repo, handover["commit_sha"], args.command)
    if tested_sha != handover["commit_sha"]:
        raise Refusal(
            f"the suite ran at {tested_sha}, not at the handed-over commit {handover['commit_sha']}",
            "the worktree moved during the run; nothing was recorded, because a verdict that names a "
            "commit it did not test is worse than no verdict",
            code=1,
        )

    record = record_verdict(
        base,
        result,
        handover,
        args.suite,
        args.command,
        exit_code,
        tested_sha,
        validator,
        args.requires_grant,
        args.implicates,
    )
    return exit_code, record


def render_routing(result: dict) -> None:
    """Print where a failing gate was routed, or say plainly that it was not
    routed at all.

    This is task t32's operator-facing half. Before it, a red gate ended in a
    person deciding — in their own head, in the most expensive session
    available — whether this was repairable, by whom, and how many more tries
    it got. Now the control plane has already decided all four, under a bound
    it states, and this prints the answer.

    The absence case is printed too, and loudly. Issue #120 is the whole
    argument: a routing that silently did not happen looks exactly like a gate
    that had nothing to route, and telling those apart took an ssh.
    """
    routing = result.get("routing")
    error = result.get("routing_error")
    if error:
        print()
        print("gate: the failure was NOT recorded as routed:")
        print(f"      {error}")
        return
    if not routing:
        return

    data = routing.get("data") or {}
    bound = data.get("bound") or {}
    selected = data.get("selected", "?")
    lane = data.get("repair_lane_actor_key") or data.get("repair_lane_actor_id") or "?"

    print()
    if selected == "repair":
        print(
            f"gate: routed to a REPAIR attempt {data.get('attempt_number', '?')} of "
            f"{bound.get('max_attempts', '?')} on {lane}"
        )
    else:
        print(f"gate: routed to a HUMAN — {data.get('reason', '?')}")
    print(f"      {data.get('rationale', '')}")
    for path in data.get("guarded_paths") or []:
        print(f"      out of scope: {path}")
    window_hours = int(bound.get("window_seconds", 0)) // 3600
    print(
        f"      bound: {bound.get('max_attempts', '?')} attempts per run over {window_hours}h; "
        f"at the ceiling, {bound.get('at_ceiling', '?')}"
    )
    print(f"      recorded as derived record {routing.get('id', '?')}")
    if data.get("dispatched") is False:
        # Said every time, not only when it might be missed. A routing a
        # reader takes for an execution is the failure mode that leaves
        # nobody looking for the dispatch that never happened.
        print(
            "      NOTE: this was decided and recorded, not dispatched — "
            "unattended repair is deliberately not enabled"
        )


# ---------------------------------------------------------------------------
# entry point
# ---------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="collect-handover.py",
        description=__doc__.splitlines()[0],
    )
    parser.add_argument("run_id", help="the run whose handover to collect")
    parser.add_argument("--json", action="store_true", help="machine-readable result on stdout")
    parser.add_argument(
        "--repo", help="the checkout to fetch into (default: the enclosing repository)"
    )
    parser.add_argument("--ref", help="gate this one ref, when a run handed over more than once")
    parser.add_argument(
        "--gate",
        action="store_true",
        help="run a suite against the collected commit and record the verdict",
    )
    parser.add_argument("--suite", help="what ran, in the spelling a reader could re-run it with")
    parser.add_argument(
        "--validator", help="the registered non-human actor the verdict is attributed to"
    )
    parser.add_argument(
        "--requires-grant",
        action="append",
        default=[],
        metavar="GRANT",
        help="a dispatch grant this suite needs beyond its own binary "
        "(network-egress, home-write, ...); repeatable. A repair lane whose posture lacks it "
        "cannot verify a fix, so it routes to a human instead",
    )
    parser.add_argument(
        "--implicates",
        action="append",
        default=[],
        metavar="PATH",
        help="a repo-relative path this failure involves, beyond the ones the control plane already "
        "measured; repeatable. A failure implicating .github/ routes to a human",
    )
    return parser


def split_command(argv: list[str]) -> tuple[list[str], list[str]]:
    """Everything after the first standalone ``--`` is the suite's argv.

    Split by hand rather than with argparse.REMAINDER: REMAINDER attaches to
    the first positional and would swallow this script's own flags, and the
    suite command must be able to contain anything at all (its own ``--json``,
    its own ``--``) without argparse trying to interpret it.
    """
    if "--" in argv:
        index = argv.index("--")
        return argv[:index], argv[index + 1 :]
    return argv, []


def main(argv: list[str] | None = None) -> int:
    own, command = split_command(list(sys.argv[1:] if argv is None else argv))
    args = build_parser().parse_args(own)
    args.command = command

    result: dict = {}
    try:
        run_id = validate_run_id(args.run_id)
        repo = operator_repo(args.repo)
        base = api_base()
        result = collect(run_id, repo, base)

        if args.ref:
            result["handovers"] = [h for h in result["handovers"] if h["ref"] == args.ref]

        if not result["collected"] or not result["handovers"]:
            raise no_ref_refusal(result)

        gate_result: dict = {}
        suite_exit = 0
        if args.gate:
            suite_exit, gate_result = gate(base, repo, result, args)

        if args.json:
            payload = dict(result)
            if args.gate:
                payload["gate"] = {
                    "suite": args.suite,
                    "exit_code": suite_exit,
                    # `record` keeps its t11 name and meaning — the verdict —
                    # so a reader's existing jq does not change under them.
                    "record": gate_result.get("verdict"),
                    "routing": gate_result.get("routing"),
                    "routing_error": gate_result.get("routing_error"),
                }
            json.dump(payload, sys.stdout, indent=2)
            print()
        else:
            render_collection(result)
            if args.gate:
                label = "passed" if suite_exit == 0 else f"FAILED (exit {suite_exit})"
                print()
                print(f"gate: {args.suite} {label} at {result['handovers'][0]['commit_sha']}")
                verdict = gate_result.get("verdict") or {}
                print(f"gate: recorded as derived record {verdict.get('id', '?')}")
                render_routing(gate_result)

        return 1 if args.gate and suite_exit != 0 else 0

    except Refusal as refusal:
        if args.json:
            json.dump(
                {
                    "run_id": args.run_id,
                    "collected": bool(result.get("handovers")),
                    "handovers": result.get("handovers", []),
                    "error": refusal.message,
                    "hint": refusal.hint,
                    "possibilities": POSSIBILITIES if not result.get("handovers") else [],
                },
                sys.stdout,
                indent=2,
            )
            print()
        print(f"error: {refusal.message}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code
    except subprocess.TimeoutExpired as exc:
        print(f"error: a git subprocess did not finish within its deadline: {exc}", file=sys.stderr)
        print(
            "hint: a stuck fetch is not an empty one; check the remote's reachability",
            file=sys.stderr,
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
