# Build Plan — post-merge prod reconciliation (#128)

slug: `post-merge-prod-reconciliation-128` · status: `exported` · from frame: `post-merge-prod-reconciliation-128`

> Prod on thor runs the same revision as main, and the two prod.env keys written outside the deploy tooling during the last deploy are either owned by tooling or written down as deliberately manual

## Tasks

### t1 — Fix the sed-delimiter hazard once, in the shared `PROD_ENV_MERGE` definition, and update its pinned test text deliberately

- instruction: Edit ONLY the `PROD_ENV_MERGE` definition in deploy/prod/install-secrets.sh and the prodEnvMergeLoop const + related assertions in tests/deploy/`prodenvmerge_test.go`. The hazard: the loop uses sed -i "s|^${k}=.\*|${line}|", so a replacement line containing a pipe makes sed exit 1 ('unknown option to s') and leaves the file byte-identical while the merge still reports success — the remote loop has no set -e. Probe it first to see it fail. Pick a substitution that cannot collide with a value (a bash parameter-expansion rewrite of the line, or a delimiter no env value can contain); do NOT add a second copy of the loop — tests assert exactly one. Run: go test ./tests/deploy/ -run ProdEnv.
- covers: c31, h24, c32, h25
- acceptance:
  - A prod.env value containing a pipe is merged correctly instead of being silently skipped, proved by a behavioral test against the fakeCluster harness
  - tests/deploy/`prodenvmerge_test.go` still asserts exactly ONE copy of the merge loop, with prodEnvMergeLoop updated to the new text
  - All three prodEnvWriters still delegate to $`PROD_ENV_MERGE`

### t2 — Add the add-if-absent deployment-settings lane to install-secrets.sh, composing `NODES_DATABASE_URL` on the host with sslmode already resolved

- instruction: Follows t1. Add the lane AFTER the two `install_env` calls in deploy/prod/install-secrets.sh. Add-if-absent only (filter out keys prod.env already has, then pipe the survivors through the ONE shared `PROD_ENV_MERGE`). Compose `NODES_DATABASE_URL` remotely from prod.env's own `POSTGRES_PASSWORD` so no password crosses the wire; refuse by name when that key is absent. CRITICAL (challenge finding c29): write a LITERAL sslmode value read from `DATABASE_SSLMODE` (default disable) — never a ${`DATABASE_SSLMODE`} placeholder, because compose interpolates env-file values recursively and resolves the placeholder to EMPTY, silently, when the referenced key lands later in the file. Report the keys actually added.
- depends on: t1
- covers: c8, h5, c29, h23, c33, h26, c21, h10
- acceptance:
  - Running install-secrets.sh unforced against an existing prod.env adds the missing settings keys and rotates NO generated secret
  - `NODES_DATABASE_URL` is composed from prod.env's OWN `POSTGRES_PASSWORD`, read on the host, so no password crosses the wire or enters an argv
  - The written URL carries a literal sslmode value, never a ${`DATABASE_SSLMODE`} placeholder, so key order in the file cannot change its meaning
  - A host whose prod.env has no `POSTGRES_PASSWORD` is refused by name rather than given a URL with an empty password
  - The lane names the keys it actually added, and says so distinctly when it added none

### t3 — Behavioral tests for the settings lane, seeded from a prod.env that lacks the key

- instruction: Follows t2. Extend tests/deploy/`prodenvmerge_test.go` using the existing fakeCluster harness. The accretedProdEnv fixture already lacks `NODES_DATABASE_URL`, which is exactly the pre-t15 shape — seed it, run install-secrets.sh UNFORCED, and assert the key appears composed from 'old-generated-postgres-password' while `POSTGRES_PASSWORD` and every other generated secret keep their seeded values and every accreted key survives byte-for-byte.
- depends on: t2
- covers: c28, h22
- acceptance:
  - A test seeds prod.env WITHOUT `NODES_DATABASE_URL`, runs install-secrets.sh unforced, and asserts the key appears composed from the SEEDED password while every generated secret keeps its seeded value
  - A test asserts docker-compose-style resolution yields a non-empty sslmode regardless of where the key lands in the file
  - A test asserts every accreted key survives the settings lane byte-for-byte

### t4 — Document the settings lane and the delimiter rule in deploy/prod/README.md

- instruction: Edit ONLY deploy/prod/README.md. Add the settings lane to the 'prod.env is merged, never rewritten' section: a newly-required deployment setting reaches an already-provisioned host by re-running install-secrets.sh, with no `FORCE_PROD` rotation — that guard exits before the merge, which is issue #124. Also record that the lane is add-if-absent, so it never overwrites an operator's edited `NODES_DATABASE_URL` or `COMPOSE_PROFILES`, and correcting a wrong value therefore means remove-secret.sh first, then re-run. Do not describe the lane's internals beyond that contract. markdownlint-cli2 must pass.
- covers: c25, h19
- acceptance:
  - The README states that a newly-required deployment setting reaches a provisioned host by re-running install-secrets.sh, with no `FORCE_PROD` rotation
  - The README records that add-if-absent never overwrites, so correcting a wrong value goes through remove-secret.sh first

