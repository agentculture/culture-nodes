"""Payload-shaping tests, mirroring `internal/notify/payload_test.go`'s
discipline: trims stay well under Discord's caps, and the Discord vs.
generic shape is chosen correctly by `is_discord_url`."""

from __future__ import annotations

import json

from notify_bridge import payload

DISCORD_URL = "https://discord.com/api/webhooks/123/token"
GENERIC_URL = "https://example.com/hooks/notify"


def test_trim_leaves_short_strings_untouched():
    assert payload.trim("hello", 10) == "hello"


def test_trim_cuts_and_marks_long_strings():
    trimmed = payload.trim("x" * 300, 256)
    assert len(trimmed) == 256
    assert trimmed.endswith(payload.ELLIPSIS)


def test_build_discord_message_shapes_title_description_fields():
    message = payload.NotifyMessage(
        content="plain text",
        title="Build failed",
        description="see the log",
        fields=(payload.NotifyField(name="Run", value="run_1", inline=True),),
    )
    body = json.loads(payload.build_message(DISCORD_URL, message))
    assert body["content"] == "plain text"
    embed = body["embeds"][0]
    assert embed["title"] == "Build failed"
    assert embed["description"] == "see the log"
    assert embed["fields"] == [{"name": "Run", "value": "run_1", "inline": True}]


def test_build_discord_message_trims_oversized_title():
    message = payload.NotifyMessage(title="x" * 500)
    body = json.loads(payload.build_message(DISCORD_URL, message))
    assert len(body["embeds"][0]["title"]) == payload.MAX_TITLE_CHARS
    assert body["embeds"][0]["title"].endswith(payload.ELLIPSIS)


def test_build_discord_message_trims_oversized_description():
    message = payload.NotifyMessage(description="y" * 5000)
    body = json.loads(payload.build_message(DISCORD_URL, message))
    assert len(body["embeds"][0]["description"]) == payload.MAX_DESCRIPTION_CHARS
    assert body["embeds"][0]["description"].endswith(payload.ELLIPSIS)


def test_build_discord_message_omits_embed_when_no_embed_fields_given():
    message = payload.NotifyMessage(content="just text")
    body = json.loads(payload.build_message(DISCORD_URL, message))
    assert "embeds" not in body
    assert body["content"] == "just text"


def test_build_generic_message_for_a_non_discord_url():
    message = payload.NotifyMessage(content="hi", title="t", description="d")
    body = json.loads(payload.build_message(GENERIC_URL, message))
    assert body == {"content": "hi", "title": "t", "description": "d", "fields": []}


def test_build_discord_message_fields_are_individually_trimmed():
    message = payload.NotifyMessage(
        title="t",
        fields=(payload.NotifyField(name="n" * 500, value="v" * 2000, inline=False),),
    )
    body = json.loads(payload.build_message(DISCORD_URL, message))
    field = body["embeds"][0]["fields"][0]
    assert len(field["name"]) == payload.MAX_FIELD_NAME_CHARS
    assert len(field["value"]) == payload.MAX_FIELD_VALUE_CHARS
