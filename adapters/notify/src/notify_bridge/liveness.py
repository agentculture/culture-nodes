"""Lane liveness (issue #308 detector, plan loop-closure-claude-codex task
t9) — the shared, backend-agnostic half of the `liveness` host fact.

**This file is byte-identical in every bridge that ships it**, exactly as
`preflight.py` and `deployment.py` are, and the same Go lint test
(`tests/lint/preflightsurface_test.go`) fails the build if it stops being.
It is a separate module from `preflight.py` for the same reason
`deployment.py` is: that file sits at the repo's 1000-line hard limit, and
the shared-module rule — one contract, never one inline copy per bridge —
applies here unchanged. It has no imports outside the stdlib and no import
from its own package.

## What this is for

Both codex lanes answered `/healthz` 200 with a spent refresh token, and the
harness-hardening wave 0 found out by dispatching into them: every first
attempt failed with

    Your access token could not be refreshed because your refresh token was
    already used. Please log out and sign in again.

(later observed as "... because your refresh token was revoked ..."). A
healthy bridge process in front of a dead session is the failure this fact
exists to name BEFORE a dispatch pays for discovering it. The router half
(spec decisions c23/c26, the control plane's task) reads the fact; this
module only defines its shape and the two ways a bridge derives it.

## The fact

    {"session_ok": true|false|null, "reason": <REASONS>,
     "checked_at": <ISO-8601 UTC>, "mode": "LOCK"|"CHECK"}

`session_ok` is three-valued on purpose: `null` is "nobody measured this",
which a reader must be able to tell apart from `false` — a stale or
unmeasured lane is never refused a lease (c26), only a measured-dead one.
`reason` says why, from a closed vocabulary, so the same word means the same
thing on every bridge: `refresh_token_spent` is codex's observed failure,
`credential_expired` is a past `expiresAt` in claude-code's credential file,
`unmeasured`/`probe_timeout`/`probe_failed` are the honest non-answers.

## The two modes

Per lane, configured by the operator (spec decision v1):

* **LOCK** — nothing is probed. The first run whose output carries the
  spent-credential text flips the lane to `session_ok=false` and it STAYS
  there until cleared (`LivenessState.clear`, which the bridge's start-up
  re-derivation performs when its probe says the lane is live again). Costs
  nothing; detects only after one dispatch has already failed.
* **CHECK** — a bridge-specific online probe (codex: a dry read-only exec
  bounded at 20 s; claude-code: the credential file's `expiresAt`) runs when
  the surface is read and the last answer is older than the lane's TTL, so
  the fact flips before any dispatch, at the cost of one micro-session per
  probe window. A run's output still locks the lane in this mode — the OR
  of both sides is what closes a lane (decision q7).

Nothing backend-specific lives here: WHAT text a backend's engine prints,
WHICH file it keeps its credential in, and HOW it is probed are the bridge's
own `capabilities.py` / CLI module's concern. What is shared is the shape,
the vocabulary, the text classifier for the one failure two bridges have
already paid for, and the latch.
"""

from __future__ import annotations

import json
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

#: The agreed keys of the `liveness` host fact, in the order they are
#: written. Exactly these: a fifth key is a second dialect.
LIVENESS_KEYS = ("session_ok", "reason", "checked_at", "mode")

MODE_LOCK = "LOCK"
MODE_CHECK = "CHECK"
MODES = frozenset((MODE_LOCK, MODE_CHECK))

REASON_OK = "ok"
REASON_UNMEASURED = "unmeasured"
# A reason NAME, not a credential: bandit pattern-matches the word "token".
REASON_REFRESH_TOKEN_SPENT = "refresh_token_spent"  # nosec B105
REASON_CREDENTIAL_EXPIRED = "credential_expired"
REASON_PROBE_TIMEOUT = "probe_timeout"
REASON_PROBE_FAILED = "probe_failed"

REASONS = frozenset(
    (
        REASON_OK,
        REASON_UNMEASURED,
        REASON_REFRESH_TOKEN_SPENT,
        REASON_CREDENTIAL_EXPIRED,
        REASON_PROBE_TIMEOUT,
        REASON_PROBE_FAILED,
    )
)

#: Lower-cased substrings of the engine text that means "this session's
#: credential is spent and only an interactive re-login restores it". Two
#: observed wordings (harness-hardening wave 0, deviation d1: "was already
#: used"; later: "was revoked") plus the clause common to both. Matched as
#: substrings of the WHOLE output, because the live failure printed the
#: sentence to stderr with no terminal turn event around it.
SPENT_CREDENTIAL_SIGNALS = (
    "refresh token was revoked",
    "refresh token was already used",
    "access token could not be refreshed",
)


class LivenessError(ValueError):
    """A liveness fact this bridge would be wrong to advertise: an unagreed
    mode or reason, a wrong-typed field, or a shape the router would misread."""


def parse_mode(raw: Any) -> str:
    """Normalise an operator-written mode (`lock`, ` CHECK `) to the agreed
    spelling, refusing anything else. Configuration is where a typo should
    fail, not the first surface read."""
    candidate = str(raw or "").strip().upper()
    if candidate not in MODES:
        raise LivenessError(
            f"liveness mode {raw!r} is not one of {sorted(MODES)}: LOCK latches on the first "
            "spent-credential failure, CHECK probes before dispatch"
        )
    return candidate


