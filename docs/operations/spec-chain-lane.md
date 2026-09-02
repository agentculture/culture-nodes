# The board-driven /think leg: publishing, granting, triggering, reading

Task t13 of `jira-flow-spec-read-related-bugs` (issue #199, dispatch
log #230; spec c7 / h5; frame decisions q1 and q4 = B). The graph is
`examples/spec-chain-lane/workflow.yaml`; its README explains the shape.
This page is the operator half: what to install, what to grant, how to
trigger one run, and how to read the result — the live proof is task t14.

## 1. Declare custody on the developer lane

Only the developer lane writes `.devague/`, only in its own worktree. The
declaration lives in that bridge's config on spark
(`~/.config/culture-nodes-bridges/developer.json`, mode 600), beside the
allowlist that already names the checkout:

```json
{
  "repo_allowlist": ["/home/spark/git/.worktrees.culture-nodes/owe-developer"],
  "custody": {
    "checkout": "/home/spark/git/.worktrees.culture-nodes/owe-developer",
    "branch_prefix": "jira-flow/",
    "devague_write": true
  }
}
```

Rules the load enforces (`adapters/claude-code/.../config.py`): every key is
required; `checkout` must be allowlisted; `branch_prefix` ends in `/`. A
malformed block stops the bridge from starting rather than starting it with
half a declaration. The same three keys can be supplied as one JSON object
in `CLAUDE_CODE_BRIDGE_CUSTODY`, which wins over the file.

What the declaration does: every mover node in the graph binds
`devague_write: true`; the bridge answers that request from this block —
`403 auth_or_policy` when the lane declares nothing, grants nothing, or the
dispatch targets another allowlisted checkout — and, when the grant holds,
appends the declaration to the session's brief so it works the declared
branch namespace. The operator's `nodes-op.sh assign --devague-write` refuses
every other actor before anything is billed.

## 2. Grant the two environment values

`post_frame.py` posts the frame snapshot to
`POST /v1alpha1/tickets/{id}/frame` **as the lane's own actor**. The
credential is the developer actor's own bearer — the value the control plane
already holds for `company/developer` under the actor row's `auth_token_env`
(`NODES_ACTOR_CLAUDE_TOKEN` in thor's `prod.env`, which is the same value as
`auth_token` in this `developer.json`). The control plane resolves the
bearer to the registered actor and records the frame as posted by it
(login-from-anywhere task t11, spec c45). The human decision token is **not**
granted to the lane — an agent never writes under a person's credential. The
token is a **granted environment value on the lane**, never a workflow
literal and never an argument. Add both to the same `developer.json`:

```json
{
  "claude_env": {
    "NODES_API_URL": "http://<thor>:18080",
    "NODES_ACTOR_TOKEN": "<this file's own auth_token — the value NODES_ACTOR_CLAUDE_TOKEN holds in thor's ~/.culture-nodes/prod.env>"
  }
}
```

`deploy/prod/lanes/account-bridges.sh` refuses to render a developer config
that carries the human decision token (q5); this grant is what it expects
instead.

Then restart the bridge and check it came up with the declaration:

```bash
systemctl --user restart culture-nodes-claude-developer.service
journalctl --user -u culture-nodes-claude-developer.service -n 5
```

A wrong token shows up as `post_frame: 401 from the control plane` in the
session's transcript and the mover node ends `contract_rejected` (its final
message is not the envelope); a missing one exits 2 naming the variable.

## 3. Register the repository identity (already done for the developer lane)

