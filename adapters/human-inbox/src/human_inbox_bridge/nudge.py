"""Discord nudge transport for parked human-actor tasks.

Shells out to the ``discord`` CLI binary (discord-bot-cli) to create
threads, post nudges with <@user_id> mentions, and poll for replies.
All Discord interaction is one-shot and poll-based — there is no
gateway listener, so the tracker owns the cadence.

Stdlib-only: no PyPI dependencies.  Missing config (no bot token, no
channel, no assignee) disables nudging quietly — never crashes the
tracker, never blocks the observe/auto-submit path.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import time
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger("human_inbox_bridge.nudge")

# --- Escalation vocabulary (lifted from steward's discord-notify) ------

#: Gentle nudge colour (blue).
_COLOR_GENTLE = 3447003
#: Overdue / escalation colour (red).
_COLOR_OVERDUE = 15158332

#: Default username for nudge embeds.
_DEFAULT_USERNAME = "Culture Nodes Nudges"


@dataclass(frozen=True)
class NudgeConfig:
    """Configuration for the Discord nudge transport.

    All fields are optional — when absent, nudging is silently disabled.
    """

    channel_id: str = ""
    bot_token: str = ""
    interval_seconds: float = 300.0
    global_throttle_seconds: float = 10.0
    escalation_after_seconds: float = 600.0

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "NudgeConfig | None":
        """Return a config when both channel and token are present, else None."""
        env = os.environ if env is None else env
        channel_id = env.get("DISCORD_NUDGE_CHANNEL_ID", "").strip()
        bot_token = env.get("DISCORD_NUDGE_BOT_TOKEN", "").strip()
        if not channel_id or not bot_token:
            return None
        return cls(
            channel_id=channel_id,
            bot_token=bot_token,
            interval_seconds=_float_env(env, "DISCORD_NUDGE_INTERVAL_SECONDS", 300.0),
            global_throttle_seconds=_float_env(env, "DISCORD_NUDGE_GLOBAL_THROTTLE_SECONDS", 10.0),
            escalation_after_seconds=_float_env(
                env, "DISCORD_NUDGE_ESCALATION_AFTER_SECONDS", 600.0
            ),
        )

    @property
    def enabled(self) -> bool:
        return bool(self.channel_id and self.bot_token)


@dataclass
class NudgeState:
    """Durable state for one task's Discord thread.

    Persisted in the task's ``nudge_state`` field on disk.
    """

    thread_id: str = ""
    last_nudge_at: float = 0.0
    last_seen_message_id: str = ""
    escalation_level: int = 0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "NudgeState":
        known = {f for f in cls.__dataclass_fields__}
        return cls(**{k: v for k, v in data.items() if k in known})


# --- Discord CLI wrapper ------------------------------------------------

_DISCORD_BIN = "discord"


def _run_discord(argv: list[str]) -> subprocess.CompletedProcess[str]:
    """Run the ``discord`` CLI binary with ``--json``.

    Never raises. A missing binary, a permission error or a timeout is an
    ABSENT nudge transport, not a tracker failure: nudging is an optional
    courtesy layered on top of the observe/auto-submit path, and that path
    must keep working on a host where discord-bot-cli was never installed.
    Every failure shape is folded into a non-zero CompletedProcess so the
    single caller-side check (`returncode != 0`) covers them all.
    """
    try:
        return subprocess.run(
            [_DISCORD_BIN] + argv + ["--json"],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except FileNotFoundError:
        logger.debug("discord CLI %r not found — nudging disabled", _DISCORD_BIN)
        return subprocess.CompletedProcess(argv, 127, "", "discord CLI not found")
    except subprocess.TimeoutExpired:
        logger.debug("discord CLI timed out")
        return subprocess.CompletedProcess(argv, 124, "", "discord CLI timed out")
    except OSError as exc:  # not executable, bad interpreter, resource limits
        logger.debug("discord CLI could not be executed: %s", exc)
        return subprocess.CompletedProcess(argv, 126, "", str(exc))


def _parse_json_output(result: subprocess.CompletedProcess[str]) -> dict[str, Any] | None:
    """Parse the ``--json`` output; return None on failure."""
    if result.returncode != 0:
        logger.debug("discord CLI exited %d: %s", result.returncode, result.stderr.strip())
        return None
    try:
        return json.loads(result.stdout)
    except (json.JSONDecodeError, ValueError):
        logger.debug("discord CLI produced non-JSON output: %s", result.stdout[:200])
        return None


# --- Public API ---------------------------------------------------------


def first_nudge(
    *,
    channel_id: str,
    bot_token: str,
    instruction: str,
    callback_url: str,
    callback_token: str,
    invocation_id: str,
) -> dict[str, Any] | None:
    """Create a thread and post the first nudge message.

    Returns a dict with ``thread_id`` and ``message_id`` on success,
    or ``None`` when the Discord CLI is unavailable or config is missing.
    """
    if not channel_id or not bot_token:
        return None

    # 1. Post an initial message to the channel.
    msg_result = _post_message(channel_id, instruction)
    if msg_result is None:
        return None
    message_id = msg_result.get("id", "")
    if not message_id:
        return None

    # 2. Create a thread anchored to that message.
    thread_result = _create_thread(channel_id, message_id, f"nudge: {invocation_id}")
    if thread_result is None:
        return None
    thread_id = thread_result.get("id", "")
    if not thread_id:
        return None

    # 3. Post the nudge into the thread with a mention.
    mention = _resolve_mention(invocation_id)
    nudge_content = _build_nudge_content(instruction, mention, escalation_level=0)
    post_result = _post_to_thread(thread_id, nudge_content)
    if post_result is None:
        return None

    return {
        "thread_id": thread_id,
        "message_id": post_result.get("id", ""),
    }


def nudge(
    *,
    channel_id: str,
    bot_token: str,
    thread_id: str,
    instruction: str,
    callback_url: str,
    callback_token: str,
    invocation_id: str,
) -> dict[str, Any] | None:
    """Post a cadence nudge into an existing thread.

    Returns a dict with ``message_id`` on success, or ``None``.
    """
    if not channel_id or not bot_token or not thread_id:
        return None

    mention = _resolve_mention(invocation_id)
    content = _build_nudge_content(instruction, mention, escalation_level=0)
    post_result = _post_to_thread(thread_id, content)
    if post_result is None:
        return None

    return {"message_id": post_result.get("id", "")}


def escalate(
    *,
    channel_id: str,
    bot_token: str,
    thread_id: str,
    instruction: str,
    callback_url: str,
    callback_token: str,
    invocation_id: str,
    escalation_level: int,
) -> dict[str, Any] | None:
    """Post an escalation nudge into an existing thread.

    Uses a red embed (overdue colour) and a higher-urgency message.
    Returns a dict with ``message_id`` on success, or ``None``.
    """
    if not channel_id or not bot_token or not thread_id:
        return None

    mention = _resolve_mention(invocation_id)
    content = _build_nudge_content(instruction, mention, escalation_level=escalation_level)
    post_result = _post_to_thread(thread_id, content)
    if post_result is None:
        return None

    return {"message_id": post_result.get("id", "")}


def poll_replies(
    *,
    channel_id: str,
    bot_token: str,
    thread_id: str,
    last_message_id: str = "",
) -> list[dict[str, Any]]:
    """Poll a thread for new messages since *last_message_id*.

    Returns a list of dicts with ``message_id``, ``content``, and
    ``author`` keys for each new reply.  Returns an empty list when
    the Discord CLI is unavailable or no new messages exist.
    """
    if not channel_id or not bot_token or not thread_id:
        return []

    result = _run_discord(["channel", "messages", thread_id, "--limit", "20"])
    parsed = _parse_json_output(result)
    if parsed is None:
        return []

    messages = parsed if isinstance(parsed, list) else parsed.get("messages", [])
    if not isinstance(messages, list):
        return []

    replies: list[dict[str, Any]] = []
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        msg_id = msg.get("id", "")
        if not msg_id:
            continue
        # Skip the message we already saw.
        if msg_id == last_message_id:
            continue
        # Only include actual replies (not system messages).
        author = msg.get("author", {})
        if isinstance(author, dict) and author.get("bot", False):
            continue
        content = msg.get("content", "")
        if not content:
            continue
        replies.append(
            {
                "message_id": msg_id,
                "content": content,
                "author": {
                    "id": author.get("id", ""),
                    "username": author.get("username", ""),
                },
            }
        )

    # Discord returns newest-first; reverse so the caller sees oldest first.
    replies.reverse()
    return replies


# --- Internal helpers ---------------------------------------------------


def _post_message(channel_id: str, content: str) -> dict[str, Any] | None:
    """Post a message to a channel. Returns the message dict or None."""
    result = _run_discord(["message", "post", channel_id, content])
    return _parse_json_output(result)


def _create_thread(channel_id: str, message_id: str, name: str) -> dict[str, Any] | None:
    """Create a thread anchored to a message. Returns the thread dict or None."""
    result = _run_discord(["thread", "create", channel_id, "--name", name, "--message", message_id])
    return _parse_json_output(result)


def _post_to_thread(thread_id: str, content: str) -> dict[str, Any] | None:
    """Post a message into a thread. Returns the message dict or None."""
    result = _run_discord(["thread", "post", thread_id, content])
    return _parse_json_output(result)


def _resolve_mention(invocation_id: str) -> str:
    """Return a Discord <@user_id> mention or empty string.

    Looks for ``discord_user_id`` in the invocation context.  Falls back
    to empty string when no user is configured — the nudge still posts
    but without a mention.
    """
    # The invocation_id is the only context we have here; the caller
    # should pass the user_id as part of the invocation context.
    # We look for a user_id in the environment or return empty.
    user_id = os.environ.get("DISCORD_NUDGE_USER_ID", "").strip()
    if user_id:
        return f"<@{user_id}>"
    return ""


def _build_nudge_content(instruction: str, mention: str, *, escalation_level: int) -> str:
    """Build the nudge message content with embed payload.

    The content is a JSON string that the discord CLI will post as
    the message body.  It includes an embed with severity colour,
    username, footer, and ISO timestamp — lifted from steward's
    discord-notify skill.
    """
    color = _COLOR_OVERDUE if escalation_level > 0 else _COLOR_GENTLE
    title = "Overdue" if escalation_level > 0 else "Nudge"
    footer_text = f"Escalation level {escalation_level} — {DEFAULT_USERNAME}"
    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    # Truncate instruction to fit in a reasonable message.
    truncated = instruction[:400] + ("..." if len(instruction) > 400 else "")

    embed = {
        "title": f"{title}: {truncated}",
        "description": f"Please respond to this task: {truncated}",
        "color": color,
        "footer": {"text": footer_text},
        "timestamp": timestamp,
    }

    return json.dumps(
        {
            "content": f"{mention} {title} — {truncated}",
            "embeds": [embed],
        }
    )


# --- Env helpers --------------------------------------------------------


def _float_env(env: dict[str, str], name: str, default: float) -> float:
    raw = env.get(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


# --- Re-export for tracker integration ----------------------------------

from dataclasses import asdict
from datetime import datetime, timezone

DEFAULT_USERNAME = _DEFAULT_USERNAME
