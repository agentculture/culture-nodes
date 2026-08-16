"""Reverse transport shared by every bridge package."""

from __future__ import annotations

import json
import logging
import os
import threading
import time
import urllib.error
import urllib.request

LOG = logging.getLogger(__name__)


def configured(prefix):
    values = (
        os.environ.get(prefix + "_CONTROL_PLANE_URL", "").rstrip("/"),
        os.environ.get(prefix + "_ACTOR_KEY", ""),
        os.environ.get(prefix + "_DIAL_TOKEN", ""),
    )
    if not any(values):
        return None
    if not all(values):
        raise ValueError(
            prefix + "_CONTROL_PLANE_URL, _ACTOR_KEY and _DIAL_TOKEN must be set together"
        )
    return values


def run(prefix, port, opener=urllib.request.urlopen, pause=time.sleep):
    base, actor, token = configured(prefix)
    headers = {"Authorization": "Bearer " + token, "X-Culture-Nodes-Actor-Key": actor}
    while True:
        try:
            req = urllib.request.Request(
                base + "/v1alpha1/inbound/poll", data=b"", headers=headers, method="POST"
            )
            try:
                response = opener(req, timeout=35)
            except urllib.error.HTTPError as exc:
                # Kept for a server that signals "nothing waiting" as an error
                # status; the shipped one does not, which is why the check
                # below exists and this branch alone was not enough.
                if exc.code == 204:
                    pause(0.25)
                    continue
                raise
            # An idle long poll returns 204 with an EMPTY body. 204 is 2xx, so
            # urllib does NOT raise HTTPError for it -- json.loads("") then
            # raised, the loop treated a normal empty poll as a connection
            # fault, and every bridge reconnected once a second forever
            # without ever claiming work. Found by the live demonstration in
            # the operator lane, which is the half that can open a socket.
            payload = response.read()
            if getattr(response, "status", None) == 204 or not payload.strip():
                pause(0.25)
                continue
            item = json.loads(payload)
            body = json.dumps(item["request"]).encode()
            local = urllib.request.Request(
                f"http://127.0.0.1:{port}/v1/invocations",
                data=body,
                headers={"Content-Type": "application/json", "Idempotency-Key": item["attempt_id"]},
                method="POST",
            )
            try:
                result = opener(local, timeout=65)
                status, payload = result.status, result.read()
            except urllib.error.HTTPError as exc:
                status, payload = exc.code, exc.read()
            complete = json.dumps({"status": status, "body": json.loads(payload)}).encode()
            done = urllib.request.Request(
                base + f"/v1alpha1/inbound/{item['id']}/complete",
                data=complete,
                headers={**headers, "Content-Type": "application/json"},
                method="POST",
            )
            opener(done, timeout=15).close()
        except Exception as exc:
            LOG.warning("dial-in reconnecting: %s", exc)
            pause(1)


def start(prefix, port):
    if configured(prefix) is None:
        return None
    thread = threading.Thread(target=run, args=(prefix, port), daemon=True, name="dial-in")
    thread.start()
    return thread
