"""Discord nudge transport: shell out to the ``discord`` CLI binary.

Stdlib-only Python — no third-party dependencies.  The module is designed
to be *fail-safe*: if the ``discord`` binary is missing, or the config is
incomplete, every public function returns gracefully (``None`` / empty
lists) without raising.  The tracker must never crash because Discord is
down.

Public API
----------
* ``NudgeConfig.from_env()`` — build config from environment variables.
* ``NudgeState`` — persisted in ``task.extra_input["nudge_state"]``.
* ``first_nudge(task, cfg)`` — post initial message + thread + nudge.
* ``escalate_nudge(task, cfg, state)`` — post escalation to the thread.
* ``poll_replies(task, cfg, state)`` — detect new replies and return them.

The caller (tracker) relays reply strings via the bridge's callback as
progress events::

    {"kind": "progress", "payload": {"note": reply_text}}
"""

from __future__ import annotations

import json
import logging
import os
import re
import subprocess
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from typing import Any

logger = logging.getLogger("human_inbox_bridge.nudge")

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

#: Discord embed colour (blue) for the first nudge.
_EMBED_COLOR_BLUE = 3447003
#: Discord embed colour (red) for overdue / escalation.
_EMBED_COLOR_RED = 15158332
#: Username used on the nudge embed.
_EMBED_USERNAME = "Culture Nodes Nudges"
#: Footer text on the nudge embed.
_EMBED_FOOTER_TEXT = "Sent by Culture Nodes"

# Regex to pull a Discord user ID (<@USER_ID>) from text.
_RE_DISCORD_MENTION = re.compile(r"<@(\d+)>")


# ---------------------------------------------------------------------------
# Data classes
# ---------------------------------------------------------------------------


@dataclass
class NudgeConfig:
    """Configuration for the Discord nudge transport.

    All fields are read from environment variables via ``from_env()``.
    Missing values are treated as *no Discord transport* — every public
    function returns gracefully.
    """

    discord_channel_id: int | None = None
    discord_bot_token: str | None = None
    nudge_interval_seconds: float = 3600.0
    global_throttle_seconds: float = 300.0
    escalation_after_seconds: float = 7200.0

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> "NudgeConfig":
        """Build a NudgeConfig from environment variables.

        Missing channel ID or bot token means the Discord transport is
        effectively disabled — callers should check ``is_configured()``.
        """
        env = os.environ if env is None else env

        channel_id_raw = env.get("DISCORD_NUDGE_CHANNEL_ID")
        channel_id: int | None = None
        if channel_id_raw is not None:
            try:
                channel_id = int(channel_id_raw)
            except ValueError:
                logger.warning("DISCORD_NUDGE_CHANNEL_ID is not an integer; disabling Discord")

        bot_token = env.get("DISCORD_NUDGE_BOT_TOKEN", "").strip() or None

        nudge_interval = _parse_float_env(env, "DISCORD_NUDGE_INTERVAL_SECONDS", 3600.0)
        global_throttle = _parse_float_env(env, "DISCORD_NUDGE_GLOBAL_THROTTLE_SECONDS", 300.0)
        escalation_after = _parse_float_env(env, "DISCORD_NUDGE_ESCALATION_AFTER_SECONDS", 7200.0)

        return cls(
            discord_channel_id=channel_id,
            discord_bot_token=bot_token,
            nudge_interval_seconds=nudge_interval,
            global_throttle_seconds=global_throttle,
            escalation_after_seconds=escalation_after,
        )

    def is_configured(self) -> bool:
        """Return True when both channel ID and bot token are present."""
        return self.discord_channel_id is not None and self.discord_bot_token is not None


@dataclass
class NudgeState:
    """Mutable state for one nudge cycle, persisted in ``task.extra_input``.

    Stored under the key ``"nudge_state"`` inside
    ``task.extra_input``.
    """

    thread_id: str | None = None
    last_nudge_at: str | None = None
    last_seen_message_id: str | None = None
    escalation_level: int = 0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "NudgeState":
        known = {f for f in cls.__dataclass_fields__}
        return cls(**{k: v for k, v in data.items() if k in known})

    @classmethod
    def load(cls, task: Any) -> "NudgeState":
        """Load state from ``task.extra_input["nudge_state"]``."""
        raw = task.extra_input.get("nudge_state")
        if raw is None:
            return cls()
        if isinstance(raw, dict):
            return cls.from_dict(raw)
        return cls()

    def save(self, task: Any) -> None:
        """Persist state back into ``task.extra_input["nudge_state"]``."""
        task.extra_input["nudge_state"] = self.to_dict()


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------


