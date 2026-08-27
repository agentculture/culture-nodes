# The qwen bridge's first dispatch, and the five defects it took to get there

**Date:** 2026-08-27
**Scope:** bring `adapters/qwen` up as a registered actor, route issue #220 to it,
monitor for failures.
**Outcome:** the bridge executes. Issue #220 is **not** delivered. Effort on the
qwen backend is **parked** in favour of the colleague backend (operator decision,
same day).

## What was asked, and what the premises turned out to be

The request was to handle #220 "using qwen developer", open a Jira ticket, follow
it through a scope → think → challenge chain, and report when it failed to push.
Three of those four premises did not hold:

| Premise | Reality |
|---|---|
| a qwen developer actor exists | none registered; `adapters/qwen` had never been run |
| the Jira chain runs scope → think → challenge | `jira-intake` v1.2.0 is three nodes: comment → post → transition. #89 is open |
| something in that chain pushes | nothing does; the run ends at the board move |
| monitoring can live in culture-nodes | true, and #226 now scopes it |

The Jira half worked exactly as built. SCRUM-4 was created, the sweep picked it
up within 7 seconds of the next 5-minute cycle, the intake bridge drafted a
comment, the jira bridge posted it and moved the board to In Progress. Three
nodes, all green. It simply does not do the thing the request assumed.

## The five defects, in the order they were hit

Each was found by a dispatch failing, not by reading code.

### 1. Registration accepts a token name nothing checks — #222

`register-actor.sh` takes an `auth_token_env` and validates nothing about it.
`compose.thor.yml` enumerates those names by hand, twice, with no `env_file:`,
and `audit-credentials.sh` classifies them a third time. Registering
`company/qwen-developer` printed a clean success against a compose file that
could never have supplied its token.

The engine's error, when it came, was **good** — it named the exact missing
variable. The original issue claimed otherwise and was corrected in place.

### 2. Two workers, two credential sets — #224

thor and orin serve the same namespace from different compose files. thor
declared six actor tokens; orin declared four. Whichever worker won the poll
decided whether a dispatch authenticated.

`company/notify-discord` had been in that split state **in production**,
unrelated to this work: denied when orin claimed the item, fine when thor did.

The sharpest part is why nothing caught it. `audit-credentials.sh` already
declares `NODES_ACTOR_NOTIFY_TOKEN required`, and its codex entry already
states this issue's premise in as many words — *"either worker may claim a node
run for either host's codex actor, so a worker missing one 401s on work it
legitimately claimed."* But the audit derives its expected key set **from the
compose file it is auditing**, deliberately, so it cannot see a missing
*declaration* — only a missing *value for a declared key*. It read green on orin
the entire time. Nothing anywhere compares compose-to-compose.

Fixed in PR #223 for these two keys. Not closed: the divergence remains possible.

### 3. A policy refusal loses its message on the async path — #225

The ACP driver refuses an under-specified dispatch with a precise, actionable
message, writes it to stderr, and exits `REFUSAL_EXIT_CODE`. `run_sync` parsed
that. `async_runner` never read stderr at all — so a refusal arrived as *"killed,
crashed, or timed out"*, and the remedy text was computed and discarded. Since
`always_async` is the production setting, this was every dispatch.

Fixed in PR #223: stderr drained on its own thread, refusal reported as
`actor_rejected_input`, one `refusal_detail` shared by both paths.

**A latent deadlock fell out of the fix.** `spawn` opens stderr as a `PIPE`. An
unread pipe has a finite kernel buffer, so a session chatty on stderr would have
blocked the child while the runner waited on a stdout EOF that could never
arrive — and then reported a *timeout*, sending an operator to the model server.
Nobody was looking for this; it surfaced because fixing #225 forced the question
of who owns that pipe.

### 4. The bridge executed nothing, by construction — #227

The mode wire was built at both ends and never joined:

```text
server.py            reads 11 input fields; "mode" was not one   <- broken
async_runner:179     spawn(...) omitted mode= -> None            <- broken
qwen_cli.spawn       accepts mode: str | None = None             <- built
dispatch.build_argv  emits --mode ""                             <- built
gate.resolve_acp_mode("") raises AcpPolicyError                  <- built
driver.py:182        catches, writes refusal to stderr, exits    <- built
async_runner reads that refusal                                  <- broken (#225)
```

Every helper was implemented and unit-tested in isolation. No test covered the
**callers**, which is precisely how an adapter ships a `--mode` argv builder, a
mode gate, and a `mode=` kwarg and still cannot execute a single dispatch. The
third break hid the first two.

Proof it was the mode and not the environment: a dispatch whose run input
carried a legal `mode: "auto"` failed identically, because `server.py` never
read the field.

Fixed in PR #223, with `tests/test_mode_wire_and_refusal.py` guarding the
callers specifically.

### 5. A permission-starved session reports success — #228

