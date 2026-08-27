"""The two links that were built at both ends and never joined (#227), and
the refusal message the async path used to drop (#225).

These are regression tests in the strict sense: every helper they exercise
already existed and was already unit-tested. What was missing was any test
that the CALLERS use them, which is exactly how a bridge could ship a
`--mode` argv builder, a mode gate, and a `mode=` kwarg on `spawn`, and still
be unable to execute a single dispatch.
"""

from __future__ import annotations

import inspect
import io

from qwen_bridge import async_runner, qwen_cli, server
from qwen_bridge.acp import dispatch, wire

# --------------------------------------------------------------------------
# #227 - the mode reaches the driver
# --------------------------------------------------------------------------


def test_async_runner_start_requires_a_mode():
    """`start()` grew `mode` as a REQUIRED keyword, not an optional one.

    Optional-with-a-None-default is precisely the shape that broke: `spawn`
    already declared `mode: str | None = None`, so omitting it at the call
    site was silently legal and produced `--mode ""` on every dispatch.
    """
    params = inspect.signature(async_runner.AsyncRunner.start).parameters
    assert "mode" in params, "AsyncRunner.start must take a mode"
    assert params["mode"].default is inspect.Parameter.empty, (
        "mode must be required: a default is how the omission hid for a whole "
        "adapter's lifetime (#227)"
    )


def test_async_runner_forwards_mode_to_spawn():
    """The forwarding itself, not merely the signature.

    Asserted on the source rather than by driving a real `start()`: that call
    needs a live callback receiver, a session registry and a workspace handle,
    and a test built on all three would fail for a dozen reasons unrelated to
    the one line this guards. The narrow check is the honest one here.
    """
    source = inspect.getsource(async_runner.AsyncRunner.start)
    assert "mode=mode" in source, (
        "start() must pass mode= through to qwen_cli.spawn; the whole defect was " "that it did not"
    )


def test_server_reads_mode_from_the_invocation_input():
    """`server.py` parses eleven input fields. `mode` was not one of them, so
    `input.mode` was accepted by the API and silently discarded."""
    source = inspect.getsource(server)
    assert 'raw_input.get("mode")' in source, (
        "the invoke handler must read input.mode; without this the field is "
        "accepted over the wire and dropped (#227)"
    )
    assert "qwen_cli.ACP_MODES" in source, (
        "an out-of-vocabulary mode should be refused at the input boundary with "
        "a legible 400, not at the driver whose refusal the async path drops"
    )


# --------------------------------------------------------------------------
# #225 - a refusal keeps its message on the async path
# --------------------------------------------------------------------------


def test_refusal_detail_reads_the_marker():
    detail = dispatch.refusal_detail(
        f"some preamble\n{wire.REFUSAL_MARKER} qwen-acp-mode: no session mode given\n"
    )
    assert detail == "qwen-acp-mode: no session mode given"


def test_refusal_detail_is_none_without_a_marker():
    assert dispatch.refusal_detail("just ordinary stderr chatter\n") is None
    assert dispatch.refusal_detail("") is None
    assert dispatch.refusal_detail(None) is None


def test_both_dispatch_paths_share_one_refusal_parser():
    """Sync had the regex; async had nothing. Two implementations would let
    them drift about what a refusal even is."""
    assert dispatch.refusal_detail is qwen_cli.refusal_detail
    assert "refusal_detail(stderr)" in inspect.getsource(dispatch.run_sync)


def test_stderr_reader_drains_into_its_sink():
    """The drain exists for two reasons and the second one is a deadlock: an
    unread PIPE has a finite kernel buffer, so a session chatty on stderr
    would block the child while this runner waited on a stdout EOF that could
    never arrive."""
    sink: list[str] = []
    async_runner._stderr_reader(io.StringIO("one\ntwo\n"), sink)
    assert "".join(sink) == "one\ntwo\n"


def test_stderr_reader_survives_a_closed_pipe():
    closed = io.StringIO("x\n")
    closed.close()
    sink: list[str] = []
    async_runner._stderr_reader(closed, sink)  # must not raise
    assert sink == []


def test_async_refusal_is_classed_as_policy_not_execution():
    """A refusal is the bridge working correctly. Reporting it as an execution
    failure inverts the meaning and sends an operator to inspect a healthy
    host."""
    source = inspect.getsource(async_runner.AsyncRunner)
    assert "qwen_cli.refusal_detail(stderr_text)" in source
    assert (
        "mapping.CLASS_ACTOR_REJECTED_INPUT" in source
    ), "an ACP policy refusal must not be reported with CLASS_EXECUTION"
    assert "qwen_cli.REFUSAL_EXIT_CODE" in source, (
        "the exit code is checked too, mirroring run_sync: a refusal exit with "
        "no marker is a driver fault worth naming, not a crash"
    )
