-- 0008_run_output.sql
--
-- Expand-only: adds `runs.output`, the workflow result a completed run
-- produces (prd-spec §11.3 spec.contract.output, resolved from the end
-- node's output binding).
--
-- Why it was not in 0002: that migration built the runtime tables from
-- §14's table list, and §14 names tables rather than columns. The gap only
-- became visible once the engine (task t9) had to answer "what did this run
-- produce?" — and §14 is explicit that current-state tables are
-- authoritative for orchestration, so the answer belongs in `runs`, not
-- only in the run.completed audit event.
--
-- Nullable with no default: a run that has not completed has no output, and
-- spelling that as NULL keeps "no result yet" distinguishable from a run
-- whose result genuinely is JSON null.

ALTER TABLE runs ADD COLUMN output JSONB;
