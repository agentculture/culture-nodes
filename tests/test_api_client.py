"""Unit tests for culture_nodes.api_client: base-URL resolution, scheme
validation, and error mapping (API error bodies, connection failures,
malformed JSON)."""

from __future__ import annotations

import pytest

from culture_nodes.api_client import (
    DEFAULT_API_URL,
    ApiClient,
    resolve_base_url,
)
from culture_nodes.cli._errors import EXIT_ENV_ERROR, EXIT_USER_ERROR, CliError

# --- resolve_base_url -------------------------------------------------------


def test_resolve_base_url_prefers_cli_flag(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("NODES_API_URL", "http://env-host:1111")
    assert resolve_base_url("http://flag-host:2222") == "http://flag-host:2222"


def test_resolve_base_url_falls_back_to_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("NODES_API_URL", "http://env-host:1111")
    assert resolve_base_url(None) == "http://env-host:1111"


def test_resolve_base_url_falls_back_to_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("NODES_API_URL", raising=False)
    assert resolve_base_url(None) == DEFAULT_API_URL


def test_resolve_base_url_strips_trailing_slash(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("NODES_API_URL", raising=False)
    assert resolve_base_url("http://host:9/") == "http://host:9"


# --- scheme validation -------------------------------------------------------


@pytest.mark.parametrize("bad_url", ["file:///etc/passwd", "ftp://host", "not-a-url", ""])
def test_api_client_rejects_unsupported_scheme(bad_url: str) -> None:
    with pytest.raises(CliError) as exc:
        ApiClient(bad_url)
    assert exc.value.code == EXIT_USER_ERROR
    assert "http://" in exc.value.remediation


def test_api_client_accepts_http_and_https() -> None:
    ApiClient("http://127.0.0.1:8080")
    ApiClient("https://example.test:8443")


# --- connection failures -----------------------------------------------------


def test_connection_refused_maps_to_env_error() -> None:
    # Port 1 is a reserved low port with nothing listening; connecting there
    # without root privileges reliably raises ECONNREFUSED.
    client = ApiClient("http://127.0.0.1:1", timeout=2.0)
    with pytest.raises(CliError) as exc:
        client.request("GET", "/v1alpha1/healthz")
    assert exc.value.code == EXIT_ENV_ERROR
    assert "cannot reach the nodes API at http://127.0.0.1:1" in exc.value.message
    assert "nodes serve" in exc.value.remediation
    assert "--api-url" in exc.value.remediation


# --- API error body mapping ---------------------------------------------------


def test_api_error_body_relayed_verbatim(fake_api) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows/(?P<digest>[^/]+)",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": "no workflow version with digest bogus",
                "remediation": "check the digest",
            },
        ),
    )
    fake_api.start()
    client = ApiClient(fake_api.base_url, timeout=5.0)
    with pytest.raises(CliError) as exc:
        client.request("GET", "/v1alpha1/workflows/bogus")
    assert exc.value.code == EXIT_USER_ERROR
    assert exc.value.message == "no workflow version with digest bogus"
    assert exc.value.remediation == "check the digest"


def test_api_error_body_env_code_relayed(fake_api) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(
            500, {"code": 2, "message": "database unavailable", "remediation": "retry later"}
        ),
    )
    fake_api.start()
    client = ApiClient(fake_api.base_url, timeout=5.0)
    with pytest.raises(CliError) as exc:
        client.request("GET", "/v1alpha1/workflows")
    assert exc.value.code == EXIT_ENV_ERROR
    assert exc.value.message == "database unavailable"


def test_unrecognized_error_body_falls_back(fake_api) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflows",
        lambda h, m, q, b: h.send_json(400, {"oops": "not the documented shape"}),
    )
    fake_api.start()
    client = ApiClient(fake_api.base_url, timeout=5.0)
    with pytest.raises(CliError) as exc:
        client.request("GET", "/v1alpha1/workflows")
    assert exc.value.code == EXIT_USER_ERROR
    assert "HTTP 400" in exc.value.message


# --- malformed JSON success body ---------------------------------------------


def test_malformed_json_success_body_maps_to_env_error(fake_api) -> None:
    def handler(h, m, q, b):
        h.send_response(200)
        h.send_header("Content-Type", "application/json")
        body = b"{not json"
        h.send_header("Content-Length", str(len(body)))
        h.end_headers()
        h.wfile.write(body)

    fake_api.route("GET", r"/v1alpha1/healthz", handler)
    fake_api.start()
    client = ApiClient(fake_api.base_url, timeout=5.0)
    with pytest.raises(CliError) as exc:
        client.request("GET", "/v1alpha1/healthz")
    assert exc.value.code == EXIT_ENV_ERROR
    assert "not valid JSON" in exc.value.message


# --- request/response round trip ---------------------------------------------


def test_request_round_trip_query_and_body(fake_api) -> None:
    def handler(h, m, q, b):
        assert q.get("limit") == ["5"]
        assert b == b'{"workflow_digest": "sha256:abc"}'
        h.send_json(201, {"id": "run-1", "state": "running"})

    fake_api.route("GET", r"/v1alpha1/echo", handler)
    fake_api.start()
    client = ApiClient(fake_api.base_url, timeout=5.0)
    resp = client.request(
        "GET",
        "/v1alpha1/echo",
        query={"limit": 5},
        json_body={"workflow_digest": "sha256:abc"},
    )
    assert resp.status == 201
    assert resp.payload == {"id": "run-1", "state": "running"}
    assert resp.raw == b'{"id": "run-1", "state": "running"}'
