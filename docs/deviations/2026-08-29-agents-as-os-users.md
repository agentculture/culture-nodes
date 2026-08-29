# Agents work as dedicated OS users, and the sandbox stops being the fence (#243)

Status: proposed. Supersedes `docs/deviations/2026-08-15-handover-fence.md`
— its conclusion, not its measurements, which still stand for the posture
they measured.

Spec: `docs/specs/2026-08-29-agents-as-os-users-243.md` (claim ids below
cite that frame's `c`/`h` records; `s` cites its scope-exploration entries,
which carry the file:line and host probes each fact rests on).

## What the sandbox fenced, and what it cost

Until this cycle every bridge on a host ran as the single login user
(`spark`, `thor`, `orin`) that also owns the control plane, the docker group,
the backups and every secret (s10). Codex sessions ran inside a bubblewrap
sandbox with no network, no Postgres, no loopback socket and a read-only
`uv` cache; claude, colleague and qwen sessions had no kernel boundary at
all, and the exact-match repository allowlist was the only thing keeping
them out of the operator checkout (s4, s7).

The sandbox bought one guarantee, and `2026-08-15-handover-fence.md` said
it plainly: **the handover was unfenced by design and unreachable in
practice.** A session could attempt `git push origin`; it could not succeed,
because there was no route out. That record also named the condition under
which its own conclusion would expire:

> If a bridge ever runs where a dispatch has network, the ruleset design
> comes back, and t9's two criteria come back with it.

That condition is now met on purpose. The premise — the sandbox's lack of
egress *was* the handover fence — no longer holds, because the sandbox is
no longer where a dispatch runs. What replaces the fence is listed below,
and the ruleset half of it is exactly what the superseded record predicted.

