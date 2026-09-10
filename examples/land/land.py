#!/usr/bin/env python3
"""The land node core (loop-closure task t6; spec c3/c15/c35/c39/c40, honesty
h9/h12/h22/h26/h27; issues #315 and #309 step 2).

A deterministic code node the nodes-runner executes under the culture-land
engine account. It takes ONE handover ref a finished actor run produced and
puts it on the PR branch the way the operator did by hand last cycle -- fetch,
rebase onto the tip, push -- and writes down every step it reached, so a
landing that stopped half way is visible in the ledger and a re-run continues
instead of pushing twice. Three siblings the bootstrap fetches beside it carry
the rest: land_gate.py (the gate chain and the single version bump, t7),
land_reply.py (reply + resolve, t8), land_probe.py (the control-plane half of
the checkout lease).

NODES_INPUT_JSON carries handover_ref (refs/culture-nodes/<run-id>/<tail>),
handover_remote (where the producing actor's refs are fetchable),
target_branch, work_item (the join key, c2), and the PRODUCING actor_id/run_id.

# The steps -- one `land_step` JSON line on stdout each (the runner stores
# stdout as attempt evidence, #189), then one `land_result` line:
#   workspace fetch lease rebase gate push reply resolve checkout_lease reset

README.md carries the same ten with what each does on trouble. The one
easiest to get wrong is the push: a rejection is CLASSIFIED, not assumed. A
STALE rejection (the branch moved under us) re-fetches and rebases ONCE more
(MAX_LAND_ROUNDS = 2), then routes to a human. A POLICY rejection -- branch
protection, a pre-receive hook, the token's permissions -- is a refusal
naming what the remote SAID: another round earns the same answer, and "the
branch moved twice" is a sentence nobody can act on.

# Exit codes (the graph routes on them through a decision node)

    0 landed   2 environment   3 routed_human   4 refused   5 waiting

A code node's outcomes are only passed/failed to the worker
(internal/worker/code.go); workflow.yaml routes `failed` on `output.exit_code`
-- the development-loop / combining-loop idiom.

# The lease model (the plan asked for the choice to be recorded)

Both leases are LOCK DIRECTORIES -- mkdir is atomic everywhere git runs,
locally and over ssh -- holding a holder.json (land run, pid, host): the
branch lease in the land checkout's .git, the one place every lander of a
deployment shares, so two landers serialise without a control-plane round
trip; the checkout lease in the producing checkout's .git, reached through
the same transport the fetch used (a local path, or ssh://user@host/path)
and paired with the fact a file cannot know -- whether the engine has a LIVE
attempt on that actor's KEY (land_probe.py carries that half, including why
a probe that could not MEASURE waits rather than resets). A stale local lock
(same host, pid dead) is reclaimed and the record says so; a lock this node
cannot judge is honoured. Nothing is written to the control plane: an AGENT
actor's bearer cannot write a lease row. Waiting is non-blocking
(LAND_LEASE_WAIT_SECONDS=0): the graph parks and re-enters.

# What this never does

No `--force`, no force refspec, no merge API call, no PR merge
(human-merges-pr is the only merge path, c15/c38), no operator Access cookie,
and no URL opened by land.py itself -- the only HTTP is the siblings'. The
push token comes from the environment or ~/.culture-nodes/bridge-push.env
(LAND_PUSH_ENV_FILE), never from the input, never in argv; diagnostics are
redacted. This run's identity comes from the runner boundary (NODES_RUN_ID /
NODES_NODE_RUN_ID / NODES_ATTEMPT_ID), the checkout from NODES_WORKSPACE
(default cwd, and MEASURED before the fetch), the routing record's producer
from LAND_ACTOR_ID (default company/land).
"""

from __future__ import annotations

import importlib
import json
import os
import posixpath
import re
import shlex
import shutil
import socket
import subprocess  # noqa: S404 # nosec B404 - fixed git/ssh binaries, argv lists, no shell
import sys
import tempfile
import time
import urllib.parse
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator

EXIT_LANDED = 0
EXIT_ENVIRONMENT = 2
EXIT_ROUTED_HUMAN = 3
EXIT_REFUSED = 4
EXIT_WAITING = 5

#: The steps in order: a landing writes one record per name, a run that
#: stopped early writes the prefix it reached.
STEPS = tuple("workspace fetch lease rebase gate push reply resolve checkout_lease reset".split())

#: One re-fetch-and-rebase after a non-fast-forward rejection, then a human
#: (spec: "bounded to two rounds", mirroring internal/repair.MaxAttempts).
MAX_LAND_ROUNDS = 2

REF_NAMESPACE = "refs/culture-nodes/"
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")
BRANCH_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]*$")
_SCP_LIKE_REMOTE = re.compile(r"^(?P<target>[^@/:]+@[^/:]+):(?P<path>[^/].*)$")

PUSH_REMOTE = "origin"
PUSH_CREDENTIAL = "GITHUB_TOKEN_WORKER"
DEFAULT_PUSH_ENV_FILE = "~/.culture-nodes/bridge-push.env"
LOCK_DIR_NAME = "culture-nodes-land"
GIT_TIMEOUT_SECONDS = 300.0

