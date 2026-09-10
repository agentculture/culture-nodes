#!/usr/bin/env python3
"""The control-plane half of the land node's PER-CHECKOUT lease (loop-closure
task t6; spec c3/c15, honesty h12; issues #315 and #309 step 2).

land.py reaches this module at its `checkout_lease` step, before it runs
`checkout -B <branch> <tip>` and `reset --hard <tip>` in the PRODUCING
actor's checkout. That reset discards anything uncommitted in a working tree
another session may be sitting in, so what unlocks it has to be a fact:
"the engine has no live attempt on that actor right now".

# The three reads, and why it takes three

    GET /v1alpha1/actors/{id}   the actor KEY behind the row id the input
                                names (skipped when the caller declares the
                                key itself -- LAND_PRODUCING_ACTOR_KEY)
    GET /v1alpha1/actors        every registration revision of that key
    GET /v1alpha1/node-runs     the cross-run listing, walked to its end

Registration is APPEND-ONLY: re-registering an actor mints a NEW actors row
with a new id and revision+1, and the node runs dispatched before it keep
pointing at the OLD row id (internal/store/postgres's ListActors doc comment
-- the listing renders every revision for exactly this reason). Matching a
node run's `actor_id` against the one row id in the input therefore misses
every attempt that predates the last re-registration: the identity that owns
a checkout is the KEY, not one row of it.

The node-run listing is PAGED and newest-first by updated_at, and it has no
server-side `state` or `actor_id` filter to push the match down to SQL (only
an updated_at window -- internal/api/queries.go's listNodeRunsAcrossRuns).
A live attempt is not necessarily a recently-touched one either: a
`waiting_external` node run can sit untouched for hours while its session
holds the checkout. So the walk is the whole listing, page by page through
`next_cursor`, and the states are matched here.

# Unmeasured is not zero

Every read that fails, every listing that does not end within
MAX_NODE_RUN_PAGES, is `unmeasured` -- an AttemptProbe carrying NO count.
Returning 0 there would hand a `reset --hard` the answer it wanted from a
question that was never asked, which is the shape of defect #146 (an
instrument that reports success without having measured anything). land.py
WAITS on an unmeasured probe; the graph parks and re-enters, and a control
plane that comes back makes the next landing measure for real.

`not_configured` (NODES_API_URL unset) is a different fact and stays
distinct: it is the operator's deliberate choice to run without the probe,
and the checkout's own lock directory is then the only lease.

# What this never does

No writes -- an agent actor's bearer cannot write a lease row anyway
(internal/api/actorbearer.go). No git, no subprocess, no GitHub. Two HTTP
verbs' worth of surface: GET, and json.loads.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass

#: Node-run states in which an attempt may be executing in the actor's checkout.
ACTIVE_NODE_RUN_STATES = frozenset({"leased", "running", "waiting_external"})

#: The listing's server-side maximum page size (api/helpers.go's parseLimit
#: caps `limit` at 500) and the bound on how many pages one probe walks.
#: Twenty pages is 10,000 node runs; a namespace with more than that in one
#: listing is not something a lander should silently under-count, so hitting
#: the bound is `unmeasured` and the landing waits.
NODE_RUN_PAGE_LIMIT = 500
MAX_NODE_RUN_PAGES = 20

HTTP_TIMEOUT_SECONDS = 20.0
API_HEADERS = {"Accept": "application/json", "User-Agent": "culture-nodes-land/1"}

#: What the probe knows, as three distinguishable answers.
MEASURED = "measured"
NOT_CONFIGURED = "not_configured"
UNMEASURED = "unmeasured"


class Unmeasured(Exception):
    """A read the probe needed did not happen; the message names which."""


@dataclass(frozen=True)
class AttemptProbe:
    """The probe's answer. `count` is a number ONLY when `status` is
    `measured` -- the other two statuses carry None, and a caller that
    treats None as zero has invented a measurement."""

    count: int | None
    status: str
    detail: str = ""

    @property
    def busy(self) -> bool:
        """A measured, non-zero count: something is live on this actor."""
        return bool(self.count)

    @property
    def unmeasured(self) -> bool:
        return self.status == UNMEASURED


def api_json(url: str):
    """One GET against the control plane, parsed. Any failure -- transport,
    timeout, status, or a body that is not JSON -- is Unmeasured."""
    request = urllib.request.Request(url, headers=API_HEADERS)
    try:
        with urllib.request.urlopen(  # nosec B310 - configured http(s) URL
            request, timeout=HTTP_TIMEOUT_SECONDS
        ) as response:
            return json.loads(response.read().decode("utf-8"))
    except (urllib.error.URLError, TimeoutError, ValueError, OSError) as exc:
        raise Unmeasured(f"could not read {urllib.parse.urlsplit(url).path}: {exc}") from exc


def items_of(body, what: str) -> list:
    if not isinstance(body, dict) or not isinstance(body.get("items"), list):
        raise Unmeasured(f"{what} answered without an items list")
    return body["items"]


def actor_rows(base: str, actor_id: str, actor_key: str | None) -> tuple[str, set[str]]:
    """The actor's key, and EVERY actors-table row id that shares it -- the
    set a node run's `actor_id` is matched against. The row named in the
    input is always in it, even if the listing no longer carries it."""
    if not actor_key:
        detail = api_json(f"{base}/v1alpha1/actors/{urllib.parse.quote(actor_id, safe='')}")
        actor_key = detail.get("actor_key") if isinstance(detail, dict) else None
        if not actor_key:
            raise Unmeasured(f"/v1alpha1/actors/{actor_id} named no actor_key")
    ids = {actor_id}
    for item in items_of(api_json(f"{base}/v1alpha1/actors"), "/v1alpha1/actors"):
        if isinstance(item, dict) and item.get("actor_key") == actor_key and item.get("id"):
            ids.add(str(item["id"]))
    return actor_key, ids


def count_active(base: str, ids: set[str], exclude_run_id: str) -> int:
    """Live node runs on any of `ids`, other than the land run's own, over
    the WHOLE listing. A listing that does not end within the page bound is
    Unmeasured -- a partial count is not a count."""
    count = 0
    cursor = ""
    for _page in range(MAX_NODE_RUN_PAGES):
        query = {"limit": str(NODE_RUN_PAGE_LIMIT)}
        if cursor:
            query["cursor"] = cursor
        url = f"{base}/v1alpha1/node-runs?{urllib.parse.urlencode(query)}"
        body = api_json(url)
        for item in items_of(body, "/v1alpha1/node-runs"):
            if not isinstance(item, dict) or item.get("actor_id") not in ids:
                continue
            if item.get("run_id") == exclude_run_id:
                continue
            if item.get("state") in ACTIVE_NODE_RUN_STATES:
                count += 1
        cursor = str(body.get("next_cursor") or "")
        if not cursor:
            return count
    raise Unmeasured(
        f"the node-run listing did not end within {MAX_NODE_RUN_PAGES} pages "
        f"of {NODE_RUN_PAGE_LIMIT}; the count would be partial"
    )


def active_attempts(
    api_url: str | None,
    actor_id: str,
    *,
    exclude_run_id: str,
    actor_key: str | None = None,
) -> AttemptProbe:
    """How many node runs the control plane has LIVE on this actor's
    IDENTITY, other than the land run's own. See the module docstring for
    why the identity is the key and why a partial read carries no count."""
    if not api_url:
        return AttemptProbe(None, NOT_CONFIGURED, "NODES_API_URL is unset: not checked")
    base = api_url.rstrip("/")
    try:
        key, ids = actor_rows(base, actor_id, actor_key)
        count = count_active(base, ids, exclude_run_id)
    except Unmeasured as exc:
        return AttemptProbe(None, UNMEASURED, str(exc))
    return AttemptProbe(count, MEASURED, f"{key}: {len(ids)} registration revision(s)")