A trigger-created run's input is the ticket fact, which carries no checkout
path. The developer actor is registered with a repository identity that the
bridge resolves to its own worktree (t2, #125). If a fresh deployment gets
`input.repo is required` on the `think` node, that registration — not the
graph — is what is missing (`deploy/prod/register-actor.sh`).

## 4. Publish

```bash
bash .claude/skills/nodes-operator/scripts/nodes-op.sh validate examples/spec-chain-lane/workflow.yaml
bash .claude/skills/nodes-operator/scripts/nodes-op.sh publish  examples/spec-chain-lane/workflow.yaml
bash .claude/skills/nodes-operator/scripts/nodes-op.sh workflows | grep spec-chain-lane
```

Only the newest version of a workflow key is a trigger candidate. The kill
switch is the same as jira-intake's: copy the file, delete the `triggers:`
block, bump `metadata.version`, publish. To stop one in-flight run:
`curl -X POST $NODES_API_URL/v1alpha1/runs/<run-id>/cancel`.

## 5. Trigger one run

The trigger is the sweep's `pr-upkeep.jira.transitioned.in-progress` fact
with `source == "jira"`. It fires on a transition **into** In Progress
(watermarked per issue), so an already-In-Progress ticket does not fire on
publish. Two ways to raise it on a real ticket:

- let a pickup chain into it: move the ticket to To Do; jira-intake picks
  it up and moves it to In Progress; the next sweep pass emits the fact;
- move it yourself: `/jira-status move SCRUM-N "In Progress"` (the
  operator's own hand on the board, outside the engine), then wait one sweep
  interval.

One run per subject at a time (`maxConcurrentSubjectRuns: 1`); a second
In-Progress fact for the same ticket attaches to the open run.

## 6. Read the result (the t14 signals)

```bash
# the run and its trigger
bash .claude/skills/nodes-operator/scripts/nodes-op.sh runs 5
curl -s $NODES_API_URL/v1alpha1/runs/<run-id> | python3 -c 'import json,sys; r=json.load(sys.stdin)["run"]; print(r["trigger_event_id"], r["state"])'
# the frame on the ticket projection
curl -s $NODES_API_URL/v1alpha1/tickets/SCRUM-N | python3 -c 'import json,sys; t=json.load(sys.stdin); f=t.get("latest_frame") or {}; print("frame version", f.get("version"), "posted_by", f.get("posted_by"))'
```

What a healthy round looks like, in order:

1. `think` ends `question_raised`; the ledger holds its `proposed` claim;
   `.devague/frames/<ticket-lowercase>.json` is committed on
   `jira-flow/<ticket-lowercase>` in the lane worktree; the ticket
   projection shows `latest_frame.version >= 1`.
2. `post-question` posts the marked question; the comment ends with
   `[culture-nodes:jira-actor question_id=<ticket-lowercase>.qN]`.
3. The run parks on `await-answer`. Every ticket's reply wakes it (#239);
   `correlate` sends an uncorrelated wake back to the park — visible as
   repeated `await-answer`/`correlate` visits, no session billed.
4. A reply to the marked comment (or on `/tickets/<id>`) reaches
   `transact`, which runs exactly `devague question --resolve qN
   --decision "<reply>" --frame <slug>`, commits, posts version N+1, and
   raises the next question or ends the round.

Confirmations are never made from the board: a frame blocked on them parks
on `needs-human` with the blockers in the mover's output, and a person
confirms in the checkout (`devague confirm ...` on the lane branch) or on
the page, then decides the human task.

## Who reads suite-verdict and gate-report authority (t11 consumer list)

Under the gate's own principal (`NODES_ACTOR_MERGE_GATE_TOKEN`, registered
as `company/merge-gate`), suite verdicts and gate reports land **agent-origin,
`proposed`** instead of validator-origin, `derived`. This is every consumer
of those records' authority in `internal/` and `examples/`, and what each
does about it:

| Consumer | What it reads | Under proposed verdicts |
| --- | --- | --- |
| `internal/api/repairroute.go` `routeGateFailure` / `lastAgentActorID` | the run's newest agent-origin proposed record, as the session whose work the gate judged | **accepts proposed verdicts** — `review` records are skipped when finding that session, so a repair is never routed at the gate itself (`repairroute_internal_test.go`) |
| `internal/repair/route.go` (`decision`/`derived` routing records; attempt bound) | the control plane's own derived routing records, provenance-linked to the verdict | **accepts proposed verdicts** — the bound is counted over routing records, which stay engine-derived whatever the verdict's authority |
| `internal/worker/successsignal.go` `derivedReviewSubjects` | derived review records whose subject is a success signal, as "already evaluated" | **unaffected** — a gate verdict's subject is the handover evidence, never a success signal; a proposed verdict is not mistaken for an evaluation |
| `internal/worker/acceptance.go`, `hooks.go` | compose their own validator-origin derived reviews | **unaffected** — producers, not readers |
| `examples/merge-gate/workflow.yaml`, `examples/development-loop/workflow.yaml` | the gate node's routable outcome (`gates_passed` / `changes_required` / `measurement_incomplete`), computed by the control plane from the report | **accept proposed verdicts** — routing follows the outcome, not the ledger authority; a `measurement_incomplete` still parks at the `human-overrides` node |
| the human decision bearer path (`scripts/collect-handover.py --validator`, `NODES_GATE_VALIDATOR_ACTOR_ID`) | validator-origin derived records | **re-pointed at a human node** — an operator who wants a derived record posts under the decision bearer naming a registered validator, which is a person's act on the human-authority surface |
| `web/` (`pending-decisions`, run ledger views) | any proposed record is a decision for a person | **accepts proposed verdicts** — the gate's claim now shows up where a person decides it, which is the point (out of this task's scope to change; noted for t18's measurement) |

## What this does not claim

- The devague moves a session reports are **agent-reported** (`proposed`),
  not engine-verified; the committed `.devague/` and the posted snapshot are
  the evidence to read them against. The engine-verified form is the
  code-node graph, deferred to #237.
- The sweep does not yet send `subject` on transition facts, so the
  per-subject ceiling arms only for deliveries that carry one (the same gap
  jira-comment-consumer declares).
- A question nobody answers parks its round indefinitely.
