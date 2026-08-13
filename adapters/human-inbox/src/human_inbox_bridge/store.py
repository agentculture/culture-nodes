"""The durable inbox: one JSON file per parked task under the state dir.

This is the module that makes t12's acceptance real: "an invocation POST
parks as 202 with a DURABLE pending task" and "pending tasks survive a
bridge restart". Everything the bridge needs to complete an invocation
later — including the callback URL/token and the per-task event-sequence
counter — is persisted here, because a human may answer days after the
process that accepted the invocation has been restarted. The sibling agent
bridges keep the callback credentials in memory for the lifetime of one
subprocess; this bridge cannot, which is why the state dir is created
`0700` and task files `0600` (they carry a live callback token).

Writes are atomic (temp file + `os.replace`) so a crash mid-write never
leaves a half-written task behind, and a corrupt file is skipped with a
warning rather than taking the whole inbox down with it.
"""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

logger = logging.getLogger("human_inbox_bridge.store")

#: The task lifecycle. `pending` is the only state a submission or a
#: cancellation may act on; both terminal states keep their file (the inbox
#: is also the audit trail of what was asked and answered).
STATUS_PENDING = "pending"
STATUS_COMPLETED = "completed"
STATUS_CANCELLED = "cancelled"


@dataclass
class HumanTask:
    """One parked invocation waiting on a person."""

    invocation_id: str
    status: str = STATUS_PENDING
    created_at: str = ""
    #: What the workflow asked the human to do (input.instruction).
    instruction: str = ""
    run_id: str = ""
    node_run_id: str | None = None
    attempt_id: str | None = None
    #: §13.4 callback destination + credential, persisted because delivery
    #: happens at submission time, possibly after a restart.
    callback_url: str = ""
    callback_token: str = ""
    #: How many §13.4 events have had a sequence number reserved for this
    #: task — persisted so a restart never reuses a sequence.
    events_sent: int = 0
    #: The human's submission `{outcome, output, note, submitted_at}`, once
    #: one has been delivered.
    submission: dict[str, Any] | None = None
    completed_at: str | None = None
    cancelled_at: str | None = None
    #: Extra input fields beyond `instruction`, kept verbatim so the human
    #: surface can show whatever context the workflow attached.
    extra_input: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "HumanTask":
        known = {f for f in cls.__dataclass_fields__}
        return cls(**{k: v for k, v in data.items() if k in known})

    def public_dict(self) -> dict[str, Any]:
        """The task as the human surface shows it — WITHOUT the callback
        URL/token (the credential authenticates the bridge to the control
        plane; no reader of the inbox needs it)."""
        data = self.to_dict()
        data.pop("callback_url", None)
        data.pop("callback_token", None)
        return data


class TaskStore:
    """File-backed task store, safe for one process's concurrent threads
    (the HTTP handler thread and the accepted-event delivery thread both
    reserve sequences and save tasks)."""

    def __init__(self, state_dir: str | Path, *, create: bool = True) -> None:
        self.tasks_dir = Path(state_dir) / "tasks"
        if create:
            self.tasks_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        self._lock = threading.Lock()

    def _path(self, invocation_id: str) -> Path:
        # Invocation ids are bridge-minted (`hit_<hex>`), never caller-
        # supplied paths — but basename-sanitise anyway so a corrupt id can
        # never traverse.
        return self.tasks_dir / f"{Path(invocation_id).name}.json"

    def save(self, task: HumanTask) -> None:
        with self._lock:
            self._save_locked(task)

    def _save_locked(self, task: HumanTask) -> None:
        path = self._path(task.invocation_id)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(task.to_dict(), indent=2), encoding="utf-8")
        os.chmod(tmp, 0o600)  # the file carries a live callback token
        os.replace(tmp, path)

    def get(self, invocation_id: str) -> HumanTask | None:
        with self._lock:
            return self._get_locked(invocation_id)

    def _get_locked(self, invocation_id: str) -> HumanTask | None:
        return self._read(self._path(invocation_id))

    def _read(self, path: Path) -> HumanTask | None:
        try:
            raw = path.read_text(encoding="utf-8")
        except OSError:
            return None
        try:
            data = json.loads(raw)
        except ValueError:
            logger.warning("skipping unparseable task file %s", path)
            return None
        if not isinstance(data, dict) or "invocation_id" not in data:
            logger.warning("skipping malformed task file %s", path)
            return None
        return HumanTask.from_dict(data)

    def list(self, status: str | None = None) -> list[HumanTask]:
        """Every stored task (optionally filtered), oldest first."""
        with self._lock:
            tasks = []
            for path in self.tasks_dir.glob("*.json"):
                task = self._read(path)
                if task is None:
                    continue
                if status is not None and task.status != status:
                    continue
                tasks.append(task)
        tasks.sort(key=lambda t: (t.created_at, t.invocation_id))
        return tasks

    def reserve_sequence(self, invocation_id: str) -> tuple[str, int]:
        """Reserve the next §13.4 event identity for *invocation_id* and
        persist the counter BEFORE the event is sent — a crash between
        reserve and send loses one sequence number, never reuses one.
        """
        with self._lock:
            task = self._get_locked(invocation_id)
            if task is None:
                raise KeyError(f"no such task: {invocation_id}")
            task.events_sent += 1
            self._save_locked(task)
            return f"evt_{invocation_id}_{task.events_sent}", task.events_sent
