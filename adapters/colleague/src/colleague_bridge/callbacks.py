"""§13.4 callback delivery: stable event ids, a monotonic sequence, and
redelivery-with-the-same-identity on a refused or unreachable delivery.

Mirrors `tests/conformance/reference.go`'s `postEvent` (the kit's own worked
example of a conforming actor): every event carries a stable `event_id` and
the NEXT sequence number, and a non-2xx delivery is retried with the exact
same event — never a fresh id, never a skipped sequence — because that
identity is what makes the control plane's own deduplication meaningful
(PRD §13.4: "repeated callbacks are idempotent").
"""

from __future__ import annotations

import json
import logging
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any

logger = logging.getLogger("colleague_bridge.callbacks")

#: Statuses where retrying cannot help: the credential or the attempt is
#: gone, so redelivery would just repeat the same refusal forever
#: (tests/conformance/reference.go's own `postEvent` treats these
#: identically).
_GIVE_UP_STATUSES = frozenset({401, 404})


@dataclass
class CallbackConfig:
    timeout_seconds: float = 10.0
    max_retries: int = 5
    backoff_seconds: float = 0.25


class CallbackEmitter:
    """Posts one invocation's §13.4 event stream to its callback URL.

    Sequence numbers are assigned by this object (`next_sequence`), starting
    at 1 and incrementing once per DISTINCT event — a redelivery of the same
    event reuses its already-assigned sequence rather than consuming a new
    one, so "monotonically increasing" holds across the whole delivered
    stream, not just the accepted subset.
    """

    def __init__(
        self, callback_url: str, callback_token: str, task_id: str, cfg: CallbackConfig
    ) -> None:
        self._url = callback_url
        self._token = callback_token
        self._task_id = task_id
        self._cfg = cfg
        self._lock = threading.Lock()
        self._n = 0

    def next_event_id_and_sequence(self) -> tuple[str, int]:
        with self._lock:
            self._n += 1
            return f"evt_{self._task_id}_{self._n}", self._n

    def send(self, kind: str, payload: dict[str, Any] | None = None) -> bool:
        """Send one event, retrying on failure. Returns whether it was ever accepted."""
        event_id, sequence = self.next_event_id_and_sequence()
        return self.resend(event_id, sequence, kind, payload)

    def resend(
        self, event_id: str, sequence: int, kind: str, payload: dict[str, Any] | None
    ) -> bool:
        """Deliver a specific (event_id, sequence, kind, payload) with retries.

        Split from `send` so a caller that must redeliver a KNOWN event
        (e.g. the bridge's own future extensions) never risks minting a new
        id/sequence for what must remain the same event.
        """
        body = json.dumps(
            {"event_id": event_id, "sequence": sequence, "kind": kind, "payload": payload or {}}
        ).encode("utf-8")

        attempts = self._cfg.max_retries + 1
        for attempt in range(attempts):
            status, ok = self._post_once(body)
            if ok:
                return True
            if status in _GIVE_UP_STATUSES:
                logger.warning(
                    "callback %s (event %s) refused with %s; not retrying", kind, event_id, status
                )
                return False
            if attempt < attempts - 1:
                time.sleep(self._cfg.backoff_seconds * (attempt + 1))
        logger.warning("callback %s (event %s) exhausted %d attempt(s)", kind, event_id, attempts)
        return False

    def _post_once(self, body: bytes) -> tuple[int, bool]:
        req = urllib.request.Request(  # noqa: S310 - fixed callback URL from the invocation itself
            self._url,
            data=body,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {self._token}",
            },
        )
        try:
            with urllib.request.urlopen(
                req, timeout=self._cfg.timeout_seconds
            ) as resp:  # noqa: S310
                status = resp.status
                resp.read()
                return status, 200 <= status < 300
        except urllib.error.HTTPError as exc:
            exc.read()
            return exc.code, False
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            logger.debug("callback delivery to %s failed: %s", self._url, exc)
            return 0, False
