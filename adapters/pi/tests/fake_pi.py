#!/usr/bin/env python3
"""Executable fake pi: replay a fixture, optionally with a child sleeper."""

import os
import signal
import subprocess
import sys
import time
from pathlib import Path


def main() -> int:
    argv_file = os.environ.get("FAKE_PI_ARGV_FILE")
    if argv_file:
        Path(argv_file).write_text("\n".join(sys.argv[1:]), encoding="utf-8")
    if os.environ.get("FAKE_PI_IGNORE_SIGTERM") == "1":
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    fixture = Path(os.environ["FAKE_PI_FIXTURE"])
    pid_file = os.environ.get("FAKE_PI_CHILD_PID_FILE")
    if os.environ.get("FAKE_PI_SLEEP_CHILD") == "1":
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3600)"])
        Path(pid_file).write_text(str(child.pid), encoding="utf-8")
    elif pid_file:
        # No separate child requested — record this process's own pid, so a
        # test can confirm the fake itself survives a SIGTERM it ignores.
        Path(pid_file).write_text(str(os.getpid()), encoding="utf-8")
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
