#!/usr/bin/env python3
"""Executable fake pi: replay a fixture, optionally with a child sleeper."""

import os
import subprocess
import sys
import time
from pathlib import Path


def main() -> int:
    fixture = Path(os.environ["FAKE_PI_FIXTURE"])
    if os.environ.get("FAKE_PI_SLEEP_CHILD") == "1":
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3600)"])
        Path(os.environ["FAKE_PI_CHILD_PID_FILE"]).write_text(str(child.pid), encoding="utf-8")
    delay = float(os.environ.get("FAKE_PI_DELAY", "0"))
    for line in fixture.read_text(encoding="utf-8").splitlines():
        if line.startswith("#"):
            continue
        print(line, flush=True)
        if delay:
            time.sleep(delay)
    if os.environ.get("FAKE_PI_HANG") == "1":
        time.sleep(3600)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
