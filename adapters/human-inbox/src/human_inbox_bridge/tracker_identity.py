"""The tracker's startup identity guard (issue #72), rebuilt for task t7.

Split out of tracker.py for the same reason tracker_cycle.py was: the guard
is one self-contained concern with its own long argument, and reading it
should not mean scrolling past the GitHub rate-limit arithmetic.
tracker.py re-exports `verify_bridge_serves_actor` so `main` and existing
callers keep their public path.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable

from human_inbox_bridge import identity

from .tracker import BridgeIdentityError, TrackerConfig, logger

# --- startup identity check (issue #72) --------------------------------
#
# Why this is a refusal and not a warning: the bridge's idempotency store is
# per-bridge and file-based — one JSON file per key under Config.state_dir —
# so it can only deduplicate submissions that pass through the SAME bridge
# process's state directory. Two bridges serving one actor do not see each
# other's replays at all. "One logical human inbox" is therefore enforced by
# deployment convention, and this check is the only mechanism that can
# notice the convention has been broken. A tracker that keeps running
# against the wrong bridge submits observations no store can dedupe.
#
# What changed in task t7 (issues #121/#136) is the EVIDENCE, not the rule.
# The original check resolved the actor's registered `endpoint_ref` and
# compared host and port against this tracker's own bridge URL, resolving
# two spellings of one machine along the way. Migration 0036 removes that
# column — after the dial-in cutover the control plane stores no participant
# address — so the comparison has no left-hand side any more. Three facts
# replace it, and none of them is an address:
#
#   1. The store this tracker reads tasks from has an id (identity.py), and
#      the bridge reports the id of the store IT owns on GET /identity. A
#      match proves the process being submitted to is the process whose
#      tasks are being read. This is the strongest of the three and the one
#      that directly forbids the #72 split.
#   2. That bridge reports the actor it serves, which must not be a
#      DIFFERENT registered actor. Note the trap `_check_actor_agreement`
#      exists for: HUMAN_INBOX_BRIDGE_ACTOR_ID deliberately holds two
#      different values in the bridge's and the tracker's env files (row id
#      vs actor key), so this comparison cannot be a string equality.
#   3. The control plane reports dial-in presence for that actor_key, which
#      is how it decides where the actor's work goes now that it holds no
#      address. Its job here is the one the registered endpoint used to do:
#      say whether the bridge beside us is the one the engine will dispatch
#      to.


@dataclass(frozen=True)
class BridgeIdentity:
    """What the bridge at `bridge_url` says about itself (GET /identity)."""

    actor_id: str
    store_id: str
    #: The actor key its dial-in client presents, or "" when it does not
    #: dial in at all (an unconverted bridge in mixed mode, or a broken
    #: HUMAN_INBOX_BRIDGE_ACTOR_KEY).
    dial_in_actor_key: str


@dataclass(frozen=True)
class ConfirmedIdentity:
    """The startup check's verdict, kept for the log line and for tests."""

    actor_key: str
    store_id: str
    dials_in: bool
    #: `connected` / `disconnected` / `never_dialled`, or None when no
    #: control plane was configured and the presence half did not run.
    presence: str | None


