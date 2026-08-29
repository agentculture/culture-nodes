# Build Plan — agents-as-os-users-243

slug: `agents-as-os-users-243` · status: `exported` · from frame: `agents-as-os-users-243`

> Agents work freely as dedicated OS users per host: every worker actor on spark and orin runs as its own Unix account — its own home, checkout, caches and engine credentials — instead of inside a bwrap or docker sandbox; a codex-orin workspace-write dispatch fetches its base, runs the formatters and loopback tests, commits and pushes its handover ref with no operator pre-fetch or harvest; and every capability surface reports confinement: unix-user:`<name>` so the ledger says which account ran what

## Tasks

### t1 — lanes/unix-user.sh: bootstrap (root: useradd -m, chmod 750, linger, operator pubkey, copy engine creds), provision (as the account over ssh: pinned standalone codex/claude/qwen installs, per-role clones, env files under umask 077, inventory assertion), session-in-flight refusal, rollback pair in the summary; never touches /home/`<login>`/git; fake-host pytest harness tests/`test_deploy_unix_user.py`

- covers: c2, h2, c26, h23, c30, h25, c31, h26, c32, h27, c33, h29, c34, h28, c24, h21
- acceptance:
  - bash -n passes and the lane is sourced by deploy.sh inside marker comments so `codexdeploylane_test.go` sees it
  - fake-host test: running provision twice yields byte-identical account state (idempotent) and only the bootstrap function calls useradd/loginctl
  - fake-host test: with a fake 'codex exec' process for the login user the lane exits non-zero before any systemctl stop; `SKIP_SESSION_CHECK`=1 warns and proceeds
  - fake-host test: /home/`<login>`/git/culture-nodes-agent is never written; HOME mode 750 and \*.env mode 600 are asserted by the lane and the test
  - the lane prints the stop/start rollback pair for each migrated unit and the per-account inventory (bridge token, actor token, dial-in, worker token, engine creds) and refuses if anything else is present

### t2 — confinement prose: the five adapters' capabilities.py prepend 'unix-user:`<name>`: ' from pwd.getpwuid(os.getuid()).`pw_name`; codex's `_REQUIRES_USERNS` becomes () so `sandbox_modes_unavailable` no longer blocks; five `test_capabilities.py` updated; preflight.py untouched

- covers: c5, h5, c6, h6
- acceptance:
  - each advertising adapter's --print-capabilities confinement value starts with unix-user:`<current user>`:
  - tests/lint byte-identity guards (`preflightsurface_test.go`, `workspacereaper_test.go`, `dialintransport_test.go`) still pass; preflight.py has zero diff
  - codex capabilities on a host with the userns sysctl restricted report the restriction in the prose but list no `sandbox_modes_unavailable`

### t3 — deploy/prod/codex-preflight.sh: check 7 (userns) downgrades to a warning; new check 8 refuses when id -nG contains sudo or docker or when an allowlisted checkout is not owned by the running uid; shell tests updated

- covers: c6, h6
- acceptance:
  - preflight passes on a host with the userns sysctl restricted (warning printed, exit 0)
  - preflight exits non-zero when run as a user in the docker group or when the checkout owner differs from the running uid

### t4 — register-actor.sh: --metadata `os_user`=`<name>` written by the deploy lane; `registeractor_test.go` pins that a re-register keeps prior keys and adds `os_user`

- covers: c7, h7
- acceptance:
  - `registeractor_test.go`: registering with `os_user` then re-registering with another key yields a row carrying both

### t5 — nodes-op.sh + nodes-operator SKILL.md: actor table repointed to /home/culture-`<engine>`/git/culture-nodes-`<role|agent>`; c42 comment rewritten as an ownership fact; .devague custody paths follow; SKILL.md lines 76-79 and 109-118 rewritten (write path is proven by c9's run ids, filled in after t11)

- covers: c4, h4, c12
- acceptance:
  - nodes-op.sh assign codex-orin --dry-run (or the payload print) ships repo=/home/culture-codex/git/culture-nodes-agent
  - grep 'only boundary there is' nodes-op.sh returns nothing; SKILL.md names the engine accounts and the harvest path ssh `<engine>`@`<host>`

### t6 — docs/deviations/2026-08-29-agents-as-os-users.md: what the sandbox fenced, what the account fences, what it does NOT fence (network egress — controls: Protect main ruleset, Contents-only worker token, bridge-trusted `base_ref`, the ledger), PRD 31/222 departure in PRD:1860 style, supersedes 2026-08-15-handover-fence.md; README deploy section + harvest runbook updated (ssh `<engine>`@`<host>`, old trees stay harvestable); linked from #243 as a Record

- covers: c8, h8, h19, c22
- acceptance:
  - the record names docs/deviations/2026-08-15-handover-fence.md as superseded and quotes PRD lines 31 and 222
  - the record has a section titled what the account does not fence naming network egress and the four controls
  - markdownlint-cli2 passes on the new and edited docs