### t5 — Reconcile the hosts: re-run install-secrets.sh on thor and orin, then deploy.sh orin, and verify the pair is no longer split-revision

- instruction: Operator-only, after t2 and t3 merge. Check /v1alpha1/runs for non-terminal runs FIRST — deploying a host whose bridge serves an in-flight run orphans the session. Then install-secrets.sh (unforced) against both hosts, then deploy.sh orin. Verify: deploy exits 0, orin's prod-worker-1 start time is after it, thor's /v1alpha1/version still equals origin/main, and orin's prod.env key set gained `NODES_DATABASE_URL` with no generated secret changed — compare key SETS only, never read a value.
- depends on: t2, t3
- covers: c1, h1, c22, h11, c26, h20, c24, h18, c2, h2, c3, h3, c13, h8
- acceptance:
  - deploy.sh orin exits 0 and orin's prod-worker-1 start time is after that deploy
  - thor's /v1alpha1/version still equals git rev-parse origin/main afterwards
  - orin's prod.env gained `NODES_DATABASE_URL` with no generated secret changed, verified by key-set diff without reading any value

### t6 — Record the operator hand-turns and the live #125 evidence on their issues

- instruction: No repo files. Comment on the existing issues via the cicd/communicate lane so the signature is appended automatically. On #124: orin reproduced the defect on 2026-08-16 — deploy.sh orin exited 1 with 'error while interpolating services.worker.environment.`NODES_DATABASE_URL`: required variable `NODES_DATABASE_URL` is missing a value', leaving prod-worker-1 on a pre-68024ac image while thor's control plane ran 68024ac; this is the SECOND hand-turn the same guard has caused. On #125: runs 01M04P8DGCX33AFTBQXX1SJR2Y and 01M04Q26ZPNKTXVNBGSDS1YR9F both failed `contract_rejected` ~1s after create, because developer.json's `repo_allowlist` holds two entries (owe-developer, upkeep-lane) so Config.`only_allowed_repo`() returns None — config.py names #125 in its own comment. Zero tokens billed. Do NOT change the allowlist; the owner decided it stays #125's call.
- covers: c27, h21, c18, h9, c19, h17
- acceptance:
  - \#124 carries orin's failure as a second occurrence, with the compose interpolation error quoted
  - \#125 carries the two failed run ids, the allowlist contents, and the config.py line that names it
  - Each record cites the commit or run it came from, so the trail runs both ways

### t7 — Bump the version and prepend a CHANGELOG entry

- instruction: Use the repo's /version-bump skill (patch), which updates pyproject.toml and prepends the CHANGELOG entry. Do this near the end, after the code tasks merge, so the entry can name what actually landed.
- acceptance:
  - pyproject.toml's version is bumped and CHANGELOG.md carries a Keep-a-Changelog entry naming the settings lane, the delimiter fix and issue #124
  - The version-check CI job passes on the resulting branch

### t8 — Live-test the fix on the real fleet, beyond the host reconciliation itself

- instruction: Live-testing means the real hosts, not the harness. The remove-secret.sh round trip is the sharpest probe: it proves add-if-absent restores a genuinely missing key without touching secrets. Check the container's resolved env for sslmode, since an empty one is the silent failure the challenge pass found and a config-file check would not catch it. Check for non-terminal runs before touching either host.
- depends on: t5
- acceptance:
  - A key deliberately removed from a host's prod.env via remove-secret.sh is restored by re-running install-secrets.sh, with no generated secret changed — the add-if-absent path proved on a live host, not only in the fakeCluster harness
  - Both hosts' /v1alpha1/version and both codex bridges' preflight.host.deployment report the same revision, so the pair is demonstrably no longer split
  - The resulting `NODES_DATABASE_URL` resolves to a NON-EMPTY sslmode in the running container's environment, checked on the host — the silent-failure mode c29 describes is confirmed absent in production
  - Any discrepancy found is reported as a finding, not smoothed over

### t9 — Summarize the delivery before opening the PR

- instruction: Run the /summarize-delivery skill. The plan the owner confirmed is the contract; record where execution obeyed it and where it did not. This cycle has real drift to report: the plan omitted a repo-mandated version bump, claim c18 asserted both failed runs shared one cause when they did not, and a secret count in the README was wrong. Report failures faithfully.
- depends on: t7, t8
- acceptance:
  - A delivery summary records planned versus actual, every mid-run decision the owner made, the plan drift (the omitted version task, the corrected c18, the corrected secret count), and what is genuinely safe to claim as delivered
  - It states plainly what was NOT done and why — including anything left stale or deferred
  - Operator hand-turns in this cycle are counted, per the convention #118 tracks

## Deferred targets