#: The routing record vocabulary, in internal/repair/route.go's shape. The two
#: land-specific reasons extend that file's Reason enum for situations only a
#: lander meets; `out_of_workflow_scope` is inherited verbatim (#102). The
#: router method differs from repair's `gate_failure_routing` so that
#: repair.PriorAttempts never counts a landing's routing as a repair round.
ROUTER_COLLECTION_METHOD = "land_routing"
REASON_REBASE_CONFLICT = "rebase_conflict"
REASON_STALE_AFTER_RETRY = "stale_after_retry"
REASON_OUT_OF_WORKFLOW_SCOPE = "out_of_workflow_scope"
GUARDED_PATH_PREFIXES = (".github/",)
GUARDED_BARE = tuple(p.rstrip("/") for p in GUARDED_PATH_PREFIXES)
BOUND = {"max_attempts": 2, "window_seconds": 86400, "at_ceiling": "route to a human node"}

#: The producing actor's KEY, when the operator declares it rather than
#: letting the probe resolve it from the row id (land_probe.actor_rows).
PRODUCING_ACTOR_KEY = "LAND_PRODUCING_ACTOR_KEY"
#: The wait reason when the control-plane probe could not measure at all.
REASON_ATTEMPTS_UNMEASURED = "active_attempts_unmeasured"

#: A `!` rejection whose reason is one of these is the branch MOVING under the
#: landing -- the only rejection another round can fix; everything else the
#: remote rejects is its policy speaking, and would say so again. The last two
#: are the same race in compare-and-swap form (a branch that moves between git
#: reading the old value and the remote updating the ref).
STALE_REJECTION_MARKERS = (
    "non-fast-forward",
    "fetch first",
    "stale info",
    "failed to update ref",
    "cannot lock ref",
)

#: The routing rationales, operator-facing sentences in the record.
RATIONALE_CONFLICT = (
    "rebasing {handover} onto {branch}@{tip} conflicts in {paths}; the rebase was aborted, "
    "nothing was pushed, and the conflict goes to a person -- a lander that resolved it "
    "would be inventing content nobody reviewed"
)
RATIONALE_SCOPE = (
    "the handover changes {paths}, which a dispatch may not modify -- GitHub Actions "
    "administration is separately authorized work (#102); it goes to a person and nothing "
    "is pushed"
)
RATIONALE_STALE = (
    "{ref} moved under this landing {rounds} times in a row; the bound is {max} "
    "rebase-and-push rounds, so it goes to a person rather than racing a writer that keeps "
    "winning"
)

ASKPASS_SCRIPT = (
    "#!/bin/sh\n"
    'case "$1" in\n'
    "  *Username*) printf '%s\\n' x-access-token ;;\n"
    "  *) printf '%s\\n' \"$" + PUSH_CREDENTIAL + '" ;;\n'
    "esac\n"
)
#: A configured credential helper silently outranks GIT_ASKPASS on push
#: (measured on this fleet; internal/handover/merge.go carries the same fix).
RESET_HELPER_ARGS = ("-c", "credential.helper=", "-c", "credential.https://github.com.helper=")


class Refusal(Exception):
    def __init__(self, message: str, hint: str, code: int = EXIT_ENVIRONMENT) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


class Routed(Exception):
    """The landing was routed to a human; the record is already written."""


class Waiting(Exception):
    """A lease is held elsewhere; the wait record is already written."""


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def short(sha: str) -> str:
    return sha[:12]


# -- identity and records ---------------------------------------------------


@dataclass(frozen=True)
class LandIdentity:
    """This LAND run, from the runner boundary -- distinct from the producing
    run named in the input. The routing record is attributed to it."""

    run_id: str
    node_run_id: str
    attempt_id: str
    actor_id: str
    revision: str

    @classmethod
    def from_env(cls) -> LandIdentity:
        env = os.environ.get
        return cls(
            run_id=env("NODES_RUN_ID", ""),
            node_run_id=env("NODES_NODE_RUN_ID", ""),
            attempt_id=env("NODES_ATTEMPT_ID", ""),
            actor_id=env("LAND_ACTOR_ID", "company/land"),
            revision=env("LAND_REVISION", ""),
        )


