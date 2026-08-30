# Runner-operation orphan sweep (#129)

Status: decided 2026-08-30 (cycle ticket `SCRUM-6`, task t4).
Disposition: accept the polled-forever orphan as a deliberate bounded-cost
trade-off. This record closes issue #129.

## Decision

Do not add a runner-operation orphan sweep or a max-age abandonment rule.
If a worker crashes after `CompleteAttempt` commits but before
`CloseRunnerOperation` changes the invocation state, the
`runner_invocations` row remains `waiting_external` and is polled forever.
The bounded cost is **one status poll per sampling tick per orphan**, subject
to the existing per-pass batch limit.

This is accepted because the sampler cannot honestly infer abandonment from
age. A runner may still return a terminal result, while the already-completed
attempt fence prevents that late result from changing run state. Marking the
operation abandoned after an arbitrary max-age would add a second lifecycle
decision with no new evidence. The current failure mode spends small,
predictable query and runner-request capacity while preserving the audit trail.

## Evidence and consequence

- `internal/store/postgres/runnerasync.go:405-409` states the recovery model:
  no unstick step, orphan sweep, or lease exists; a claimed row simply becomes
  due again.
- `internal/store/postgres/runnerasync.go:416-429` selects every due
  `waiting_external` row and bounds a pass with `LIMIT`.
- `internal/worker/runnerasync.go:571-589` commits the attempt first and closes
  the runner operation afterward, leaving the stated crash window.
- `internal/worker/runnerasync.go:410-418` performs one status read and
  reschedules a non-terminal operation; sampling failures follow the same
  rescheduling path at `internal/worker/runnerasync.go:411-413`.

Operators should therefore treat a growing population of completed attempts
with `waiting_external` runner invocations as capacity leakage to investigate,
not as work that the system will reap. A future change may add an administrative
cleanup operation, but it must be explicit bookkeeping and must not manufacture
a runner outcome or ledger evidence.
