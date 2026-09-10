"""examples/land/land_probe.py and the land node's PER-CHECKOUT lease --
the two halves that decide whether a `reset --hard` may run in the producing
actor's checkout (loop-closure task t6; spec c3/c15, honesty h12).

Split out of tests/test_land_node.py when the probe became its own module.
The fixtures are that file's (imported, not copied): the scratch bare remote,
the land account's checkout, the producing actor's checkout. What is local
here is a fake control plane with the REAL API's shapes -- a paged node-run
listing with `next_cursor`, an actor row behind an id, and an actor listing
carrying every registration revision of a key -- so a probe that has to walk
pages and match a KEY can be watched doing it.

Nothing here reaches a network beyond 127.0.0.1.
"""

from __future__ import annotations

import http.server
import json
import os
import threading
import urllib.parse

import pytest

from tests.test_land_node import (  # noqa: F401 - fixtures by name
    ACTOR_ID,
    ACTOR_KEY,
    LAND_RUN,
    OLD_ACTOR_ID,
    PRODUCING_RUN,
    TARGET,
    _load_land,
    _quiet_env,
    actor_checkout,
    git,
    land,
    land_ws,
    load_land_probe,
    load_land_reply,
    mint_handover,
    origin,
    result,
    run_land,
    steps,
)

# ---------------------------------------------------------------------------
# the per-checkout lease on the producing actor's checkout
# ---------------------------------------------------------------------------