def credential_spent(*texts: Any) -> bool:
    """True when any of *texts* carries the spent-credential sentence."""
    lowered = "\n".join(str(t) for t in texts if t).lower()
    return any(signal in lowered for signal in SPENT_CREDENTIAL_SIGNALS)


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def liveness_fact(
    *, session_ok: bool | None, reason: str, mode: str, checked_at: str | None = None
) -> dict[str, Any]:
    """Build one validated fact. `checked_at` defaults to now."""
    fact = {
        "session_ok": session_ok,
        "reason": reason,
        "checked_at": checked_at or now_iso(),
        "mode": mode,
    }
    validate_liveness(fact)
    return fact


def validate_liveness(value: Any) -> None:
    """Raise `LivenessError` unless *value* is a fact the router can read."""
    if not isinstance(value, Mapping):
        raise LivenessError("the liveness fact must be an object")
    if tuple(sorted(value)) != tuple(sorted(LIVENESS_KEYS)):
        raise LivenessError(
            f"the liveness fact carries exactly {list(LIVENESS_KEYS)}, got {sorted(value)}"
        )
    if value["session_ok"] is not None and not isinstance(value["session_ok"], bool):
        raise LivenessError("liveness.session_ok must be true, false, or null (unmeasured)")
    if value["reason"] not in REASONS:
        raise LivenessError(f"liveness.reason {value['reason']!r} is not one of {sorted(REASONS)}")
    if not isinstance(value["checked_at"], str) or not value["checked_at"]:
        raise LivenessError("liveness.checked_at must be a non-empty ISO-8601 timestamp")
    if value["mode"] not in MODES:
        raise LivenessError(f"liveness.mode {value['mode']!r} is not one of {sorted(MODES)}")


def unmeasured(mode: str) -> dict[str, Any]:
    """The honest starting fact: nobody has measured this lane yet."""
    return liveness_fact(session_ok=None, reason=REASON_UNMEASURED, mode=mode)


def from_expiry(
    expires_at_ms: int | None, *, mode: str, now: float | None = None
) -> dict[str, Any]:
    """Derive the fact from a credential's expiry, a millisecond epoch.

    `None` (no file, no field, not an integer) is `unmeasured`, never a
    verdict: a missing credential file on a host is a fact about the host,
    but which fact — logged out, or a differently configured home — this
    module cannot tell, and guessing `false` would park a lane that works.
    """
    if expires_at_ms is None:
        return unmeasured(mode)
    current = time.time() if now is None else now
    if expires_at_ms / 1000.0 <= current:
        return liveness_fact(session_ok=False, reason=REASON_CREDENTIAL_EXPIRED, mode=mode)
    return liveness_fact(session_ok=True, reason=REASON_OK, mode=mode)


def read_json_int(path: Path | str, *keys: str) -> int | None:
    """Read an integer at *keys* inside the JSON document at *path*, or
    `None` for any reason at all — a missing file, unreadable bytes, a
    non-object, a missing key, a value that is not an integer. This runs
    inside a capability read, which must never crash the bridge."""
    try:
        node: Any = json.loads(Path(path).expanduser().read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    for key in keys:
        if not isinstance(node, Mapping) or key not in node:
            return None
        node = node[key]
    if isinstance(node, bool) or not isinstance(node, int):
        return None
    return node


class LivenessState:
    """The per-process latch and cache behind a bridge's `liveness` fact.

    Thread-safe because the async runner (its own threads) and the HTTP
    handler both touch it. Starts `unmeasured`; `record` replaces the fact;
    `observe_text` locks on the spent-credential sentence and is what both
    modes hang off a run's output; `clear` returns to `unmeasured`, which is
    the only way out of a lock other than a fresh probe recorded over it.
    """

    def __init__(self, mode: str) -> None:
        self.mode = parse_mode(mode)
        self._lock = threading.Lock()
        self._fact = unmeasured(self.mode)
        self._recorded_at: float | None = None

    def fact(self) -> dict[str, Any]:
        with self._lock:
            return dict(self._fact)

    @property
    def locked(self) -> bool:
        with self._lock:
            return self._fact["session_ok"] is False

    def record(self, fact: Mapping[str, Any]) -> None:
        validate_liveness(fact)
        with self._lock:
            self._fact = dict(fact)
            self._recorded_at = time.monotonic()

    def lock(self, reason: str) -> None:
        self.record(liveness_fact(session_ok=False, reason=reason, mode=self.mode))

    def clear(self) -> None:
        with self._lock:
            self._fact = unmeasured(self.mode)
            self._recorded_at = None

    def observe_text(self, *texts: Any) -> bool:
        """Lock the lane if *texts* carry the spent-credential sentence.
        Returns whether the lane is now locked BY THIS observation."""
        if not credential_spent(*texts):
            return False
        self.lock(REASON_REFRESH_TOKEN_SPENT)
        return True

    def fresh(self, ttl_seconds: float) -> bool:
        """True when a measured fact was recorded less than *ttl_seconds*
        ago. An unmeasured lane is never fresh: there is nothing to cache."""
        with self._lock:
            if self._recorded_at is None:
                return False
            return (time.monotonic() - self._recorded_at) < ttl_seconds
