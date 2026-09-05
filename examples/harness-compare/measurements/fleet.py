#!/usr/bin/env python3
"""Fleet-facing half of the measurement runner (task t11): the control-plane
API client, the bridge revision gate, and actor/slot resolution.

Split out of ``run.py`` so the file-length contract (1000 physical lines,
``tests/lint/filelength_test.go``) holds without compressing the reasoning
either half carries. ``run.py`` is the pass — plan, dispatch, check, grade,
report; this module is everything that talks to the fleet and everything
that decides *who* is being measured.

Zero third-party dependencies (``urllib`` and ``json``).

The auth convention and every request shape here are copied from
``.claude/skills/nodes-operator/scripts/nodes-op.sh`` so the runner and the
operator lane cannot drift — see ``run.py``'s module docstring for the
verb-by-verb correspondence. The Cloudflare Access cookie is read from the
environment only, is held on the client rather than passed around, and is
never printed, logged, written to the report, or rendered into an error.
"""

from __future__ import annotations

import importlib.util
import json
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Iterable, Sequence

HERE = Path(__file__).resolve().parent

#: Run states the control plane treats as terminal (nodes-op.sh `watch`).
TERMINAL_STATES = ("completed", "failed", "cancelled")

#: actor_key local-part prefix -> harness-compare workflow slot. The workflow
#: has five FIXED slots (examples/harness-compare/workflow.yaml); a run names
#: exactly one of them, so each run measures exactly one actor.
SLOT_PREFIXES = {
    "pi": "pi",
    "qwen": "qwen",
    "codex": "codex",
    "colleague": "colleague",
    "claude": "claude",
}

#: The four claude-bridge identities on spark do not carry their harness in
#: their key, so they are named outright rather than guessed at.
SLOT_BY_NAME = {
    "developer": "claude",
    "planner": "claude",
    "verifier": "claude",
    "intake": "claude",
}

#: The qwen bridge refuses a dispatch that names no ACP session mode
#: (adapters/qwen's gate; nodes-op.sh's `--mode` note). It is the one
#: per-harness input the graph carries.
DEFAULT_QWEN_MODE = "default"

EXIT_OK = 0
EXIT_USER_ERROR = 1
EXIT_ENV_ERROR = 2


class RunnerError(Exception):
    """A refusal with a remediation, rendered as `error:` + `hint:`.

    ``code`` follows culture_nodes/cli/_errors.py's policy: 1 for a user
    error (a bad flag, a stale revision, a missing repo path), 2 for an
    environment error (the API or a bridge could not be reached).
    """

    def __init__(self, message: str, hint: str = "", code: int = EXIT_USER_ERROR) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


# ---------------------------------------------------------------------------
# Siblings are FILES, not an installed package
# ---------------------------------------------------------------------------


def load_sibling(name: str) -> Any:
    """Import ``<name>.py`` from beside this file.

    The measurements directory is not on any import path (it is an example,
    not a package), so its modules are loaded by location — the same thing
    tests/test_measurement_manifest.py does.
    """
    spec = importlib.util.spec_from_file_location(
        f"harness_compare_measurement_{name}", HERE / f"{name}.py"
    )
    if spec is None or spec.loader is None:
        raise RunnerError(
            f"could not load {name}.py from beside run.py",
            f"run.py and {name}.py must live in the same directory",
            EXIT_ENV_ERROR,
        )
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


# ---------------------------------------------------------------------------
# HTTP: the operator lane's own conventions, in urllib
# ---------------------------------------------------------------------------


#: Cloudflare Access fronts nodes.culture.dev and its bot rules answer error
#: 1010 (browser-signature ban) to urllib's default "Python-urllib/x.y"
#: user-agent, while curl's is admitted; name the tool instead.
USER_AGENT = "culture-nodes-measure-runner/1"