def test_a_second_active_attempt_on_the_actor_checkout_blocks_the_reset(
    monkeypatch, capsys, origin, land_ws, actor_checkout  # noqa: F811
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    head_before = git(actor_checkout, "rev-parse", "HEAD")
    seen: list[tuple] = []

    probe_module = load_land_probe()

    def busy(api_url, actor_id, *, exclude_run_id):
        seen.append((api_url, actor_id, exclude_run_id))
        return probe_module.AttemptProbe(1, probe_module.MEASURED, "one live attempt")

    monkeypatch.setenv("NODES_API_URL", "http://control-plane.invalid")
    monkeypatch.setattr(land, "active_attempts", busy)
    # The t8 reply step reads the producing run from the same URL; seam it
    # to "no PR findings" so this test stays about the checkout lease.
    monkeypatch.setattr(load_land_reply(), "producing_run_input", lambda *_a, **_k: None)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_WAITING, records
    assert result(records)["outcome"] == "waiting"
    by = steps(records)
    assert by["push"]["outcome"] == "ok", "the landing itself is not what waits"
    wait = by["checkout_lease"]
    assert wait["outcome"] == "waiting" and wait["scope"] == "actor_checkout"
    assert wait["active_attempts"] == 1
    assert "reset" not in by, "the reset did not run"
    assert seen == [("http://control-plane.invalid", ACTOR_ID, LAND_RUN)]
    assert git(actor_checkout, "rev-parse", "HEAD") == head_before
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")

    # h27: the re-run picks up from where the first attempt stopped -- push
    # skipped, reset performed, exactly one commit on the branch.
    free = probe_module.AttemptProbe(0, probe_module.MEASURED, "nothing live")
    monkeypatch.setattr(land, "active_attempts", lambda *_a, **_k: free)
    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED, records
    by = steps(records)
    assert by["push"]["outcome"] == "skipped"
    assert by["checkout_lease"]["outcome"] == "ok" and by["checkout_lease"]["active_attempts"] == 0
    assert by["reset"]["outcome"] == "ok"
    assert git(actor_checkout, "rev-parse", "HEAD") == tip
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip


def test_a_held_checkout_file_lock_blocks_the_reset_too(
    monkeypatch, capsys, origin, land_ws, actor_checkout  # noqa: F811
):
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    head_before = git(actor_checkout, "rev-parse", "HEAD")
    lock = land.checkout_lock_path(actor_checkout)
    with land.LocalLock(lock).held(holder={"land_run_id": "01M0OTHERLANDER", "pid": os.getpid()}):
        code, records = run_land(
            monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
        )
    assert code == land.EXIT_WAITING, records
    wait = steps(records)["checkout_lease"]
    assert wait["outcome"] == "waiting" and wait["holder"]["land_run_id"] == "01M0OTHERLANDER"
    assert wait["active_attempts"] is None, "no control plane configured: the probe says so, not 0"
    assert git(actor_checkout, "rev-parse", "HEAD") == head_before


class _ControlPlaneHandler(http.server.BaseHTTPRequestHandler):
    """The three reads the checkout probe performs, with the real API's
    shapes: the PAGED node-run listing (`next_cursor`, newest first, no
    server-side actor or state filter -- internal/api/queries.go's
    listNodeRunsAcrossRuns), the actor row behind an id, and the actor
    listing (EVERY revision of every key). `fail_paths` makes a read fail
    the way a control plane that is down, slow or unauthorised does."""

    items: list[dict] = []
    actors: list[dict] = []
    page_size: int = 500
    seen_paths: list[str] = []
    fail_paths: tuple[str, ...] = ()

    def _send(self, status: int, payload) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 - http.server's name
        cls = type(self)
        cls.seen_paths.append(self.path)
        path, _, query = self.path.partition("?")
        if any(path.startswith(prefix) for prefix in cls.fail_paths):
            return self._send(503, {"error": "the control plane is unavailable"})
        if path == "/v1alpha1/node-runs":
            start = int(urllib.parse.parse_qs(query).get("cursor", ["0"])[0])
            page = cls.items[start : start + cls.page_size]
            out = {"items": page}
            if start + cls.page_size < len(cls.items):
                out["next_cursor"] = str(start + cls.page_size)
            return self._send(200, out)
        if path == "/v1alpha1/actors":
            return self._send(200, {"items": cls.actors})
        if path.startswith("/v1alpha1/actors/"):
            wanted = path.rsplit("/", 1)[1]
            for actor in cls.actors:
                if actor["id"] == wanted:
                    return self._send(200, actor)
            return self._send(404, {"error": "no such actor"})
        return self._send(404, {"error": "no such route"})

    def log_message(self, *_a):  # silence
        return


@pytest.fixture
def node_runs_api():
    server = http.server.HTTPServer(("127.0.0.1", 0), _ControlPlaneHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    _ControlPlaneHandler.actors = [
        {"id": OLD_ACTOR_ID, "actor_key": ACTOR_KEY, "revision": 1},
        {"id": ACTOR_ID, "actor_key": ACTOR_KEY, "revision": 2},
        {"id": "01M0SOMEONEELSE0000000000", "actor_key": "codex/orin", "revision": 1},
    ]
    try:
        yield server, _ControlPlaneHandler
    finally:
        server.shutdown()
        _ControlPlaneHandler.items = []
        _ControlPlaneHandler.actors = []
        _ControlPlaneHandler.seen_paths = []
        _ControlPlaneHandler.page_size = 500
        _ControlPlaneHandler.fail_paths = ()


def probe(server, **kwargs):
    """The un-monkeypatched probe against the fake control plane."""
    real = _load_land()
    url = f"http://127.0.0.1:{server.server_address[1]}"
    return real.active_attempts(url, ACTOR_ID, exclude_run_id=LAND_RUN, **kwargs)


def test_active_attempts_counts_live_node_runs_of_the_actor_from_the_control_plane(node_runs_api):
    server, handler = node_runs_api
    handler.items = [
        {"id": "1", "run_id": "RUN-X", "actor_id": ACTOR_ID, "state": "running"},
        {"id": "2", "run_id": "RUN-Y", "actor_id": ACTOR_ID, "state": "leased"},
        {"id": "3", "run_id": "RUN-Z", "actor_id": ACTOR_ID, "state": "waiting_external"},
        {"id": "4", "run_id": "RUN-DONE", "actor_id": ACTOR_ID, "state": "completed"},
        {
            "id": "5",
            "run_id": "RUN-OTHER",
            "actor_id": "01M0SOMEONEELSE0000000000",
            "state": "running",
        },
        {"id": "6", "run_id": LAND_RUN, "actor_id": ACTOR_ID, "state": "running"},
    ]
    measured = probe(server)
    assert (measured.count, measured.status) == (3, "measured")
    assert any(p.startswith("/v1alpha1/node-runs") for p in handler.seen_paths)


def test_active_attempts_is_unchecked_without_a_control_plane():
    real = _load_land()
    for url in ("", None):
        unchecked = real.active_attempts(url, ACTOR_ID, exclude_run_id=LAND_RUN)
        assert unchecked.count is None
        assert unchecked.status == "not_configured"


def test_a_live_attempt_on_a_later_page_of_the_listing_is_still_counted(node_runs_api):
    """The listing is newest-first and paged; an attempt that has not been
    touched recently sits on page 2. One page is not the fleet."""
    server, handler = node_runs_api
    handler.page_size = 2
    handler.items = [
        {"id": "1", "run_id": "RUN-A", "actor_id": "01M0SOMEONEELSE0000000000", "state": "running"},
        {"id": "2", "run_id": "RUN-B", "actor_id": "01M0SOMEONEELSE0000000000", "state": "leased"},
        {"id": "3", "run_id": "RUN-C", "actor_id": ACTOR_ID, "state": "waiting_external"},
    ]
    measured = probe(server)
    assert (measured.count, measured.status) == (1, "measured")
    pages = [p for p in handler.seen_paths if p.startswith("/v1alpha1/node-runs")]
    assert len(pages) == 2, pages
    assert "cursor=2" in pages[1], pages


def test_an_attempt_from_an_earlier_registration_revision_is_still_counted(node_runs_api):
    """Re-registering an actor mints a NEW row id; the attempts dispatched
    before it keep the old one. The identity is the KEY, not the row."""
    server, handler = node_runs_api
    handler.items = [
        {"id": "1", "run_id": "RUN-OLD", "actor_id": OLD_ACTOR_ID, "state": "running"},
        {"id": "2", "run_id": "RUN-NEW", "actor_id": ACTOR_ID, "state": "leased"},
        {
            "id": "3",
            "run_id": "RUN-OTHER",
            "actor_id": "01M0SOMEONEELSE0000000000",
            "state": "running",
        },
    ]
    measured = probe(server)
    assert (measured.count, measured.status) == (2, "measured")
    assert ACTOR_KEY in measured.detail


def test_a_declared_actor_key_skips_the_actor_lookup(monkeypatch, node_runs_api):
    """LAND_PRODUCING_ACTOR_KEY: the operator declares the identity, and the
    probe spends one fewer read resolving what it was already told."""
    server, handler = node_runs_api
    handler.items = [{"id": "1", "run_id": "RUN-OLD", "actor_id": OLD_ACTOR_ID, "state": "running"}]
    monkeypatch.setenv(land.PRODUCING_ACTOR_KEY, ACTOR_KEY)
    measured = probe(server)
    assert measured.count == 1
    assert not [p for p in handler.seen_paths if p.startswith(f"/v1alpha1/actors/{ACTOR_ID}")]


def test_a_listing_that_does_not_end_is_unmeasured_never_zero(node_runs_api):
    server, handler = node_runs_api
    handler.page_size = 1
    pages = load_land_probe().MAX_NODE_RUN_PAGES
    handler.items = [
        {
            "id": str(i),
            "run_id": f"RUN-{i}",
            "actor_id": "01M0SOMEONEELSE0000000000",
            "state": "running",
        }
        for i in range(pages + 5)
    ]
    truncated = probe(server)
    assert truncated.count is None, "a walk that hit its bound has not measured anything"
    assert truncated.status == "unmeasured"
    assert str(pages) in truncated.detail
    walked = [p for p in handler.seen_paths if p.startswith("/v1alpha1/node-runs")]
    assert len(walked) == pages, walked


@pytest.mark.parametrize("failing", ["/v1alpha1/node-runs", "/v1alpha1/actors"])
def test_a_control_plane_read_that_fails_is_unmeasured_never_zero(node_runs_api, failing):
    server, handler = node_runs_api
    handler.fail_paths = (failing,)
    handler.items = []
    failed = probe(server)
    assert failed.count is None
    assert failed.status == "unmeasured"
    assert failing in failed.detail


@pytest.mark.parametrize("unmeasurable", ["failed_read", "truncated_listing"])
def test_an_unmeasured_probe_makes_the_reset_wait_rather_than_proceed(
    monkeypatch, capsys, origin, land_ws, actor_checkout, node_runs_api, unmeasurable  # noqa: F811
):
    """The count is what unlocks `checkout -B` + `reset --hard` on a checkout
    that may have a session running in it. Not measured is not zero (h12) --
    whether the read failed outright or the listing never ended."""
    server, handler = node_runs_api
    if unmeasurable == "failed_read":
        handler.fail_paths = ("/v1alpha1/node-runs",)
    else:
        handler.page_size = 1
        handler.items = [
            {
                "id": str(i),
                "run_id": f"RUN-{i}",
                "actor_id": "01M0SOMEONEELSE0000000000",
                "state": "done",
            }
            for i in range(load_land_probe().MAX_NODE_RUN_PAGES + 3)
        ]
    ref, _ = mint_handover(actor_checkout, PRODUCING_RUN, "src/a.py", "a = 1\n", "t6: add a")
    head_before = git(actor_checkout, "rev-parse", "HEAD")
    monkeypatch.setenv("NODES_API_URL", f"http://127.0.0.1:{server.server_address[1]}")
    monkeypatch.setattr(land, "active_attempts", _load_land().active_attempts)
    monkeypatch.setattr(load_land_reply(), "producing_run_input", lambda *_a, **_k: None)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_WAITING, records
    assert result(records)["outcome"] == "waiting"
    by = steps(records)
    assert by["push"]["outcome"] == "ok", "the landing itself is not what waits"
    wait = by["checkout_lease"]
    assert wait["outcome"] == "waiting"
    assert wait["reason"] == "active_attempts_unmeasured"
    assert wait["active_attempts"] is None
    assert wait["attempts_probe"] == "unmeasured"
    assert "reset" not in by, "the reset did not run on an unmeasured probe"
    assert git(actor_checkout, "rev-parse", "HEAD") == head_before
