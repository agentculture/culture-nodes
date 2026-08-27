"""The ACP client seam for Qwen Code (plan task t2 of qwen-bridge-acp).

qwen-bridge drives Qwen Code over ACP (`qwen --acp`, stdio JSON-RPC):
the bridge acts as the ACP client, Qwen Code as the agent. The seam is
a small package of focused modules, split from the monolithic t2 draft
that hit the repo-wide 1000-line hard limit (tests/lint/
filelength_test.go) - every module here stays well under it, and the
PUBLIC surface stays a single importable module:

    qwen_bridge.qwen_cli          the thin facade (the ONE path the
                                  tests and plan t5's entrypoint
                                  import; its __main__ block runs the
                                  driver child)
    qwen_bridge.acp               this package:
      wire.py         pinned ACP wire contract (the c19 constants:
                      protocolVersion 1, agentInfo qwen-code 0.22.0,
                      the method/stopReason/mode vocabularies, the
                      driver's stderr markers)
      errors.py       the seam's exception vocabulary (the 503-mapped
                      SpawnError family, the gate's AcpPolicyError,
                      the driver's DriverTerminated)
      facts.py        SeamFacts - the measured handshake/session facts
                      t3's capability surface consumes BY VALUE
      probe.py        the qwen binary probe + boot refusal (#113 leg,
                      t3's h5: known install paths, never the
                      non-interactive PATH)
      gate.py         the pre-serve gates: the initialize handshake
                      (c19/h16) and the session mode policy (c18/h15)
      classifier.py   parse_session - the terminal-event classifier
                      (c4/c16/h3/h13): end_turn -> ok, cancelled ->
                      the 13 cancellation outcome, JSON-RPC error ->
                      error, no terminal -> incomplete, never ok;
                      plus the c21 downsampled transcript
      transport.py    the stdio JSON-RPC channel: the transcript echo,
                      the local transcript file (c21), and the v1
                      client obligations (lenient unknown-ext answers,
                      fail-closed request_permission, no
                      fs/terminal handlers)
      driver.py       the driver CHILD (python -m qwen_bridge.qwen_cli):
                      one qwen --acp process per invocation (c17), the
                      initialize -> session/new -> set_mode -> prompt
                      turn, the cooperative SIGTERM session/cancel
      dispatch.py     the parent side: spawn / run_sync / SyncRunResult
                      / _driver_argv - the boundary the t1-ported core
                      (server.py, async_runner.py) reads

The import graph is flat: every module imports wire/errors (leaves) and
never the driver or the facade, so `python -m qwen_bridge.qwen_cli` in
the child process re-imports the package cleanly.
"""
