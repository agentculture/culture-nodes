"""Stdlib HTTP client for the Culture Nodes control-plane REST API.

Spec decision c28: the Python side of culture-nodes is *only* a CLI front
over the REST API (``api/openapi/openapi.yaml``) — no engine logic lives in
this package. Every product-verb command module
(:mod:`culture_nodes.cli._commands.workflow`,
:mod:`culture_nodes.cli._commands.run`,
:mod:`culture_nodes.cli._commands.ledger`,
:mod:`culture_nodes.cli._commands.review`) builds one HTTP call through
:class:`ApiClient` and renders the response; none of them touch the
compiler, the engine, or the ledger directly.

Zero third-party runtime dependencies (``pyproject.toml``'s
``dependencies = []``) means this client is built on :mod:`urllib.request`
rather than e.g. ``requests`` or ``httpx``.

Base URL resolution (see :func:`resolve_base_url`): ``--api-url`` flag, then
the ``NODES_API_URL`` environment variable, then
``http://127.0.0.1:8080`` (the Go binary's own ``defaultListenAddr``,
``cmd/nodes/serve.go``).

Error mapping
-------------
The API deliberately mirrors the CLI's own error shape (see
``api/openapi/openapi.yaml``'s ``Error`` schema and
``internal/clifmt.CliError``): every non-2xx response body is
``{code, message, remediation}`` with ``code`` in ``{1, 2}`` — the same
exit-code buckets :mod:`culture_nodes.cli._errors` defines. This client
therefore does not re-classify API errors; it re-raises the API's own
``code``/``message``/``remediation`` verbatim as a
:class:`~culture_nodes.cli._errors.CliError`, so e.g. a stale-review 409
from ``POST /v1alpha1/reviews/{id}/commit`` surfaces exactly the
remediation the API server wrote for it.

A connection failure (refused, DNS, timeout) never reaches the API at all,
so there is no error body to relay — those map to a client-authored
``CliError`` in the environment-error bucket (code 2) pointing at
``nodes serve`` / ``--api-url``.

Bandit B310 note
-----------------
``urllib.request.urlopen`` on a variable URL is flagged by bandit because an
unvalidated scheme (``file://``, ``ftp://``, ...) can be used to read local
files or reach unintended protocols. :func:`_validate_base_url` restricts
every base URL this client will ever open to ``http``/``https`` *before* any
request is built — see ``ApiClient.__init__`` — so by the time a call
reaches ``urlopen`` the scheme is already known-safe; the ``# nosec B310``
markers on the two call sites below record that justification.
"""

from __future__ import annotations

import http.client
import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlencode, urlsplit

from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError

#: Path prefix every operation in api/openapi/openapi.yaml lives under.
API_PREFIX = "/v1alpha1"

#: Env var carrying the API base URL (second in the resolution order).
ENV_API_URL = "NODES_API_URL"

#: Last-resort default: cmd/nodes/serve.go's defaultListenAddr, ":8080",
#: reachable at this loopback address for a locally started `nodes serve`.
DEFAULT_API_URL = "http://127.0.0.1:8080"

#: Timeout for ordinary request/response calls.
DEFAULT_TIMEOUT = 10.0

#: Timeout for the SSE events stream. Longer than DEFAULT_TIMEOUT because the
#: server (internal/api/events.go) only writes bytes when a new event
#: exists — it sends no heartbeat during a quiet run, so a short read
#: timeout would disconnect during perfectly normal idle gaps between
#: engine transitions.
DEFAULT_STREAM_TIMEOUT = 300.0

_ALLOWED_SCHEMES = ("http", "https")


def resolve_base_url(cli_value: str | None) -> str:
    """Resolve the API base URL: ``--api-url`` > ``NODES_API_URL`` > default."""
    if cli_value:
        return cli_value.rstrip("/")
    env_value = os.environ.get(ENV_API_URL)
    if env_value:
        return env_value.rstrip("/")
    return DEFAULT_API_URL


def _validate_base_url(url: str) -> str:
    parts = urlsplit(url)
    if parts.scheme not in _ALLOWED_SCHEMES or not parts.netloc:
        raise CliError(
            code=EXIT_USER_ERROR,
            message=f"unsupported or malformed API URL: {url!r}",
            remediation="use an http:// or https:// URL, e.g. http://127.0.0.1:8080",
        )
    return url.rstrip("/")


def add_api_url_argument(parser: Any) -> None:
    """Add the shared ``--api-url`` flag to a product-verb subparser."""
    parser.add_argument(
        "--api-url",
        dest="api_url",
        default=None,
        help=f"nodes API base URL (default: ${ENV_API_URL}, else {DEFAULT_API_URL}).",
    )


def client_from_args(args: Any, *, timeout: float = DEFAULT_TIMEOUT) -> "ApiClient":
    """Build an :class:`ApiClient` from a parsed ``argparse.Namespace``."""
    return ApiClient(resolve_base_url(getattr(args, "api_url", None)), timeout=timeout)


@dataclass
class ApiResponse:
    """One API call's result: HTTP status, raw bytes, and the parsed JSON."""

    status: int
    raw: bytes
    payload: Any


def _connection_error(base_url: str) -> CliError:
    return CliError(
        code=EXIT_ENV_ERROR,
        message=f"cannot reach the nodes API at {base_url}",
        remediation="start it with 'nodes serve' (Go binary) or pass --api-url",
    )


