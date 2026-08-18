# Registration tooling residue (#8): take-or-defer

Status: decided 2026-08-18 (plan `jira-driven-idea-to-shipped-loop`, task t19).
Disposition: **gap (a) TAKEN, gap (b) already CLOSED — not deferred, corrected.**

## What t19 was asked to weigh

Issue #8 ("Registration tooling: actors and runner services are raw SQL
inserts") named three gaps after the first working session. The second
comment on #8 (2026-08-16, "Advanced, not closed") recorded two of the three
as done and one as genuinely partial:

1. **Runner services load from `NODES_RUNNER_SERVICES_FILE` at worker start
   only** — no reload, no live update. Recorded as **partial**.
2. **No deployment preflight** for code-runner identity. Recorded as **done**
   (`cmd/nodes/worker.go`'s `workerConfigPreflight`).
3. **Namespace discovery is not on the API**, deploy scripts grep the DB.
   Recorded as **done** (`GET /v1alpha1/namespaces`, both deploy paths
   switched over).

t19's brief named two gaps to weigh: (a) the runner-services reload gap
(gap 1 above), and (b) namespace discovery (gap 3 above), describing (b) as
still open ("deploy scripts grep the DB"). Re-reading the current tree before
touching anything surfaced that (b) was **already closed** one day before this
task ran, and the brief's framing had gone stale. That correction is recorded
below rather than silently re-deferring work that is already done — "not by
silence" applies to false-negative framing exactly as much as it applies to a
gap nobody wrote down.

## Gap (b): namespace discovery — already closed, nothing to take or defer

Verified directly against the tree at task time, not taken on the issue
comment's word alone:

- `internal/api/namespaces.go` implements `handleListNamespaces`, and
  `internal/api/server.go:440` registers
  `GET /v1alpha1/namespaces` against it. `internal/api/namespaces_test.go`
  covers it.
- `deploy/prod/deploy.sh:658` and `:683` both resolve the namespace id via
  `curl -fsS http://localhost:18080/v1alpha1/namespaces | python3 -c '...'`
  — no `psql` namespace query remains in that script.

**Disposition: no action.** This record exists so the residue tracked against
issue #8 is accurate rather than assumed — a future reader of #8 or of this plan's
delivery summary should not re-open a namespace-discovery task believing it
is still outstanding. The PRD §26 framing in this repo's `CLAUDE.md` ("deploy
scripts grep the DB") is now stale prose left over from before the
2026-08-16 change; it names a fact worth fixing there separately (a doc
drift, not a functional gap), not a reason to redo the work here.

## Gap (a): runner-service live reload — taken

### Why it was in scope

`RegisterService`'s own doc comment (`internal/runners/registry.go`) already
names the correct shape for changing a registered identity: *"rebuild the
registry rather than repointing a name in place."* The actor registry
(`internal/worker/registry.go`'s `DBRegistry`) already resolves fresh against
PostgreSQL on every call — actors need no worker restart today, which is
exactly why #8's first comment could report the actors half "proven end to
end" while flagging runner services as the one place still baked into a
one-time process-start load. Runner services were the outlier, not the
pattern; closing the gap means making the file-backed path behave like the
DB-backed one, not inventing new machinery.

### What was built

- `internal/runners/registry.go`: `FunctionRegistry.ReloadServices(services
  map[string]ServiceIdentity) error` — validates every incoming identity
  before touching anything, then swaps the registry's internal map in one
  lock-held assignment. A single invalid entry refuses the whole reload and
  leaves every already-registered name exactly as it was. A FUNCTION identity
  registered through `RegisterFunction` survives a reload untouched, unless
  an incoming service claims the same name, which is refused — the registry's
  one-name-one-kind invariant holds across a reload the same way it holds
  across two direct `RegisterX` calls.
- `internal/runners/protocolclient.go`: `ReloadableSecrets`, a
  `SecretResolver` whose backing `StaticSecrets` map is replaced with one
  atomic pointer write (`sync/atomic.Pointer`). A `ProtocolClient` built over
  one keeps dispatching through the same client value while the credentials
  underneath it change — no client rebuild, and a request resolving a secret
  concurrently with a reload sees either the complete old map or the complete
  new one, never a mix.
- `cmd/nodes/runnerservices.go`: `runnerServiceConfig()` now also returns a
  `*runnerServiceReloader` (nil when the protocol path is disabled). The
  reloader is mtime-gated — `checkAndReload()` stats the file and does no
  parsing, no secret-file reads, and no registry/secrets mutation when the
  mtime has not advanced since the last successful load — and holds the exact
  `*runners.FunctionRegistry` / `*runners.ReloadableSecrets` pair the worker
  was constructed with, not copies. `poll()` runs `checkAndReload` on a
  ticker until its context ends. Secrets are stored *before* the registry
  swap, deliberately: the brief window between the two writes can only ever
  look like "the old world, slightly early," never "a name resolves to an
  identity whose credential is not there yet."
- `cmd/nodes/worker.go` (`cmdWorker`) and `cmd/nodes/serve.go` (`nodes
  serve`/`nodes all`'s in-process worker via `buildWorker`) both start
  `reloader.poll(ctx, ...)` on the worker's own poll cadence, scoped to the
  same shutdown context the worker itself runs on.

### What was deliberately not built

- **No SIGHUP or other explicit reload signal.** Mtime-polling was chosen
  because it needed nothing new in the worker's own architecture (no signal
  handler wiring, no distinction between a worker started as a foreground
  process versus inside `nodes serve`'s goroutine), and it mirrors the
  cadence the worker already polls PostgreSQL at rather than inventing a
  second control channel. A future deployment that wants push-triggered
  reload (e.g., a file-watcher or an explicit CLI-to-running-worker signal)
  is free to add one without touching `checkAndReload`'s core swap logic.
- **No general multi-source registry reconciliation.** `ReloadServices`
  treats its input map as the complete, authoritative service set — a name
  registered directly via `RegisterService` by some other caller and absent
  from a later `ReloadServices` call is removed. This is correct for the
  actual current wiring (the worker's registry only ever receives services
  from this one file-backed path), and is called out explicitly in
  `ReloadServices`'s doc comment as a constraint a future caller must respect
  if the registry is ever fed from two independent sources at once.

### Test evidence

```text
$ go test ./cmd/nodes/... -run RunnerServices -v
=== RUN   TestRunnerServicesAbsentEnvDisablesTheProtocolPath
--- PASS: TestRunnerServicesAbsentEnvDisablesTheProtocolPath (0.00s)
=== RUN   TestRunnerServicesFileBuildsRegistryAndClient
--- PASS: TestRunnerServicesFileBuildsRegistryAndClient (0.00s)
=== RUN   TestRunnerServicesMissingSecretFileIsAnEnvError
--- PASS: TestRunnerServicesMissingSecretFileIsAnEnvError (0.00s)
=== RUN   TestRunnerServicesMalformedJSONIsAnEnvError
--- PASS: TestRunnerServicesMalformedJSONIsAnEnvError (0.00s)
=== RUN   TestRunnerServicesReloadTakesEffectWithoutRebuildingTheRegistry
--- PASS: TestRunnerServicesReloadTakesEffectWithoutRebuildingTheRegistry (0.00s)
=== RUN   TestRunnerServicesReloadIsANoOpWhenTheFileIsUnchanged
--- PASS: TestRunnerServicesReloadIsANoOpWhenTheFileIsUnchanged (0.00s)
=== RUN   TestRunnerServicesReloadRefusesAnInvalidFileWithoutDisturbingTheRegistry
--- PASS: TestRunnerServicesReloadRefusesAnInvalidFileWithoutDisturbingTheRegistry (0.00s)
=== RUN   TestRunnerServicesReloaderPollAppliesAFileChangeOnItsOwn
--- PASS: TestRunnerServicesReloaderPollAppliesAFileChangeOnItsOwn (0.02s)
PASS
ok      github.com/agentculture/culture-nodes/cmd/nodes       3.590s
```

`TestRunnerServicesReloadTakesEffectWithoutRebuildingTheRegistry` is the
acceptance criterion this task named, proven directly rather than by
inference: it obtains the `*runners.FunctionRegistry` and
`*runners.ProtocolClient` a worker would be built with, appends a second
entry to the same file on disk with no worker rebuild in between, calls
`checkAndReload()`, and then resolves and **dispatches a real HTTP request**
to the newly registered service through the *original* registry/client pair —
asserting the `Authorization` header the fake runner receives, exactly the
way `internal/runners/protocolclient_test.go`'s existing tests assert secret
handling, rather than reaching into unexported fields.

```text
$ go test ./internal/runners/... -run Reload -v -race
=== RUN   TestReloadableSecretsStoreReplacesWhatDispatchSends
--- PASS: TestReloadableSecretsStoreReplacesWhatDispatchSends (0.00s)
=== RUN   TestReloadableSecretsResolveBeforeAnyStoreIsAnError
--- PASS: TestReloadableSecretsResolveBeforeAnyStoreIsAnError (0.00s)
=== RUN   TestReloadableSecretsStoreIsSafeConcurrentlyWithResolve
--- PASS: TestReloadableSecretsStoreIsSafeConcurrentlyWithResolve (0.00s)
=== RUN   TestReloadServicesReplacesTheWholeServiceSet
--- PASS: TestReloadServicesReplacesTheWholeServiceSet (0.00s)
=== RUN   TestReloadServicesRefusesAnInvalidEntryWithoutChangingAnything
--- PASS: TestReloadServicesRefusesAnInvalidEntryWithoutChangingAnything (0.00s)
=== RUN   TestReloadServicesRefusesCollidingWithAFunctionIdentity
--- PASS: TestReloadServicesRefusesCollidingWithAFunctionIdentity (0.00s)
PASS
ok      github.com/agentculture/culture-nodes/internal/runners        1.059s
```

Full-suite regression (docker available, real PostgreSQL exercised):
`go test ./...` passed for every package, including `internal/worker`
(18.977s, real PostgreSQL) and the pre-existing runner-service dispatch tests
in `internal/worker/runnerasync_test.go`, unmodified and still green.

## No parallel registration path

Nothing here adds a second way to register a runner service. `nodes
runner-services register` still writes the one JSON file
`NODES_RUNNER_SERVICES_FILE` names; the only change is that a **running**
worker now notices that file changed. The registry and client objects a
worker dispatches through remain the same two production types
(`runners.FunctionRegistry`, `runners.ProtocolClient`) with one additive
method and one new resolver implementation, not a rewritten dispatch path.

## What is still open after this task

- The reload cadence is tied to each worker's own `--poll-interval` /
  `worker.DefaultPollInterval`; an operator who needs a change to land faster
  than that tunes the same flag that already governs dispatch latency. No new
  flag was added.
- Gap 2's deployment preflight (`workerConfigPreflight`) and gap 3's namespace
  discovery remain exactly as closed as the 2026-08-16 comment recorded — this
  task re-verified, did not re-touch, either.
- `docs/initial-design/culture-nodes-prd-spec.md` PRD §26 and this repo's
  `CLAUDE.md` reference to it are unaffected by this change (namespace
  discovery, not runner-service reload, is what that PRD passage discusses);
  the `CLAUDE.md` bullet describing namespace discovery as still-open is the
  drift this record's gap-(b) section names, tracked as a doc-accuracy note
  rather than fixed here — CLAUDE.md is not part of this task's touched
  surface, and repointing it belongs with whoever owns that file's next edit.