class ApiClient:
    """The control-plane API, with the operator lane's auth conventions.

    The cookie is held here and attached as a header; it is never logged,
    never rendered into an exception message, and never written to the
    report. ``repr`` is overridden for the same reason — a traceback that
    printed this object must not print the credential.
    """

    def __init__(
        self,
        base_url: str,
        cookie: str = "",
        bearer: str = "",
        timeout: float = 60.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self._cookie = cookie
        self._bearer = bearer
        self.timeout = timeout

    def __repr__(self) -> str:  # pragma: no cover - defensive
        return f"<ApiClient base_url={self.base_url!r} auth=<redacted>>"

    def _headers(self) -> dict[str, str]:
        headers = {"Accept": "application/json", "User-Agent": USER_AGENT}
        if self._cookie:
            headers["Cookie"] = f"CF_Authorization={self._cookie}"
        if self._bearer:
            headers["Authorization"] = f"Bearer {self._bearer}"
        return headers

    def request(self, method: str, path: str, body: Any = None) -> Any:
        url = f"{self.base_url}{path}"
        if method == "GET":
            # The Access edge (nodes.culture.dev) caches API GETs that carry
            # no Cache-Control (observed: cf-cache-status HIT, age 366 s, a
            # completed run still served as "running"), so a watch loop
            # would never see the terminal state. A unique query value
            # defeats the cache until the control plane sends no-store.
            sep = "&" if "?" in url else "?"
            url = f"{url}{sep}_nocache={time.monotonic_ns()}"
        data = None
        headers = self._headers()
        headers["Cache-Control"] = "no-cache"
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # nosec B310
                raw = resp.read()
        except urllib.error.HTTPError as exc:  # pragma: no cover - exercised via fake
            detail = _safe_detail(exc)
            raise RunnerError(
                f"{method} {path} returned HTTP {exc.code}: {detail}",
                "check NODES_API_URL and that NODES_OP_COOKIE is a current Access cookie",
                EXIT_ENV_ERROR,
            ) from None
        except urllib.error.URLError as exc:
            raise RunnerError(
                f"{method} {path} could not reach {self.base_url}: {exc.reason}",
                "check NODES_API_URL and that the control plane is up",
                EXIT_ENV_ERROR,
            ) from None
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def get(self, path: str) -> Any:
        return self.request("GET", path)

    def post(self, path: str, body: Any) -> Any:
        return self.request("POST", path, body)


def _safe_detail(exc: urllib.error.HTTPError) -> str:
    """The server's own message, truncated — never the request we sent."""
    try:
        payload = json.loads(exc.read().decode("utf-8"))
    except Exception:  # noqa: BLE001 - a non-JSON error body is still an error
        return exc.reason or "no detail"
    if isinstance(payload, dict):
        return str(payload.get("message") or payload.get("error") or payload)[:300]
    return str(payload)[:300]


def fetch_deployment(endpoint_ref: str, token: str = "", timeout: float = 20.0) -> dict[str, Any]:
    """The ``deployment`` block from a bridge's ``GET /v1/capabilities``.

    The block is a HOST fact under the capability document
    (``{"preflight": {"host": {"deployment": {...}}}}`` —
    adapters/*/preflight.py's ``capability_block`` + ``HOST_KEYS``), so it is
    looked for there first and at the top level second. A bridge that
    reports no deployment block at all is not an error here: it is recorded
    as an unknown revision, which is exactly what CLAUDE.md says such a
    bridge means ("a bridge reporting no revision was deployed by something
    that does not stamp, and its age is unknown").

    The route is authenticated whenever the bridge has a token configured
    (adapters/*/server.py ``_require_auth``), so a token may be supplied.
    """
    url = endpoint_ref.rstrip("/") + "/v1/capabilities"
    headers = {"Accept": "application/json", "User-Agent": USER_AGENT}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # nosec B310
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raise RunnerError(
            f"bridge {endpoint_ref} answered /v1/capabilities with HTTP {exc.code}",
            "the capabilities route is authenticated when the bridge has a token; "
            "pass --bridge-token <slot-or-actor-key>=TOKEN or set NODES_BRIDGE_TOKEN",
            EXIT_ENV_ERROR,
        ) from None
    except (urllib.error.URLError, OSError) as exc:
        raise RunnerError(
            f"bridge {endpoint_ref} is unreachable: {exc}",
            "check the bridge is running and its endpoint_ref is right",
            EXIT_ENV_ERROR,
        ) from None
    return extract_deployment(payload)


def extract_deployment(payload: Any) -> dict[str, Any]:
    """Pull the ``deployment`` block out of a capability document."""
    if not isinstance(payload, dict):
        return {}
    advertised = payload.get("preflight")
    if isinstance(advertised, dict):
        host = advertised.get("host")
        if isinstance(host, dict) and isinstance(host.get("deployment"), dict):
            return dict(host["deployment"])
    if isinstance(payload.get("deployment"), dict):
        return dict(payload["deployment"])
    host = payload.get("host")
    if isinstance(host, dict) and isinstance(host.get("deployment"), dict):
        return dict(host["deployment"])
    return {}


# ---------------------------------------------------------------------------
# Actors
# ---------------------------------------------------------------------------


def newest_actor_rows(items: Iterable[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """The newest registration revision per ``actor_key``.

    Registration is append-only: re-registering a key INSERTs the next
    revision row rather than mutating the old one
    (``internal/api/actors.go``), so a listing carries every revision of
    every key and the newest is the one a dispatch will resolve.
    """
    newest: dict[str, dict[str, Any]] = {}
    for row in items:
        key = row.get("actor_key")
        if not key:
            continue
        current = newest.get(key)
        if current is None or int(row.get("revision") or 0) >= int(current.get("revision") or 0):
            newest[key] = row
    return newest


def slot_for(actor_key: str, overrides: dict[str, str] | None = None) -> str:
    """The workflow slot that dispatches to *actor_key*.

    ``company/pi-thor`` -> ``pi``; ``company/qwen-orin`` -> ``qwen``;
    ``company/developer`` -> ``claude``. Refuses rather than guesses when a
    key matches nothing — a wrong slot dispatches to the wrong bridge.
    """
    if overrides and actor_key in overrides:
        return overrides[actor_key]
    local = actor_key.split("/", 1)[-1]
    if local in SLOT_BY_NAME:
        return SLOT_BY_NAME[local]
    prefix = local.split("-", 1)[0]
    if prefix in SLOT_PREFIXES:
        return SLOT_PREFIXES[prefix]
    raise RunnerError(
        f"cannot map actor {actor_key!r} to a harness-compare slot",
        "pass --slot <actor-key>=<claude|codex|qwen|pi|colleague>",
    )


def resolve_actors(
    api: ApiClient,
    manifest_actors: Sequence[str],
    slot_overrides: dict[str, str],
) -> list[dict[str, Any]]:
    """Resolve every manifest actor to its newest registered row + slot."""
    listing = api.get("/v1alpha1/actors")
    items = listing.get("items", []) if isinstance(listing, dict) else listing
    newest = newest_actor_rows(items or [])
    missing = [key for key in manifest_actors if key not in newest]
    if missing:
        raise RunnerError(
            "these manifest actors are not registered: " + ", ".join(sorted(missing)),
            "register them with deploy/prod/register-actor.sh, or drop them from the manifest",
        )
    resolved = []
    for key in manifest_actors:
        row = newest[key]
        resolved.append(
            {
                "actor_key": key,
                "actor_id": row.get("id", ""),
                "revision": row.get("revision"),
                "kind": row.get("kind", ""),
                "endpoint_ref": row.get("endpoint_ref", ""),
                "slot": slot_for(key, slot_overrides),
            }
        )
    return resolved


def refuse_slot_collisions(actors: Sequence[dict[str, Any]]) -> None:
    """Refuse a manifest whose actors cannot each be reached.

    THE LIMITATION THIS ENFORCES. examples/harness-compare/workflow.yaml has
    five FIXED slots and each one's ``uses:`` is a *static registry id* —
    slot ``pi`` resolves to ``actor://company/pi-thor``, and nothing in the
    run input redirects it. So two actors that map to the same slot
    (``company/pi-thor`` and ``company/pi-orin``) cannot both be dispatched
    through this graph: naming slot ``pi`` twice would run pi-thor twice
    while the report claimed one of them was pi-orin.

    That is a fabricated measurement, so it is refused here rather than
    produced. The fix is a graph that can reach the second host — a slot per
    host, or a per-host copy of the graph — not a flag on this runner;
    editing the graph is out of this task's scope (task t11 does not touch
    workflow.yaml), so the refusal names the constraint plainly and
    ``--slot`` is the only override, for a deployment that really did
    register the other host under a different slot's id.
    """
    by_slot: dict[str, list[str]] = {}
    for actor in actors:
        by_slot.setdefault(actor["slot"], []).append(actor["actor_key"])
    collisions = {slot: keys for slot, keys in by_slot.items() if len(keys) > 1}
    if not collisions:
        return
    detail = "; ".join(
        f"slot {slot} <- {', '.join(sorted(keys))}" for slot, keys in collisions.items()
    )
    raise RunnerError(
        "refusing to measure: two manifest actors map to one workflow slot (" + detail + ")",
        "each harness-compare slot's `uses:` is a static registry id, so a slot reaches "
        "exactly one actor — give the second host its own slot in the graph (or its own "
        "graph), or map it with --slot <actor-key>=<slot> if it is registered under one",
    )


def served_actor_id(view: dict[str, Any], slot: str, fallback: str) -> str:
    """Who ACTUALLY served this run, from the node run's last attempt.

    ``node_runs[].attempts[].actor_id`` is the same field nodes-op.sh's
    ``grade`` uses to default ``--actor``. Grading the served actor rather
    than the intended one means a routing surprise shows up as a mismatch in
    the report instead of as a grade filed against the wrong identity.
    """
    for node_run in view.get("node_runs") or []:
        if node_run.get("node_id") != slot:
            continue
        for attempt in reversed(node_run.get("attempts") or []):
            if attempt.get("actor_id"):
                return str(attempt["actor_id"])
    return fallback


def require_grading_principal(actor_id: str) -> str:
    """Refuse to start at all without an explicit grading principal.

    Checked before anything else touches the network, because the whole
    point of c29/h28 is that there is no default: nodes-op.sh's ``grade``
    falls back to "the first registered kind=human actor", and a runner
    inheriting that fallback would mint confirmed grades in bulk.
    """
    if not actor_id:
        raise RunnerError(
            "no grading principal: MEASURE_RUNNER_ACTOR_ID is unset and --as was not given",
            "register an agent actor for the runner (plan task t12) and pass its id — "
            "a human id would mint CONFIRMED grades for work no human read",
        )
    return actor_id


def resolve_grading_actor(api: ApiClient, actor_id: str) -> dict[str, Any]:
    """Look the grading principal up and refuse a human one (c29 / h28)."""
    require_grading_principal(actor_id)
    listing = api.get("/v1alpha1/actors")
    items = listing.get("items", []) if isinstance(listing, dict) else listing
    for row in items or []:
        if row.get("id") == actor_id:
            if row.get("kind") == "human":
                raise RunnerError(
                    f"refusing to grade as {actor_id}: that actor is registered kind=human",
                    "a human grade lands confirmed on arrival (internal/api/grades.go); "
                    "the runner must grade as an agent so its grades land proposed",
                )
            return row
    raise RunnerError(
        f"grading principal {actor_id} is not a registered actor",
        "register it (kind=agent) and re-run; the control plane resolves the grader's "
        "kind from the registry",
    )


# ---------------------------------------------------------------------------
# The revision gate (c30 / h24)
# ---------------------------------------------------------------------------


def gate_revisions(
    actors: Sequence[dict[str, Any]],
    expect_revision: str | None,
    bridge_tokens: dict[str, str],
    fetch=fetch_deployment,
) -> dict[str, dict[str, Any]]:
    """Read every actor's bridge deployment block; refuse a stale one.

    Returns ``{actor_key: deployment}``. With ``expect_revision`` set, any
    actor whose reported revision is not that sha (prefix match, so a short
    sha works) makes this raise, and the message NAMES every offending actor
    and what it is actually running. Without it, nothing is refused and the
    blocks are simply returned for recording.
    """
    seen: dict[str, dict[str, Any]] = {}
    for actor in actors:
        endpoint = actor.get("endpoint_ref") or ""
        if not endpoint:
            seen[actor["actor_key"]] = {"revision": None, "install_mode": "no-endpoint"}
            continue
        token = bridge_tokens.get(actor["actor_key"]) or bridge_tokens.get(actor["slot"], "")
        seen[actor["actor_key"]] = fetch(endpoint, token)
    if not expect_revision:
        return seen
    stale = []
    for actor in actors:
        block = seen.get(actor["actor_key"], {})
        revision = str(block.get("revision") or "")
        if not revision or not revision.startswith(expect_revision):
            stale.append(
                f"{actor['actor_key']} is running "
                f"{revision or 'an unstamped/unknown revision'}"
                f" (install_mode={block.get('install_mode', 'unknown')},"
                f" dirty={block.get('revision_is_dirty', 'unknown')})"
            )
    if stale:
        raise RunnerError(
            f"refusing to measure: {len(stale)} actor(s) are not on {expect_revision}: "
            + "; ".join(stale),
            "redeploy those bridges (deploy/prod/deploy.sh stamps the copies it installs), "
            "or drop --expect-revision to measure whatever is deployed and record it",
        )
    return seen
