# Delivery Summary — Agents work freely as dedicated OS users per host

plan: `agents-as-os-users-243` · run: `complete` · date: `2026-08-29`
baseline: `devague summary skeleton`

## Intent

Issue #243: every worker actor runs as its own Unix account on its host —
`culture-codex` on thor and orin, `culture-claude` and `culture-qwen` on
spark — instead of inside a bwrap/docker sandbox, with a live proof through
nodes that an account can fetch, format, test and hand its work over. The
run executed the eleven-task plan seeded from frame
`agents-as-os-users-243` (spec `docs/specs/2026-08-29-agents-as-os-users-243.md`,
plan `docs/plans/2026-08-29-agents-as-os-users-243.md`): six wave-0 code
tasks fanned out to local subagents, t7 in wave 1, the operator's bootstrap
hand-turns, the three-host cutover, nine proof dispatches, and this record.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — lanes/unix-user.sh: bootstrap (root: useradd -m, chmod 750, linger, operator pubkey, copy engine creds), provision (as the account over ssh: pinned standalone codex/claude/qwen installs, per-role clones, env files under umask 077, inventory assertion), session-in-flight refusal, rollback pair in the summary; never touches /home/`<login>`/git; fake-host pytest harness tests/`test_deploy_unix_user.py`
- `t2` — confinement prose: the five adapters' capabilities.py prepend 'unix-user:`<name>`: ' from pwd.getpwuid(os.getuid()).`pw_name`; codex's `_REQUIRES_USERNS` becomes () so `sandbox_modes_unavailable` no longer blocks; five `test_capabilities.py` updated; preflight.py untouched
- `t3` — deploy/prod/codex-preflight.sh: check 7 (userns) downgrades to a warning; new check 8 refuses when id -nG contains sudo or docker or when an allowlisted checkout is not owned by the running uid; shell tests updated
- `t4` — register-actor.sh: --metadata `os_user`=`<name>` written by the deploy lane; `registeractor_test.go` pins that a re-register keeps prior keys and adds `os_user`
- `t5` — nodes-op.sh + nodes-operator SKILL.md: actor table repointed to /home/culture-`<engine>`/git/culture-nodes-`<role|agent>`; c42 comment rewritten as an ownership fact; .devague custody paths follow; SKILL.md lines 76-79 and 109-118 rewritten (write path is proven by c9's run ids, filled in after t11)
- `t6` — docs/deviations/2026-08-29-agents-as-os-users.md: what the sandbox fenced, what the account fences, what it does NOT fence (network egress — controls: Protect main ruleset, Contents-only worker token, bridge-trusted `base_ref`, the ledger), PRD 31/222 departure in PRD:1860 style, supersedes 2026-08-15-handover-fence.md; README deploy section + harvest runbook updated (ssh `<engine>`@`<host>`, old trees stay harvestable); linked from #243 as a Record
- `t7` — deploy.sh: spark host arm (bridge lanes only, local exec through ssh culture-`<engine>`@localhost), codex/claude/qwen bridge lanes routed through ssh culture-`<engine>`@`<host>` with `XDG_RUNTIME_DIR` from that account's id -u, old login-user units stopped+disabled, register-actor `os_user` call, install-secrets.sh writes bridge-push.env and re-issues the developer dial-in into the account, spark bridge configs rendered without `NODES_HUMAN_DECISION_TOKEN`; Go/pytest deploy guards updated
- `t8` — operator hand-turns: run the root bootstrap once per host (spark: '! sudo bash deploy/prod/lanes/unix-user.sh bootstrap claude qwen'; orin: over ssh with sudo; thor: NOPASSWD inside the lane) and post one #243 comment per typed sudo naming host and command
- `t9` — deploy thor, orin, spark with the new lanes and verify every host-level honesty condition by pasted command output: is-active under the account and inactive under the login user (h3), culture-codex active on thor (h11), ssh culture-claude@localhost ls /home/spark/git/culture-nodes denied (h13), codex login status / claude auth as each account (h14, h23), passwordless ssh to each account (h15), old checkout HEADs unchanged (h25), rollback pair exercised once on orin (h27), home/env modes (h28), inventory (h29), no decision token (h32), capabilities confinement prefix live (h5), registry `os_user` (h7)
- `t10` — live proof through nodes: nodes-op.sh assign codex-orin, codex-thor (workspace-write) and developer with a brief that fetches origin, runs black/isort/flake8 via uv, runs one adapter HTTP loopback test, commits and pushes the handover ref; read run + ledger for each, decide the claims through the approval surface, /remember an actor-quality note per actor
- `t11` — close the loop: fill the run ids into SKILL.md (t5 leaves a placeholder), /summarize-delivery record under docs/deliveries with the before/after facts cited to scope entries and the pasted host outputs, version bump, PR via the cicd skill, close #243 as Record pointing at the deviation and delivery records, update docs/triage

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `deploy/prod/lanes/unix-user.sh` (bootstrap, provision, session check, rollback pair) + `tests/test_deploy_unix_user.py`; merged `24e4c09`; hardened in-run by `bc8dd5a` (pgrep self-match), `27ccccb` (codex package with `codex-code-mode-host`), `1546b4e` (account codex config), `66a3b26` (git identity) |
| `t2` | delivered | five `capabilities.py` report `unix-user:<name>: …`; codex `_REQUIRES_USERNS = ()`; merged `349b364`; live: every bridge's `/v1/capabilities` starts `unix-user:culture-<engine>:` |
| `t3` | delivered | `codex-preflight.sh` check 7 advisory, check 8 account gate; merged `b8746e2` |
| `t4` | delivered | `register-actor.sh --os-user`; merged `065e063`; live: `os_user` on codex-thor/orin (rev 2), developer (rev 3), intake/planner/verifier/qwen-developer (rev 2) |
| `t5` | delivered | `nodes-op.sh` actor table + SKILL.md; merged `011e1de`; placeholder filled in `a610c14` |
| `t6` | delivered | `docs/deviations/2026-08-29-agents-as-os-users.md` + `deploy/prod/README.md` runbooks; merged `92cbb8e` |
| `t7` | delivered | `deploy/prod/lanes/account-bridges.sh`, `deploy.sh spark`, five spark config templates, `culture-nodes-qwen-developer.service`, `install-secrets.sh` account steps, `tests/test_deploy_account_bridges.py`; merged `53f915d`; `codex-bridge.json.template` gained the uv-under-/tmp env in `00efef8` |
| `t8` | delivered | the operator ran `deploy/prod/bootstrap-accounts.sh` (`9927919`) on spark, orin and thor; three hand-turn comments on #243 |
| `t9` | delivered | thor, orin, spark cut over; `docs/audits/2026-08-29-agents-as-os-users-cutover.md` pastes every host-level honesty condition (`6561eaf`); orin and thor redeployed twice more in-run for the uv env and parity |
| `t10` | delivered | nine dispatches (see Evidence); network, formatters, loopback tests and denials proven on both engines; handover refs minted by the bridges as the accounts and fetched by the operator as the accounts: `ce2046f` (culture-claude), `e0b6324` (culture-codex) |
| `t11` | delivered | SKILL.md run ids (`a610c14`), this record, version 0.44.0 (`d7cda1d`); the PR and the `Record` close of #243 follow this commit |

## Mid-work Decisions

- `d1` — t10 proof dispatches ran codex with `--sandbox danger-full-access`
  instead of the plan's workspace-write: codex's own workspace-write sandbox
  denies network regardless of the account, and `--handover` never pushes —
  approved; the two runs it produced are kept as the network/formatter/
  loopback/denial evidence.
- `d2` — the spec's "commit and push its handover ref" is replaced by the
  repo's own actor rule (AGENTS.md: never `git push` from a session, never
  commit onto a branch); the write path is a handover ref under
  `refs/culture-nodes/<run-id>` minted by the bridge as the account, with
  network enabled in the account's codex config — approved.
