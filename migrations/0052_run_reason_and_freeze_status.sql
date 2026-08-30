-- 0052_run_reason_and_freeze_status.sql
--
-- Task t17 (spec c28/h19): freezing a ticket must END that ticket's runs,
-- and each ended run must SAY WHY. Before this, handleFreezeTicket wrote
-- the ticket state and the page banner and touched no run at all -- the
-- SCRUM-5 spec-chain run 01M16GMQMWYCA0EW0V7MHHQFWN sat at 'running' on a
-- Done, frozen ticket, parked on a question nobody would ever answer.
--
-- Two columns, both expand-only (docs/adr/0002-migration-policy.md), both
-- nullable with no default, following 0034_run_actor_affinity.sql and
-- 0038_run_trigger_subject.sql for the same reason: every reader names its
-- columns explicitly (internal/store/postgres/engine_store.go's
-- insertRunSQL / selectRunSQL and internal/api/queries.go's listRuns never
-- use SELECT *), so a binary built before this migration reads and writes
-- runs exactly as it does today and simply never sees these columns.

-- Why this run is in the state it is in, when the reason is a control-plane
-- decision rather than something an actor reported. The one writer today is
-- the ticket freeze ('ticket_frozen'), and it is a RUN-level field rather
-- than a mutation of the last attempt's result on purpose: an attempt's
-- result is evidence of what an actor actually reported, and records are
-- immutable (PRD ground rule) -- overwriting one to carry a control-plane
-- decision would forge the actor's own account of its work. NULL means
-- nothing has recorded a reason, which is every run that reaches a terminal
-- state the ordinary way.
ALTER TABLE runs ADD COLUMN reason TEXT;

-- The board status the freeze was decided against, recorded because it is
-- what chose cancel over park and the control plane cannot re-derive it
-- later: the Jira bridge has post_comment / transition_issue / create_issue
-- and NO read verb (spec s13/s18), so a ticket's status is only ever
-- knowable here from what the caller of POST /v1alpha1/tickets/{id}/freeze
-- said it was. NULL means the caller did not say -- the pr.merged fact
-- carries no status field (examples/pr-upkeep/sweep.py's merged_pr_fact) --
-- and an unknown status parks rather than cancels, because a park is
-- reversible and a cancel is not.
ALTER TABLE ticket_freezes ADD COLUMN ticket_status TEXT;