With the wire fixed, the bridge ran: `initialize → session/new →
session/set_mode → session/prompt`, 2920 transcript lines, ten minutes. It
reported:

```text
task: completed  outcome=completed  attempts=[('succeeded', …qwen-developer)]
workspace_measured: {"changed_files": [], "diffstat": "", head unchanged}
```

Nothing was written. One `session/request_permission`, answered `cancelled`, one
skipped tool, and `stopReason: end_turn`.

Two correct decisions compose into a wrong one. `transport.py:93` fail-closes on
permission requests — right; a bridge with no human attached must not invent
consent. `classifier.py:90` maps `end_turn` to OK — also right, and measured.
Together: the bridge refuses, the agent gives up cleanly, and a clean `end_turn`
is success.

This is the worst failure shape of the five. Every other one was loud. This one
says the work is **done**, while the measurement proving otherwise sits in the
same payload and nothing compares the two. Per PRD §10.4 a completion claim is
not evidence; here the bridge produced both and let the claim outrank the
measurement it took itself.

## Why qwen is parked

The permission story is not a bug with a two-line fix. With a fail-closed
`session/request_permission` and no human attached:

| Mode | Completes shell-dependent work? |
|---|---|
| `plan` | no — analysis only by definition |
| `default` | no — every edit and shell asks, every ask is cancelled |
| `auto-edit` | no — edits pass, shell still asks |
| `auto` | no — classifier blocks what it dislikes, that ask is cancelled |
| `yolo` | would — **refused by the bridge** (h15) |

Admitting `yolo` was considered and started, then reverted with the change of
direction. Worth recording for whoever picks this up: **h15's live check is now
satisfied.** Run `01M11NNKGNR8PMG995C2JPQ1G9` showed a fresh `session/new`
returning `yolo` among `availableModes`, `session/set_mode` round-tripping with a
confirming `mode-update` echo, and `auto` demonstrably behaving as its
description claims. And admitting it would not widen confinement: the bridge's
own capability document says qwen-code runs its tools **in-process as the bridge
user**, so every mode already grants everything the process can do. `yolo`
removes an approval prompt, not a kernel boundary — there is not one to widen.

That makes the real question not "is yolo safe" but "what does an unattended
dispatch's authorization actually mean here", which is #228's third item and a
design decision, not a patch.

## What qwen actually did

Credit where it is due. In its one working session it read both skill surfaces,
stat'd the executable bits, read an existing `.qwen` skill to infer the
adaptation pattern, and caught **two errors in the instruction it was given**:
that `tests/test_qwen_skill_surface.py` requires a `lineage:` comment in every
`.qwen/skills/*/SKILL.md` — so the byte-identical copy it was asked for would
have failed CI — and that `.claude/skills` holds 20 directories, not the 21 it
was told.

Graded 3 (`ledger_01M11PBCZ68ATSS6E6P2A2R2BF`): blocked by the harness rather
than by judgment, but heavy on deliberation (650 thought chunks to 6 tool calls)
and it stopped mid-sentence rather than reporting.

## Operator hand-turns (the #118 count)

Nine, all of them credential or deploy work no automation covers:

1. generate the qwen bearer and write the bridge config
2. merge `NODES_ACTOR_QWEN_TOKEN` into thor's `prod.env`
3. sync `compose.thor.yml` to thor's checkout
4. recreate thor's `api` + `worker`
5. merge `NODES_ACTOR_QWEN_TOKEN` into orin's `prod.env`
6. relay `NODES_ACTOR_NOTIFY_TOKEN` thor → orin
7. sync `compose.orin.yml` to orin's checkout and recreate its worker
8. install the systemd unit on spark
9. register the actor

Both prod checkouts now sit detached with hand-modified tracked files. That is
visible in `git status` on each host and collected nowhere — which is half of
what #226 is for.

## Still true, still open

- **#220 is not delivered.** No qwen session produced a line of work.
- **#120 remains untested.** `--handover` was set on the working run; the result
  carries no `handover` key at all, because an empty change set gave
  `async_runner` nothing to attempt. The async handover path has still never
  created a ref in any bridge.
- **#18 remains unproven**, now for a second and unrelated reason.
- `NODES_ACTOR_JIRA_TOKEN` is still unclassified in `audit_classification()` —
  the orin audit warns about it. Small, pre-existing, unrelated to this work.

## Issues opened

| # | Type | What |
|---|---|---|
| #222 | Bug | register-actor.sh validates no `auth_token_env`; three files enumerate the list |
| #224 | Bug | two workers, two credential sets; notify-discord already affected in prod |
| #225 | Bug | async policy refusal loses its message (**fixed**, PR #223) |
| #226 | Feature | mesh awareness: what every machine is serving, and peer divergence |
| #227 | Bug | the bridge executed nothing — `input.mode` read by nobody (**fixed**, PR #223) |
| #228 | Bug | a permission-starved session reports `completed` |