def _parse_float_env(env: dict[str, str], name: str, default: float) -> float:
    raw = env.get(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError:
        logger.warning("%s=%r is not a number; using default %.1f", name, raw, default)
        return default


def _run_discord(argv: list[str]) -> subprocess.CompletedProcess[bytes] | None:
    """Run the ``discord`` CLI binary with *argv* and return the result.

    Returns ``None`` when the binary is not found or the channel/token is
    missing, so callers never need to handle exceptions.
    """
    # The caller builds argv with all required flags (--channel-id, --token,
    # etc.).  We simply execute and return the result.
    try:
        result = subprocess.run(
            argv,
            capture_output=True,
            timeout=30,
        )
    except FileNotFoundError:
        logger.debug("discord CLI not found on PATH")
        return None
    except subprocess.TimeoutExpired:
        logger.warning("discord CLI timed out after 30 s")
        return None
    except OSError as exc:
        logger.warning("discord CLI failed: %s", exc)
        return None
    return result


def _discord_json_output(argv: list[str]) -> dict[str, Any] | None:
    """Run ``discord`` and parse its ``--json`` output.

    Returns the parsed JSON dict on success, or ``None`` on any failure.
    """
    result = _run_discord(argv)
    if result is None:
        return None
    if result.returncode != 0:
        stderr = result.stderr.decode("utf-8", errors="replace").strip()
        logger.warning("discord CLI returned %d: %s", result.returncode, stderr)
        return None
    raw = result.stdout.decode("utf-8", errors="replace").strip()
    if not raw:
        return None
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as exc:
        logger.warning("discord CLI produced non-JSON output: %s", exc)
        return None
    if not isinstance(data, dict):
        logger.warning("discord CLI output is not a JSON object")
        return None
    return data


def _post_message(channel_id: int, content: str, token: str) -> str | None:
    """Post a message to *channel_id* and return the message ID.

    Returns ``None`` on any failure.
    """
    argv = [
        "discord",
        "message",
        "post",
        "--channel-id",
        str(channel_id),
        "--token",
        token,
        "--content",
        content,
        "--json",
    ]
    data = _discord_json_output(argv)
    if data is None:
        return None
    return data.get("id")


def _create_thread(channel_id: int, message_id: str, name: str, token: str) -> str | None:
    """Create a thread anchored to *message_id* in *channel_id*.

    Returns the thread (channel) ID on success, ``None`` otherwise.
    """
    argv = [
        "discord",
        "thread",
        "create",
        "--channel-id",
        str(channel_id),
        "--message-id",
        message_id,
        "--name",
        name,
        "--token",
        token,
        "--json",
    ]
    data = _discord_json_output(argv)
    if data is None:
        return None
    return data.get("id")


def _post_to_thread(thread_id: str, content: str, token: str) -> str | None:
    """Post a message to *thread_id* and return the message ID.

    Returns ``None`` on any failure.
    """
    argv = [
        "discord",
        "thread",
        "post",
        "--thread-id",
        thread_id,
        "--token",
        token,
        "--content",
        content,
        "--json",
    ]
    data = _discord_json_output(argv)
    if data is None:
        return None
    return data.get("id")


def _poll_thread(thread_id: str, limit: int = 20, token: str | None = None) -> list[dict[str, Any]]:
    """Read messages from *thread_id*.

    Returns a list of dicts with keys ``id``, ``content``, and
    ``author`` (itself a dict with ``id`` and ``username``).
    """
    argv = [
        "discord",
        "channel",
        "messages",
        "--channel-id",
        thread_id,
        "--limit",
        str(limit),
        "--json",
    ]
    if token is not None:
        argv.insert(4, "--token")
        argv.insert(5, token)
    data = _discord_json_output(argv)
    if data is None:
        return []
    messages = data if isinstance(data, list) else data.get("messages", [])
    if not isinstance(messages, list):
        return []
    result = []
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        author = msg.get("author", {})
        if not isinstance(author, dict):
            author = {}
        result.append(
            {
                "id": msg.get("id", ""),
                "content": msg.get("content", ""),
                "author": {
                    "id": author.get("id", ""),
                    "username": author.get("username", ""),
                },
            }
        )
    return result


def _resolve_user_id(task: Any) -> str | None:
    """Look up a Discord user ID from *task*.

    Checks ``task.extra_input["discord_user_id"]`` first, then falls back
    to parsing the instruction for a ``<@USER_ID>`` mention.  Returns
    ``None`` if not found.
    """
    # Direct lookup from extra_input.
    raw = task.extra_input.get("discord_user_id")
    if isinstance(raw, str) and raw.strip():
        return raw.strip()

    # Fallback: parse the instruction for a Discord mention.
    instruction = task.instruction if hasattr(task, "instruction") else ""
    if isinstance(instruction, str):
        match = _RE_DISCORD_MENTION.search(instruction)
        if match:
            return match.group(1)

    return None


def _build_embed(content: str, *, color: int, is_overdue: bool = False) -> str:
    """Build an embed JSON string for the Discord CLI content field.

    The embed is embedded inside a JSON object that the ``discord`` CLI
    expects as the ``content`` value.
    """
    embed = {
        "title": "Culture Nodes Nudge" if not is_overdue else "Overdue Task",
        "description": content,
        "color": color,
        "footer": {
            "text": _EMBED_FOOTER_TEXT,
        },
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }
    return json.dumps(embed)


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def first_nudge(task: Any, cfg: NudgeConfig) -> NudgeState | None:
    """Post the first nudge: message → thread → nudge mention.

    Returns a ``NudgeState`` on success, or ``None`` if the config is
    incomplete or the Discord CLI fails.
    """
    if not cfg.is_configured():
        logger.debug("Discord not configured; skipping first_nudge")
        return None

    user_id = _resolve_user_id(task)
    channel_id = cfg.discord_channel_id  # type: ignore[union-attr]
    token = cfg.discord_bot_token  # type: ignore[union-attr]

    # 1. Post the initial message in the channel.
    initial_content = _build_embed(
        "A new task has been assigned. Check your thread for details.",
        color=_EMBED_COLOR_BLUE,
    )
    message_id = _post_message(channel_id, initial_content, token)
    if message_id is None:
        logger.warning("first_nudge: failed to post initial message")
        return None

    # 2. Create a thread anchored to that message.
    thread_name = f"Nudge: {task.invocation_id if hasattr(task, 'invocation_id') else 'task'}"
    thread_id = _create_thread(channel_id, message_id, thread_name, token)
    if thread_id is None:
        logger.warning("first_nudge: failed to create thread")
        return None

    # 3. Post the nudge mention into the thread.
    mention = f"<@{user_id}>" if user_id else "@here"
    nudge_content = _build_embed(
        f"New task assigned. {mention} — please review and complete.",
        color=_EMBED_COLOR_BLUE,
    )
    _post_to_thread(thread_id, nudge_content, token)

    # 4. Build and persist state.
    state = NudgeState(
        thread_id=thread_id,
        last_nudge_at=datetime.now(timezone.utc).isoformat(),
        last_seen_message_id=None,
        escalation_level=0,
    )
    state.save(task)
    return state


def escalate_nudge(task: Any, cfg: NudgeConfig, state: NudgeState) -> NudgeState | None:
    """Post an escalation message to the existing thread.

    Uses a red embed to signal urgency.  Returns the updated ``NudgeState``
    on success, or ``None`` on failure.
    """
    if not cfg.is_configured():
        logger.debug("Discord not configured; skipping escalate_nudge")
        return None

    if state.thread_id is None:
        logger.warning("escalate_nudge: no thread_id in state")
        return None

    token = cfg.discord_bot_token  # type: ignore[union-attr]
    user_id = _resolve_user_id(task)
    mention = f"<@{user_id}>" if user_id else "@here"

    escalation_content = _build_embed(
        f"Task is overdue. {mention} — please act now.",
        color=_EMBED_COLOR_RED,
        is_overdue=True,
    )
    _post_to_thread(state.thread_id, escalation_content, token)

    state.escalation_level += 1
    state.last_nudge_at = datetime.now(timezone.utc).isoformat()
    state.save(task)
    return state


def poll_replies(task: Any, cfg: NudgeConfig, state: NudgeState) -> list[str]:
    """Poll the thread for new replies since ``last_seen_message_id``.

    Returns a list of reply content strings.  Updates
    ``state.last_seen_message_id`` to the newest message ID seen.
    """
    if not cfg.is_configured():
        logger.debug("Discord not configured; skipping poll_replies")
        return []

    if state.thread_id is None:
        logger.debug("poll_replies: no thread_id in state")
        return []

    token = cfg.discord_bot_token  # type: ignore[union-attr]
    messages = _poll_thread(state.thread_id, limit=20, token=token)
    if not messages:
        return []

    last_seen = state.last_seen_message_id
    new_contents: list[str] = []
    newest_id: str | None = None

    for msg in messages:
        msg_id = msg.get("id", "")
        if not msg_id:
            continue
        # Track the newest message ID overall.
        if newest_id is None or msg_id > newest_id:
            newest_id = msg_id
        # Skip messages we have already seen.
        if last_seen is not None and msg_id <= last_seen:
            continue
        content = msg.get("content", "")
        if content:
            new_contents.append(content)

    # Update state with the newest message ID.
    if newest_id is not None:
        state.last_seen_message_id = newest_id
        state.save(task)

    return new_contents