- `c4` (boundary): This cycle applies no database migration. Prod's `schema_migrations` tops out at `0035_inbound_transport` and main embeds through 0035; 0036 is held in migrations/pending/, which migrations/pending/README.md states //go:embed \*.sql does not descend into. Redeploy is a code-only change, so the deploy must not be treated as needing a schema rollback plan. — deferred: Established by scope, not built by this plan: the redeploy applies no migration and 0036 stays held under #121. Recorded as a boundary the cycle respected.
- `h12` (honesty): `schema_migrations` on prod tops out at the same version as the highest migrations/\*.sql in the shipped tree, and migrations/pending/ is not reachable by //go:embed \*.sql. — deferred: Honesty condition on c4, deferred with it.
- `c7` (requirement): The two by-hand prod.env keys are exactly `NODES_DATABASE_URL` and `NODES_EVENT_TOKEN_SECRET`, established by diffing the key SETS of thor's live prod.env against both timestamped .bak files (values never read). Both are present now, and every deploy lane merges key-by-key and can never delete a line, so the redeploy preserves them. — deferred: Half of it is parked as v1: `NODES_EVENT_TOKEN_SECRET` is an owner decision about whether the scheduled-sweep emit path is on in prod. The `NODES_DATABASE_URL` half is covered by t2.
- `h4` (honesty): Diffing the key SETS of thor's prod.env against both .bak files names exactly `NODES_DATABASE_URL` and `NODES_EVENT_TOKEN_SECRET` and no third key, and a subsequent deploy leaves both in place. — deferred: Honesty condition on c7, deferred with it.
- `c9` (requirement): `NODES_EVENT_TOKEN_SECRET` is generated by no tooling at all: install-secrets.sh mints six secrets and this is not one of them, compose.thor.yml declares it open (${`NODES_EVENT_TOKEN_SECRET`:-}) and audit-credentials.sh classifies it optional. It is a genuinely unmanaged hand-generated key, and the audit would not have reported its absence. — deferred: The subject of parked unknown v1 — an owner decision, not work this plan may take.
- `h6` (honesty): A grep of install-secrets.sh for `NODES_EVENT_TOKEN_SECRET` returns nothing, compose.thor.yml declares it with the open :- form, and audit-credentials.sh lists it under optional. — deferred: Honesty condition on c9, deferred with it.
- `c10` (boundary): The post-deploy credential audit will pass on thor's current key set and is not evidence the reconciliation is done: it classifies `NODES_DATABASE_URL` required (compose's :? form) but `NODES_EVENT_TOKEN_SECRET` optional, so it can only ever detect one of the two hand-added keys. — deferred: A scope finding about what the credential audit can and cannot detect; it constrains how evidence is read, not what this plan builds.
- `h13` (honesty): audit-credentials.sh reports `NODES_EVENT_TOKEN_SECRET` under 'optional' and still exits 0 on a host missing it. — deferred: Honesty condition on c10, deferred with it.
- `c11` (requirement): thor's runner.env carries no `NODES_EVENT_TOKEN` (keys: `NODES_RUNNER_HEADSPACE_BIN`/PROFILES, LISTEN, `SECRET_FILE`, `STATE_DIR`, `PR_UPKEEP_REPOSITORIES`, `PR_UPKEEP_SWEEP_SOURCE_URL`/SHA256), so a scheduled sweep still cannot emit. deploy.sh rewrites runner.env every deploy from the deploying operator's environment, so granting it means exporting it at deploy time — this redeploy will not grant it by accident. — deferred: The runner's `NODES_EVENT_TOKEN` grant belongs to parked unknown v1, not to this plan.
- `h7` (honesty): thor's runner.env contains no `NODES_EVENT_TOKEN` after a completed deploy, and deploy.sh's runner lane rewrites that file from the deploying operator's environment on every run. — deferred: Honesty condition on c11, deferred with it.
- `c12` (boundary): The redeploy cannot ship uncommitted work: deploy.sh resolves REVISION=$(git rev-parse ${BRANCH:-HEAD}) and ships 'git archive' of that COMMIT, so the one dirty file in the checkout (.eidetic/memory/culture-`nodes__public.json`l) cannot reach prod. Local main, origin/main and HEAD are all 68024ac. — deferred: A property of deploy.sh that scope verified and this plan does not change.
- `h14` (honesty): git archive of the resolved commit contains no file that git status reports as modified in the working tree. — deferred: Honesty condition on c12, deferred with it.
- `c14` (boundary): After this deploy the human-inbox bridge still cannot report a revision: deployment.py ships in adapters/codex, claude-code, colleague and notify but NOT adapters/human-inbox, so #120 item 4 stays partly delivered and no redeploy closes it. — deferred: The human-inbox bridge's missing deployment.py is #120 item 4, tracked there — widening this plan to it would be scope creep.
- `h15` (honesty): adapters/human-inbox/src/`human_inbox_bridge`/ contains no deployment.py, and its bridge's capability surface carries no deployment key after a deploy. — deferred: Honesty condition on c14, deferred with it.
- `c16` (boundary): The `SONAR_CLOUD_SWEEP` naming item changes nothing in-repo — it was resolved in favour of `SONAR_TOKEN` during the #122 cycle. The only remaining act is an edit to the operator's own .env on the host, so it produces no commit and no PR. — deferred: Resolved in the #122 cycle with nothing in-repo to change; the remaining act is an operator .env edit on a host.
- `h16` (honesty): A scoped git grep for `SONAR_CLOUD_SWEEP` across the repo excluding docs/specs, docs/plans and .devague returns nothing. — deferred: Honesty condition on c16, deferred with it.
