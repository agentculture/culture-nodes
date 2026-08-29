# Fleet and bridge evaluation — the jira-flow build cycle (2026-08-29)

An audit of **what the bridges and culture-nodes actually did** while
building `docs/plans/2026-08-29-jira-flow-spec-read-related-bugs.md`
(dispatch log #230): what worked, what cost operator hand-turns, and what to
change so the next cycle runs with less of the operator's hands in it. Every
row cites a run id, a commit, or an issue; nothing here is recollection.

Scope: waves 0–1 (six codex packages, two developer packages, two colleague
write runs, three colleague reviews), t7 (the r4 prod deploy), and the first
hour of t8 (the live proof). Written mid-cycle by the operator lane; the
delivery summary (`/summarize-delivery`) will cite it.

## 1. The numbers

| Measure | Value |
|---|---|
| packages delivered by the fleet | 8 (WP-A…WP-H minus the colleague pair) — every one merged |
| billable runs wasted on lane defects, not work | 4 (#240 ×2, no-network step 0 ×2) |
| gate fixup commits the workers could not produce themselves | 11 (formatters ×5, PG-only test defects ×3, TypeScript ×1, stale openapi.json ×1, import-path flake ×1) |
| operator hand-turns logged on #230 | 14 by the end of t7 |
| deviations recorded through `/deviate` | 2 (d1 colleague → codex; d2 #242 hardening), both approved |
| colleague as writer | 0 of 2 delivered (runs `11b1701688e2`, `4229519b496c`, 1/5 each) |
| colleague as reviewer | 3 of 3 found real defects (`a51591c92f56`, `7eeb2ac593c2`, `c3b74cf74a72`, 5/5 each): a scope-guard bypass (#242), a dispatcher deadlock, a refspec injection surface |
| prod api downtime during t7 | ~47 min (09:22–10:09Z), across two fail-closed stops |
| issues filed from the cycle | #240 #241 #242 #243 |

## 2. What worked

- **Codex on well-specified Go/Python packages.** WP-A (t3+t6+t9, 20
  files), WP-D (t10+t12), WP-B2 (#242 across three bridges), WP-B/WP-C/WP-D
  on orin: all merged; the code was right where the brief named files and
  contracts. Sessions of 30–45 min per package.
- **Codex's honesty.** Every completion claim listed what it could *not*
  run ("PG-backed acceptance is not claimed as passing"; "changes remain
  uncommitted for handover"; "Blocked at mandatory Step 0"). The gate could
  plan around each line. The failure is never the actor lying; it is the
  bridge reporting a blocked or partial session as `completed`.
- **The developer (claude) lane without a sandbox.** WP-F (t2, 782 lines
  incl. a 336-line harness) arrived **needing no gate fixup** — the only
  package that did. It has network, a warm `uv` cache and a loopback, i.e.
  the conditions #243 asks for everywhere.
- **Colleague as the second mind.** Three reviews, three real findings,
  each turned into a fix within the same wave. As a writer under GPU
  contention it produced nothing; route accordingly.
- **The devague gates.** Two mid-run departures were named, approved and
  recorded before anyone built against them; the delivery summary will
  quote `d1`/`d2` rather than reconstruct drift.
- **deploy.sh's fail-closed design (t2).** Both prod stops happened *before*
  the point of no return, with a pre-migrate dump on disk each time, and the
  restore path had been rehearsed on spark against a real dump.
- **The live proof paid for itself in one pass**: signal 8 (page reply →
  fact → outbox → Jira comment → projection) proven end to end, and the
  fresh-issue regression (§3.10) — invisible to every unit test — surfaced
  on the first real ticket.

## 3. What cost hands, and the fix for each

Ordered by how much of the operator's time each consumed.

### 3.1 The codex sandbox has no network — every dispatch is pre-fetched by hand

Runs `01M1679Q0RAXMEV7KJ6H3S15C8` and `01M1679WR9KX9TTRMJA5BNH4X3` both blocked
at step 0: `git fetch origin` → `Could not resolve host: github.com`. The
same pre-fetch was #203's hand-turn 1 on 2026-08-19; nothing had changed.
**Fix landed:** WP-B2's bridge-trusted `base_ref` — the bridge (a host
process with network) fetches the base before the session (#242, commit
`5801c31`). **Structural fix:** #243.

### 3.2 No Postgres, no formatters, no loopback, no npm in the sandbox

Every PG-backed test "discovered but skipped"; `uv` cannot lock its cache;
`socket(2)` denied; orin has no `npm`. Consequences: a schema-invalid test
fixture (`bec177a`), three failing PG tests in WP-D, eleven TypeScript errors
in WP-E, five formatter commits. **Fix:** a deterministic **gate node** —
formatters, `scripts/lint-all.sh`, `regen-openapi-json`, the PG suite, web
build+vitest — run by the runner on the harvested branch *before* the
operator looks, its output recorded as `observed` evidence. This is the
single largest reducer of gate effort after #243.

### 3.3 The assign template broke every non-qwen dispatch (#240)

PR #223 added an unconditional `mode: /run/input/mode` binding; the input
omits `mode`; the worker refuses a binding to an absent member
(`internal/worker/bindings.go:276`) — `contract_rejected` in 150 ms, no
bridge call. Two runs lost. **Fix landed** (`3a47b4a`). **Gap:** the assign
verb has no self-test against a deployed control plane.

### 3.4 `contract_rejected` carries no reason anywhere (#241)

Diagnosing 3.3 took a workflow-source diff and an engine read. The same
shape recurred twice more: the ticket-report dispatcher's `resolve bridge`
failure (§3.9) and the scheduler's tick errors are returned, not logged, and
the outbox row has no error column. **Fix:** reason on the attempt record and
event; `last_error` on outbox rows; tick errors logged.

### 3.5 A blocked session is reported `completed`

The two step-0-blocked runs ended `completed / succeeded` with a claim that
says "Blocked". **Fix:** the codex bridge maps a session that did no work to
a domain outcome (`blocked`) with an edge, never to completion.

### 3.6 Work left uncommitted; the handover ref never leaves the host

WP-C: "Changes remain uncommitted for handover" — committed over ssh as the
actor. Every package harvested with `git fetch <host>:git/culture-nodes-agent
<branch>` because the run output carries no ref. **Fix:** the bridge commits
on exit when `handover=true` and pushes the branch from the host (it has the
credential), recording the ref in the run output.

### 3.7 Bridge restart kills in-flight sessions; the run never learns

The operator restarted `culture-nodes-claude-developer` to pick up a merged
fix and killed WP-F's session (run `01M16B2QR2JP1EPBFZJAWTX61J`, exit 143);
the run stayed `running`. The unit runs from the **operator's checkout**
(`uv run claude-code-bridge` in `adapters/claude-code`), so it serves
whatever branch the operator is on. **Fix:** SIGTERM drains (preserve, then
`failed(reason=bridge_restart)`); per-actor checkouts (#243) end the
shared-tree hazard.

### 3.8 t1's fake-host harness proved presence, not runnability

`deploy.sh`'s rewritten `runner.env` carried a literal `$HOME`; the harness
asserted the keys survived, not that the runner could start on the file.
Prod's runner sat in auto-restart until patched by hand (hand-turn 11; fix
`82d8bc5`). **Rule for briefs:** acceptance must include "the consumer starts
on the artifact", not only "the artifact contains X".

### 3.9 Two services, two token sets — on one host this time

The ticket-report dispatcher moved to the active scheduler in wave 0; the
scheduler container had no `NODES_ACTOR_*` environment, so every page-reply
mirror failed at `resolve bridge` and backed off silently (hand-turn 14; fix
`ae48e40`). #224 was the same defect across hosts. **Fix:** a compose-parity
test across *services* that invoke actors, not only across hosts.

### 3.10 A freshly created issue is invisible to the history-faithful emitter

SCRUM-5 (0 changelog entries) produced no fact across five passes: the
timeline is built from changelog + comments, so creation is not a position.
`jira-intake`'s `transitioned.to-do` can never fire for a new ticket — a
regression of the #193 cycle. WP-H (codex-orin) is fixing it as "creation is
history position zero". Found only because t8 ran on a real ticket.

### 3.11 `nodes-cutover` had never met prod

The host-side adopter read `prod.env`'s compose-internal `postgres` hostname;
unresolvable from the host; stopped fail-closed with the api down (hand-turn
12; fix `5d9be94`). **Fix:** a connectivity dry run in PREFLIGHT.

## 4. Routing verdicts (for the next split plan)

| Worker | Good at | Not for |
|---|---|---|
| codex-thor | Go packages with named contracts; PG-tested code written blind | anything needing a running DB or lint in-session |
| codex-orin | Python bridges, adapters, sweep code; the web route was correct but untested | anything needing `npm` |
| developer (claude) | ops/deploy judgement, custody, anything needing network or a warm cache | shared-checkout hazards until #243 |
| colleague | reviews and explores (3/3) | writing under GPU contention (0/2); one run at a time |
| operator lane | merge gates, deploy, live proofs, adjudication | mechanical fixups that a gate node should do |

## 5. The automation backlog this cycle produced

In order of hand-turns saved:

1. **#243** — agents as OS users, no sandbox (removes §3.1, §3.2, §3.6,
   §3.7's shared checkout).
2. **Gate node** (§3.2) — formatters + lint + regen + PG suite + web build on
   the harvested branch, recorded as observed evidence.
3. **#241** — rejection/retry reasons on records, tick errors logged.
4. Bridge outcome mapping (§3.5) and commit-on-exit + push (§3.6).
5. Bridge drain on SIGTERM (§3.7).
6. Compose-parity test across services (§3.9); `nodes-cutover --check` in
   PREFLIGHT (§3.11).
7. Assign-verb self-test against the deployed control plane (§3.3).

## 6. Residual

- Signal 2 (a human typing in Jira) cannot be produced from the operator
  lane — the only credential is the system account, whose comments are
  self-echo by design. It stays `unverified` until a human account posts.
- Whether the codex bridge serializes concurrent runs per host was never
  measured; the cycle treated one session per host as the safe assumption.
