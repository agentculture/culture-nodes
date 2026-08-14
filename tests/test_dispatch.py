"""Tests for `nodes dispatch` — the clarify-then-commit gate's thin client.

Task t14 shipped `culture_nodes/cli/_commands/dispatch.py` without tests: its
session was cut off by the node deadline while it was still working (issue
#82), and what it had committed was the implementation. These cover the
surface it left.

The point of each verb, and so of each test here: `pending` answers "what is
waiting", `show` prints the briefing IN FULL so it can actually be read, and
`confirm` is the separate second action that turns a composed `verdict: hold`
into a dispatch. A confirm that fires without a show is a keystroke rather
than an acknowledgement — so `show`'s completeness is a behaviour worth
pinning, not a formatting detail.
"""

from __future__ import annotations

import json

from culture_nodes.cli import main

# --- pending -----------------------------------------------------------------


def test_dispatch_pending_text_lists_each_waiting_preflight(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/preflights",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "items": [
                    {
                        "id": "pf-1",
                        "actor_key": "company/codex-thor",
                        "node_id": "review",
                        "expires_at": "2026-08-14T12:00:00Z",
                    },
                    {
                        "id": "pf-2",
                        "actor_key": "company/developer",
                        "node_id": "fix",
                        "expires_at": "2026-08-14T13:00:00Z",
                    },
                ]
            },
        ),
    )
    fake_api.start()
    rc = main(["dispatch", "pending", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    for expected in ("pf-1", "company/codex-thor", "review", "2026-08-14T12:00:00Z", "pf-2", "fix"):
        assert expected in out


def test_dispatch_pending_forwards_actor_key_and_limit_as_query(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(200, {"items": []})

    fake_api.route("GET", r"/v1alpha1/preflights", handler)
    fake_api.start()
    rc = main(
        [
            "dispatch",
            "pending",
            "--actor-key",
            "company/codex-thor",
            "--limit",
            "5",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["query"] == {"actor_key": ["company/codex-thor"], "limit": ["5"]}


def test_dispatch_pending_says_so_when_nothing_waits(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/preflights", lambda h, m, q, b: h.send_json(200, {"items": []})
    )
    fake_api.start()
    rc = main(["dispatch", "pending", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no preflights waiting to be acknowledged" in out


def test_dispatch_pending_json_is_byte_exact_passthrough(fake_api, capsys) -> None:
    payload = {"items": [{"id": "pf-1"}]}
    fake_api.route(
        "GET", r"/v1alpha1/preflights", lambda h, m, q, b: _write_compact(h, 200, payload)
    )
    fake_api.start()
    rc = main(["dispatch", "pending", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- show --------------------------------------------------------------------


def test_dispatch_show_prints_the_briefing_in_full(fake_api, capsys) -> None:
    """The document is printed whole, not summarised.

    A reader given a four-line digest has been *told about* a briefing rather
    than handed one, which is the failure this gate exists to prevent. So the
    assertion is on a value nested deep inside the document, not on the header.
    """
    document = {
        "acknowledgement": {"verb": "nodes dispatch confirm"},
        "task": {"instruction": "review the diff and report blocking issues"},
        "verdict": "hold",
    }
    fake_api.route(
        "GET",
        r"/v1alpha1/preflights/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "id": "pf-1",
                "actor_key": "company/codex-thor",
                "node_id": "review",
                "run_id": "run-1",
                "record_id": "rec-1",
                "expires_at": "2026-08-14T12:00:00Z",
                "acknowledged": False,
                "expired": False,
                "document": document,
            },
        ),
    )
    fake_api.start()
    rc = main(["dispatch", "show", "pf-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "id: pf-1" in out
    assert "actor_key: company/codex-thor" in out
    assert "run_id: run-1" in out
    assert "record_id: rec-1" in out
    assert "review the diff and report blocking issues" in out
    assert "nodes dispatch confirm" in out


def test_dispatch_show_renders_the_two_flags_lowercase(fake_api, capsys) -> None:
    """`acknowledged` and `expired` decide whether a confirm can still work.

    They render as bare lowercase words rather than Python's True/False, so
    the text output stays greppable by the same token in both languages.
    """
    fake_api.route(
        "GET",
        r"/v1alpha1/preflights/(?P<id>[^/]+)",
        lambda h, m, q, b: h.send_json(
            200, {"id": "pf-1", "acknowledged": True, "expired": True, "document": {}}
        ),
    )
    fake_api.start()
    rc = main(["dispatch", "show", "pf-1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "acknowledged: true" in out
    assert "expired: true" in out
    assert "True" not in out


def test_dispatch_show_json_is_byte_exact_passthrough(fake_api, capsys) -> None:
    payload = {"id": "pf-1", "document": {"verdict": "hold"}}
    fake_api.route(
        "GET",
        r"/v1alpha1/preflights/(?P<id>[^/]+)",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(["dispatch", "show", "pf-1", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- confirm -----------------------------------------------------------------


def test_dispatch_confirm_posts_the_actor_and_a_proceed_verdict(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b or b"{}")
        h.send_json(
            200,
            {
                "id": "pf-1",
                "acknowledged_by": "actor_dev_1",
                "acknowledgement_record_id": "rec-2",
                "expires_at": "2026-08-14T12:00:00Z",
            },
        )

    fake_api.route("POST", r"/v1alpha1/preflights/(?P<id>[^/]+)/acknowledge", handler)
    fake_api.start()
    rc = main(
        ["dispatch", "confirm", "pf-1", "--actor-id", "actor_dev_1", "--api-url", fake_api.base_url]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["body"] == {"actor_id": "actor_dev_1", "verdict": "proceed"}
    assert "preflight_id: pf-1" in out
    assert "acknowledged_by: actor_dev_1" in out
    assert "acknowledgement_record_id: rec-2" in out


def test_dispatch_confirm_omits_note_entirely_when_not_given(fake_api, capsys) -> None:
    """An absent note is an absent key, never an empty string.

    The note is documentary and lands on a ledger record; an empty one would
    read as "the acknowledger said nothing" rather than "was not asked".
    """
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b or b"{}")
        h.send_json(200, {"id": "pf-1"})

    fake_api.route("POST", r"/v1alpha1/preflights/(?P<id>[^/]+)/acknowledge", handler)
    fake_api.start()
    rc = main(
        ["dispatch", "confirm", "pf-1", "--actor-id", "actor_dev_1", "--api-url", fake_api.base_url]
    )
    assert rc == 0
    assert "note" not in seen["body"]


def test_dispatch_confirm_includes_the_note_when_given(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b or b"{}")
        h.send_json(200, {"id": "pf-1"})

    fake_api.route("POST", r"/v1alpha1/preflights/(?P<id>[^/]+)/acknowledge", handler)
    fake_api.start()
    rc = main(
        [
            "dispatch",
            "confirm",
            "pf-1",
            "--actor-id",
            "actor_dev_1",
            "--note",
            "read the briefing, proceeding",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["body"]["note"] == "read the briefing, proceeding"


def test_dispatch_confirm_relays_the_gates_own_refusal(fake_api, capsys) -> None:
    """A 409 is the gate refusing — single-use spent, or the window closed.

    The client must relay the API's own code/message/remediation rather than
    re-word them, or the protocol ends up with a second, drifting copy in the
    client. So the assertion is that the API's words reach stderr intact.

    The envelope is FLAT — `message` and `remediation` at the top level, which
    is what `api_client._error_from_response` recognises. A body that nests
    them under an `error` key falls through to the generic "unrecognized error
    body", which is exactly the drift this test guards against.
    """
    fake_api.route(
        "POST",
        r"/v1alpha1/preflights/(?P<id>[^/]+)/acknowledge",
        lambda h, m, q, b: h.send_json(
            409,
            {
                "message": "this preflight has already been acknowledged",
                "remediation": "dispatch a new preflight; an acknowledgement is single-use",
            },
        ),
    )
    fake_api.start()
    rc = main(
        ["dispatch", "confirm", "pf-1", "--actor-id", "actor_dev_1", "--api-url", fake_api.base_url]
    )
    err = capsys.readouterr().err
    assert rc != 0
    assert "already been acknowledged" in err
    assert "single-use" in err


def test_dispatch_confirm_json_is_byte_exact_passthrough(fake_api, capsys) -> None:
    payload = {"id": "pf-1", "acknowledged_by": "actor_dev_1"}
    fake_api.route(
        "POST",
        r"/v1alpha1/preflights/(?P<id>[^/]+)/acknowledge",
        lambda h, m, q, b: _write_compact(h, 200, payload),
    )
    fake_api.start()
    rc = main(
        [
            "dispatch",
            "confirm",
            "pf-1",
            "--actor-id",
            "actor_dev_1",
            "--json",
            "--api-url",
            fake_api.base_url,
        ]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


# --- the bare noun -----------------------------------------------------------


def test_dispatch_bare_noun_names_its_verbs_and_succeeds(capsys) -> None:
    """`nodes dispatch` with no verb is a description, not an error.

    The agent-first rubric wants a bare noun to teach its surface rather than
    fail, so this exits 0 and names all three verbs plus where to read more.
    """
    rc = main(["dispatch"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "pending" in out
    assert "show" in out
    assert "confirm" in out
    assert "nodes explain dispatch" in out


def _write_compact(handler, status: int, payload: object) -> None:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)
