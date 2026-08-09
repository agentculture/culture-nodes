# Phase-2 cycle baseline — 2026-08-09

Task `t1` of `docs/plans/2026-08-09-self-hosted-phase-2-cycle.md`: the
already-delivered surface re-verified on the branch point (`33cc5ee`,
`phase2/build` = main + spec/plan docs) before any new work builds on it —
honesty condition h8: verified, not trusted.

## Results

| Suite | Command | Result |
|-------|---------|--------|
| Go (all packages) | `go test ./...` | all ok — includes `tests/e2e` (9.99s), `tests/fault` (21.08s), `internal/runners/headspace` live (27.26s), `internal/queue/sqs` fake-backed (5.80s) |
| Python front | `uv run pytest -n auto` | 88 passed |
| colleague adapter | `uv run --project adapters/colleague pytest adapters/colleague` | 100 passed |
| Web unit | `npx vitest run` | 75 passed (7 files) |
| Web e2e | `npx playwright test` | 12 passed |

## Boundary checks (h7)

- No AWS credentials exist on this machine; the `awslive`-tagged tests remain
  skipped (no `NODES_TEST_LAMBDA_ARN`), and the SQS/Lambda suites ran against
  fakes in the default pass, as required by boundary c9.

## Environment note (friction, per h15)

- The Go toolchain was **absent** on this dev machine at cycle start
  (`go: command not found`) despite the prior cycle's recorded full-suite
  runs. Installed user-space: Go 1.26.5 linux/arm64 at `~/.local/go`, PATH
  exported via `~/.profile`. Recorded as dogfooding friction for the delivery
  summary.
