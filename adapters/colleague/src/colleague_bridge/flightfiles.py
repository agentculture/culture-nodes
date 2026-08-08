"""Pure-stdlib mirror of colleague's flight-control-plane file conventions.

The bridge does not import the `colleague` Python package (it is a separate,
stdlib-only deliverable that only ever shells out to the `colleague`
binary — see the package docstring), so it cannot call
`colleague.flight.write_stop` directly. What it *can* do, because the
convention is documented and frozen (`docs/features/flight.md`, pinned
against `/home/spark/git/colleague/docs/contract.md`), is speak the same two
files a `colleague flight` client would:

* ``<repo>/.colleague/flight/<task_id>.feed.jsonl`` — one JSON record per
  turn boundary, appended by the colleague loop.
* ``<repo>/.colleague/flight/<task_id>.control.json`` — ``{"stop": bool,
  "guidance": [str, ...]}``, read by the loop at the next turn boundary.

This module only ever *reads* the feed and *writes* the control file
cooperatively (never touches a process, never signals anything) — matching
the contract's own description of the control plane as file-polling, not a
live socket.
"""

from __future__ import annotations

import json
from pathlib import Path


def flight_dir(repo_path: str | Path) -> Path:
    return Path(repo_path) / ".colleague" / "flight"


def feed_path(repo_path: str | Path, task_id: str) -> Path:
    return flight_dir(repo_path) / f"{task_id}.feed.jsonl"


def control_path(repo_path: str | Path, task_id: str) -> Path:
    return flight_dir(repo_path) / f"{task_id}.control.json"


class FeedTail:
    """Tracks how much of one flight feed file has already been read.

    A feed file is append-only for the lifetime of one work item and is
    reaped at finish, so tailing it is just "remember the byte offset
    already consumed and read past it" — no inotify, no polling library.
    """

    def __init__(self, repo_path: str | Path, task_id: str) -> None:
        self._path = feed_path(repo_path, task_id)
        self._offset = 0

    def read_new_records(self) -> list[dict]:
        """Return every complete JSON record appended since the last call.

        Absent file (not armed yet, or already reaped) reads back as no new
        records — never an error, matching the rest of the flight
        convention's degrade stance. A trailing partial line (a write still
        in flight) is left for the next call rather than guessed at.
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
        # Only advance the offset past whole lines: a write mid-line must be
        # re-read in full next time, not split.
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


def write_stop(repo_path: str | Path, task_id: str) -> None:
    """Set ``stop: true`` in the control file, preserving existing guidance.

    Best-effort and idempotent: safe to call more than once (e.g. a
    redelivered cancellation), and safe to call after the work item has
    already finished (the loop is long gone; this just leaves a stray file
    the flight plane's own reap sweeps up later) — cancellation is
    cooperative and best-effort by protocol design (PRD §13.6), never a
    hard failure at the bridge.
    """
    cp = control_path(repo_path, task_id)
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
