# Delivery Summary — post-merge prod reconciliation (#128)

plan: `post-merge-prod-reconciliation-128` · run: `complete` · date: `2026-08-16`
baseline: `devague summary skeleton`

## Intent

Close #128: bring production onto the merged `main`, redeploy the codex bridges,
and reconcile the two `prod.env` keys that reached thor outside the deploy
tooling during the #127 session. The cycle ran the full devague chain —
`/scope` surveyed 23 surfaces, `/think` converged 33 claims under 26 honesty
conditions, a rigorous `/challenge` pass probed the result, `/spec-to-plan`
produced nine tasks in six waves, and `/assign-to-workforce` fanned them out.

The cycle grew well beyond its brief, because the fix's own delivery path kept
producing evidence. What began as "redeploy thor" ended as a defect fix in the
credential merge, a new deployment lane, a compose correction that closed a
production outage this cycle itself caused, and five new issues.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Fix the sed-delimiter hazard once, in the shared `PROD_ENV_MERGE` definition, and update its pinned test text deliberately
- `t2` — Add the add-if-absent deployment-settings lane to install-secrets.sh, composing `NODES_DATABASE_URL` on the host with sslmode already resolved
- `t3` — Behavioral tests for the settings lane, seeded from a prod.env that lacks the key
- `t4` — Document the settings lane and the delimiter rule in deploy/prod/README.md
- `t5` — Reconcile the hosts: re-run install-secrets.sh on thor and orin, then deploy.sh orin, and verify the pair is no longer split-revision
- `t6` — Record the operator hand-turns and the live #125 evidence on their issues
- `t7` — Bump the version and prepend a CHANGELOG entry
- `t8` — Live-test the fix on the real fleet, beyond the host reconciliation itself
- `t9` — Summarize the delivery before opening the PR

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | The `sed s///` expression is gone from `PROD_ENV_MERGE` entirely, replaced by a pure-POSIX literal rewrite through a `chmod 600` temp file. Merged `54221dd`. |
| `t2` | delivered | `install_deployment_settings` added: unguarded, add-if-absent, composing `NODES_DATABASE_URL` on the host. Merged `4572d72`. |
| `t3` | delivered | Six behavioral tests, each verified against a deliberately broken script. Merged `3972cc8`. |
| `t4` | delivered | New README subsection; plus a correction to an adjacent claim the lane invalidated. Merged `7ac8a32`, `c34c128`. |
| `t5` | delivered | Both hosts reconciled — but only after the first attempt caused an outage and a second fix. See Drift. |
| `t6` | delivered | Comments on #124 and #125, each signed once, each verified against live sources rather than transcribed. |
| `t7` | delivered | `v0.32.0` (`bd2ee0e`) — minor, not the patch first proposed. |
| `t8` | delivered | Live remove-and-restore round trip on orin; `sslmode` confirmed non-empty in both running containers. |
| `t9` | delivered | This artifact. |

## Mid-work Decisions

No `devague deviate` records were written this run — the owner chose to add the
omitted task directly rather than record a deviation. Every decision below is
therefore captured here directly.

- **Two failed prod runs were diagnosed before deploying**, on the reasoning
  that a redeploy swaps the binary underneath whatever is failing. Both turned
  out to be #125, not new.
- **orin was deliberately left stale** rather than hand-fixed after its first
  deploy failed. Hand-adding the key would have been the identical hand-turn
  #124 exists to count, and would have spent the only live reproduction.
- **The pre-deploy dump was kept** (claim `c15` rejected): "stable" was not yet
  true while the pair was split-revision.
- **The developer bridge's allowlist was not pruned** — picking one of its two
  entries would answer #125's open question by accident.
- **`t2` restructured more than its brief allowed**, moving the non-secret keys
  out of the rotation-guarded block. Accepted: leaving `NODES_DATABASE_URL`
  there would have kept writing the `${DATABASE_SSLMODE}` placeholder the
  cycle exists to eliminate. The rotation consequence became #133.
- **The version bump was minor, not patch** — this adds a lane and changes what
  a `FORCE_PROD` rotation does.
- **orin was restored by deploying the unmerged local branch**, knowingly
  re-entering the #128 condition, because the fix orin needed existed nowhere
  else. Requires a redeploy from `origin/main` after this PR merges.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| plan omitted a version task | The repo mandates a version bump on every PR and `version-check` gates merge; no planned task covered it. `t7` was added mid-run. | acceptable |