class Records:
    """One JSON line per record on stdout. Every record carries the join key
    (work_item), the producing run and the land run, so a ledger reader can
    tell which landing a step belongs to without the graph."""

    def __init__(self, common: dict[str, Any], land: LandIdentity) -> None:
        self.common = common
        self.land = land

    @staticmethod
    def _emit(record: dict[str, Any]) -> dict[str, Any]:
        sys.stdout.write(json.dumps(record) + "\n")
        sys.stdout.flush()
        return record

    def step(self, name: str, outcome: str, **fields: Any) -> dict[str, Any]:
        head = {"record": "land_step", "step": name, "outcome": outcome, "at": now_iso()}
        return self._emit({**head, **self.common, **fields})

    def result(self, outcome: str, **fields: Any) -> dict[str, Any]:
        head = {"record": "land_result", "outcome": outcome, "at": now_iso()}
        return self._emit({**head, **self.common, **fields})

    def routing(self, reason: str, rationale: str, **extra: Any) -> dict[str, Any]:
        """A derived decision selecting a human, in internal/repair's shape:
        computed from recorded facts, dispatching nothing, by an identified
        deterministic producer (PRD 10.4)."""
        subject = f"{self.common['handover_ref']} on {self.common['target_branch']}"
        data = {
            "question": f"landing {subject} could not proceed -- where does that go?",
            "selected": "human",
            "options": ["repair", "human"],
            "reason": reason,
            "rationale": rationale,
            "router": ROUTER_COLLECTION_METHOD,
            "bound": {**BOUND, "deadline": None},
            "attempt_number": 0,
            "attempts_used": 0,
            "attempts_remaining": BOUND["max_attempts"],
            "dispatched": False,
            "dispatch_note": "the land node decided and recorded the route; it did not dispatch "
            "it. A landing a human must look at is not retried by the node (spec c35)",
            "work_item": self.common["work_item"],
            "producing_run_id": self.common["run_id"],
            "producing_actor_id": self.common["actor_id"],
            **extra,
        }
        land = self.land
        origin = {"kind": "validator", "actor_id": land.actor_id, "actor_revision": land.revision}
        return self._emit(
            {
                "record": "routing",
                "record_type": "decision",
                "authority": "derived",
                "origin": origin,
                "run_id": self.land.run_id,
                "node_run_id": self.land.node_run_id,
                "attempt_id": self.land.attempt_id,
                "subject_ref": None,
                "provenance_refs": [],
                "at": now_iso(),
                "work_item": self.common["work_item"],
                "land_run_id": self.land.run_id,
                "data": data,
            }
        )


# -- git --------------------------------------------------------------------


def redact(text: str) -> str:
    token = os.environ.get(PUSH_CREDENTIAL)
    return text.replace(token, "<redacted>") if token else text


def git_env(extra: dict[str, str] | None) -> dict[str, str]:
    env = {**os.environ, "GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never"}
    env["GIT_EDITOR"] = "true"
    # A rebase needs a committer; the account may not have one configured.
    env.setdefault("GIT_COMMITTER_NAME", "culture-nodes land node")
    env.setdefault("GIT_COMMITTER_EMAIL", "land@culture-nodes.invalid")
    return {**env, **(extra or {})}


def git(
    cwd: Path | str, *args: str, check: bool = True, env: dict[str, str] | None = None
) -> subprocess.CompletedProcess:
    try:
        proc = subprocess.run(  # nosec B603 B607 - fixed binary on PATH, argv list
            ["git", *args],
            cwd=str(cwd),
            capture_output=True,
            text=True,
            timeout=GIT_TIMEOUT_SECONDS,
            env=git_env(env),
            check=False,
        )
    except FileNotFoundError as exc:
        raise Refusal("git is not on PATH", "install git on the runner host") from exc
    except subprocess.TimeoutExpired as exc:
        raise Refusal(f"git {args[0]} timed out", "a stuck git is an environment fault") from exc
    if check and proc.returncode != 0:
        detail = redact(proc.stderr.strip() or proc.stdout.strip())
        raise Refusal(f"git {' '.join(args[:2])} failed ({proc.returncode}): {detail}", "see git")
    return proc


def out(proc: subprocess.CompletedProcess) -> str:
    return proc.stdout.strip()


