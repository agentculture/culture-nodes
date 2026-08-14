"""Message shaping: a `notify` node's `input` -> the JSON body posted to
the webhook.

Unlike `internal/notify/payload.go`'s ``Payload`` -- a fixed five-field
run-lifecycle envelope that structurally keeps ledger records, node
output, and workflow input out of an unattended notification -- this
bridge's message *is* exactly what the workflow node's `input` asked to
be sent. The workflow author is the trust boundary here, the same as any
node's `input` is author-controlled; there is no "boundary: minimal
metadata only" rule to enforce. The one rule carried over unmodified is
Discord's own limits: the same defensive-trim numbers
`internal/notify/payload.go` uses, applied to whatever the author wrote
rather than to an auto-generated string.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

from .webhook import is_discord_url

#: Kept well under Discord's documented caps (embed title 256, embed
#: description 4096, message content ~2000, embed field value 1024) --
#: matching `internal/notify/payload.go`'s own defensive margin, not
#: Discord's ceiling exactly, so a near-limit payload never gets the POST
#: rejected.
MAX_TITLE_CHARS = 256
MAX_DESCRIPTION_CHARS = 1900
MAX_CONTENT_CHARS = 1900
MAX_FIELD_NAME_CHARS = 256
MAX_FIELD_VALUE_CHARS = 1024

#: Marks a trimmed field so a truncated value reads as truncated rather
#: than silently cut off mid-word.
ELLIPSIS = "…"


def trim(s: str, limit: int) -> str:
    """Cut *s* to at most *limit* characters, appending `ELLIPSIS` when it
    had to cut. Mirrors `internal/notify/payload.go`'s `trim` (which counts
    runes; Python `str` is already codepoints, so `len` is the rune count
    here too)."""
    if len(s) <= limit:
        return s
    if limit <= len(ELLIPSIS):
        return ELLIPSIS[:limit]
    return s[: limit - len(ELLIPSIS)] + ELLIPSIS


@dataclass(frozen=True)
class NotifyField:
    """One Discord embed field: ``{name, value, inline}``."""

    name: str
    value: str
    inline: bool = False


@dataclass(frozen=True)
class NotifyMessage:
    """Everything a `notify` node's `input` may carry. Every value here
    comes straight from that `input` -- see `mapping.parse_message` for
    the validation that builds one."""

    content: str = ""
    title: str = ""
    description: str = ""
    fields: tuple[NotifyField, ...] = field(default_factory=tuple)


def build_message(raw_url: str, message: NotifyMessage) -> bytes:
    """Shape *message* for delivery to *raw_url*: a Discord embed envelope
    when *raw_url* classifies as a Discord webhook (`is_discord_url`),
    otherwise a generic flat-JSON object any other webhook receiver can
    read without knowing Discord's format."""
    if is_discord_url(raw_url):
        body = _build_discord(message)
    else:
        body = _build_generic(message)
    return json.dumps(body, separators=(",", ":")).encode("utf-8")


def _build_discord(m: NotifyMessage) -> dict[str, Any]:
    body: dict[str, Any] = {}
    if m.content.strip():
        body["content"] = trim(m.content, MAX_CONTENT_CHARS)

    embed: dict[str, Any] = {}
    if m.title.strip():
        embed["title"] = trim(m.title, MAX_TITLE_CHARS)
    if m.description.strip():
        embed["description"] = trim(m.description, MAX_DESCRIPTION_CHARS)
    if m.fields:
        embed["fields"] = [
            {
                "name": trim(f.name, MAX_FIELD_NAME_CHARS),
                "value": trim(f.value, MAX_FIELD_VALUE_CHARS),
                "inline": bool(f.inline),
            }
            for f in m.fields
        ]
    if embed:
        body["embeds"] = [embed]

    # A Discord webhook rejects a message with neither content nor embeds;
    # mapping.parse_message already refuses an all-empty input, but an
    # empty-after-trim edge case (e.g. content = "   ") falls back to an
    # empty content string rather than posting a body Discord would 400 on.
    if not body:
        body["content"] = ""
    return body


def _build_generic(m: NotifyMessage) -> dict[str, Any]:
    return {
        "content": m.content,
        "title": m.title,
        "description": m.description,
        "fields": [{"name": f.name, "value": f.value, "inline": f.inline} for f in m.fields],
    }
