"""Who this bridge is, and the proof a co-located tracker can check.

Issue #72's startup guard used to be answered with an ADDRESS. The tracker
asked the control plane for the actor's registered ``endpoint_ref`` and
refused to run unless that host and port were the bridge it submits through.
Migration 0036 (issue #121) drops that column: after the dial-in cutover the
control plane holds no participant address at all, so there is nothing left
to compare and the unit would simply stop starting.

What replaces it is deliberately not another address.

The question the guard actually answers is *"is the bridge I submit to the
bridge whose durable store I read tasks out of?"* — the tracker reads pending
task files straight off ``state_dir`` and posts results over HTTP, and those
two halves belonging to different processes is precisely the split #72
refuses. That question can be settled locally: the bridge mints one random
``store_id``, keeps it inside the state directory beside ``tasks/`` and
``idempotency/``, and reports it on ``GET /identity``. Only a process that
can read that directory can produce the value, so a matching answer proves
the responder owns the store — wherever it happens to listen, and whatever
the control plane does or does not remember about it.

Two properties are load-bearing and easy to break by accident:

* The id identifies the STORE, not the process. It is minted once and
  re-read on every later start, so restarting a bridge does not invalidate a
  correctly co-located tracker.
* It is created with ``O_EXCL`` and mode ``0600``. Exclusive creation means
  two processes racing over one state directory cannot end up believing in
  two different ids; ``0600`` means the proof is worth something, since a
  value any local user could read is a value any local bridge could claim.

The dial-in half (``dial_in_actor_key``) reads the SAME environment
``dialin.configured`` reads, rather than a copy of it, so the identity
surface cannot report a key the dial-in client is not actually presenting.
"""

from __future__ import annotations

import json
import os
import secrets
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from human_inbox_bridge import dialin

#: The file, directly under ``state_dir``. Never inside ``tasks/`` (the task
#: store globs that directory and would log a corrupt-task warning on every
#: list) and never inside ``idempotency/`` (same reason, different reader).
STORE_IDENTITY_FILENAME = "store-identity.json"

#: The env-var prefix this bridge's dial-in client is started with — see
#: ``__main__._cmd_serve``'s ``dialin.start("HUMAN_INBOX_BRIDGE", ...)``.
#: Note that the dial-in identity is ``HUMAN_INBOX_BRIDGE_ACTOR_KEY`` while
#: the ledger identity is ``HUMAN_INBOX_BRIDGE_ACTOR_ID``: two variables one
#: letter apart, which is exactly why the identity surface reports both.
DIAL_IN_ENV_PREFIX = "HUMAN_INBOX_BRIDGE"


@dataclass(frozen=True)
class StoreIdentity:
    """The identity of one durable state directory."""

    store_id: str
    created_at: str


def store_identity_path(state_dir: str | Path) -> Path:
    return Path(state_dir) / STORE_IDENTITY_FILENAME


def read_store_identity(state_dir: str | Path) -> StoreIdentity | None:
    """The store's identity, or None when there is none to read.

    Absent and corrupt both read as None on purpose: a caller that cannot
    obtain the value must fail closed, and giving it two shapes of failure
    to handle only invites one of them to be forgotten.
    """
    try:
        raw = store_identity_path(state_dir).read_text(encoding="utf-8")
    except OSError:
        return None
    try:
        data = json.loads(raw)
    except ValueError:
        return None
    if not isinstance(data, dict):
        return None
    store_id = data.get("store_id")
    if not isinstance(store_id, str) or not store_id:
        return None
    created_at = data.get("created_at")
    return StoreIdentity(
        store_id=store_id,
        created_at=created_at if isinstance(created_at, str) else "",
    )


def ensure_store_identity(state_dir: str | Path) -> StoreIdentity:
    """Return this state directory's identity, minting one the first time.

    Exclusive creation rather than write-then-replace: if two processes ever
    start against one state directory, both must end up with the SAME id.
    Whichever loses the race re-reads the winner's file instead of
    overwriting it, so a tracker is never told two different truths about
    one store.
    """
    directory = Path(state_dir)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    existing = read_store_identity(directory)
    if existing is not None:
        return existing

    minted = StoreIdentity(
        store_id=secrets.token_hex(16),
        created_at=datetime.now(timezone.utc).isoformat(),
    )
    payload = json.dumps({"store_id": minted.store_id, "created_at": minted.created_at})
    try:
        fd = os.open(
            store_identity_path(directory),
            os.O_CREAT | os.O_EXCL | os.O_WRONLY,
            0o600,
        )
    except FileExistsError:
        # Another process minted it between the read above and here, or the
        # file is present but unreadable/corrupt. Re-read; if it is still
        # unusable, say so rather than silently inventing a second identity.
        existing = read_store_identity(directory)
        if existing is not None:
            return existing
        raise
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        handle.write(payload)
    return minted


def dial_in_actor_key() -> str:
    """The actor key this bridge's dial-in client presents, or "".

    Empty means the client is not running: either nothing is configured, or
    the configuration is partial — and ``dialin.configured`` refuses a
    partial one, so no dial happens either way. Reporting a key in that case
    would tell a tracker this bridge receives work it will never be sent.
    """
    try:
        configured = dialin.configured(DIAL_IN_ENV_PREFIX)
    except ValueError:
        return ""
    if configured is None:
        return ""
    return configured[1]