The cost of the fence was measured on the jira-flow cycle (#230), and every
lane defect in its wave 0 and wave 1 was a sandbox limitation, not an agent
limitation. The evidence table from #243, by run id:

| Problem the account solves | Evidence (from `gh issue view 243 --comments`) |
|---|---|
| No network in the sandbox | runs `01M1679Q0RAXMEV7KJ6H3S15C8` (codex-thor) and `01M1679WR9KX9TTRMJA5BNH4X3` (codex-orin), 2026-08-29: both completion claims read *"Blocked at mandatory Step 0 … Could not resolve host: github.com"* — #230 hand-turn 3; the identical pre-fetch hand-turn is #203 hand-turn 1 (2026-08-19). Root design gap tracked as #242's bridge-trusted `base_ref` (WP-B2, commit `5801c31`). |
| No Postgres / Docker in the sandbox | run `01M167BTTQ69VHME0B06YDTC32` (WP-A): *"PG-backed tests were discovered but skipped because neither NODES_TEST_DATABASE_URL nor Docker access is available"* — four schema-invalid fixture tests, fixed at the gate in `bec177a`; run `01M16BB5YFC0SAZ13P88KG37YA` (WP-D): three PG tests failing at the gate. #231 shipped untested for the same reason on 2026-08-19. |
| Formatters and linters unrunnable (`uv` cannot lock the read-only cache) | claims on `01M167C1DYPPJ1K0YM2ZYQ10Y7`, `01M167BTTQ69VHME0B06YDTC32` and the `01M16AWGNM2K04YB94R1PRD6Z9`-era runs: *"black/isort/flake8 were unmeasurable"* — gate fixups `b4a16a9`, `81a1ddc`, `c5d2efd`. |
| Loopback denied | the same claims: *"HTTP-level tests could not start because the sandbox prohibits loopback sockets"* / *"socket(2) denial"* — every `internal/api` and bridge server test. |
| No `npm` on orin | the WP-E run (t11, commit `6a7841b`): *"Vitest/Playwright could not run because this host has no npm executable or web/node_modules"* — 11 TypeScript errors found only at the gate (`974673b`). |
| Work left uncommitted, ref never pushed | WP-C claim: *"Changes remain uncommitted for handover"* — committed over ssh as the actor (#230 hand-turn 5); every package harvested by `git fetch <host>:git/culture-nodes-agent <branch>` because the run output carries no ref (#230 hand-turn 4). |
| Blocked session reported as `completed` | the two step-0-blocked runs above ended `completed / succeeded` (#230 hand-turn 3) — outcome mapping, not sandboxing, but the sandbox caused the block. |
| Bridge restart kills in-flight work | developer run `01M16B2QR2JP1EPBFZJAWTX61J` lost when the operator restarted `culture-nodes-claude-developer` (exit 143) to pick up #207's fix; #120's `revision_is_dirty` names the same hazard. |
| Write path never proven under the sandbox | #18 (open since the first codex dispatch), #63 (userns sysctl needed for bwrap at all), #203 "the bridge write path is unproven". |
| Shared checkout with the operator | the `nodes-op.sh` comment on the c42 concurrent-writer mode (developer used to default to the operator's checkout). |
| Qwen lane | `docs/deliveries/2026-08-27-qwen-bridge-first-dispatch.md`: nine credential/deploy hand-turns, zero lines of work; #225/#227/#228 all confinement-shaped ("the ACP modes are an approval policy, not a kernel boundary"). |

## What ships instead

One Unix account per engine per host: `culture-codex` on thor and orin,
`culture-claude` and `culture-qwen` on spark (c2, c10, c24). Each account
has its own home, its own clone(s) of the repository, its own engine
binary, caches and copied engine credentials, and its bridge units are
installed into *that account's* `systemd --user` instance (c3). The bridge
process itself is the actor user — there is no `setuid`, `runuser` or
`systemd-run` seam inside the bridges, and the unit files are unchanged
byte for byte (c13). Every advertising capability surface reports
`confinement: unix-user:<name>`, read from the running uid, so the ledger
says which account ran what (c5); the actor registry carries
`os_user=culture-<engine>` as metadata (c7).

The proof this cycle asks for is one codex-orin `workspace-write` dispatch
and one spark developer dispatch, sent through `nodes-op.sh assign`, whose
ledgers show a `git fetch`, the three formatters, a loopback HTTP test, a
commit and a pushed handover ref (c9). Those run ids belong in the delivery
summary, not here: this record is written before the deploy, and it does
not claim them.

## Departure from the PRD

The PRD's runtime picture puts code in a container. Line 31:

> Use **headspace-cli as the first code-runner integration**. Code nodes
> execute typed operations in disposable Docker headspaces and return
> structured results plus runner-observed evidence.

and line 222, in the goals list:

> 17\. Execute code through headspace-cli without granting the orchestration
> worker a shell or Docker socket.

This cycle departs from the first sentence for **actor bridges**: a codex,
claude, colleague or qwen session runs as a Unix account on the host, not
in a disposable Docker headspace or a bubblewrap sandbox. The departure is
precise, and it is narrower than it sounds:

- **Actor bridges are actors and adapters, not the generic worker.** PRD
  §16.4 (lines 1648–1661) constrains what *the generic worker* must never
  do — mount the Docker socket, execute repository scripts directly,
  evaluate arbitrary shell — and says the code adapter "is a separate
  deployment and security boundary from the generic worker". That section
  is not touched. `nodes-runner`, compose (`prod-worker-1`, `api`,
  `scheduler`), the backups and `prod.env` stay under the operator account
  on every host; no actor account is in `docker` or `sudo` (c11, h2). Line
  222's promise about the orchestration worker still holds exactly as
  written; ADR-0005 (no Docker socket) is unchanged for the same reason.
- **The headspace runner boundary is unchanged.** Code nodes that go
  through the headspace-cli adapter still run where the PRD says. What
  moves is the agent session, which the PRD never placed in a headspace —
  it placed *code* there, and an agent session was already a host process
  under the login user before this cycle. The change is which user, and
  what that user can reach.
- **Reduced isolation is made explicit rather than drifted into.** PRD
  line 1860 gives the phrasing for the local profile: "uses a separate
  runner boundary and makes that reduced local isolation explicit". This
  record does that for the actor fleet. The reduced isolation is the
  removal of the network and filesystem sandbox around a session; the
  boundary that remains is POSIX ownership, and the next two sections say
  what that boundary does and does not cover. PRD lines 1490 and 2291
  ("Docker provides isolation, not truth"; "Docker isolation is not treated
  as evidence of truth") already refuse to let the sandbox stand in for the
  ledger, so nothing in the evidence model changes: an agent's "done" was a
  completion claim before and is a completion claim now (§10.4).

## What the account fences

- **No `sudo`, no `docker` group.** `id -nG culture-<engine>` lists neither
  on any host, asserted by the deploy lane before a bridge is started under
  the account (h1, h2). An account in either would void the confinement
  claim, which is why the runner and compose stay with the login user.
- **0750 homes.** `useradd -m` then an explicit `chmod 750`, asserted
  rather than assumed from `login.defs` (c33). Engine accounts cannot read
  each other or the login user.
- **The operator checkout is unreadable.** `/home/spark` is already
  `drwxr-x---`; `ssh culture-claude@localhost ls /home/spark/git/culture-nodes`
  is denied (h13). The exact-match repository allowlist stops being the
  only thing between a session and the operator's tree — it is now a
  path the session cannot open.
- **Credentials scoped by file mode.** An account's `~/.culture-nodes`
  holds only what its bridge needs — bridge auth token, actor token, dial-in
  credential, the worker GitHub token, the copied engine credentials —
  written under `umask 077`, and never `prod.env`, runner secrets, the
  operator's `gh` or Jira credentials, or the operator's ssh private key
  (c12, h29). `ssh <engine>@<host> cat /home/<login>/.culture-nodes/prod.env`
  is denied.
- **No human decision token in an agent session.** The developer bridge's
  `claude_env` used to carry `NODES_HUMAN_DECISION_TOKEN` — the bearer that
  makes *human* decisions on the approval surface. Inside a sandbox with no
  egress that was a latent authority hazard; inside a networked account it
  is an exfiltrable human credential. The key is dropped from the
  re-rendered `developer.json`, and `grep -rl NODES_HUMAN_DECISION_TOKEN
  /home/culture-claude` finding nothing is a post-provision assertion (c34,
  h32). This is PRD §10.4 applied: no actor promotes its own proposal, and a
  session that tries the approval surface as a human is refused.

## What the account does not fence

**Network egress.** A session running as `culture-codex` or
`culture-claude` has the host's network. It can `git fetch origin`, resolve
`api.github.com` and `pypi.org`, bind a loopback socket, and — with the
worker token it is given — `git push` to GitHub. Every one of those was
impossible under the sandbox, and the first three are the whole point.
The last is the one the superseded record fenced, and it is now fenced by
four controls that live outside the session instead of around it:

1. **The `Protect main` ruleset.** Rules `deletion`, `non_fast_forward`
   and `pull_request` on `~DEFAULT_BRANCH`, with **no bypass actors** —
   re-read from `gh api repos/agentculture/culture-nodes/rulesets/20588587`
   on 2026-08-29 while writing this record. A networked worker can push a
   branch; it cannot land on `main` without a pull request, and it cannot
   delete or force-push the branch it did not create. Classic branch
   protection is absent (the `branches/main/protection` API returns 404);
   the ruleset is the control (s14, decision 2).
2. **A Contents-only `GITHUB_TOKEN_WORKER`.** Each codex account's
   `bridge-push.env` carries a token scoped to repository contents and
   nothing else — no issues, no pull requests, no workflows, no
   administration — so a session that pushes a branch still cannot open,
   approve or merge the PR that would carry it to `main`, and cannot edit
   CI configuration through the API (c27). The token is absent on both
   codex hosts today, which is why no handover ref was ever pushed.
3. **The bridge-trusted `base_ref` (#242).** The scope guard used to trust
   the session-set `@{upstream}` as its baseline, so a committed `.github`
   edit could bypass it. #242 closed that: the bridge fetches and pins the
   base itself, and a networked account is what lets it do so without an
   operator pre-fetch. The guard's verdict on what a session changed no
   longer depends on anything the session could rewrite.
4. **The ledger.** A pushed ref is a `proposed` handover, not evidence. The
   run's ledger carries the confinement prose (`unix-user:<name>`), the
   registry carries `os_user`, and the gate reads the actual diff between
   the bridge-trusted base and the handover ref. Nothing a session says
   about its own work is promoted by the session (§10.4).

Stated as the superseded record stated its own trade: the fence was
**local and silent** — a config change or a codex-cli default could restore
egress with nothing to notice. The four controls above are **remote and
loud** — a ruleset is enforced by GitHub, a token scope is enforced by
GitHub, the base is pinned by the bridge, and the ledger is read by a
person. That is the stronger shape t9 of #90 originally wanted; it costs
one ruleset (already present) and one credential per codex account.

**LAN reachability, unexamined.** An engine account with network can also
reach every service on the LAN: the vLLM endpoints (`:8000`/`:8001`), the
nodes API, and a Postgres port on thor if one is ever published. Nothing in
this cycle firewalls that. The account holds no database credential and
the API needs the account's own actor token, so reachability is not
access — but the reachability itself has not been measured, and this
record does not claim it is harmless. It is an open park on the frame,
carried here so that the trade is explicit rather than implied.

## What becomes optional

The #63 chain — `kernel.apparmor_restrict_unprivileged_userns=0` on every
host, the `bwrap --unshare-user` probe, `codex-preflight.sh` check 7,
`nodes doctor`'s `unprivileged_userns` check — existed to make bubblewrap
degradation loud, because a codex `--sandbox workspace-write` dispatch
would otherwise silently lose every file write while still running shell
commands. Under a Unix-account boundary that chain is no longer
load-bearing for codex, and it was never relevant to claude, colleague or
qwen (s6).

So: the userns sysctl becomes **optional**, and the codex preflight's
userns check becomes **advisory** — it warns and reports the fact in the
capability surface, and no longer refuses the deploy or blocks dispatch.
What refuses instead is an account gate: the account exists, is in neither
`sudo` nor `docker`, and owns the allowlisted checkout (c6). The probes,
the doctor check and `GRANT_NESTED_CONFINEMENT` all stay, because
`internal/repair` fails closed on an absent key and because a host that
*can* still nest a sandbox should say so. Codex keeps its `--sandbox`
argument; `workspace-write` becomes redundant but harmless (c15).

## What this does not change

- The bridge argv, the four `Popen` sites and the zero-runtime-dependency
  guard: the bridge process is the account, so no per-user spawn seam is
  needed and none is added (c13, c15).
- `notify`, `jira` and `human-inbox` do not gain unix-user confinement
  reporting; `notify` still sources `prod.env` on thor and stays with the
  login user (c14, open park).
- The old agent checkouts under `/home/<login>/git/culture-nodes-agent` on
  thor and orin — with their unpushed commits and unmerged branches — are
  never touched by the lane; the account clones fresh, and the operator
  harvests the old trees exactly as before until they are emptied (c30).
- Rollback is one command pair per host, no deploy: `systemctl --user stop`
  under the account, `systemctl --user start` under the login user. The old
  units are disabled, never removed (c32).
- The cutover refuses while a session is in flight, because a restart
  mid-session kills the run and leaves it `running` in the ledger (#230
  hand-turn 8, exit 143) (c31).

## Provenance

- Live hosts probed 2026-08-29 (s10): no engine accounts existed; bridges
  ran as the login users under `--user` units with linger; `sudo -n` needed
  a password on spark and orin and was `NOPASSWD` on thor; all three login
  users held `docker`; the userns sysctl was `0` fleet-wide; orin had no
  `npm`.
- Ruleset and token scope (s14): copied engine credentials have coexisted
  across hosts since 2026-08-19 (`~/.codex/auth.json` mtimes), and `main`
  is PR-only by ruleset with no bypass.
- The handover-fence record's measurements (runs
  `01M039NZ2TZYFG68YZT93A6DC7`, `01M03CD5V0WE9CBSHBM1Y3E9S7`) are not
  disputed; they describe the sandbox posture this record retires.