def git_dir(repo: Path) -> Path:
    return Path(out(git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")))


def sanitize_ref_component(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]", "-", value)


# -- input ------------------------------------------------------------------

INPUT_KEYS = ("actor_id", "handover_ref", "handover_remote", "run_id", "target_branch", "work_item")


@dataclass(frozen=True)
class Inputs:
    actor_id: str
    handover_ref: str
    handover_remote: str
    run_id: str
    target_branch: str
    work_item: str


def refused(message: str, hint: str) -> Refusal:
    return Refusal(message, hint, EXIT_REFUSED)


def read_inputs() -> Inputs:
    raw = os.environ.get("NODES_INPUT_JSON")
    if not raw:
        raise refused("NODES_INPUT_JSON is not set", "the runner boundary supplies the bound input")
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise refused(f"NODES_INPUT_JSON is not JSON: {exc}", "fix the binding") from exc
    if not isinstance(payload, dict):
        raise refused("NODES_INPUT_JSON is not an object", "fix the binding")
    unknown = sorted(set(payload) - set(INPUT_KEYS))
    if unknown:
        raise refused(f"unknown input key(s): {', '.join(unknown)}", f"reads exactly {INPUT_KEYS}")
    values: dict[str, str] = {}
    for key in INPUT_KEYS:
        value = payload.get(key)
        if not isinstance(value, str) or not value.strip():
            raise refused(f"input {key} must be a non-empty string", "bind it from the run input")
        values[key] = value.strip()
    inputs = Inputs(**values)
    validate_ref(inputs.handover_ref, inputs.run_id)
    branch = inputs.target_branch
    if not BRANCH_PATTERN.match(branch) or branch.startswith("refs/"):
        raise refused(f"target_branch {branch!r} is not a branch name", "pass the branch name")
    if inputs.handover_remote.startswith("-"):
        raise refused("handover_remote would be read by git as an option", "fix the actor metadata")
    return inputs


def validate_ref(ref: str, run_id: str) -> None:
    """The client-side half of internal/handover.ValidateRef, plus the run
    fence: preserve.py mints refs/culture-nodes/<run-id>/<tail>, so a ref
    under another run's id is not this run's handover."""
    hint = "only a bridge-minted handover ref of the named run is ever fetched"
    if not ref.startswith(REF_NAMESPACE) or ".." in ref or ref.endswith("/") or ref != ref.strip():
        raise refused(f"handover_ref {ref!r} is outside {REF_NAMESPACE}", hint)
    parts = ref[len(REF_NAMESPACE) :].split("/")
    if len(parts) < 2 or not all(parts):
        raise refused(f"handover_ref {ref!r} is not refs/culture-nodes/<run-id>/<tail>", hint)
    if parts[0] != sanitize_ref_component(run_id):
        raise refused(f"handover_ref {ref!r} was minted under run {parts[0]}, not {run_id}", hint)


# -- the producing checkout, reached through handover_remote ----------------


@dataclass
class Checkout:
    """How this node reaches the producing actor's checkout: the place its
    handover ref was fetched from. `kind` is local (a path on this host), ssh
    (user@host + path), or none (an https remote is a server, not a checkout
    anyone can reset)."""

    kind: str
    path: str = ""
    ssh_target: str = ""

    @classmethod
    def from_remote(cls, remote: str) -> Checkout:
        parts = urllib.parse.urlsplit(remote)
        if parts.scheme == "ssh" and parts.netloc and parts.path:
            return cls("ssh", parts.path, parts.netloc)
        if parts.scheme in ("", "file"):
            path = parts.path if parts.scheme == "file" else remote
            if Path(path).is_dir():
                return cls("local", str(Path(path).resolve()))
        scp = _SCP_LIKE_REMOTE.match(remote)
        if scp and not parts.scheme:
            return cls("ssh", scp.group("path"), scp.group("target"))
        return cls("none")

    def git(self, *args: str, check: bool = True) -> subprocess.CompletedProcess:
        if self.kind == "local":
            return git(self.path, *args, check=check)
        return self.ssh(["git", "-C", self.path, *args], check=check)

    def ssh(self, argv: list[str], check: bool = True) -> subprocess.CompletedProcess:
        # The `--` goes AFTER the destination, which is where ssh consumes it:
        # it ends local option parsing and is not sent as the first word of the
        # remote command. Measured against a live sshd (OpenSSH 9.6p1) with this
        # exact argv: SSH_ORIGINAL_COMMAND is `remote_cmd` alone. It is what
        # stops a remote_cmd starting with `-` from being read as a local flag;
        # the destination itself is operator config (cutover.sh), not run input.
        remote_cmd = " ".join(shlex.quote(a) for a in argv)
        options = ("-o", "BatchMode=yes", "-o", "ConnectTimeout=15")
        proc = subprocess.run(  # nosec B603 B607 - fixed binary on PATH, argv list
            ["ssh", *options, self.ssh_target, "--", remote_cmd],
            capture_output=True,
            text=True,
            timeout=GIT_TIMEOUT_SECONDS,
            check=False,
        )
        if check and proc.returncode != 0:
            detail = redact(proc.stderr.strip())
            raise Refusal(
                f"ssh {self.ssh_target} {argv[0]} failed ({proc.returncode}): {detail}",
                "the producing checkout's host must accept the land account's key",
            )
        return proc

    def lock(self) -> Lock:
        if self.kind == "local":
            return LocalLock(checkout_lock_path(Path(self.path)))
        return SshLock(self, posixpath.join(self.path, ".git", LOCK_DIR_NAME, "checkout.lock"))


# -- leases: lock directories with a holder record --------------------------


def branch_lock_path(workspace: Path, branch: str) -> Path:
    return git_dir(workspace) / LOCK_DIR_NAME / f"branch-{sanitize_ref_component(branch)}.lock"


def checkout_lock_path(checkout: Path) -> Path:
    return git_dir(checkout) / LOCK_DIR_NAME / "checkout.lock"


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def holder_record(land: LandIdentity) -> dict[str, Any]:
    return {
        "land_run_id": land.run_id,
        "attempt_id": land.attempt_id,
        "pid": os.getpid(),
        "host": socket.gethostname(),
        "at": now_iso(),
    }


Acquired = tuple[bool, "dict[str, Any] | None", bool]  # (acquired, current holder, reclaimed_stale)


class Lock:
    def acquire(self, holder: dict[str, Any]) -> Acquired:
        raise NotImplementedError

    def release(self) -> None:
        raise NotImplementedError

    def acquire_or_wait(self, holder: dict[str, Any]) -> Acquired:
        wait = float(os.environ.get("LAND_LEASE_WAIT_SECONDS") or 0)
        poll = float(os.environ.get("LAND_LEASE_POLL_SECONDS") or 1)
        deadline = time.monotonic() + wait
        while True:
            result = self.acquire(holder)
            if result[0] or time.monotonic() >= deadline:
                return result
            time.sleep(poll)

    @contextmanager
    def held(self, holder: dict[str, Any]) -> Iterator[None]:
        acquired, current, _ = self.acquire(holder)
        if not acquired:
            raise Refusal(f"lock already held by {current}", "release it first")
        try:
            yield
        finally:
            self.release()


class LocalLock(Lock):
    def __init__(self, path: Path) -> None:
        self.path = path

    def read_holder(self) -> dict[str, Any] | None:
        try:
            return json.loads((self.path / "holder.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None

    @staticmethod
    def is_stale(holder: dict[str, Any] | None) -> bool:
        # Only a lock this host can judge: same host, and the pid is gone.
        if not holder or holder.get("host") != socket.gethostname():
            return False
        pid = holder.get("pid")
        return isinstance(pid, int) and not pid_alive(pid)

    def acquire(self, holder: dict[str, Any]) -> Acquired:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        reclaimed = False
        for _attempt in range(2):
            try:
                os.mkdir(self.path)
            except FileExistsError:
                current = self.read_holder()
                if self.is_stale(current) and not reclaimed:
                    shutil.rmtree(self.path, ignore_errors=True)
                    reclaimed = True
                    continue
                return False, current, False
            (self.path / "holder.json").write_text(json.dumps(holder), encoding="utf-8")
            return True, holder, reclaimed
        return False, self.read_holder(), False

    def release(self) -> None:
        shutil.rmtree(self.path, ignore_errors=True)


class SshLock(Lock):
    """The same lock directory on the producing checkout's host. No stale
    reclaim: a pid on another host is not something this node can judge, so
    a lock there is honoured until its holder or a human removes it."""

    def __init__(self, checkout: Checkout, path: str) -> None:
        self.checkout = checkout
        self.path = path

    def acquire(self, holder: dict[str, Any]) -> Acquired:
        q = shlex.quote
        script = (
            f"mkdir -p {q(posixpath.dirname(self.path))} && mkdir {q(self.path)} "
            f"&& printf %s {q(json.dumps(holder))} > {q(self.path + '/holder.json')}"
        )
        if self.checkout.ssh(["sh", "-c", script], check=False).returncode == 0:
            return True, holder, False
        current = self.checkout.ssh(["cat", self.path + "/holder.json"], check=False)
        try:
            return False, json.loads(current.stdout), False
        except ValueError:
            return False, None, False

    def release(self) -> None:
        self.checkout.ssh(["rm", "-rf", self.path], check=False)


# -- the push credential ----------------------------------------------------


def push_token() -> str | None:
    """GITHUB_TOKEN_WORKER from the process environment, else from
    bridge-push.env (the #90 seam install-secrets.sh writes for
    culture-land). Never from the input."""
    value = os.environ.get(PUSH_CREDENTIAL)
    if value:
        return value
    path = Path(os.environ.get("LAND_PUSH_ENV_FILE") or DEFAULT_PUSH_ENV_FILE).expanduser()
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return None
    for line in lines:
        if line.strip().startswith(f"{PUSH_CREDENTIAL}="):
            return line.strip().split("=", 1)[1].strip().strip('"').strip("'") or None
    return None


def remote_needs_credential(url: str) -> bool:
    return urllib.parse.urlsplit(url).scheme in ("http", "https")


# -- the sibling modules the bootstrap fetches beside this file -------------


def sibling(module: str, attr: str):
    """One attribute of a module fetched BESIDE this file, resolved at the
    step by path -- the bootstrap verified its digest before any of it ran."""
    here = str(Path(__file__).resolve().parent)
    if here not in sys.path:
        sys.path.insert(0, here)
    return getattr(importlib.import_module(module), attr)


def gate_hook(ctx: Landing) -> dict[str, Any]:
    """t7: the gate chain and the landing's single bump (land_gate.py)."""
    return sibling("land_gate", "run_gate")(ctx, refusal=Refusal)


def reply_hook(ctx: Landing) -> dict[str, Any]:
    """t8: one reply per landed finding, naming the sha (land_reply.py)."""
    return sibling("land_reply", "reply_step")(ctx, refusal=Refusal)


def resolve_hook(ctx: Landing) -> dict[str, Any]:
    """t8: resolve each landed finding's review thread (land_reply.py)."""
    return sibling("land_reply", "resolve_step")(ctx, refusal=Refusal)


def active_attempts(api_url: str | None, actor_id: str, *, exclude_run_id: str):
    """The control-plane half of the checkout lease (land_probe.py). The
    AttemptProbe carries a COUNT only when it measured one: a failed read and
    a listing that did not end within its page bound carry none, and this
    node waits on those rather than resetting on a guess (h12)."""
    return sibling("land_probe", "active_attempts")(
        api_url,
        actor_id,
        exclude_run_id=exclude_run_id,
        actor_key=os.environ.get(PRODUCING_ACTOR_KEY) or None,
    )


# -- the landing ------------------------------------------------------------


@dataclass
class Landing:
    inputs: Inputs
    land: LandIdentity
    workspace: Path
    records: Records
    handover_commit: str = ""
    target_tip: str = ""
    landed_commit: str = ""
    token: str | None = None
    worktree: Path | None = None
    held: list[Lock] = field(default_factory=list)

    @property
    def branch_ref(self) -> str:
        return f"refs/heads/{self.inputs.target_branch}"

    def route(self, step: str, reason: str, rationale: str, **facts: Any) -> None:
        """Write the step's `routed` record and the derived routing record,
        then stop: nothing past this point runs, nothing is pushed."""
        self.records.step(step, "routed", reason=reason, **facts)
        facts.setdefault("handover_commit", self.handover_commit)
        self.records.routing(reason, rationale, **facts)
        raise Routed

    # -- fetch ---------------------------------------------------------------

    def fetch(self) -> None:
        remote, ref = self.inputs.handover_remote, self.inputs.handover_ref
        git(self.workspace, "fetch", "--no-tags", "--quiet", "--", remote, ref)
        sha = out(git(self.workspace, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}"))
        if not COMMIT_PATTERN.match(sha):
            raise Refusal(f"the fetch of {ref} produced no commit", "the ref must name a commit")
        self.handover_commit = sha
        subject = out(git(self.workspace, "log", "-1", "--format=%s", sha))
        kind = Checkout.from_remote(remote).kind
        self.records.step("fetch", "ok", commit=sha, subject=subject, remote_kind=kind)

    # -- the credential, before the first contact with origin ---------------

    def resolve_credential(self) -> None:
        """An https origin needs the token to fetch a private branch as much
        as to push, and a lease should not be held for a landing that cannot
        end -- so this runs before the lease and before fetch_tip."""
        origin_url = out(git(self.workspace, "remote", "get-url", PUSH_REMOTE))
        if not remote_needs_credential(origin_url):
            return
        self.token = push_token()
        if not self.token:
            scheme = urllib.parse.urlsplit(origin_url).scheme
            raise Refusal(
                f"{PUSH_REMOTE} is {scheme}:// and {PUSH_CREDENTIAL} is not available",
                "install culture-land's bridge-push.env (deploy/prod/lanes/land-secrets.sh)",
            )

    @contextmanager
    def origin_env(self) -> Iterator[dict[str, str]]:
        """GIT_ASKPASS wired to the token for the duration of one or more
        origin operations; nothing when the remote needs no credential."""
        if not self.token:
            yield {}
            return
        askpass_dir = Path(tempfile.mkdtemp(prefix="culture-nodes-land-askpass-"))
        try:
            askpass = askpass_dir / "askpass.sh"
            askpass.write_text(ASKPASS_SCRIPT, encoding="utf-8")
            askpass.chmod(0o700)
            yield {PUSH_CREDENTIAL: self.token, "GIT_ASKPASS": str(askpass)}
        finally:
            shutil.rmtree(askpass_dir, ignore_errors=True)

    # -- the branch lease -----------------------------------------------------

    def lease_branch(self) -> None:
        lock = LocalLock(branch_lock_path(self.workspace, self.inputs.target_branch))
        acquired, current, reclaimed = lock.acquire_or_wait(holder_record(self.land))
        facts = {"scope": "target_branch", "lock": str(lock.path)}
        if not acquired:
            self.records.step("lease", "waiting", holder=current, **facts)
            raise Waiting
        self.held.append(lock)
        self.records.step("lease", "ok", reclaimed_stale=reclaimed, **facts)

    # -- rebase --------------------------------------------------------------

    def fetch_tip(self) -> str:
        with self.origin_env() as env:
            argv = (*RESET_HELPER_ARGS, "fetch", "--no-tags", "--quiet", "--", PUSH_REMOTE)
            proc = git(self.workspace, *argv, self.branch_ref, env=env, check=False)
        if proc.returncode != 0:
            detail = redact(proc.stderr.strip())
            if "couldn't find remote ref" in detail:
                raise refused(f"{self.branch_ref} is not on {PUSH_REMOTE}", "no branch is created")
            raise Refusal(f"could not fetch {self.branch_ref}: {detail}", "check the remote")
        self.target_tip = out(git(self.workspace, "rev-parse", "--verify", "FETCH_HEAD^{commit}"))
        return self.target_tip

    def already_on_branch(self) -> bool:
        args = ("merge-base", "--is-ancestor", self.handover_commit, self.target_tip)
        if git(self.workspace, *args, check=False).returncode == 0:
            return True
        cherry = out(git(self.workspace, "cherry", self.target_tip, self.handover_commit))
        lines = cherry.splitlines()
        return bool(lines) and all(line.startswith("-") for line in lines)

    def guarded_paths(self) -> list[str]:
        base = out(git(self.workspace, "merge-base", self.target_tip, self.handover_commit))
        changed = out(git(self.workspace, "diff", "--name-only", base, self.handover_commit))
        hits: list[str] = []
        for path in changed.splitlines():
            candidate = path.strip().replace("\\", "/")
            while candidate.startswith("./"):
                candidate = candidate[2:]
            guarded = candidate.startswith(GUARDED_PATH_PREFIXES) or candidate in GUARDED_BARE
            if guarded and candidate not in hits:
                hits.append(candidate)
        return hits

    def add_worktree(self) -> Path:
        if self.worktree is None:
            path = Path(tempfile.mkdtemp(prefix="culture-nodes-land-"))
            args = ("worktree", "add", "--detach", "--quiet", str(path), self.handover_commit)
            git(self.workspace, *args)
            self.worktree = path
        return self.worktree

    def remove_worktree(self) -> None:
        if self.worktree is None:
            return
        git(self.workspace, "worktree", "remove", str(self.worktree), check=False)
        shutil.rmtree(self.worktree, ignore_errors=True)
        git(self.workspace, "worktree", "prune", check=False)
        self.worktree = None

    def rebase_onto_tip(self, *, round_number: int) -> None:
        """Rebase the worktree's HEAD onto the current tip; a conflict is
        aborted (the worktree stays clean) and routed to a human."""
        wt = self.add_worktree()
        proc = git(wt, "rebase", self.target_tip, check=False)
        if proc.returncode == 0:
            self.landed_commit = out(git(wt, "rev-parse", "HEAD"))
            return
        diff = out(git(wt, "diff", "--name-only", "--diff-filter=U", check=False))
        conflicted = sorted(diff.splitlines())
        git(wt, "rebase", "--abort", check=False)
        if not conflicted:
            detail = redact(proc.stderr.strip())
            raise Refusal(f"rebase onto {short(self.target_tip)} failed: {detail}", "see git")
        rationale = RATIONALE_CONFLICT.format(
            handover=short(self.handover_commit),
            branch=self.inputs.target_branch,
            tip=short(self.target_tip),
            paths=", ".join(conflicted),
        )
        facts = {"conflicted_paths": conflicted, "target_tip": self.target_tip}
        self.route("rebase", REASON_REBASE_CONFLICT, rationale, round=round_number, **facts)

    def rebase(self) -> bool:
        """Returns True when there is something to push."""
        self.fetch_tip()
        facts = {"target_tip": self.target_tip, "handover_commit": self.handover_commit}
        if self.already_on_branch():
            self.landed_commit = self.target_tip
            self.records.step("rebase", "skipped", reason="already_on_branch", **facts)
            return False
        guarded = self.guarded_paths()
        if guarded:
            rationale = RATIONALE_SCOPE.format(paths=", ".join(guarded))
            prefixes = list(GUARDED_PATH_PREFIXES)
            facts = {"guarded_paths": guarded, "guarded_path_prefixes": prefixes, **facts}
            self.route("rebase", REASON_OUT_OF_WORKFLOW_SCOPE, rationale, **facts)
        self.rebase_onto_tip(round_number=1)
        facts["rebased_commit"] = self.landed_commit
        rewritten = self.landed_commit != self.handover_commit
        self.records.step("rebase", "ok", rewritten=rewritten, **facts)
        return True

    # -- push -----------------------------------------------------------------

    def push_once(self, env: dict[str, str]) -> bool:
        """One push of the rebased commit to the branch ref -- a plain refspec,
        never a force. Returns True when the remote rejected it as STALE (the
        branch moved; another round can fix it). A rejection the remote made
        on POLICY is a refusal naming what the remote itself said, never a
        second round that would be told the same thing again."""
        refspec = f"{self.landed_commit}:{self.branch_ref}"
        argv = (*RESET_HELPER_ARGS, "push", "--porcelain", "--", PUSH_REMOTE, refspec)
        proc = git(self.workspace, *argv, env=env, check=False)
        if proc.returncode == 0:
            return False
        rejected = [line for line in proc.stdout.splitlines() if line.startswith("!")]
        echoed = [x for x in proc.stderr.splitlines() if x.startswith("remote:")]
        said = "\n".join(rejected + echoed)
        if any(marker in said or marker in proc.stderr for marker in STALE_REJECTION_MARKERS):
            return True
        if rejected:
            detail = redact(said.strip()) or "no reason given"
            raise Refusal(
                f"{PUSH_REMOTE} rejected the push to {self.branch_ref}: {detail}",
                "branch protection, a pre-receive hook, or the push credential's permissions "
                "-- a lander does not work around a remote's policy; a person does",
                EXIT_REFUSED,
            )
        detail = redact(proc.stderr.strip())
        raise Refusal(f"push to {PUSH_REMOTE} {self.branch_ref} failed: {detail}", "see git")

    def push(self) -> None:
        rounds = 0
        with self.origin_env() as env:
            while self.push_once(env):
                rounds += 1
                if rounds >= MAX_LAND_ROUNDS:
                    rationale = RATIONALE_STALE.format(
                        ref=self.branch_ref, rounds=rounds, max=MAX_LAND_ROUNDS
                    )
                    facts = {"rounds": rounds, "target_tip": self.target_tip}
                    self.route("push", REASON_STALE_AFTER_RETRY, rationale, **facts)
                # Someone pushed between our rebase and our push. Once more.
                self.fetch_tip()
                self.rebase_onto_tip(round_number=rounds + 1)
            rounds += 1
            # h7: re-read the remote rather than trust the push's exit code.
            argv = (*RESET_HELPER_ARGS, "ls-remote", "--", PUSH_REMOTE, self.branch_ref)
            remote_sha = (out(git(self.workspace, *argv, env=env)).split() or [""])[0]
        if remote_sha != self.landed_commit:
            raise Refusal(
                f"push exited 0 but {PUSH_REMOTE} {self.branch_ref} is at {remote_sha or '<none>'}",
                "a push that exits 0 is not proof the remote advanced",
            )
        credential = "askpass" if self.token else "not_required"
        facts = {"commit": self.landed_commit, "rounds": rounds, "credential": credential}
        self.records.step("push", "ok", verified_remote_sha=remote_sha, **facts)

    # -- the producing checkout ---------------------------------------------

    def reset_checkout(self) -> None:
        checkout = Checkout.from_remote(self.inputs.handover_remote)
        api_url = os.environ.get("NODES_API_URL")
        probe = active_attempts(api_url, self.inputs.actor_id, exclude_run_id=self.land.run_id)
        facts = {"scope": "actor_checkout", "checkout": checkout.path}
        facts |= {"active_attempts": probe.count, "attempts_probe": probe.status}
        facts["attempts_detail"] = probe.detail
        if checkout.kind == "none":
            reason = "remote_is_not_a_checkout"
            self.records.step("checkout_lease", "skipped", reason=reason, **facts)
            self.records.step("reset", "skipped", reason=reason)
            return
        # h12: the count is what unlocks `checkout -B` + `reset --hard` on a
        # checkout a session may be sitting in. Not measured is not zero.
        if probe.unmeasured:
            reason = REASON_ATTEMPTS_UNMEASURED
            self.records.step("checkout_lease", "waiting", reason=reason, holder=None, **facts)
            raise Waiting
        if probe.busy:
            self.records.step("checkout_lease", "waiting", holder=None, **facts)
            raise Waiting
        lock = checkout.lock()
        acquired, current, reclaimed = lock.acquire_or_wait(holder_record(self.land))
        if not acquired:
            self.records.step("checkout_lease", "waiting", holder=current, **facts)
            raise Waiting
        self.held.append(lock)
        self.records.step("checkout_lease", "ok", reclaimed_stale=reclaimed, **facts)

        before = out(checkout.git("rev-parse", "HEAD"))
        checkout.git("fetch", "--no-tags", "--quiet", "--", PUSH_REMOTE, self.branch_ref)
        fetched = out(checkout.git("rev-parse", "--verify", "FETCH_HEAD^{commit}"))
        if fetched != self.landed_commit:
            raise Refusal(
                f"the producing checkout's {PUSH_REMOTE} {self.branch_ref} is at {short(fetched)}",
                "the checkout's origin must be the repository this node pushed to",
            )
        checkout.git("checkout", "--quiet", "-B", self.inputs.target_branch, self.landed_commit)
        checkout.git("reset", "--quiet", "--hard", self.landed_commit)
        facts = {"checkout": checkout.path, "head_before": before, "head_after": self.landed_commit}
        self.records.step("reset", "ok", remote_kind=checkout.kind, **facts)

    # -- release, and the whole thing -----------------------------------------

    def cleanup(self) -> None:
        self.remove_worktree()
        for lock in reversed(self.held):
            lock.release()
        self.held.clear()

    def run(self) -> None:
        self.records.step("workspace", **sibling("land_gate", "check_workspace")(self, Refusal))
        self.fetch()
        self.resolve_credential()
        self.lease_branch()
        must_push = self.rebase()
        self.records.step("gate", **gate_hook(self))
        if must_push:
            self.push()
        else:
            self.records.step(
                "push", "skipped", reason="already_on_branch", commit=self.landed_commit
            )
        self.records.step("reply", **reply_hook(self))
        self.records.step("resolve", **resolve_hook(self))
        self.reset_checkout()


# -- main -------------------------------------------------------------------


def main(_argv: list[str] | None = None) -> int:
    land = LandIdentity.from_env()
    workspace = Path(os.environ.get("NODES_WORKSPACE") or os.getcwd()).resolve()
    try:
        inputs = read_inputs()
    except Refusal as refusal:
        common = {"work_item": None, "run_id": None, "land_run_id": land.run_id}
        Records(common, land).result("refused", error=str(refusal), hint=refusal.hint)
        return refusal.code
    # Every input but the remote rides on every record (the remote may name a host).
    common = {k: v for k, v in asdict(inputs).items() if k != "handover_remote"}
    records = Records({**common, "land_run_id": land.run_id}, land)
    landing = Landing(inputs=inputs, land=land, workspace=workspace, records=records)
    try:
        landing.run()
        records.result("landed", landed_commit=landing.landed_commit)
        return EXIT_LANDED
    except Waiting:
        records.result("waiting", landed_commit=landing.landed_commit or None)
        return EXIT_WAITING
    except Routed:
        records.result("routed_human", landed_commit=None)
        return EXIT_ROUTED_HUMAN
    except Refusal as refusal:
        outcome = "refused" if refusal.code == EXIT_REFUSED else "environment"
        records.result(outcome, error=str(refusal), hint=refusal.hint)
        print(f"error: {refusal}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code
    finally:
        landing.cleanup()


if __name__ == "__main__":
    sys.exit(main())
