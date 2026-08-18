#!/usr/bin/env python3
"""The combining-loop workflow's code node — the ONE program every code node
in the harvest / stage / merge chain runs (plan task t5's program half; the
workflow yaml that dispatches it is authored separately by the operator).

    scripts/combining-loop-node.py harvest
    scripts/combining-loop-node.py stage
    scripts/combining-loop-node.py merge

The subcommand is `argv[1]`. Each one reads its own per-run values from the
env var NODES_INPUT_JSON — a JSON object the engine provides, being wired in
parallel (issue #170 tracks a code node's input contract still being
discarded before the process ever sees it; this program is written to the
contract, not to the gap) — and its grants from named env vars. Nothing
secret ever appears in argv or in a printed diagnostic.

# What this replaces

A published actor handover is, until this program exists, three hand-turns:
fetch the claimed ref, merge it onto the feature branch and eyeball the diff
for anything under .github/, then fast-forward and push once a gate looks
green. Each of those is exactly the kind of manual step
`docs/deliveries/close-the-backlog-bootstrap-honesty.md` found invisible by
default. This program is what turns them into three deterministic, re-runnable
steps a workflow node dispatches — not a smaller set of things an operator
still types.

# Why three subcommands and not one

harvest, stage and merge are three separate MEASUREMENTS, and folding them
into one command would blur exactly the boundary
`internal/handover/doc.go` draws between them: harvest fetches and pins what a
session claims to have produced, stage measures what applying it actually
changes (and is the ONLY place a change touching .github/ can be caught before
a gate ever runs), and merge advances a branch only once a gate verdict names
the very candidate it is about to push. A single command computing all three
in one process could not fail in the middle and leave the earlier, already-true
measurement recoverable — which is the specific property `harvest` exists to
guarantee (see below).

# The exit-code contract

Five codes are shared across all three subcommands, and the same number never
means two different things in two subcommands:

    0   success                  the subcommand's own domain success
    2   environment              a missing grant, an unreachable git object,
                                  a subprocess that failed to run, a timeout —
                                  the machinery could not produce an answer
    4   refused                  the declared NODES_INPUT_JSON contract was
                                  violated (unknown key, wrong shape, a ref
                                  outside the handover fence) OR a
                                  security-relevant mismatch this program
                                  will not silently proceed past (a fetched
                                  commit that is not the one that was claimed,
                                  a verdict that does not name the candidate
                                  about to be merged)

Two subcommand-specific domain codes exist ONLY on `stage`, because staging is
the one step with a real third answer beside pass/fail — mirroring
`cmd/nodes-harvest`'s own exit-status contract for the same operation in Go:

    1   merge_conflict (stage)   the harvested package conflicts with the
                                  feature tip — an expected routing outcome,
                                  never a `Refusal`
    3   routes_to_human (stage)  the candidate diff touches .github/ — no
                                  gate verdict may bypass this; it is checked
                                  against the feature branch, unconditionally

A refusal (exit 4) and an environment failure (exit 2) both mean "no useful
record was produced"; a domain outcome (0 on harvest/merge, or 0/1/3 on
stage) always means the git objects this program's caller relies on are
exactly what it says.

Nor is the exit code the whole story (h7): each subcommand also emits `verified` with the fact
its own exit code claims — harvest's `commit`, stage's `candidate_parents`, merge's `remote_sha`,
release's `event_ids` — never inferred from a status alone.

# harvest — fetch what a session claims to have produced, and pin it first

Input: {"handover_ref": "refs/culture-nodes/<run>/<node-run>",
        "expected_commit": "<40-char sha>"}
Grant: HARVEST_REMOTE (a URL, GRANTED — never accepted from the input. A
       session that could name its own remote could point this program's
       measurement at a repository it prepared itself; the measurement would
       be real and the subject would be forged. This mirrors
       `internal/handover/doc.go`'s central rule almost verbatim: "The remote
       is the control plane's configuration and never the agent's report.")

Only a ref under refs/culture-nodes/ is ever fetched (mirrors
`internal/handover/handover.go`'s ValidateRef — the client-side half of the
server-side ref fence). The fetch writes STRAIGHT into
refs/culture-nodes/harvested/<run-id> (run id from NODES_RUN_ID) — the same
refspec shape `internal/handover/harvest.go`'s Harvest uses — so the object is
durable and recoverable the moment `git fetch` returns, before this program
ever compares the fetched commit to what was claimed. If the comparison then
fails, the ref is left exactly where it is: a mismatch is refused, not undone,
so what was actually fetched stays inspectable.

# stage — measure what applying the package actually changes

Input: {"harvested_commit": "<sha>", "feature_branch": "<name>"}

A disposable, detached worktree is added against the SAME repository (so any
commit it creates lands in the shared object database and is later resolvable
from the main worktree with no re-fetch), reset onto the feature branch tip,
then `git merge --no-commit --no-ff <harvested_commit>` is attempted. A real
conflict is `merge_conflict` (exit 1) with the conflicted paths reported — not
a `Refusal`, because the machinery worked and this is exactly what it is meant
to measure. A clean merge is diffed against the feature tip; any path equal to
or under `.github/` makes this `routes_to_human` (exit 3) and no candidate
commit is ever created — the same unbypassable check
`internal/handover/harvest.go`'s StageCandidate performs before a gate verdict
can exist to bypass it. Only once neither applies is the merge materialized as
a detached commit, whose sha is printed as `{"candidate_commit": "..."}`.

# merge — advance the feature branch only at the exact verdict candidate

Input: {"candidate_commit": "<sha>", "feature_branch": "<name>",
        "verdict": {"outcome": "gates_passed", "candidate": "<sha>"}}
Grant: GITHUB_TOKEN_WORKER (the #90 worker push credential seam)

The merge is refused unless verdict.outcome == "gates_passed" AND
verdict.candidate == candidate_commit — checked together, so a stale verdict
for an earlier candidate can never authorize pushing a later one (no TOCTOU).
The branch is advanced with a single compare-and-swap `git update-ref` (old
value = the feature tip the candidate was actually built on, read straight off
the candidate's own two parents) — a concurrent feature-branch move is
refused, never silently overwritten, mirroring
`internal/handover/merge.go`'s MergeCandidate almost line for line.

The push runs with credential.helper and credential.https://github.com.helper
RESET via `-c` (`-c credential.helper= -c
"credential.https://github.com.helper="`) alongside a temporary GIT_ASKPASS
script that reads GITHUB_TOKEN_WORKER only from its OWN environment. This is
not defensive redundancy: on any host where `gh auth setup-git` has run, a
configured credential helper silently OUTRANKS GIT_ASKPASS on push, and the
push would use the helper's identity instead of the worker token with nothing
saying so — measured on this fleet, and the exact fix
`internal/handover/merge.go` just landed for the Go merge path (mirrored here
verbatim, not reinvented). The token is never placed in argv; it is read once
from the environment and, if a git diagnostic happens to have echoed it back,
that occurrence is redacted before this program ever writes the diagnostic
anywhere.

`git push`'s exit 0 is not trusted as proof (h7): some transports report success while their own
`--porcelain` line carries a rejection. Right after, `git ls-remote` re-reads the SAME remote and
refuses on any mismatch.

# What is deliberately NOT here

No workflow yaml, no examples/combining-loop/ fixtures — that half is the
operator's, authored separately, and this program has no opinion on how the
engine schedules it. No retry, no polling, no queueing: one invocation is one
measurement, exactly like `scripts/merge-gate.py`, and a caller that wants a
retry runs this program again.

Environment (grants and identity, all supplied by the runner boundary or the
deployment, none inferred):

    NODES_INPUT_JSON      this run's per-node input, a JSON object (see each
                          subcommand's schema above). Unknown keys are refused.
    NODES_RUN_ID          this run's id (harvest: names the recovery ref).
    NODES_WORKSPACE       the git repository every subcommand operates on
                          (`-C <workspace>` on every git invocation); defaults
                          to the process's own working directory.
    HARVEST_REMOTE        (harvest only) the control-plane-configured remote
                          URL to fetch from. Never read from the input.
    GITHUB_TOKEN_WORKER   (merge only) the #90 worker push credential. Never
                          read from the input, never placed in argv.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess  # noqa: S404 # nosec B404 - fixed git binary, argv lists, no shell
import sys
import tempfile
from pathlib import Path
from typing import Any

#: The published exit-status contract. See the module docstring.
EXIT_OK = 0
EXIT_MERGE_CONFLICT = 1  # stage only
EXIT_ENVIRONMENT = 2
EXIT_ROUTES_TO_HUMAN = 3  # stage only
EXIT_REFUSED = 4

GIT_TIMEOUT_SECONDS = 120.0

#: The only namespace this program will ever fetch a ref out of — the
#: client-side half of the same fence internal/handover/handover.go's
#: ValidateRef enforces server-side.
REF_NAMESPACE = "refs/culture-nodes/"

#: A lowercase 40-character git object id, mirroring commitPattern in
#: internal/handover/harvest.go and internal/handover/merge.go.
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")

#: A safe branch name, mirroring branchPattern in internal/handover/merge.go.
BRANCH_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")

#: A safe short identifier (run ids, remote names), mirroring namePattern in
#: internal/handover/harvest.go.
NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

#: The #90 credential seam, installed on the actor host. Its value is
#: environment-only: never a flag, never in argv, never in a printed message.
WORKER_PUSH_CREDENTIAL = "GITHUB_TOKEN_WORKER"

#: The control-plane-configured remote URL harvest fetches from. Deliberately
#: never accepted from NODES_INPUT_JSON — see the module docstring.
HARVEST_REMOTE_ENV = "HARVEST_REMOTE"

#: merge pushes to the feature branch's own already-configured remote. This is
#: a distinct concern from HARVEST_REMOTE: harvest measures FROM a remote the
#: control plane names explicitly (the fence an agent must not choose), while
#: merge PUSHES an already-verdicted candidate through the repository's
#: ordinary remote — the credential grant (GITHUB_TOKEN_WORKER) is what makes
#: that push authorized, not the choice of remote name.
MERGE_REMOTE = "origin"


class Refusal(Exception):
    """A subcommand's refusal to proceed, carrying the exit code it produces.

    Every `Refusal` this program raises is one of exactly two things: an
    environment failure (code 2 — the machinery could not produce an answer)
    or a contract/security refusal (code 4 — the declared input was not
    honored, or a measured fact did not match what was claimed). Nothing else
    raises a plain exception; `main` treats an uncaught one as a bug, not a
    third kind of failure.
    """

    def __init__(self, message: str, hint: str, code: int = EXIT_ENVIRONMENT) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


# --------------------------------------------------------------------------
# Small shared helpers
# --------------------------------------------------------------------------


def emit(payload: dict[str, Any]) -> None:
    """Write exactly one JSON object to stdout. Diagnostics never share it —
    stdout is a result, stderr is everything else, the same split
    scripts/merge-gate.py and the `nodes` CLI both keep."""
    json.dump(payload, sys.stdout, sort_keys=True)
    sys.stdout.write("\n")


def env_or_refuse(name: str, hint: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise Refusal(f"{name} is not set", hint, code=EXIT_ENVIRONMENT)
    return value


def resolve_workspace() -> Path:
    raw = os.environ.get("NODES_WORKSPACE", "").strip()
    return Path(raw).resolve() if raw else Path.cwd().resolve()


def redact_remote_arg(arg: str) -> str:
    """Hide a URL-shaped or credential-bearing argv entry from a printed
    diagnostic. A configured remote (HARVEST_REMOTE, or a URL a `git remote
    -v` might report) can embed a token, and this text is logged — the same
    reasoning internal/handover/git.go's `redact` gives for the identical
    substitution on the control-plane side."""
    if "://" in arg or "@" in arg:
        return "<remote>"
    return arg


def redact_secret_text(text: str) -> str:
    """Strip a live WORKER_PUSH_CREDENTIAL value out of arbitrary diagnostic
    text, mirroring internal/handover/merge.go's mergeCommand redaction. Cheap
    insurance: nothing in this program ever puts the token in argv, but a
    subprocess's own stderr is not this program's to control."""
    secret = os.environ.get(WORKER_PUSH_CREDENTIAL, "")
    if secret:
        return text.replace(secret, "<redacted>")
    return text


def git(
    workspace: Path,
    *args: str,
    env: dict[str, str] | None = None,
    timeout: float = GIT_TIMEOUT_SECONDS,
) -> str:
    """Run `git -C <workspace> <args>`, returning stripped stdout.

    Any nonzero exit is an EXIT_ENVIRONMENT `Refusal` — the machinery could
    not produce an answer. A caller that expects a nonzero exit to be a real
    finding (stage's merge attempt) must not route through this helper; it
    calls subprocess directly instead, exactly as scripts/merge-gate.py's
    `run_gate` distinguishes "could not launch" from "ran and failed".
    """
    full_env = env if env is not None else {**os.environ, "GIT_TERMINAL_PROMPT": "0"}
    try:
        proc = subprocess.run(  # noqa: S603 # nosec B603 - fixed binary, argv list, no shell
            ["git", "-C", str(workspace), *args],
            capture_output=True,
            text=True,
            timeout=timeout,
            env=full_env,
            check=False,
        )
    except FileNotFoundError as exc:
        raise Refusal(
            "git is not on PATH", "install git, or run this program on a host that has it"
        ) from exc
    except subprocess.TimeoutExpired as exc:
        shown = " ".join(redact_remote_arg(a) for a in args)
        raise Refusal(
            f"git {shown} did not finish within {timeout}s",
            "a stuck git invocation is not a slow one; the timeout is what stops it holding "
            "a lease it should have released",
        ) from exc
    if proc.returncode != 0:
        shown = " ".join(redact_remote_arg(a) for a in args)
        detail = redact_secret_text((proc.stderr or proc.stdout).strip())
        raise Refusal(
            f"git {shown} failed: {detail}", "see the message above for what git reported"
        )
    return proc.stdout.strip()


def split_nul_paths(raw: str) -> list[str]:
    return [p for p in raw.split("\x00") if p]


# --------------------------------------------------------------------------
# NODES_INPUT_JSON — strict per-subcommand decoding
# --------------------------------------------------------------------------


def read_input(known_keys: frozenset[str]) -> dict[str, Any]:
    """Decode NODES_INPUT_JSON and refuse anything this subcommand does not
    declare. Mirrors scripts/merge-gate.py's check_gate_declaration: a key
    nobody reads declares nothing while looking exactly like it does (#148),
    so an unrecognised key is refused by name rather than silently ignored.
    """
    raw = os.environ.get("NODES_INPUT_JSON", "")
    if not raw.strip():
        raise Refusal(
            "NODES_INPUT_JSON is not set",
            "the engine wires this subcommand's per-run input through NODES_INPUT_JSON; "
            "grant it to the operation",
            code=EXIT_ENVIRONMENT,
        )
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise Refusal(
            f"NODES_INPUT_JSON is not valid JSON: {exc}",
            "the input is a JSON object matching this subcommand's declared schema",
            code=EXIT_REFUSED,
        ) from exc
    if not isinstance(payload, dict):
        raise Refusal(
            f"NODES_INPUT_JSON is a {type(payload).__name__}, not a JSON object",
            "the input is a JSON object matching this subcommand's declared schema",
            code=EXIT_REFUSED,
        )
    unknown = sorted(set(payload) - known_keys)
    if unknown:
        named = ", ".join(repr(key) for key in unknown)
        vocabulary = ", ".join(sorted(known_keys))
        raise Refusal(
            f"NODES_INPUT_JSON declares {named}, which this subcommand never reads",
            f"a key nobody reads declares nothing while looking like it does; the keys this "
            f"subcommand reads are: {vocabulary}",
            code=EXIT_REFUSED,
        )
    return payload


def require_str(payload: dict[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or not value:
        raise Refusal(
            f"{key!r} is required and must be a non-empty string",
            f"NODES_INPUT_JSON must declare a non-empty string {key!r}",
            code=EXIT_REFUSED,
        )
    return value


def require_commit(payload: dict[str, Any], key: str) -> str:
    value = require_str(payload, key)
    if not COMMIT_PATTERN.match(value):
        raise Refusal(
            f"{key!r} is {value!r}, not a lowercase 40-character git object id",
            f"a commit must be resolved with `git rev-parse` before it is passed as {key!r}",
            code=EXIT_REFUSED,
        )
    return value


def require_branch(payload: dict[str, Any], key: str) -> str:
    value = require_str(payload, key)
    if (
        not BRANCH_PATTERN.match(value)
        or ".." in value
        or value.endswith("/")
        or value.endswith(".lock")
    ):
        raise Refusal(
            f"{key!r} is {value!r}, which is not a safe branch name",
            "a branch name must not begin with '-', contain '..', or end with '/' or '.lock'",
            code=EXIT_REFUSED,
        )
    return value


def validate_ref(ref: str) -> None:
    """Refuse any ref outside refs/culture-nodes/, mirroring
    internal/handover/handover.go's ValidateRef almost verbatim — the
    client-side half of the same fence that package's doc comment describes.
    This is the load-bearing security check in `harvest`: only a ref this
    fence covers is ever fetched, so a claimed "ref" naming an arbitrary
    branch measures nothing.
    """
    if not ref:
        raise Refusal("handover_ref is empty", "no ref was claimed", code=EXIT_REFUSED)
    if not ref.startswith(REF_NAMESPACE):
        raise Refusal(
            f"handover_ref {ref!r} is outside {REF_NAMESPACE}",
            "this program fetches only the namespace the handover fence covers",
            code=EXIT_REFUSED,
        )
    if len(ref) > 512:
        raise Refusal(
            f"handover_ref is {len(ref)} characters, longer than a ref this program will fetch",
            "a handover ref minted by the fence is short; a long value is not one",
            code=EXIT_REFUSED,
        )
    allowed = set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-")
    bad = sorted({c for c in ref if c not in allowed})
    if bad:
        raise Refusal(
            f"handover_ref {ref!r} contains {bad!r}, which a handover ref is not minted with",
            "a handover ref is minted by the fence and never carries these characters",
            code=EXIT_REFUSED,
        )
    if ".." in ref or "//" in ref or ref.endswith("/") or ref.endswith(".lock"):
        raise Refusal(
            f"handover_ref {ref!r} is not a well-formed ref name",
            "check the claimed ref for typos",
            code=EXIT_REFUSED,
        )
    for part in ref.split("/"):
        if part == "" or part.startswith("-") or part.startswith("."):
            raise Refusal(
                f"handover_ref {ref!r} has a component git would not accept ({part!r})",
                "a handover ref is minted by the fence and never carries a component like this",
                code=EXIT_REFUSED,
            )


def validate_run_id(run_id: str) -> None:
    if not NAME_PATTERN.match(run_id):
        raise Refusal(
            f"NODES_RUN_ID {run_id!r} contains unsafe characters",
            "the run id names a ref component; the engine's own run ids never look like this",
            code=EXIT_ENVIRONMENT,
        )


# --------------------------------------------------------------------------
# harvest
# --------------------------------------------------------------------------


def cmd_harvest(_args: argparse.Namespace) -> int:
    payload = read_input(frozenset({"handover_ref", "expected_commit"}))
    ref = require_str(payload, "handover_ref")
    expected = require_commit(payload, "expected_commit")
    validate_ref(ref)

    remote = env_or_refuse(
        HARVEST_REMOTE_ENV,
        "grant HARVEST_REMOTE to the operation — the control plane's own configured remote, "
        "never taken from NODES_INPUT_JSON, so a session cannot point this measurement at a "
        "repository it prepared itself",
    )
    run_id = env_or_refuse(
        "NODES_RUN_ID",
        "the runner boundary forwards the run id from the operation's own context; a harvest "
        "that cannot name its run cannot name a recoverable ref either",
    )
    validate_run_id(run_id)
    workspace = resolve_workspace()

    # Fetch STRAIGHT into the recovery ref, before this program ever compares
    # what it got to what was claimed. Git updates a ref atomically only
    # after it has received the objects a fetch names, so a process killed at
    # any point either leaves the OLD recovery ref in place or the fully
    # fetched new one — never a half-written one. This is the property
    # "recoverable-after-kill" asks for, and it is inherited from git rather
    # than built here: see internal/handover/harvest.go's Harvest, which uses
    # the identical "+ref:recovery" refspec for the identical reason.
    recovery_ref = f"refs/culture-nodes/harvested/{run_id}"
    refspec = f"+{ref}:{recovery_ref}"
    git(workspace, "fetch", "--no-tags", "--", remote, refspec)

    got = git(workspace, "rev-parse", "--verify", f"{recovery_ref}^{{commit}}")
    if got != expected:
        raise Refusal(
            f"fetched commit {got} does not match expected commit {expected}",
            f"the fetched object is preserved at {recovery_ref} for inspection; nothing was "
            "undone, but this harvest is refused",
            code=EXIT_REFUSED,
        )

    # A containerized graph node's local refs die with its workspace, so the
    # recovery ref is durable only on the REMOTE. Pushing back to the same
    # env-granted remote we fetched from adds no new authority.
    git(workspace, "push", "--", remote, f"+{recovery_ref}:{recovery_ref}")

    # Post-condition (h7): the comparison's own fact, restated as evidence.
    emit(
        {
            "harvested_commit": got,
            "recovery_ref": recovery_ref,
            "verified": {"recovery_ref": recovery_ref, "commit": got},
        }
    )
    return EXIT_OK


# --------------------------------------------------------------------------
# stage
# --------------------------------------------------------------------------


def cmd_stage(_args: argparse.Namespace) -> int:
    payload = read_input(frozenset({"harvested_commit", "feature_branch"}))
    harvested_commit = require_commit(payload, "harvested_commit")
    feature_branch = require_branch(payload, "feature_branch")

    workspace = resolve_workspace()
    feature_ref = f"refs/heads/{feature_branch}"
    feature_commit = git(workspace, "rev-parse", "--verify", f"{feature_ref}^{{commit}}")
    # Fail fast, before a worktree is even created, if the harvested object
    # simply is not in this repository's object database.
    git(workspace, "cat-file", "-e", f"{harvested_commit}^{{commit}}")

    stage_dir = Path(tempfile.mkdtemp(prefix="combining-loop-stage-"))
    try:
        # A worktree added against the SAME repository shares its object
        # database: the candidate commit this staging step may create is
        # resolvable from `workspace` with no re-fetch, exactly like
        # internal/handover/harvest.go's Harvest + StageCandidate.
        git(workspace, "worktree", "add", "--detach", str(stage_dir), feature_commit)
        git(stage_dir, "reset", "--hard", feature_commit)

        merge_proc = subprocess.run(  # noqa: S603 # nosec B603 - fixed binary, argv list, no shell
            [
                "git",
                "-C",
                str(stage_dir),
                # Identity on the MERGE too, not only the commit below: a
                # bare container has no git config, and merge refuses
                # without a committer identity even under --no-commit
                # (measured in the t8 live demo's sandbox).
                "-c",
                "user.name=Culture Nodes Combining Loop",
                "-c",
                "user.email=combining-loop@culture-nodes.invalid",
                "merge",
                "--no-commit",
                "--no-ff",
                harvested_commit,
            ],
            capture_output=True,
            text=True,
            timeout=GIT_TIMEOUT_SECONDS,
            env={**os.environ, "GIT_TERMINAL_PROMPT": "0"},
            check=False,
        )
        if merge_proc.returncode != 0:
            unmerged = git(stage_dir, "diff", "--name-only", "--diff-filter=U", "-z")
            conflicted = split_nul_paths(unmerged)
            if conflicted:
                # The machinery worked; this IS the measurement. A domain
                # outcome, never a Refusal.
                emit({"outcome": "merge_conflict", "conflicted_paths": conflicted})
                return EXIT_MERGE_CONFLICT
            raise Refusal(
                f"merge of harvested package {harvested_commit} onto {feature_branch} failed: "
                f"{redact_secret_text(merge_proc.stderr.strip())}",
                "the merge failed without leaving conflict markers; the staging machinery could "
                "not produce an answer",
                code=EXIT_ENVIRONMENT,
            )

        changed_raw = git(stage_dir, "diff", "--name-only", "-z", feature_commit)
        changed = split_nul_paths(changed_raw)
        guarded = [p for p in changed if p == ".github" or p.startswith(".github/")]
        if guarded:
            # Unbypassable, and checked against the feature branch — no gate
            # verdict can exist yet to bypass it, mirroring
            # internal/handover/harvest.go's StageCandidate ordering.
            emit(
                {
                    "outcome": "routes_to_human",
                    "guarded_paths": guarded,
                    "changed_paths": changed,
                }
            )
            return EXIT_ROUTES_TO_HUMAN

        git(
            stage_dir,
            "-c",
            "user.name=Culture Nodes Combining Loop",
            "-c",
            "user.email=combining-loop@culture-nodes.invalid",
            "commit",
            "--no-edit",
        )
        candidate_commit = git(stage_dir, "rev-parse", "HEAD")
        # Post-condition (h7): re-read the parents, not assume them from inputs.
        candidate_parents = git(stage_dir, "show", "-s", "--format=%P", candidate_commit).split()
        # Graph mode: a code node's output document carries operation metadata,
        # never program stdout, so the candidate travels to `gate` and `merge`
        # as a DETERMINISTIC REF keyed by the run id the runner boundary
        # already forwards. CLI callers (no NODES_RUN_ID) read stdout instead.
        run_id = os.environ.get("NODES_RUN_ID", "")
        if run_id:
            validate_run_id(run_id)
            candidate_ref = f"refs/culture-nodes/candidate/{run_id}"
            git(workspace, "update-ref", candidate_ref, candidate_commit)
            # Ephemeral-workspace rule (same as harvest's recovery ref): the
            # ref must survive this container, so graph mode also pushes it
            # to the granted remote; `gate` and `merge` fetch it back.
            remote = os.environ.get(HARVEST_REMOTE_ENV, "")
            if remote:
                git(workspace, "push", "--", remote, f"+{candidate_ref}:{candidate_ref}")
        emit(
            {
                "outcome": "candidate_staged",
                "candidate_commit": candidate_commit,
                "verified": {"candidate_parents": candidate_parents},
            }
        )
        return EXIT_OK
    finally:
        # Remove unconditionally: the candidate commit, if one was created,
        # already lives in `workspace`'s shared object database and needs no
        # working tree to be resolved later by `merge`.
        subprocess.run(  # noqa: S603 # nosec B603
            ["git", "-C", str(workspace), "worktree", "remove", "--force", str(stage_dir)],
            capture_output=True,
            text=True,
            check=False,
        )


# --------------------------------------------------------------------------
# merge
# --------------------------------------------------------------------------

ASKPASS_SCRIPT = (
    "#!/bin/sh\n"
    'case "$1" in\n'
    "  *Username*) printf '%s\\n' x-access-token ;;\n"
    "  *) printf '%s\\n' \"$" + WORKER_PUSH_CREDENTIAL + '" ;;\n'
    "esac\n"
)


#: Reset for the push AND its post-push `git ls-remote` verification (same askpass identity).
RESET_ARGS = ("-c", "credential.helper=", "-c", "credential.https://github.com.helper=")


def cmd_merge(_args: argparse.Namespace) -> int:
    payload = read_input(frozenset({"candidate_commit", "feature_branch", "verdict"}))
    feature_branch = require_branch(payload, "feature_branch")

    # Graph mode: inside examples/combining-loop, this node is reachable from
    # gate.gates_passed and from NOWHERE else (a compiler-checked property,
    # the merge-gate example's own argument), and both gate and merge resolve
    # the SAME refs/culture-nodes/candidate/<run-id> ref with
    # maxVisitsPerNode: 1 — so reachability plus ref identity is the verdict
    # fence. The residual trust is the workspace host not rewriting that ref
    # mid-run, stated in the workflow header. CLI callers keep the explicit
    # verdict object and its no-TOCTOU check below.
    graph_mode = (
        "candidate_commit" not in payload
        and "verdict" not in payload
        and bool(os.environ.get("NODES_RUN_ID", ""))
    )
    if graph_mode:
        run_id = os.environ["NODES_RUN_ID"]
        validate_run_id(run_id)
        candidate_ref = f"refs/culture-nodes/candidate/{run_id}"
        workspace_early = resolve_workspace()
        # Ephemeral-workspace rule: `stage` pushed the candidate ref to the
        # granted remote; a fresh workspace fetches it back before resolving.
        # A workspace that already has it (shared checkouts, the CLI tests)
        # skips the fetch.
        probe = subprocess.run(  # noqa: S603 # nosec B603 - fixed binary, argv list
            [
                "git",
                "-C",
                str(workspace_early),
                "rev-parse",
                "--verify",
                "--quiet",
                f"{candidate_ref}^{{commit}}",
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        if probe.returncode != 0:
            fetch_remote = env_or_refuse(
                HARVEST_REMOTE_ENV,
                "graph-mode merge in a fresh workspace fetches the candidate ref from "
                "the deployment's granted remote",
            )
            git(
                workspace_early,
                "fetch",
                "--no-tags",
                "--",
                fetch_remote,
                f"+{candidate_ref}:{candidate_ref}",
            )
        candidate_commit = git(
            workspace_early, "rev-parse", "--verify", f"{candidate_ref}^{{commit}}"
        )
    else:
        candidate_commit = require_commit(payload, "candidate_commit")

    verdict = payload.get("verdict")
    if graph_mode:
        verdict = {"outcome": "gates_passed", "candidate": candidate_commit}
    if not isinstance(verdict, dict):
        raise Refusal(
            "'verdict' is required and must be a JSON object",
            'NODES_INPUT_JSON must declare {"verdict": {"outcome": ..., "candidate": ...}}',
            code=EXIT_REFUSED,
        )
    verdict_unknown = sorted(set(verdict) - {"outcome", "candidate"})
    if verdict_unknown:
        named = ", ".join(repr(key) for key in verdict_unknown)
        raise Refusal(
            f"'verdict' declares {named}, which this subcommand never reads",
            'the verdict object carries exactly {"outcome", "candidate"}',
            code=EXIT_REFUSED,
        )
    outcome = verdict.get("outcome")
    verdict_candidate = verdict.get("candidate")

    # No TOCTOU: the outcome and the candidate it is ABOUT are checked
    # together, against the exact candidate this invocation is about to
    # merge. A verdict for an earlier candidate can never authorize pushing a
    # later one. Mirrors internal/handover/merge.go's MergeCandidate exactly.
    if outcome != "gates_passed" or verdict_candidate != candidate_commit:
        raise Refusal(
            f"verdict outcome is {outcome!r} for candidate {verdict_candidate!r}; this node is "
            f"about to merge {candidate_commit!r}",
            "the merge is refused unless the verdict says gates_passed for exactly this "
            "candidate — a stale or mismatched verdict authorizes nothing",
            code=EXIT_REFUSED,
        )

    env_or_refuse(
        WORKER_PUSH_CREDENTIAL,
        f"grant {WORKER_PUSH_CREDENTIAL} to the operation; refusing a push that would prompt "
        "for operator credentials",
    )

    workspace = resolve_workspace()
    branch_ref = f"refs/heads/{feature_branch}"

    resolved_candidate = git(workspace, "rev-parse", "--verify", f"{candidate_commit}^{{commit}}")
    if resolved_candidate != candidate_commit:
        raise Refusal(
            f"candidate resolved to {resolved_candidate}, want {candidate_commit}",
            "the candidate commit named by the input is not the object this repository has",
            code=EXIT_ENVIRONMENT,
        )

    parents = git(workspace, "show", "-s", "--format=%P", candidate_commit).split()
    if len(parents) != 2:
        raise Refusal(
            f"candidate {candidate_commit} has {len(parents)} parent(s), want the two-parent "
            "commit a merge --no-ff produces",
            "this is not a candidate `stage` produced; merge only advances onto one",
            code=EXIT_ENVIRONMENT,
        )

    feature_tip = git(workspace, "rev-parse", "--verify", f"{branch_ref}^{{commit}}")
    if feature_tip != parents[0]:
        raise Refusal(
            f"feature branch moved: {branch_ref} is at {feature_tip}, candidate was built on "
            f"{parents[0]}",
            "a concurrent feature-branch update means the gated candidate is stale; re-stage "
            "and re-gate rather than force through",
            code=EXIT_ENVIRONMENT,
        )

    # Atomic compare-and-swap: refused rather than overwritten if the branch
    # moved between the read above and this write.
    git(workspace, "update-ref", branch_ref, candidate_commit, feature_tip)

    askpass_dir = Path(tempfile.mkdtemp(prefix="combining-loop-askpass-"))
    try:
        askpass = askpass_dir / "askpass.sh"
        askpass.write_text(ASKPASS_SCRIPT, encoding="utf-8")
        askpass.chmod(0o700)
        push_env = {
            **os.environ,
            "GIT_TERMINAL_PROMPT": "0",
            "GCM_INTERACTIVE": "never",
            "GIT_ASKPASS": str(askpass),
        }
        # A configured credential helper SILENTLY outranks GIT_ASKPASS on
        # push — measured on this fleet, and the exact fix
        # internal/handover/merge.go just landed for the Go merge path.
        git(
            workspace,
            *RESET_ARGS,
            "push",
            "--porcelain",
            "--",
            MERGE_REMOTE,
            f"{branch_ref}:{branch_ref}",
            env=push_env,
        )

        # Post-condition (h7): re-read the remote, not trust the push's exit code.
        ls_remote_out = git(
            workspace, *RESET_ARGS, "ls-remote", "--", MERGE_REMOTE, branch_ref, env=push_env
        )
    finally:
        shutil.rmtree(askpass_dir, ignore_errors=True)

    remote_sha = (ls_remote_out.split() or [""])[0]
    if remote_sha != candidate_commit:
        raise Refusal(
            f"push to {MERGE_REMOTE} {branch_ref} exited 0, but git ls-remote reports "
            f"{remote_sha or '<no ref>'} there, not the pushed candidate {candidate_commit}",
            "a push that exits 0 is not proof the remote advanced; this catches a transport "
            "that reports success on a rejected update",
            code=EXIT_ENVIRONMENT,
        )

    emit(
        {
            "merged_commit": candidate_commit,
            "feature_branch": feature_branch,
            "verified": {"remote": MERGE_REMOTE, "remote_sha": remote_sha},
        }
    )
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="combining-loop-node.py",
        description=(
            "The combining-loop workflow's code node: harvest a published handover, stage it "
            "as a candidate, or merge a gated candidate. Each subcommand reads its own input "
            "from NODES_INPUT_JSON; see the module docstring (`pydoc` or the file itself) for "
            "the full input schema, grants, and exit-code contract."
        ),
    )
    subparsers = parser.add_subparsers(dest="subcommand", required=True)

    harvest = subparsers.add_parser(
        "harvest",
        help="fetch a claimed refs/culture-nodes/ ref and pin it before comparing it to what "
        "was claimed",
        description=(
            'Input (NODES_INPUT_JSON): {"handover_ref": "refs/culture-nodes/...", '
            '"expected_commit": "<sha>"}. Grant: HARVEST_REMOTE (a URL). '
            "Exit 0 harvested / 2 environment / 4 refused (ref outside refs/culture-nodes/, "
            "or the fetched commit does not match expected_commit)."
        ),
    )
    harvest.set_defaults(handler=cmd_harvest)

    stage = subparsers.add_parser(
        "stage",
        help="merge a harvested commit onto the feature branch as a detached candidate",
        description=(
            'Input (NODES_INPUT_JSON): {"harvested_commit": "<sha>", '
            '"feature_branch": "<name>"}. '
            'Exit 0 candidate_staged (prints {"candidate_commit": ...}) / '
            "1 merge_conflict (domain outcome) / 2 environment / "
            "3 routes_to_human (candidate diff touches .github/, unbypassable) / "
            "4 refused (malformed input)."
        ),
    )
    stage.set_defaults(handler=cmd_stage)

    merge = subparsers.add_parser(
        "merge",
        help="fast-forward and push the feature branch to a gates_passed candidate",
        description=(
            'Input (NODES_INPUT_JSON): {"candidate_commit": "<sha>", '
            '"feature_branch": "<name>", '
            '"verdict": {"outcome": "gates_passed", "candidate": "<sha>"}}. '
            "Grant: GITHUB_TOKEN_WORKER. "
            "Exit 0 merged / 2 environment / 4 refused (verdict does not say gates_passed for "
            "exactly this candidate)."
        ),
    )
    merge.set_defaults(handler=cmd_merge)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.handler(args))
    except Refusal as refusal:
        print(f"error: {refusal}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code


if __name__ == "__main__":
    raise SystemExit(main())
