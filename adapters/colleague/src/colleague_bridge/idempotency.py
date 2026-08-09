"""Idempotency-Key replay store (PRD §20.3).

"Every dispatch receives a stable attempt idempotency key. Actors that
perform side effects must ... honor the key": a re-invocation carrying the
same `Idempotency-Key` must get back exactly the response the first
invocation produced, without re-running `colleague work` a second time.

The store is one JSON file per key under `Config.state_dir` — durable across
bridge restarts (a redelivery arriving after a restart still replays
correctly), and simple enough that "what did we answer for this key" is
directly `cat`-able during debugging. Filenames are a hash of the key rather
than the key itself so an oddly-shaped key (arbitrarily long, containing
path separators) can never become a path-traversal or "file too long" bug.
"""

from __future__ import annotations

import hashlib
import json
import os
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass(frozen=True)
class StoredResponse:
    status_code: int
    body: dict[str, Any]
    #: The invocation's own idempotency key, kept alongside the response so
    #: a corrupted/foreign file is detectable rather than silently replayed.
    request_fingerprint: str


class IdempotencyStore:
    """File-backed replay store, safe for one process's concurrent threads.

    The bridge's HTTP server itself is single-threaded (one request handled
    at a time — see server.py's module docstring), so the lock here guards
    against the ONE genuine concurrency source: a background poller thread
    never touches this store, but a future change should not have to
    rediscover that this needs a lock — it is cheap and always correct to
    hold one.
    """

    def __init__(self, state_dir: str | Path) -> None:
        self._dir = Path(state_dir) / "idempotency"
        self._dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def _path(self, key: str) -> Path:
        digest = hashlib.sha256(key.encode("utf-8")).hexdigest()
        return self._dir / f"{digest}.json"

    def get(self, key: str) -> StoredResponse | None:
        with self._lock:
            path = self._path(key)
            try:
                raw = path.read_text(encoding="utf-8")
            except OSError:
                return None
            try:
                data = json.loads(raw)
            except ValueError:
                return None
            if not isinstance(data, dict) or "status_code" not in data or "body" not in data:
                return None
            return StoredResponse(
                status_code=int(data["status_code"]),
                body=data["body"],
                request_fingerprint=str(data.get("request_fingerprint", "")),
            )

    def put(self, key: str, status_code: int, body: dict[str, Any], *, request_fingerprint: str = "") -> None:
        """Record the response for *key*. Overwrites any prior record for the
        same key (a caller that wants "first write wins" should `get` first).
        Written atomically (temp file + rename) so a crash mid-write never
        leaves a half-written, unparseable replay file behind.
        """
        with self._lock:
            path = self._path(key)
            payload = json.dumps(
                {"status_code": status_code, "body": body, "request_fingerprint": request_fingerprint}
            )
            tmp = path.with_suffix(".tmp")
            tmp.write_text(payload, encoding="utf-8")
            os.replace(tmp, path)