- The codex accounts install the standalone **package**, not the bare
  binary: codex 0.147 spawns `codex-code-mode-host` from beside itself, and
  the bare binary blocked the first two dispatches (`27ccccb`). No record
  covers this; captured here.
- Each account gets a git identity at provision: the bridge's handover
  commit failed "Author identity unknown" and reported `workspace_export`
  missing — a result the ledger never shows (`66a3b26`). Captured here.
- The codex account's uv cache/tools live under `/tmp/culture-codex/…`:
  codex's workspace-write makes `~/.cache/uv` read-only (`00efef8`).
- spark's actors were re-registered at its tailscale address after the
  20:16 restart moved its DHCP lease (#248) — outside #243's scope, done so
  the spark cutover and the developer proof could run.
- The `ask-colleague review` reflex was attempted twice and aborted both
  times: spark's vLLM backend served no model after the reboot
  (`HTTP 503 backend_unavailable`). No colleague review of this diff exists.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t10` (`d1`) | codex's own workspace-write sandbox denies network regardless of the account (the exact #230 failure), and `--handover` only mints a local ref on a fresh workspace-write session and never pushes | acceptable |
| `t10` (`d2`) | AGENTS.md forbids session pushes; the account posture must keep file confinement and add network, not drop confinement | acceptable |
| `t1` | four in-run fixes the fake-host harness could not have caught (pgrep self-match, codex package layout, git identity, uv cache under bwrap) — each now has a regression test | acceptable |
| `t9` | first `deploy.sh thor` refused on a phantom session (the t1 pgrep defect); thor was deployed three times and orin three times in total | acceptable |

## Evidence

- tests: `uv run pytest -n auto` — 673 passed, 6 subtests; `go test ./tests/deploy/... ./tests/lint/...` — ok; adapters claude-code 395 / codex 385 / colleague 325 / notify 167 / qwen 354 passed; `scripts/lint-all.sh` — all lint steps passed
- commits: `67032ce..a610c14` on `feat/agents-as-os-users-243` (25 commits)
- hosts: `docs/audits/2026-08-29-agents-as-os-users-cutover.md` (pasted probes, h3/h5/h7/h11–h15/h21/h23/h25/h27–h29/h32)
- runs (ledger, graded through `nodes-op grade`): `01M17DN04BTX9JAS3W3H9NJ7ZP` and `01M17DNK8XF6DAY8JMQ8E4V132` (blocked by the bare-binary defect, 3/5); `01M17E36Z7G8A60WP6TA9QB0AP` and `01M17E3ZC1MBQ6DZRKY93T8PWE` (danger-full-access: fetch, formatters, 29 loopback tests, denials, refused to push per AGENTS.md, 4/5); `01M17NJ8W7B6NQ2RKT6B89SF94` and `01M17NK19NY09G55J7AJWYW7XT` (workspace-write: network OK, uv cache read-only, 4/5); `01M17NM5C0HHZ2RTHPKWPS8H70` (developer, all steps, 5/5); `01M17P15BJ28XN57N7P7Q77Y48` (developer, handover ref minted, 5/5); `01M17P5CM2DPMWFNQHTRGQ16XQ` (codex-orin, workspace-write + handover, all steps + ref, 5/5)
- handover refs fetched as the accounts: `refs/culture-nodes/01M17P15BJ28XN57N7P7Q77Y48/…` → commit `ce2046f` by `culture-claude <culture-claude@spark-f8a9.culture-nodes.invalid>`; `refs/culture-nodes/01M17P5CM2DPMWFNQHTRGQ16XQ/…` → commit `e0b6324` by `culture-codex <culture-codex@orin.culture-nodes.invalid>`
- issues: #243 (three bootstrap hand-turn comments), #248 (spark address drift)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| every bridge on thor, orin and spark runs as an engine account with no sudo/docker membership, a 750 home, and the login user's home unreadable | high | cutover audit (`id -nG`, `stat`, `ls` denials pasted per host) |
| every advertising bridge reports `confinement: unix-user:culture-<engine>: …` | high | cutover audit h5 lines (read with each bridge's bearer) |
| an engine account has network, runs uv-installed formatters and loopback tests, under codex `workspace-write` as well as under claude | high | runs `01M17P5CM2DPMWFNQHTRGQ16XQ`, `01M17P15BJ28XN57N7P7Q77Y48` (proof files inside the handover commits) |
| the write path is proven: the bridge hands a session's change over as a ref authored by the account, fetchable by the operator as the account | high | commits `ce2046f`, `e0b6324` in this checkout's `refs/proof/*` (fetched from `culture-claude@localhost`, `culture-codex@orin`) |
| the rollback pair restores the previous posture without a deploy | high | cutover audit h27 (exercised on orin, both directions) |
| codex-thor hands over refs under the same posture | medium | thor carries the identical lane, identity, config and uv env after `deploy.sh thor` (`66a3b26` parity), but no `--handover` run was dispatched to thor after those fixes |
| the old login-user checkouts and their unpushed commits are intact | high | cutover audit h25 (`05b216e` orin, `2f2778f` thor) |
| the userns sysctl is optional for the fleet | unverified | not tested: all three hosts still carry `apparmor_restrict_unprivileged_userns=0`; check 7 is advisory by code, not by a host without the sysctl |

## Remaining Work / Follow-up

- codex-thor: dispatch one `--handover` proof to mint a ref there (same
  posture as orin; medium-confidence claim above).
- #248: spark's LAN address drifts with DHCP; thor/orin registrations are
  LAN addresses too — a name or reservation, and drift reporting in #226.
- A bridge's `handover` result never reaches the ledger (only the worker's
  copy of the response sees it): the "Author identity unknown" failure was
  invisible for three runs. Same shape as #120/#241 — file or fold into
  #241.
- The handover-fence deviation's premise is superseded; the LAN reach of an
  account (vLLM, nodes API, any published Postgres port) is recorded in the
  deviation record as unexamined, not fixed.
- Colleague review of this diff was not obtained (vLLM down); run
  `ask-colleague review` once the backend serves a model.
- `lanes/preflight.sh` still checks the login user's `~/git/culture-nodes-agent`
  state before a deploy (spec park v3); those trees are now dormant.
