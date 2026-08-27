"""Shared fixtures.

Task t1 (plan qwen-bridge-acp) ports the seam-agnostic core suite —
test_callbacks, test_config, test_deployment, test_dialin,
test_idempotency, test_mapping, test_preflight, test_preserve, test_reap,
test_workspace — and none of these need fixtures beyond pytest's own
(`tmp_path`, `monkeypatch`); they run against pure functions and stdlib
subprocesses.

The seam fixtures arrive with the seam: plan task t2 ports the
`fake_codex` discipline from ``adapters/codex/tests/conftest.py`` to a fake
ACP agent — a stdio JSON-RPC script under ``tests/fixtures/acp/`` that
replays the measured 2026-08-23 probe shapes — and the sibling's
availability probe (``codex_bin``/``scratch_repo``) becomes the
``qwen_bin`` equivalent, so any test that shells out to a REAL, installed
qwen skips (never fails) when one is unavailable, which keeps the unit
suite independent of a real CLI being present.
"""
