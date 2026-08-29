-- The ticket projection (GET /v1alpha1/tickets/{id}, migration 0046) lists a
-- ticket's runs by subject with no state filter, which the partial index from
-- 0038 (active states only) cannot serve -- colleague's review of the wave-0
-- merge measured a seq scan. A full (namespace_id, subject) index serves both
-- shapes; 0038's partial index stays for the one-active-run-per-subject check.
CREATE INDEX IF NOT EXISTS runs_subject_idx ON runs (namespace_id, subject);