def fetch_bridge_identity(
    bridge_url: str, bridge_token: str, *, timeout_seconds: float
) -> dict[str, Any]:
    """GET /identity from the bridge this tracker submits through.

    Authenticated with the same bearer token every other call to this bridge
    carries: the store id is not a capability on its own, but it is the one
    value a wrong bridge would need in order to look right.
    """
    url = f"{bridge_url.rstrip('/')}/identity"
    headers = {
        "Accept": "application/json",
        "User-Agent": "culture-nodes-human-inbox-tracker",
    }
    if bridge_token:
        headers["Authorization"] = f"Bearer {bridge_token}"
    request = urllib.request.Request(  # noqa: S310 - operator-configured sibling bridge
        url,
        headers=headers,
    )
    with urllib.request.urlopen(  # nosec B310 - operator-configured sibling bridge URL
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    if not isinstance(data, dict):
        raise ValueError("bridge identity response was not a JSON object")
    return data


def fetch_dial_in_presence(
    control_plane_url: str, *, timeout_seconds: float
) -> list[dict[str, Any]]:
    """GET every registered actor's dial-in presence from the control plane.

    Unauthenticated on purpose, like the actors list this replaced: reads on
    the phase-1 API carry no bearer token (spec decision c45) and this
    tracker holds no control-plane credential of any kind. The response
    carries no address for any actor — that is the point of the surface.
    """
    url = f"{control_plane_url.rstrip('/')}/v1alpha1/dial-in-presence"
    request = urllib.request.Request(  # noqa: S310 - operator-configured control plane
        url,
        headers={
            "Accept": "application/json",
            "User-Agent": "culture-nodes-human-inbox-tracker",
        },
    )
    with urllib.request.urlopen(  # nosec B310 - operator-configured control-plane URL
        request, timeout=timeout_seconds
    ) as response:  # noqa: S310
        data = json.load(response)
    items = data.get("items") if isinstance(data, dict) else None
    if not isinstance(items, list):
        raise ValueError("control-plane dial-in presence response had no 'items' array")
    return [item for item in items if isinstance(item, dict)]


def _local_store_id(state_dir: str) -> str | None:
    """This tracker's own view of the store it reads tasks from.

    None when the directory holds no identity file: either no bridge has
    ever run against it, or the tracker is pointed somewhere else entirely.
    Both are refusals — a tracker watching a directory no bridge fills is
    the silent failure #72 was filed for.
    """
    resolved = identity.read_store_identity(state_dir)
    return None if resolved is None else resolved.store_id


def _names(row: dict[str, Any]) -> set[str]:
    """Every name one registered identity answers to in a presence row.

    Two, because the deployment uses both: `actor_key` is the stable
    identity (`company/human-ops`), and `actor_id` is the row id of its
    current registration revision — which is what a bridge stamps as
    `origin.actor_id` on a ledger claim, because
    `ledger_records.origin_actor_id` is a foreign key into `actors(id)`.
    """
    return {str(row.get(field)) for field in ("actor_key", "actor_id") if row.get(field)}


def _presence_row(items: list[dict[str, Any]], actor: str) -> dict[str, Any] | None:
    """The presence row for *actor*, or None if the actor has none.

    Matches either name (see `_names`) so a tracker configured with a row id
    resolves as readily as one configured with an actor key. Presence has
    exactly one row per registered identity — unlike the actors list, which
    renders every revision — so there is no newest-revision rule here.
    """
    for item in items:
        if actor in _names(item):
            return item
    return None


def _check_actor_agreement(
    cfg: TrackerConfig,
    bridge: BridgeIdentity,
    row: dict[str, Any],
    items: list[dict[str, Any]],
) -> None:
    """Refuse a bridge that demonstrably serves a DIFFERENT registered actor.

    `HUMAN_INBOX_BRIDGE_ACTOR_ID` is deliberately written with two different
    values by deploy/prod/deploy.sh: the bridge's copy is the actors ROW ID
    (its ledger claims carry it as a foreign key into `actors(id)`), the
    tracker's copy is the actor KEY (it resolves against the registry). A
    plain string comparison of the two would refuse every correct production
    deployment, so the presence row — which carries both names for one
    identity — is what makes them comparable.

    Three outcomes, and the middle one is the reason this is not simply
    dropped: a value that names ANOTHER registered actor is a refusal, a
    value naming this one under either name is agreement, and a value naming
    nothing the control plane knows is a warning. That last case is most
    likely a stale row id left by a re-registration the bridge has not been
    redeployed for — a defect worth a log line, but not one that makes
    submitting through this bridge unsafe, since the store proof already
    established the tasks came out of it.
    """
    if bridge.actor_id in _names(row):
        return

    for other in items:
        if other is row:
            continue
        if bridge.actor_id and bridge.actor_id in _names(other):
            raise BridgeIdentityError(
                f"this tracker observes actor {cfg.actor_id!r}, but the bridge it submits to "
                f"({cfg.bridge_url}) is configured to serve "
                f"{str(other.get('actor_key') or bridge.actor_id)!r} — a different registered "
                "actor. Refusing to start: an observation submitted against the wrong actor's "
                "inbox is a claim on someone else's run, and the per-bridge, file-based "
                "idempotency store cannot undo it. Point HUMAN_INBOX_TRACKER_BRIDGE_URL at this "
                "actor's own bridge, or fix the bridge's HUMAN_INBOX_BRIDGE_ACTOR_ID"
            )

    logger.warning(
        "the bridge at %s reports actor_id %r, which the control plane at %s does not know as "
        "either the key or the current revision id of %r. It owns the store this tracker reads, "
        "so submissions still land in the right inbox — but its ledger claims may be stamped "
        "with a superseded registration row. Redeploy the bridge to pick up the current one",
        cfg.bridge_url,
        bridge.actor_id,
        cfg.control_plane_url,
        cfg.actor_id,
    )


def _short(store_id: str) -> str:
    """A store id shortened for an error message.

    Enough to tell two stores apart in a log; not enough to present to a
    bridge as proof of owning one.
    """
    return store_id[:8] if store_id else "(none)"


def verify_bridge_serves_actor(
    cfg: TrackerConfig,
    *,
    bridge_identity_fetch: Callable[..., dict[str, Any]] | None = None,
    presence_fetch: Callable[..., list[dict[str, Any]]] | None = None,
    local_store_id: Callable[[str], str | None] | None = None,
) -> ConfirmedIdentity | None:
    """Refuse to start unless this tracker's bridge is the actor's bridge.

    Returns the confirmed identity, or raises `BridgeIdentityError` on every
    unhappy path — including an unreachable bridge or control plane, since
    an unverified identity is not a verified one and the systemd unit
    restarts (an outage costs retries, not an unguarded window).

    # What this proves, and what it does not

    The co-location half is a PROOF. The tracker reads the store id out of
    the state directory it takes tasks from, and only a process that can
    read that directory can report the same value; so a bridge that answers
    with it is the bridge whose inbox this tracker is emptying. That is
    strictly stronger than the host/port comparison it replaces, which could
    only establish that two URLs named one listening socket.

    The dispatch half is WEAKER than what it replaces, and it is worth being
    exact about how. A registered endpoint said *where the engine will send
    this actor's work*; dial-in presence says only that *some* process is
    dialled in as this actor, because presence is one row per actor_key and
    a second process polling the same mailbox refreshes the same row. So:

    * If the bridge beside us does not dial in and the control plane says
      this actor IS connected, a second bridge with its own idempotency
      store is demonstrably receiving the work. That is refused, and it is
      a case the address comparison could not detect at all.
    * If the bridge beside us dials in as this actor and the control plane
      agrees the actor is connected, the two are CONSISTENT with the bridge
      beside us being the dispatch target. It is not proof. Two correctly
      co-located trackers, each beside a bridge dialling in under one actor
      key, would both pass — a duplicate no surface here can see. Closing
      that needs a per-connection instance identity in presence, which is
      control-plane work, not tracker work.

    Both seams default to the module-level implementations and are resolved
    at call time rather than bound as defaults, so a test can monkeypatch
    any of them without reaching past `main`.
    """
    fetch_identity = (
        fetch_bridge_identity if bridge_identity_fetch is None else bridge_identity_fetch
    )
    fetch_presence = fetch_dial_in_presence if presence_fetch is None else presence_fetch
    read_store = _local_store_id if local_store_id is None else local_store_id

    our_store = read_store(cfg.state_dir)
    if not our_store:
        raise BridgeIdentityError(
            f"no bridge store identity under {cfg.state_dir} — this tracker reads its pending "
            "tasks from that directory, and no human-inbox bridge has ever opened it. Refusing "
            "to start: a tracker watching a directory no bridge fills reports pending=0 forever "
            "and nothing notices. Set HUMAN_INBOX_TRACKER_STATE_DIR to the bridge's state "
            "directory, or start the bridge first — it mints the file on startup"
        )

    try:
        raw = fetch_identity(
            cfg.bridge_url, cfg.bridge_token, timeout_seconds=cfg.http_timeout_seconds
        )
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        raise BridgeIdentityError(
            f"cannot ask {cfg.bridge_url}/identity which actor it serves: {exc}. Refusing to "
            "start: an unverified bridge identity is not a verified one, and the per-bridge "
            "idempotency store cannot deduplicate a split deployment. If the bridge is running "
            "and still answers 404, it predates the identity surface and must be upgraded"
        ) from exc

    bridge = BridgeIdentity(
        actor_id=str(raw.get("actor_id") or ""),
        store_id=str(raw.get("store_id") or ""),
        dial_in_actor_key=str((raw.get("dial_in") or {}).get("actor_key") or ""),
    )

    if bridge.store_id != our_store:
        raise BridgeIdentityError(
            f"this tracker reads its pending tasks from {cfg.state_dir} (store "
            f"{_short(our_store)}) but submits to {cfg.bridge_url}, which owns a different "
            f"durable store ({_short(bridge.store_id)}) — a different bridge process. Refusing "
            "to start: the bridge idempotency store is per-bridge and file-based, so two bridges "
            "serving one actor cannot deduplicate each other's submissions. Point "
            "HUMAN_INBOX_TRACKER_BRIDGE_URL at the bridge that owns this state directory, or "
            "HUMAN_INBOX_TRACKER_STATE_DIR at the state directory of that bridge"
        )

    if bridge.dial_in_actor_key and bridge.dial_in_actor_key != cfg.actor_id:
        raise BridgeIdentityError(
            f"the bridge at {cfg.bridge_url} dials in to the control plane as "
            f"{bridge.dial_in_actor_key!r}, but this tracker observes {cfg.actor_id!r}, so the "
            f"engine delivers {cfg.actor_id!r}'s work to a different process. Refusing to start: "
            "that process has its own file-based idempotency store and cannot deduplicate "
            "anything this tracker submits. HUMAN_INBOX_BRIDGE_ACTOR_KEY (the bridge's dial-in "
            "identity) and the tracker's HUMAN_INBOX_BRIDGE_ACTOR_ID must name the same actor "
            "key — both are actor KEYS, unlike the bridge's own copy of "
            "HUMAN_INBOX_BRIDGE_ACTOR_ID"
        )

    if not cfg.control_plane_url:
        if bridge.actor_id != cfg.actor_id:
            # Not a refusal, and deliberately so: deploy/prod/deploy.sh writes
            # HUMAN_INBOX_BRIDGE_ACTOR_ID with DIFFERENT values in the bridge's
            # and the tracker's env files. The bridge stamps its copy as
            # origin.actor_id on ledger claims, where it is a foreign key into
            # actors(id), so it holds a ROW ID; the tracker resolves its copy
            # against the registry, so it holds an actor KEY. The two are the
            # same identity under two names, and only the control plane can
            # say so — which is exactly what is missing here.
            logger.warning(
                "the bridge at %s reports actor_id %r while this tracker observes %r. These are "
                "usually the same actor under two names (the bridge stamps the actors ROW ID on "
                "ledger claims, the tracker resolves an actor KEY), but with no control plane "
                "configured nothing can confirm that",
                cfg.bridge_url,
                bridge.actor_id,
                cfg.actor_id,
            )
        logger.warning(
            "no control plane configured (HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL): %s owns the "
            "store this tracker reads (%s) and serves actor %r, but nothing has confirmed that "
            "the engine dispatches that actor's work HERE. Starting with the local half of the "
            "guard only",
            cfg.bridge_url,
            _short(our_store),
            cfg.actor_id,
        )
        return ConfirmedIdentity(
            actor_key=cfg.actor_id,
            store_id=our_store,
            dials_in=bool(bridge.dial_in_actor_key),
            presence=None,
        )

    try:
        items = fetch_presence(cfg.control_plane_url, timeout_seconds=cfg.http_timeout_seconds)
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        raise BridgeIdentityError(
            f"cannot read dial-in presence for actor {cfg.actor_id!r} from the control plane at "
            f"{cfg.control_plane_url}: {exc}. Refusing to start: an unverified bridge identity "
            "is not a verified one, and the per-bridge idempotency store cannot deduplicate a "
            "split deployment"
        ) from exc

    row = _presence_row(items, cfg.actor_id)
    if row is None:
        raise BridgeIdentityError(
            f"the control plane at {cfg.control_plane_url} has no actor registered under "
            f"{cfg.actor_id!r}, so this tracker cannot confirm that its bridge "
            f"({cfg.bridge_url}) serves the actor it observes. Set HUMAN_INBOX_BRIDGE_ACTOR_ID "
            "to the registered actor_key, or register the actor first"
        )

    _check_actor_agreement(cfg, bridge, row, items)

    presence = str(row.get("presence") or "unknown")
    since = row.get("seconds_since_last_seen")
    seen = f", last seen {since:.0f}s ago" if isinstance(since, (int, float)) else ""

    if presence == "connected" and not bridge.dial_in_actor_key:
        raise BridgeIdentityError(
            f"the control plane at {cfg.control_plane_url} shows actor {cfg.actor_id!r} dialled "
            f"in{seen}, but the bridge this tracker submits to ({cfg.bridge_url}) does not dial "
            "in at all — so a SECOND bridge is receiving this actor's work. Refusing to start: "
            "the idempotency store is per-bridge and file-based, so the two cannot deduplicate "
            "each other's submissions. Either configure this bridge's dial-in client "
            "(HUMAN_INBOX_BRIDGE_CONTROL_PLANE_URL / _ACTOR_KEY / _DIAL_TOKEN), or run this "
            "tracker beside the bridge that is dialled in"
        )

    if presence != "connected" and bridge.dial_in_actor_key:
        raise BridgeIdentityError(
            f"the bridge at {cfg.bridge_url} is configured to dial in as {cfg.actor_id!r}, but "
            f"the control plane at {cfg.control_plane_url} reports that actor {presence}{seen}. "
            "Refusing to start: the engine delivers this actor's work to whichever bridge is "
            "dialled in, and while none is, nothing establishes that this is that bridge. This "
            "is usually transient and the unit restarts; if it persists, check this bridge's "
            "dial-in credential with `nodes actors dial-in`"
        )

    if presence != "connected":
        # Mixed mode (docs/decisions/transport-inversion.md): this bridge has
        # not been converted, and nothing else is dialled in as the actor
        # either. The engine still reaches it over the outbound transport,
        # which this tracker cannot see — so co-location is the whole
        # guarantee, and the log says so rather than implying otherwise.
        logger.warning(
            "actor %s is %s: this bridge does not dial in and neither does anything else, so "
            "the engine is still reaching it over the outbound transport. %s owns the store "
            "this tracker reads (%s), which is the only thing confirmed here",
            cfg.actor_id,
            presence,
            cfg.bridge_url,
            _short(our_store),
        )

    logger.info(
        "bridge identity confirmed: %s owns store %s, serves actor %s, dial-in=%s, presence=%s",
        cfg.bridge_url,
        _short(our_store),
        cfg.actor_id,
        bridge.dial_in_actor_key or "(none)",
        presence,
    )
    return ConfirmedIdentity(
        actor_key=cfg.actor_id,
        store_id=our_store,
        dials_in=bool(bridge.dial_in_actor_key),
        presence=presence,
    )
