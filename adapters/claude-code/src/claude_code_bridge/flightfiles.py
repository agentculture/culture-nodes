"""This bridge's own file-based status-polling + cooperative-stop control
plane for an asynchronous `claude -p` invocation.

Deliberately named to match `adapters/colleague/src/colleague_bridge/
flightfiles.py` — the pattern it mirrors is the same (a tailable JSONL feed
file, plus a small JSON control file a poller checks for a stop request) —
but the mechanics differ in one load-bearing way, documented here so a
reader coming from the colleague adapter does not assume more parity than
exists:

colleague's flight files are written and read by *colleague's own engine
loop*, which polls its `.control.json` file at turn boundaries on its own
initiative — the bridge is just one more file-polling client speaking a
protocol colleague already implements. The `claude` CLI has no such external
control plane: there is nothing in headless print/stream-json mode that
polls a file for a stop request. This bridge supplies BOTH sides of that
contract itself:

* `spawn_background` (`claude_cli.py`) redirects the detached child's own
  `--output-format stream-json` stdout directly into `feed_path()` — so the
  "feed" here is the child's raw stdout, not something an engine writes
  purpose-built for this file.
* `async_runner.py`'s poller is the ONLY reader of `control_path()`; when it
  observes `{"stop": true}`, IT is what turns that into a SIGTERM sent to
  the child pid — the file itself never reaches the claude process.

Scoped under `Config.state_dir` (the bridge's own bookkeeping directory)
rather than repo-local like colleague's `<repo>/.colleague/flight/`: claude
has no native `.claude/flight/`-shaped convention of its own to sit beside,
so there is no reason to write bridge-internal state into the target repo's
working tree.
"""

from __future__ import annotations

import json
from pathlib import Path


def flight_dir(state_dir: str | Path) -> Path:
    return Path(state_dir) / "flight"


def feed_path(state_dir: str | Path, invocation_id: str) -> Path:
    return flight_dir(state_dir) / f"{invocation_id}.feed.jsonl"


def control_path(state_dir: str | Path, invocation_id: str) -> Path:
    return flight_dir(state_dir) / f"{invocation_id}.control.json"


class FeedTail:
    """Tracks how much of one invocation's feed file has already been read.

    The feed file is append-only for the lifetime of one invocation (the
    detached claude process's own stdout, redirected there at spawn time),
    so tailing it is "remember the byte offset already consumed and read
    past it" — identical in spirit to `colleague_bridge.flightfiles.FeedTail`.
    """

    def __init__(self, state_dir: str | Path, invocation_id: str) -> None:
        self._path = feed_path(state_dir, invocation_id)
        self._offset = 0

    def read_new_records(self) -> list[dict]:
        """Return every complete JSON object appended since the last call.

        Absent file (not armed yet) reads back as no new records — never an
        error. A trailing partial line (a write still in flight) is left for
        the next call rather than guessed at.
        """
        try:
            with open(self._path, "rb") as f:
                f.seek(self._offset)
                chunk = f.read()
        except OSError:
            return []
        if not chunk:
            return []
        text = chunk.decode("utf-8", errors="replace")
        consumed = 0
        records: list[dict] = []
        for line in text.splitlines(keepends=True):
            if not line.endswith("\n"):
                break  # partial trailing line: leave it for next read
            consumed += len(line.encode("utf-8"))
            stripped = line.strip()
            if not stripped:
                continue
            try:
                record = json.loads(stripped)
            except ValueError:
                continue
            if isinstance(record, dict):
                records.append(record)
        self._offset += consumed
        return records


def write_stop(state_dir: str | Path, invocation_id: str) -> None:
    """Set ``stop: true`` in the control file.

    Best-effort and idempotent: safe to call more than once (a redelivered
    cancellation), and safe to call after the invocation has already
    finished — cancellation is cooperative and best-effort by protocol
    design (PRD §13.6), never a hard failure at the bridge.
    """
    cp = control_path(state_dir, invocation_id)
    cp.parent.mkdir(parents=True, exist_ok=True)
    data: dict = {}
    if cp.exists():
        try:
            loaded = json.loads(cp.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                data = loaded
        except (OSError, ValueError):
            data = {}
    data["stop"] = True
    data.setdefault("guidance", [])
    cp.write_text(json.dumps(data), encoding="utf-8")


def stop_requested(state_dir: str | Path, invocation_id: str) -> bool:
    """True iff `write_stop` has been called for *invocation_id* and the
    control file is still readable as such. Absent/corrupt file reads back
    as False — never an error; a poller checks this every cycle and must
    never crash the invocation it is trying to service over a bookkeeping
    file it can't parse."""
    cp = control_path(state_dir, invocation_id)
    try:
        data = json.loads(cp.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    return bool(isinstance(data, dict) and data.get("stop") is True)
