#!/usr/bin/env python3
"""combining-loop `release`: emit one combining-loop.package.ready signal
event per unlocked package (plan task t5; split from combining-loop-node.py
to hold the 1000-line gate — each node program is fetched as ONE file by its
bootstrap, so release ships as its own file behind its own
COMBINING_LOOP_RELEASE_URL / _SHA256 grant).

    combining-loop-release.py release

Input (NODES_INPUT_JSON): {"package_id": "<id>", "unlocks": ["<id>", ...]}.
Grants: NODES_API_URL, NODES_EVENT_TOKEN.
Exit 0 released (empty unlocks is a stated no-op) / 2 environment /
4 refused. Post-condition (h7): a delivery counts only when the response is
2xx AND carries EventDeliveryResult's event.id; the ids ride the output as
`verified.event_ids`.

The four helpers below are duplicated verbatim from combining-loop-node.py
(single-file-fetch rule: a bootstrap fetches exactly one file, so no
imports). Change them there first, then copy.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from typing import Any

EXIT_OK = 0
EXIT_ENVIRONMENT = 2
EXIT_REFUSED = 4


class Refusal(Exception):
    def __init__(self, message: str, hint: str, *, code: int) -> None:
        super().__init__(message)
        self.hint = hint
        self.code = code


def emit(payload: dict[str, Any]) -> None:
    json.dump(payload, sys.stdout)
    sys.stdout.write("\n")


def env_or_refuse(name: str, hint: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        raise Refusal(f"{name} is not set", hint, code=EXIT_ENVIRONMENT)
    return value


def redact_secret_text(text: str) -> str:
    secret = os.environ.get(EVENT_TOKEN_ENV, "")
    return text.replace(secret, "[redacted]") if secret else text


def read_input(known_keys: frozenset[str]) -> dict[str, Any]:
    raw = os.environ.get("NODES_INPUT_JSON", "")
    if not raw:
        raise Refusal(
            "NODES_INPUT_JSON is not set",
            "the engine provides it for graph dispatches; CLI callers export it",
            code=EXIT_REFUSED,
        )
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError as err:
        raise Refusal(
            f"NODES_INPUT_JSON is not valid JSON: {err}",
            "provide a JSON object",
            code=EXIT_REFUSED,
        ) from err
    if not isinstance(payload, dict):
        raise Refusal(
            "NODES_INPUT_JSON must be a JSON object",
            "provide a JSON object",
            code=EXIT_REFUSED,
        )
    unknown = sorted(set(payload) - known_keys)
    if unknown:
        named = ", ".join(repr(key) for key in unknown)
        raise Refusal(
            f"NODES_INPUT_JSON declares {named}, which this subcommand never reads",
            f"known keys: {', '.join(sorted(known_keys))}",
            code=EXIT_REFUSED,
        )
    return payload


def require_str(payload: dict[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or not value:
        raise Refusal(
            f"{key!r} is required and must be a non-empty string",
            f"declare {key} in NODES_INPUT_JSON",
            code=EXIT_REFUSED,
        )
    return value


EVENT_TOKEN_ENV = "NODES_EVENT_TOKEN"  # noqa: S105 - an env var NAME, not a secret
API_URL_ENV = "NODES_API_URL"
READY_EVENT_NAME = "combining-loop.package.ready"


def delivered_event_id(status: int, raw_body: bytes) -> str:
    """Post-condition (h7): EventDeliveryResult's `event.id`. ValueError on
    anything short of that — non-2xx, bad JSON, or a 2xx error-shaped body."""
    if not 200 <= status < 300:
        raise ValueError(f"HTTP {status}")
    event_id = json.loads(raw_body)["event"]["id"]
    if not isinstance(event_id, str) or not event_id:
        raise ValueError("no usable event.id")
    return event_id


def cmd_release(_args: argparse.Namespace) -> int:
    """Emit one combining-loop.package.ready signal event per unlocked
    package. The engine's pacing sits between a ready event and any dispatch
    it triggers, so a burst here queues rather than overdrawing the session
    window (#48). An empty unlocks list is a successful no-op, stated."""
    payload = read_input(frozenset({"package_id", "unlocks"}))
    package_id = require_str(payload, "package_id")
    unlocks = payload.get("unlocks", [])
    if not isinstance(unlocks, list) or any(
        not isinstance(item, str) or not item for item in unlocks
    ):
        raise Refusal(
            "'unlocks' must be a list of non-empty strings",
            'NODES_INPUT_JSON must declare {"unlocks": ["<package-id>", ...]} or omit it',
            code=EXIT_REFUSED,
        )
    if not unlocks:
        emit({"outcome": "released", "events": 0})
        return EXIT_OK

    api = env_or_refuse(
        API_URL_ENV, "grant NODES_API_URL to the operation - the control plane's own base URL"
    ).rstrip("/")
    token = env_or_refuse(
        EVENT_TOKEN_ENV,
        "grant NODES_EVENT_TOKEN to the operation - the event-ingress bearer "
        "(the server side is NODES_EVENT_TOKEN_SECRET; internal/api/signalevents.go)",
    )

    emitted = 0
    delivered_ids: list[str] = []
    for unlocked in unlocks:
        body = json.dumps(
            {
                "name": READY_EVENT_NAME,
                "subject": unlocked,
                "emitter": "combining-loop",
                "payload": {"package_id": unlocked, "unlocked_by": package_id},
            }
        ).encode("utf-8")
        request = urllib.request.Request(
            f"{api}/v1alpha1/events",
            data=body,
            method="POST",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:  # noqa: S310
                status = response.status
                raw_body = response.read()
        except (urllib.error.URLError, TimeoutError) as err:
            # The token never reaches a diagnostic: URLError reasons carry
            # transport detail only, but redact defensively anyway.
            detail = redact_secret_text(str(err))
            raise Refusal(
                f"event delivery for {unlocked!r} failed after {emitted} delivered: {detail}",
                "already-delivered events stand (the ingress dedupes by source key upstream); "
                "fix the environment and re-run release for the remainder",
                code=EXIT_ENVIRONMENT,
            ) from err
        try:
            event_id = delivered_event_id(status, raw_body)
        except (ValueError, json.JSONDecodeError, KeyError, TypeError) as exc:
            raise Refusal(
                f"event delivery for {unlocked!r} did not verify ({exc}), after {emitted} "
                "delivered",
                "a 2xx status is not delivery on its own; EventDeliveryResult must carry a "
                "usable event.id",
                code=EXIT_ENVIRONMENT,
            ) from exc
        delivered_ids.append(event_id)
        emitted += 1

    emit({"outcome": "released", "events": emitted, "verified": {"event_ids": delivered_ids}})
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="combining-loop-release.py")
    subparsers = parser.add_subparsers(dest="subcommand", required=True)
    release = subparsers.add_parser(
        "release",
        help="emit combining-loop.package.ready for everything this package unlocks",
    )
    release.set_defaults(handler=cmd_release)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.handler(args))
    except Refusal as refusal:
        print(f"error: {redact_secret_text(str(refusal))}", file=sys.stderr)
        print(f"hint: {refusal.hint}", file=sys.stderr)
        return refusal.code


if __name__ == "__main__":
    raise SystemExit(main())