def _timeout_error(base_url: str) -> CliError:
    return CliError(
        code=EXIT_ENV_ERROR,
        message=f"timed out reaching the nodes API at {base_url}",
        remediation="the API may be slow or unreachable; retry, or pass --api-url",
    )


def _malformed_json_error(detail: str) -> CliError:
    return CliError(
        code=EXIT_ENV_ERROR,
        message=f"the nodes API returned a response that is not valid JSON: {detail}",
        remediation="check the API server logs; this may indicate a client/server version mismatch",
    )


def _error_from_body(status: int, raw: bytes) -> CliError:
    """Map a non-2xx response body to a CliError.

    The API mirrors the CLI's own error shape (see the module docstring), so
    a well-formed body's ``code``/``message``/``remediation`` are relayed
    verbatim rather than re-derived from the HTTP status.
    """
    payload: Any = None
    if raw:
        try:
            payload = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            payload = None
    if isinstance(payload, dict) and "message" in payload:
        code = payload.get("code")
        if code not in (EXIT_USER_ERROR, EXIT_ENV_ERROR):
            code = EXIT_USER_ERROR if status < 500 else EXIT_ENV_ERROR
        return CliError(
            code=code,
            message=str(payload["message"]),
            remediation=str(payload.get("remediation", "")),
        )
    return CliError(
        code=EXIT_USER_ERROR if status < 500 else EXIT_ENV_ERROR,
        message=f"nodes API returned HTTP {status} with an unrecognized error body",
        remediation="check the API server logs; this may indicate a client/server version mismatch",
    )


class ApiClient:
    """Thin synchronous JSON/SSE client for the Culture Nodes control-plane API."""

    def __init__(
        self,
        base_url: str,
        *,
        timeout: float = DEFAULT_TIMEOUT,
        stream_timeout: float = DEFAULT_STREAM_TIMEOUT,
    ) -> None:
        self.base_url = _validate_base_url(base_url)
        self.timeout = timeout
        self.stream_timeout = stream_timeout

    def _build_request(
        self,
        method: str,
        path: str,
        query: dict[str, Any] | None,
        json_body: Any,
        headers: dict[str, str] | None,
    ) -> urllib.request.Request:
        url = self.base_url + path
        if query:
            clean = {k: v for k, v in query.items() if v is not None and v != ""}
            if clean:
                url += "?" + urlencode(clean)
        data = None
        hdrs = dict(headers or {})
        if json_body is not None:
            data = json.dumps(json_body, ensure_ascii=False).encode("utf-8")
            hdrs.setdefault("Content-Type", "application/json")
        hdrs.setdefault("Accept", "application/json")
        return urllib.request.Request(url, data=data, method=method, headers=hdrs)

    def _open(self, req: urllib.request.Request, timeout: float | None) -> http.client.HTTPResponse:
        # req.full_url's scheme was validated to http/https in
        # _validate_base_url before this client would build any request —
        # see the module docstring's "Bandit B310 note".
        return urllib.request.urlopen(req, timeout=timeout)  # nosec B310

    def request(
        self,
        method: str,
        path: str,
        *,
        query: dict[str, Any] | None = None,
        json_body: Any = None,
    ) -> ApiResponse:
        """Issue one request and return its status, raw bytes, and parsed JSON.

        Raises :class:`CliError` for a non-2xx response (mapped from the
        API's own error body), a connection failure, or a timeout.
        """
        req = self._build_request(method, path, query, json_body, None)
        try:
            with self._open(req, self.timeout) as resp:
                raw = resp.read()
                status = resp.status
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            raise _error_from_body(exc.code, raw) from None
        except TimeoutError:
            raise _timeout_error(self.base_url) from None
        except urllib.error.URLError:
            raise _connection_error(self.base_url) from None

        payload: Any = None
        if raw:
            try:
                payload = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise _malformed_json_error(str(exc)) from None
        return ApiResponse(status=status, raw=raw, payload=payload)

    def open_stream(
        self,
        method: str,
        path: str,
        *,
        query: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> http.client.HTTPResponse:
        """Open a streaming (SSE) response. Caller must close it.

        A non-2xx response is read fully and raised as a :class:`CliError`
        (mapped from the API's error body) before returning — this only
        ever returns a genuinely-open ``200`` stream.
        """
        req = self._build_request(method, path, query, None, headers)
        effective_timeout = timeout if timeout is not None else self.stream_timeout
        try:
            return self._open(req, effective_timeout)
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            raise _error_from_body(exc.code, raw) from None
        except TimeoutError:
            raise _timeout_error(self.base_url) from None
        except urllib.error.URLError:
            raise _connection_error(self.base_url) from None


def probe_health(base_url: str, *, timeout: float = 2.0) -> tuple[bool, str]:
    """GET ``{base_url}/v1alpha1/healthz`` without raising.

    Used by ``nodes doctor``'s ``nodes_api_reachable`` check, which must
    stay non-fatal (the CLI's identity verbs work with no API running at
    all) — so this never raises, it only reports.
    """
    try:
        client = ApiClient(base_url, timeout=timeout)
        resp = client.request("GET", f"{API_PREFIX}/healthz")
    except CliError as err:
        return False, err.message
    ok = (
        resp.status == 200 and isinstance(resp.payload, dict) and resp.payload.get("status") == "ok"
    )
    return ok, ("healthy" if ok else f"unexpected response (HTTP {resp.status})")
