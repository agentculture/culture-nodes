-- An attempt's start is a measured fact, not a stamp. CompleteAttempt used to
-- write started_at = completed_at = now() for every attempt (issue #116), so
-- duration_percentiles reported zeros on live data. The start now comes from
-- the invocation row (actor_invocations / runner_invocations.created_at); an
-- attempt that has no invocation row — the synchronous path — has an unknown
-- start, and unknown is recorded as NULL rather than replaced with now().
-- Expand-only: no row is rewritten; readers treat NULL as "not measured".
ALTER TABLE attempts ALTER COLUMN started_at DROP NOT NULL;
