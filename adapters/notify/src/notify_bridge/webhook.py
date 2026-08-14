"""Discord webhook transport: a Python port of `internal/notify/webhook.go`
(itself a Go port of devex's proven design -- `internal/notify/doc.go`
names the source files). The rules below are carried over deliberately,
not reinvented, matching that package field for field:

* the URL is env-only, resolved fresh on every call and NEVER stored,
  logged, or written to a config file -- it embeds a bearer token. This
  module has no place a caller could even hand it a URL to persist:
  ``resolve_webhook`` is the only function that reads one, and its return
  value is the only way to learn it.
* one bounded 5-second POST, no retries: a webhook problem must never make
  a caller wait longer than this once, and must never cascade into a
  second attempt.
* redirects are never followed. A validated ``https://`` URL redirecting
  to an attacker-controlled host (or a non-http(s) scheme) is exactly the
  SSRF / scheme-guard bypass a followed redirect would open, so a 3xx
  response is treated like any other non-2xx: ``FAILED``.
* every failure mode -- a non-http(s) scheme, a redirect, a non-2xx
  response, a timeout, a DNS failure, a connection refusal -- collapses to
  the same ``FAILED`` result, so a fail-open caller never needs a
  cause-specific branch.
"""

from __future__ import annotations

import enum
import os
import urllib.error
import urllib.request
from urllib.parse import urlsplit

#: The only two places the webhook URL is ever read from. Both are
#: environment variables, matching `internal/notify/webhook.go`'s
#: envPrimary/envFallback names exactly.
ENV_PRIMARY = "CULTURE_NODES_WEBHOOK_URL"
ENV_FALLBACK = "DISCORD_WEBHOOK_URL"

#: Bounds the single POST attempt `post()` makes.
POST_TIMEOUT_SECONDS = 5.0

#: Identifies this transport in the (non-secret) request headers it sends.
#: Names neither the URL nor the run/node it is notifying about.
USER_AGENT = "culture-nodes-notify-bridge/1"

#: The set of hosts a Discord webhook URL can use. Matches devex's
#: `_DISCORD_HOSTS` and `internal/notify/webhook.go`'s `discordHosts`.
_DISCORD_HOSTS = frozenset(
    {"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"}
)


class PostResult(str, enum.Enum):
    """The outcome of `post()`. Deliberately coarse -- see module
    docstring: `post()` never distinguishes *why* a delivery failed."""

    #: The resolved URL was empty -- `post()` made no network call.
    DISABLED = "disabled"
    #: The request was sent and the response was 2xx.
    POSTED = "posted"
    #: Every other outcome: a non-http(s) scheme, a 3xx redirect (never
    #: followed), a non-2xx response, a timeout, or any other transport
    #: error.
    FAILED = "failed"


def resolve_webhook(env: dict[str, str] | None = None) -> tuple[str, bool]:
    """Read the webhook URL from the environment: ``ENV_PRIMARY`` wins when
    set to a non-blank value, else ``ENV_FALLBACK`` is tried the same way.
    A value that is empty or all whitespace counts as unset, exactly like a
    value never set at all.

    Returns ``(url, enabled)``. ``enabled`` is False, and ``url`` is ``""``,
    when neither variable resolves to a usable value -- the disabled state
    every caller must treat as "make no network call". This function never
    logs, wraps, or otherwise surfaces the URL through anything but this
    direct return.

    *env* defaults to `os.environ` and is only ever a parameter so tests can
    supply a clean mapping instead of mutating the real process environment.
    """
    env = os.environ if env is None else env
    primary = (env.get(ENV_PRIMARY) or "").strip()
    if primary:
        return primary, True
    fallback = (env.get(ENV_FALLBACK) or "").strip()
    if fallback:
        return fallback, True
    return "", False


def is_http_url(raw_url: str) -> bool:
    """Reports whether raw_url has an http or https scheme -- the only
    schemes `post()` ever sends a request to. A malformed URL reports
    False, not an error: a bad URL is exactly the case this guard exists
    to catch."""
    try:
        parsed = urlsplit(raw_url)
    except ValueError:
        return False
    return parsed.scheme.lower() in ("http", "https")


def is_discord_url(raw_url: str) -> bool:
    """Reports whether raw_url is a Discord webhook endpoint: a host in
    `_DISCORD_HOSTS` *and* a path containing ``/api/webhooks/``. Both
    conditions matter -- a bare discord.com link that is not a webhook
    path must not be shaped as one. Matches devex's `is_discord_url` and
    `internal/notify/webhook.go`'s `IsDiscordURL`."""
    try:
        parsed = urlsplit(raw_url)
    except ValueError:
        return False
    host = (parsed.hostname or "").lower()
    return host in _DISCORD_HOSTS and "/api/webhooks/" in parsed.path


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Returning None from `redirect_request` tells urllib to stop at the
    first redirect instead of following it -- `post()` then treats the 3xx
    response like any other non-2xx status: FAILED. This is the Python
    equivalent of `internal/notify/webhook.go`'s
    ``CheckRedirect: http.ErrUseLastResponse``."""

    def redirect_request(
        self, req, fp, code, msg, headers, newurl
    ):  # noqa: N802 - stdlib signature
        return None


_opener = urllib.request.build_opener(_NoRedirect)


def post(raw_url: str, body: bytes) -> tuple[PostResult, int | None]:
    """Send *body* (already-shaped JSON -- see `payload.build_message`) to
    *raw_url* as a single bounded POST. Never raises.

    Returns ``(result, status_code)``. ``status_code`` is the HTTP status a
    response actually carried -- present for both `POSTED` and a `FAILED`
    non-2xx response (including a refused redirect), absent (``None``) when
    no response was ever received at all (disabled, a bad scheme, a
    timeout, a DNS failure, a connection refusal).

    * ``raw_url == ""`` (or all whitespace) -> ``(DISABLED, None)``, no
      network call at all -- the path `resolve_webhook`'s disabled state
      takes.
    * a non-http(s) scheme -> ``(FAILED, None)``, also with no network call
      (the scheme guard rejects it before dialing anything).
    * otherwise: one POST, bounded to `POST_TIMEOUT_SECONDS`, redirects
      never followed, 2xx -> ``(POSTED, status)``, anything else
      -> ``(FAILED, status_or_None)``.
    """
    trimmed = (raw_url or "").strip()
    if not trimmed:
        return PostResult.DISABLED, None
    if not is_http_url(trimmed):
        return PostResult.FAILED, None

    req = urllib.request.Request(
        trimmed,
        data=body,
        method="POST",
        headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
    )
    try:
        # The URL is resolved by resolve_webhook() from CULTURE_NODES_WEBHOOK_URL
        # / DISCORD_WEBHOOK_URL and scheme-guarded above; redirects are
        # disabled by _NoRedirect.
        with _opener.open(req, timeout=POST_TIMEOUT_SECONDS) as resp:  # nosec B310
            status = resp.status
    except urllib.error.HTTPError as exc:
        # Includes a refused 3xx redirect (raised as an HTTPError by
        # urllib once _NoRedirect declines to follow it) and any 4xx/5xx.
        return PostResult.FAILED, exc.code
    except (urllib.error.URLError, OSError, ValueError):
        return PostResult.FAILED, None

    if 200 <= status < 300:
        return PostResult.POSTED, status
    return PostResult.FAILED, status
