-- 0057_run_work_item.sql
--
-- Plan loop-closure-claude-codex, task t1 (spec c2 as amended by decision
-- c41 / q6): every run a driven work item produces carries that item's key
-- -- the Jira issue key, e.g. SCRUM-9 -- so `GET /v1alpha1/runs?work_item=KEY`
-- returns exactly the runs that belong to it. Before this the only
-- candidates were `runs.category` (already a stats slicing dimension,
-- internal/store/postgres/actorstats.go, and the harness-compare rule id,
-- examples/harness-compare/measurements/run.py) and `runs.subject` (the
-- one-active-run-per-subject correlation key, 0038 -- and pr-upkeep.pr facts
-- deliberately carry no subject because a subject re-enters the guard #268
-- removed, docs/operations/pr-upkeep-lane.md). Overloading either would
-- change what an existing consumer's field means. So the key gets its OWN
-- column, and category is untouched.
--
-- Expand-only (docs/adr/0002-migration-policy.md): nullable, no default,
-- following 0052's reasoning -- every reader names its columns explicitly
-- (insertRunSQL / selectRunSQL in internal/store/postgres/engine_store.go
-- and listRuns in internal/api/queries.go never SELECT *), so a binary
-- built before this migration reads and writes runs exactly as it does
-- today and never sees the column. NULL means "no work item declared",
-- which is every operator-created run that did not pass work_item and every
-- triggered run whose event payload carried no string `work_item`.
--
-- Writers: POST /v1alpha1/runs (the request's optional `work_item`) and the
-- engine's event->run minting (internal/engine/trigger.go stamps it from the
-- triggering payload's `work_item` field). PATCH /v1alpha1/runs/{id} does NOT
-- write it: the key is set once, when the run is minted for the item.
ALTER TABLE runs ADD COLUMN work_item TEXT;

-- The list filter's index: (namespace_id, work_item), partial over the rows
-- that actually carry a key so the many NULL rows cost nothing. Not
-- CONCURRENTLY -- `nodes migrate` runs inside a transaction, and the table
-- is small at every deployment this ships to.
CREATE INDEX IF NOT EXISTS runs_namespace_work_item_idx
    ON runs (namespace_id, work_item)
    WHERE work_item IS NOT NULL;
