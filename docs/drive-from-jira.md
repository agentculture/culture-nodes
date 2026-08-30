# Drive it from Jira

This page is for a person who works on the board. You do not need a
terminal, a git checkout, or any of the `curl` commands the rest of this
repo's documentation is written in. Everything here happens in two places:
the Jira ticket, and one web page that culture-nodes links to from the
ticket.

The loop, in one sentence: **you move a ticket or write a comment,
culture-nodes notices, does the work, and comes back to you — on the ticket
and on its own page — and a person, not the system, decides when it is
Done.**

Two things to know before anything else:

- **Nothing happens the instant you click.** culture-nodes does not receive
  Jira webhooks. A scheduled *sweep* reads the board on an interval and turns
  what changed into facts. Expect minutes, not seconds, between your move and
  the first comment.
- **The system writes to Jira under one Atlassian account** — today, the
  operator's own. It filters that account's writing out of what it reads, so
  it cannot react to itself. That one fact is why some replies have to go on
  the page instead of the board; the ["Answering a
  question"](#answering-a-question-do-it-on-the-page) section below is the
  whole of it.

## What a ticket must contain

| Field | Needed? | What culture-nodes does with it |
| --- | --- | --- |
| Project | **required** | Only one project is read. If your ticket is not in it, nothing will ever happen. |
| Summary (title) | **required** | Handed to the agent, and quoted back in the intake comment. |
| Description | **strongly recommended** | This is the brief. It is handed to the agent verbatim (first 4000 characters; a longer one is passed truncated and flagged as such). A ticket with an empty description gives the agent nothing but its title to work from. |
| Status | **required — this is the trigger** | The status *change* is what starts a flow. See the table below. |
| Issue type, priority | optional | Passed through as `kind` and `severity`; nothing branches on them today. |
| Labels, components, assignee, story points, epics, attachments | **not read at all** | Use them for your own purposes; culture-nodes never sees them. |

Two limits worth knowing:

- A ticket is only read while it is **unresolved, or was resolved in the last
  seven days**. An old closed ticket is invisible to the sweep.
- **Do not put secrets in a ticket.** The description is handed to an agent
  session and can be quoted back into a public comment. The ticket is a
  brief, not a vault.

## Which move triggers which flow

| What you do on the board | What culture-nodes does |
| --- | --- |
| Move a ticket to **To Do** (or create it there) | *Intake*: posts an acknowledging comment and moves the ticket to **In Progress** |
| Move a ticket to **In Progress** | *The spec lane*: an agent drafts a frame from the description, posts a marked question, and waits for your answer |
| Write a plain comment | *The comment consumer*: an agent reads your comment in the ticket's context and replies on the ticket |
| Answer a **marked question** | Resumes the run that asked it — **answer on the page, not on the board** |
| (culture-nodes needs a decision) | Posts a comment listing the options, moves the ticket to **Pending**, and posts to Discord |
| A pull request naming the ticket **merges** | The ticket **freezes**: replies close and its runs end |
| Move a ticket to **Done** | Nothing automatic. Done is a person's statement, and only a person makes it |

Each row in detail below.

### To Do → intake acknowledges and moves the ticket

The trigger is the **transition into** To Do, not the ticket sitting in it.
Publishing the workflow does not sweep up every ticket already in To Do; only
a fresh move does. Creating a ticket directly in To Do counts as one.

Intake does exactly three things: an agent drafts a short comment (it
acknowledges the pickup, restates the request in a sentence or two, and asks
the single most important clarifying question if there is one), the comment is
posted, and the ticket is moved to **In Progress**. That board move is
culture-nodes' way of telling you it has the ticket.

Moving a ticket **back** to To Do picks it up again. That is deliberate — it
is how you re-triage something. At most two tickets are in intake at once; a
third waits.

**The exception you will hit**: a status change made by the same Atlassian
account culture-nodes posts under is discarded as the system's own echo. Today
that account is the operator's. So if the operator drags a ticket to To Do by
hand, intake does *not* fire; a move by anyone else does, and so does creating
the ticket. Ask the operator if a move seems to have been ignored.

### In Progress → the spec lane

A transition into **In Progress** starts the `/think` spec lane: an agent
reads the ticket, drafts a frame (an announcement, claims, open questions),
commits it, posts the frame snapshot to the ticket page, and then posts **one
marked question** and parks — holding nothing, billing nothing — until you
answer.

A healthy round reads like this on the ticket: a "started run" comment, then a
question comment ending in a marker (see below), then silence until you
answer. Your answer resolves that question, the frame moves to its next
version, and the lane either asks the next question or ends the round.

Intake chains into this on its own: intake moves the ticket to In Progress,
and the next sweep pass sees that transition. You do not have to do both moves
yourself.

One run per ticket at a time. A question nobody answers waits indefinitely —
there is no timeout on silence.

### A plain comment → the comment consumer

Any comment that is **not** an answer to a marked question is a bare
go-signal. An agent reads it against the ticket and replies on the ticket.
This is the "just tell it something" channel: a nudge, a correction, a new
constraint.

Comments the system itself wrote are never treated as your comments.

### Answering a question, do it on the page

culture-nodes marks a question it is waiting on. A marked question is a
comment that ends with a line like:

```text
[culture-nodes:jira-actor question_id=scrum-6.q1]
```

**Answer it on the ticket page, not in the Jira comment box.**

The reason is the account. culture-nodes posts to Jira as the operator's own
Atlassian account, and the sweep is told that account id so it never reacts to
its own comments — it looks at who wrote the newest comment and drops it if
that is the system's account. Because the system's account *is* the operator's
account, a reply the operator types into Jira looks exactly like the system
talking to itself, and is discarded. The parked run never wakes.

Answering on the page has none of that ambiguity. The page hands your answer
straight to the control plane as a fact — no sweep, no filter, no waiting for
the next pass — and *then* mirrors your words onto the Jira ticket so the
board still reads as one conversation. The mirrored comment ends with:

```text
via <your name>
```

So the board stays complete either way; the page is what makes the answer
*land*.

(If you are not the operator — if you have your own Atlassian account — a
board reply does reach culture-nodes. The page still works and is still the
recommended route, because it is the one that behaves the same for everyone.)

To answer on the page: open it (below), type the decision token, put your name
in **Your name**, choose the question from the **Question** dropdown, write
your answer, and press **Send reply**. Leave the dropdown on "General reply"
to say something that is not an answer to any particular question.

### A pending decision → options, Pending, and Discord

When a run reaches a point only a person can settle, culture-nodes does not
wait quietly on a page nobody visits. It fans the decision out three ways at
once:

1. **A Jira comment** on the ticket listing the options and where to answer:

   ```text
   culture-nodes is waiting on a decision.

   task: 01M16FX0BWK9X6TKE9BHAAW88Y
   run: 01M16GMQMWYCA0EW0V7MHHQFWN
   node: approval
   options: approved, rejected
   decide: http://<origin>/tickets/SCRUM-6
   ```

2. **A board move to `Pending`.** That is the ticket's way of showing, at a
   glance on the board, that it is blocked on a human. The comment is always
   posted before the status moves, so the status change never appears
   unexplained.
3. **A Discord post** in the team channel, carrying the same options, the same
   link, the run id, and the ticket key.

The options listed are exactly the answers the engine will accept — not a
menu someone wrote by hand. A task that is an alert rather than a choice says
so instead of offering an empty list.

**No message in any of these three channels ever contains the decision
token.** A Jira comment and a Discord post are readable by everyone who can
read the board or the channel; the token is not handed to that audience. You
bring your own token to the page.

The decision stays pending until somebody answers it, with one exception: see
the next section.

### The PR merges → the ticket freezes and its runs end

When a pull request whose **branch name or description names the ticket key**
merges, culture-nodes records that merge as a fact, and:

- the ticket is **frozen** — the page shows a banner naming the merged PR, and
  the reply form closes;
- every one of the ticket's still-live runs is ended, each stamped with the
  reason `ticket_frozen`, and the banner counts them ("Ticket status unknown:
  0 runs cancelled and 2 parked with reason ticket_frozen");
- any decision still pending on a merged PR expires with the reason
  `pr_merged`, so nobody is asked to approve work that already shipped.

Merging **parks** the runs rather than cancelling them: parking is reversible
and keeps everything a resume would need. Runs are only *cancelled* — ended
for good — when the freeze is made against a ticket whose status is **Done**,
which is a deliberate act by a person. When culture-nodes cannot tell what the
board status is, it takes the reversible option; that is why the banner says
"status unknown" on a merge-triggered freeze.

If the branch and the PR description both fail to name the ticket key, nothing
correlates and the ticket does not freeze. Naming the ticket in the branch is
the reliable habit.

### Done → a person moves it

**Nothing in culture-nodes moves a ticket to Done.** Not intake, not the spec
lane, not a merge. A person does it, and that is on purpose: Done is a claim
that the work is finished and correct, and an agent saying "done" is a
completion claim, not evidence for one. (Teaching the system to make that move
is an open, named feature — it is a decision nobody has taken, not an
oversight.)

What Done means when you do move it: the work is accepted. If you then freeze
the ticket while it reads Done, its runs are **cancelled** rather than parked —
the difference described above.

## What each system comment means

Every comment culture-nodes writes is one of these. They are posted by the
narrow Jira bridge, which is the only thing in the system that may write to
your board, and each one is marked as the system's so the loop cannot feed
itself.

| Comment | Looks like | What it means |
| --- | --- | --- |
| **Started** | `culture-nodes started run <run-id> (workflow <name>, trigger event <id>)` | Your move was picked up; a run exists. Posted once per run. |
| **Finished** | `culture-nodes finished run <run-id> with outcome <outcome>` | That run ended, and the outcome word says how (`completed`, `failed`, `cancelled`, …). A *finished* comment always follows its own *started* comment. |
| **Page link** | `culture-nodes page: <origin>/tickets/SCRUM-6 [culture-nodes:ticket-page-link]` | Where this ticket's page lives. Posted **once per ticket**, not once per run — you will not see it repeated. |
| **Question** | Ordinary prose ending `[culture-nodes:jira-actor question_id=…]` | A run is parked waiting for your answer. Answer it on the page. |
| **Options** | `culture-nodes is waiting on a decision.` followed by task/run/node/options/decide lines | A decision is pending. The `decide:` line is the link. |
| **Intake / reply prose** | An ordinary comment signed `- culture-nodes (Claude)` | An agent's own words: the acknowledgement on pickup, or a reply to something you wrote. |
| **Your mirrored reply** | Your text, then `via <your name>` | Something you sent from the page, echoed onto the board so the ticket reads as one thread. |

The bracketed markers exist for the machine, not for you — they are how the
system recognises its own writing. Ignore them, but do not delete them from a
comment you are quoting.

## Opening the page

Two ways, both of them a link somebody already sent you:

- the **page link comment** on the ticket (`culture-nodes page: …`), which is
  the ticket's permanent address;
- the **`decide:` line** in an options comment, or the **Decide** field of the
  Discord post — both point at the same page.

### It only opens from the office network

**The link works from the LAN or over tailscale, and nowhere else.** That is
the accepted state of the deployment until the sign-in work lands: there is no
login, so the page is not exposed to the internet. From a phone on mobile
data, the link will simply not load.

Nothing is hidden from you when that happens — the Jira ticket carries the
record of what was asked and what was decided. What you cannot do from
outside is *act*: answering a question and pressing a decision button both
require the page.

If the link in a comment shows as plain text rather than something clickable,
the deployment has not been told its own address. That is a one-line fix on
the server, and worth reporting to the operator rather than working around.

## The decision token, and where it comes from

The page is read-only until you hold a **decision token**.

- It is **one shared secret for the whole deployment** — not a per-person
  password, not something you can set or reset yourself. **The operator hands
  it to you.** If you do not have one, ask; there is no self-service.
- Type it into the **Decision token** box on the page. It is kept in that
  browser tab only, and closing the tab forgets it. It is never stored in
  the page's code, never sent to Jira, and never posted to Discord.
- The token says *this deployment trusts whoever holds this*. It does **not**
  say who you are — which is why the page also asks for **Your name** (the
  decider id). Both are required: the token authorises the write, your name
  records who made the call. Your name is remembered between visits; the token
  deliberately is not.
- If you are working on a shared machine, close the tab when you walk away.

## Deciding from the page

1. Open the page from the link.
2. Type the decision token.
3. Put your name in the decider field.
4. Under **Decisions**, each waiting item shows its task id, its run, its
   deadline if it has one, and **one button per answer the engine will
   accept**. Press one.

That is the whole ceremony. Three things it will do that are worth expecting:

- **Only real options are offered.** The buttons are built from what that
  particular task declared, so there is no way to submit an answer the engine
  would then reject.
- **A stale page is refused, not silently applied.** The page submits the
  version of the run it showed you. If the run moved on while you were
  reading, the decision is refused and nothing is written — reload, read the
  new state, and decide again. This is the intended outcome: what you read is
  no longer what you would be deciding.
- **Nothing is rewritten afterwards.** Your decision is recorded as its own
  permanent record naming you and the moment. Corrections are new records,
  never edits.

A **frozen** ticket shows no decisions and takes no replies: its runs already
ended.

The rest of the page is there to read: **Claims** (what the agents asserted,
each marked with whether it is still just a claim or has been confirmed),
**Questions and decisions**, **Runs and reports** (every run this ticket has
had, linked), and the **Reply thread**. **Open in Jira**, top right, always
takes you back to the board.

## When something does not happen

| Symptom | Most likely cause |
| --- | --- |
| Moved a ticket, nothing happened at all | Give it a sweep interval. Then: is the ticket in the read project? Did *the operator's own account* make the move (see intake, above)? |
| Answered a marked question on the board, run still parked | Exactly the case this page's ["Answering a question"](#answering-a-question-do-it-on-the-page) section describes. Answer again on the page. |
| The page link is plain text, not a link | The deployment's own address is not configured. Report it. |
| The link will not open | You are off the LAN/tailscale. Nothing is broken. |
| A decision button is greyed out | No token typed, or no name in the decider field. |
| Pressing a button reported a conflict | The run moved while you read. Reload and decide against what it says now. |
| A ticket went to **Pending** and you do not know why | Read up: the options comment is always posted before the status moves. |

## Related reading

- [`examples/jira-intake/README.md`](../examples/jira-intake/README.md) — the
  To Do pickup graph, node by node.
- [`docs/operations/spec-chain-lane.md`](operations/spec-chain-lane.md) — the
  In Progress `/think` lane, for whoever installs and operates it.
- [`examples/jira-question-round-trip/README.md`](../examples/jira-question-round-trip/README.md)
  — the ask/answer/resume machinery and the question marker contract.
- [`examples/pr-upkeep/README.md`](../examples/pr-upkeep/README.md) — the
  sweep that reads the board, and the exact event vocabulary it emits.
- [`deploy/prod/README.md`](../deploy/prod/README.md) — the operator's half:
  the runner grants, the page-link origin, and the board statuses the Jira
  bridge is allowed to move a ticket to.