### t7 — deploy.sh: spark host arm (bridge lanes only, local exec through ssh culture-`<engine>`@localhost), codex/claude/qwen bridge lanes routed through ssh culture-`<engine>`@`<host>` with `XDG_RUNTIME_DIR` from that account's id -u, old login-user units stopped+disabled, register-actor `os_user` call, install-secrets.sh writes bridge-push.env and re-issues the developer dial-in into the account, spark bridge configs rendered without `NODES_HUMAN_DECISION_TOKEN`; Go/pytest deploy guards updated

- depends on: t1, t3, t4
- covers: c3, h3, c25, h22, c27, h24, c35, h32, c11, c18
- acceptance:
  - tests/deploy Go suite and tests/`test_deploy_two_host.py` + `test_deploy_runner_env.py` pass; deploy.sh stays under the 1000-line guard
  - fake-host run of 'deploy.sh spark' installs the five spark units under the fake culture-claude/culture-qwen homes and never invokes compose or the runner lane
  - the rendered developer.json under the fake account has no `NODES_HUMAN_DECISION_TOKEN` key
  - nodes-runner and compose steps still execute as the login user on thor/orin in the fake-host run

### t8 — operator hand-turns: run the root bootstrap once per host (spark: '! sudo bash deploy/prod/lanes/unix-user.sh bootstrap claude qwen'; orin: over ssh with sudo; thor: NOPASSWD inside the lane) and post one #243 comment per typed sudo naming host and command

- depends on: t1
- covers: c10, h10, h12, c1, h1
- acceptance:
  - id culture-codex on thor and orin, id culture-claude and culture-qwen on spark succeed; id -nG for each lists neither sudo nor docker
  - \#243 carries one comment per host naming the command typed

### t9 — deploy thor, orin, spark with the new lanes and verify every host-level honesty condition by pasted command output: is-active under the account and inactive under the login user (h3), culture-codex active on thor (h11), ssh culture-claude@localhost ls /home/spark/git/culture-nodes denied (h13), codex login status / claude auth as each account (h14, h23), passwordless ssh to each account (h15), old checkout HEADs unchanged (h25), rollback pair exercised once on orin (h27), home/env modes (h28), inventory (h29), no decision token (h32), capabilities confinement prefix live (h5), registry `os_user` (h7)

- depends on: t7, t8, t2, t5
- covers: c18, h11, c12, h13, c3, h3, h5, h7, h23, h24, h25, h27, h28, h29, h32, c19, h16, c20, h17
- acceptance:
  - a scratch log (docs/audits/2026-08-29-agents-as-os-users-cutover.md) pastes the command and output for every listed h-condition per host
  - curl /v1/capabilities on each bridge (with its scoped token) shows confinement starting with unix-user:culture-`<engine>`
  - the three login-user bridge units are inactive and disabled; nodes-runner and compose are unchanged

### t10 — live proof through nodes: nodes-op.sh assign codex-orin, codex-thor (workspace-write) and developer with a brief that fetches origin, runs black/isort/flake8 via uv, runs one adapter HTTP loopback test, commits and pushes the handover ref; read run + ledger for each, decide the claims through the approval surface, /remember an actor-quality note per actor

- depends on: t9
- covers: c9, h9, c23, h20, h4
- acceptance:
  - three run ids whose ledger statements show fetch, formatters, loopback test, commit and handover ref push; a failed leg is recorded as failed, not smoothed
  - each proposed claim is confirmed or rejected through the approval surface by the operator, not by the actor
  - git ls-remote origin shows each pushed handover ref

### t11 — close the loop: fill the run ids into SKILL.md (t5 leaves a placeholder), /summarize-delivery record under docs/deliveries with the before/after facts cited to scope entries and the pasted host outputs, version bump, PR via the cicd skill, close #243 as Record pointing at the deviation and delivery records, update docs/triage

- depends on: t10, t6
- covers: c21, h18, c22, h19, h17, h20, c1, h1
- acceptance:
  - docs/deliveries/2026-08-29-agents-as-os-users.md quotes the run ids with ledger statements and the id -u / permission-denied outputs; every before-state fact cites an s-entry or issue comment
  - PR merged green on all lint/test/version-check jobs; #243 closed via scripts/close-issue.sh --artifact

## Risks

- [unknown_nonblocking] the operator's subscription window is shared by the interactive session, local subagents and every bridge session (#48): wave 0 fans out 6 local subagents, waves 2-3 add three bridge dispatches — declare the per-wave session count in the split plan and stagger t10's three dispatches
- [unknown_nonblocking] per-role clones under culture-claude may contend on one uv cache during parallel installs; the lane serialises installs unless measured otherwise (frame v4) (task t1)
- [unknown_nonblocking] an engine account with network can reach LAN services (vLLM, nodes API, published Postgres port if any); nothing in this cycle firewalls it — recorded in the deviation record, not fixed (frame v5) (task t6)
- [unknown_nonblocking] the fake-host harness models one HOME per host; t1 adds a per-account axis in its own test file rather than reshaping `test_deploy_two_host.py` (frame v2) (task t1)
- [unknown_blocking] bootstrap on spark and orin needs the operator's password: t8 blocks the whole deploy wave until typed; thor does not (task t8)
- [unknown_nonblocking] spark bridges flip from editable installs to stamped copies (#120): a fix to a bridge on spark no longer takes effect until deploy.sh spark re-runs — the runbook must say so (task t7)