| `t5` | Its first execution left orin's worker crash-looping (exit 2, 11 restarts) on a second, unrelated missing-variable class — `NODES_CODE_RUNNER_NAME` hardcoded in compose while `REVISION`/`ACTOR_ID` were empty. orin went from stale-but-running to current-but-down. Fixed in `7f49a22`, then re-run successfully. | risky |
| `t5` | orin now runs `7f49a22`, an unmerged revision, while thor runs `68024ac`. Deliberate and owner-approved, but it re-enters the exact condition #128 was opened to clear. | needs-follow-up |
| `c18` (frame claim) | Asserted both of the day's failed runs shared one cause. They had identical error classes and different 400 reasons. Amended and re-confirmed before export. | acceptable |
| `c31` (frame claim) | Called the delimiter failure silent. It is loud today and becomes silent only once `NODES_DATABASE_URL` joins the multi-key block. Amended and re-confirmed. | acceptable |
| test file size | `prodenvmerge_test.go` reached 1117 lines against the repo's 1000-line hard limit — caught by `tests/lint` during evidence gathering, not by any task's own gate. Split in `5faf101`. | acceptable |

## Evidence

- tests: `go test ./...` — pass (full suite, after the file split)
- tests: `tests/deploy` — `TestDeploymentSettings*` (6), `TestCodeRunnerNameFollowsTheRestOfItsTuple`, `TestProdEnvMergeReplacesAValueContainingAPipe` — pass
- tests: `uv run pytest -n auto` — 304 passed
- lint: `go test ./tests/lint/` — pass (line-limit gate)
- lint: `black --check`, `flake8`, `markdownlint-cli2 "**/*.md"` — clean (129 md files)
- lint: both adapter invocation styles (root-scope and adapter-dir) — clean
- commits: `68024ac..5faf101`
- issues: #124, #125, #128, #133, #134, #135, #136
- live: thor `/v1alpha1/version` = `68024ac9a00c`; orin `prod-worker-1` up, `restarts=0`
- live: remove-and-restore round trip on orin — key/hash table identical afterwards

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| A missing deployment setting reaches a provisioned host by re-running `install-secrets.sh`, with no rotation | high | live round trip on orin: `DATABASE_SSLMODE` removed, restored by an unforced re-run, key/hash table identical |
| `NODES_DATABASE_URL` is composed on the host from the password already there | high | orin's URL embeds the seeded password, verified by on-host hash comparison; test `TestDeploymentSettingsReachAProvisionedHostWithoutRotating` |
| The empty-`sslmode` silent failure is absent in production | high | `docker inspect` on both workers: `?sslmode=disable`, no empty form |
| `PROD_ENV_MERGE` can no longer drop a value containing the old delimiter | high | commit `54221dd`; test `TestProdEnvMergeReplacesAValueContainingAPipe`, verified failing against the pre-change script |
| A host with no code-runner tuple boots; a host with the other two keeps its capability | high | orin up with all three absent, thor with all three present; test `TestCodeRunnerNameFollowsTheRestOfItsTuple`, mutation-verified |
| thor's control plane is on `origin/main` and healthy | high | `/v1alpha1/version` = `68024ac9a00cf3613a93c89ea251bde5b3cdfe32`; readyz 200 |
| Both codex bridges were redeployed and report their revision | high | `preflight.host.deployment`: thor `68024ac`, orin `7f49a22`, both `build_stamp` |
| The production pair runs one revision | **unverified — false today** | orin is on `7f49a22`, thor on `68024ac`. Not claimed done. |
| The rotation path is coherent after `t2`'s restructure | unverified | #133 — `POSTGRES_PASSWORD` and the URL can disagree after a rotation; no rotation was performed to test it |

## Remaining Work / Follow-up

- **Redeploy orin from `origin/main` once this PR merges.** The pair is
  knowingly split-revision until then. This is the one item that leaves #128's
  original condition partly open.
- **#133** — a `FORCE_PROD` rotation can leave `POSTGRES_PASSWORD` and the
  URL's embedded password disagreeing, silently. Deferred by owner decision.
- **#134** — probes of `install-secrets.sh` relay live operator credentials
  into throwaway files. The mechanism is general, not Discord-specific.
- **#135** — the lane can write a `DATABASE_SSLMODE` that contradicts the URL
  beside it; `env_has`/`env_get` disagree on duplicate keys; hostnames are
  hardcoded in a script that takes host arguments.
- **#136** — five spark-hosted actors are registered at a LAN address that no
  longer resolves. **Dispatch to every spark actor is failing right now.**
  Discovered incidentally when `install-secrets.sh` exited 255 on an
  unreachable ssh target.
- **#125 stays open** — which repo a multi-lane bridge should use is unanswered,
  and triggered `pr-upkeep` runs keep failing closed until it is.
- **#121 unaffected** — migration `0036` remains held in `migrations/pending/`.

### The operator-hand-turn count

Six operator hand-turns this cycle, all in the main session rather than
automated: the two host deploys, the orin restore, the test-file split, the
README correction, and the version bump. #118 is the issue that changes that.
The count is the point — this cycle removed *one* recurring hand-turn (typing a
deployment setting into a provisioned host) and performed six others.
