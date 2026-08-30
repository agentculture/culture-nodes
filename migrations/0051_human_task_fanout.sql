-- 0051_human_task_fanout.sql
--
-- Fan-out for a newly created human task, and an expiry status for one whose
-- subject PR has already merged (task t11, spec c6, plan decisions q1/q4/q5).
--
-- WHAT WENT WRONG. A pending human task was visible on exactly two surfaces,
-- both of which a person has to go and look at: /inbox and /decisions. Nobody
-- is paged to either. On 2026-08-30 that left 26 pending pr-upkeep
-- 'human-merges-pr' approvals on prod whose PRs (#236/#238/#244/#246) had all
-- already merged: the decision they were asking for had been made elsewhere,
-- weeks earlier, and nothing in the system could notice.
--
-- WHY AN OUTBOX AND NOT A CALL. The fan-out is a side effect on three systems
-- the control plane must not talk to inside a run transaction (Jira, Discord,
-- and whatever posts to GitHub). Writing the intent transactionally and
-- draining it afterwards is the same discipline jira_ticket_report_outbox
-- (0043/0048) already uses for lifecycle comments, and it is what makes
-- "exactly one fan-out per task" a database constraint rather than a promise:
-- UNIQUE (human_task_id, channel) means the same task enqueued twice inserts
-- nothing the second time.
--
-- WHY A SEPARATE TABLE FROM jira_ticket_report_outbox. That table is keyed on
-- a Jira issue key and carries a Jira verb; this one carries intents for two
-- different bridges (the narrow Jira actor and the notify actor) and is keyed
-- on the human task. Widening the Jira table would have made issue_key
-- nullable for rows that are not about a ticket at all, and its
-- (run_id, phase) / (namespace_id, issue_key, phase) unique indexes cannot
-- express "one per task per channel".
--
-- WHY github_pr_comment IS NOT A CHANNEL HERE. No bridge in this repo can
-- post a GitHub PR comment today: adapters/human-inbox only READS the PR
-- thread (fetch_github_issue_comments / fetch_github_pull) and writes solely
-- to its own /inbox/tasks/<id>/submit surface, and the claude-code and codex
-- bridges advertise no GitHub-writing verb at all. Declaring a channel no
-- drain could ever publish would queue rows that silently rot. A PR-sourced
-- run therefore fans out to notify only, and adding the channel is a
-- deliberate later migration once a bridge advertises the capability.

CREATE TABLE human_task_fanout_outbox (
    id               TEXT PRIMARY KEY,
    namespace_id     TEXT NOT NULL REFERENCES namespaces (id),
    human_task_id    TEXT NOT NULL REFERENCES human_tasks (id),
    run_id           TEXT NOT NULL REFERENCES runs (id),
    channel          TEXT NOT NULL CHECK (channel IN ('jira_comment', 'jira_transition', 'notify')),
    target_actor_key TEXT NOT NULL,
    payload          JSONB NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    attempts         INTEGER NOT NULL DEFAULT 0,
    available_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (human_task_id, channel)
);

CREATE INDEX human_task_fanout_outbox_pending_idx
    ON human_task_fanout_outbox (status, available_at) WHERE status = 'pending';

-- human_tasks.status has never had a CHECK constraint, so 'expired' needs no
-- schema change to be storable. What it does need is a reason: a task that
-- expired because its PR merged and a task that expired because its deadline
-- passed are different facts, and human_tasks.response is where the decision
-- path already records why a task left 'pending'. This column exists so the
-- reason is queryable without unpacking JSON -- the count an operator wants is
-- "how many did pr_merged expire", not "how many expired".
ALTER TABLE human_tasks ADD COLUMN expiry_reason TEXT;

CREATE INDEX human_tasks_expiry_reason_idx
    ON human_tasks (namespace_id, expiry_reason) WHERE expiry_reason IS NOT NULL;
