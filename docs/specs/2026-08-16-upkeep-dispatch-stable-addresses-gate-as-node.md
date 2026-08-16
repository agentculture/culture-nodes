# upkeep-dispatch-stable-addresses-gate-as-node

> Triggered upkeep runs dispatch again, no bridge is reachable by an address anyone had to record because every bridge dials out and identifies itself with a control-plane-issued token, and the TDD merge gate leaves derived evidence instead of a human's recollection
> instruction: Deliver in the owner's stated order: #125 first, then the address-retirement cutover (#136 / #121 / #111's dial-in half), then #101. Each half ends with a live run id, not a passing test.

## Audience

- The operator running culture-nodes cycles, and the mesh actors the control plane dispatches to — plus anyone reading the ledger afterwards to decide whether a claim is evidence.

## Before → After

- Before: Every triggered upkeep run fails ~1s after creation with an unreadable 'actor answered Bad Request'; five actors' reachability depends on which of two DHCP wireless interfaces happens to be up, with nothing watching; and the TDD merge gate is typed and read by the operator, nine times in the #87 batch and six more hand-turns in the #128/#137 cycle.
- After: A triggered pr-upkeep run dispatches and completes without an operator touching a bridge config; no participant address is stored anywhere, because every bridge dials OUT to the control plane holding a control-plane-issued token that is its identity; and a package cannot merge without a derived gate record naming the suites, the exit codes and the commit.

## Why it matters

- All three are the same defect in different clothes: the system cannot see its own state. It cannot say why a dispatch was refused, cannot say whether an actor is reachable, and cannot say whether a merge was verified. Issue #118 closes only when those stop being facts a human holds in their head.

## Requirements

- The live blocker is the DEPLOYED bridge config, not the graph: ~/.config/culture-nodes-bridges/developer.json lists TWO `repo_allowlist` entries (/home/spark/git/.worktrees.culture-nodes/owe-developer and .../upkeep-lane, both leftover merged worktrees), so `only_allowed_repo`() returns None and the dispatch is refused with 'input.repo is required'.
  - instruction: Do not clean the two leftover worktree entries as the fix — that would answer the question by deleting it. Leave the allowlist multi-entry and make the dispatch succeed anyway, which is what proves c4's mechanism.
  - honesty: A triggered pr-upkeep run dispatched to the developer bridge reaches the agent session with the allowlist still holding more than one entry — proven by a live run, not by a unit test.
- A multi-lane bridge needs a way to say WHICH repo it defaults to, because the allowlist's cardinality is the wrong signal: an allowlist is a permission surface, and 'exactly one entry' conflates permission with intent.
  - instruction: Add a repository IDENTITY to actor.metadata (same shape as the shipped `handover_remote` key). The control plane sends the identity on dispatch; the bridge maps identity to a local checkout and STILL validates it against `repo_allowlist`. Propagate to all five adapters — the all-backends rule.
  - honesty: A bridge whose allowlist holds several repositories resolves the right checkout from the registry-supplied identity, and refuses with a named error when the identity maps to nothing it is permitted to touch.
- The control plane discards the bridge's rejection REASON. Attempt 01M04Q276TPTA9GT8ME7PFVRFY records only '`actor_rejected_input` (HTTP 400): actor answered Bad Request'; the bridge's JSON body naming input.repo never reaches the ledger, so diagnosing #125 required reproducing the call by hand.
  - instruction: Carry the actor's JSON error body and class through internal/actors into the attempt result so GET /v1alpha1/runs/{id} shows it. Bound the copied body; a bridge must not be able to write unbounded text into the run view.
  - honesty: The run view for a `contract_rejected` attempt shows the bridge's own error text and class, so #125 could have been diagnosed from GET /v1alpha1/runs/{id} alone.
- The real cause is worse than a DHCP lease change: spark has TWO wireless NICs on the same /24, both dynamic — wlP9s9 at 192.168.1.157 and wlx90de80db7994 at 192.168.1.118. Whichever is up decides which address answers, so re-registering at .118 would be exactly as fragile as staying at .157.
  - instruction: Do not re-register at either wireless address. The cutover removes the stored address entirely; this claim exists to record WHY re-registration was rejected.
  - honesty: Nothing in the deployed system reads a spark LAN address after the cutover — verified by dispatching to a spark actor with both wireless interfaces' addresses unregistered.
- There is no validator/gate node kind. internal/compiler/vocabulary.go closes the enum at agent, code, action.http, decision, approval, wait, parallel, join, end — so a gate today can only be expressed as a code node through the headspace runner boundary, or as an agent node (which would be the actor grading itself).
  - instruction: Express the gate as kind: code with uses: runner://headspace/... and declare `gates_passed` / `changes_required` / `measurement_incomplete` as domain outcomes. Do not add a validator kind to the compiler.
  - honesty: The gate node is expressed with an existing node kind and internal/compiler/vocabulary.go is unchanged by this cycle.
- Only spark can run the full TDD gate. Measured: thor has node/npm and uv but go is off-PATH; orin has neither node nor npm and its uv/go sit off a login shell's PATH. The gate's declared suites need Go, npm and uv together, so 'run the gate on a host with the toolchain' resolves today to the operator's own machine.
  - instruction: Declare the gate's host explicitly and record that only spark satisfies it today. A lane missing an instrument emits `not_applicable` naming the uncovered files — never a pass.
  - honesty: The gate node's declared suites are chosen from a measured capability surface, and a lane that cannot run one of them yields `not_applicable` naming the uncovered files — never a pass.
- The toolchain baseline does not measure node or npm, so the capability surface #101 wants the gate to choose a host FROM cannot answer the question 'can this lane run the web build'.
  - instruction: Add node and npm to TOOLS in scripts/toolchain-baseline.sh, recapture all three host baselines, and surface them through the capability surface so the gate's host choice is measured rather than assumed.
  - honesty: scripts/toolchain-baseline.sh measures node and npm, the three host baselines are recaptured, and a capability surface can answer 'can this lane run the web build'.
- The owner's decision makes this cycle's third item the #121 cutover, which transport-inversion.md sequences strictly: convert all five bridges, disable the outbound fallback, THEN git mv migration 0036 into the applied sequence. Applying 0036 early was already tried once and failed fourteen tests.
  - instruction: Follow transport-inversion.md's sequence exactly: convert all five bridges, register actor.metadata.`handover_remote` (no actor carries it today), disable the outbound fallback, THEN git mv migrations/pending/`0036_`\*.sql into migrations/ and run nodes migrate. The full suite passing with the column gone is the check.
  - honesty: The full suite passes with actors.`endpoint_ref` and `runner_invocations`.endpoint gone — the check that caught the premature merge the first time.
- Control-plane-issued per-bridge tokens ARE issue #111. transport-inversion.md states migration 0031's simple verifier record must be replaced by #111's per-actor model — or that debt explicitly accepted and time-bounded — BEFORE the first production bridge accepts inbound dials. The owner's 'the main app assigns a token each bridge must keep' resolves that in favour of building it.
  - instruction: Build issuance in the control plane: a per-actor credential the operator never invents, revocable without restarting other bridges, replacing migration 0031's single simple verifier record. Bridges read the issued token from configuration and present it on every dial.
  - honesty: A bridge cannot dial in with an operator-invented token: the credential it presents was issued by the control plane, is per-actor, and can be revoked without restarting any other bridge.
- transport-inversion.md's list of non-dispatch `endpoint_ref` consumers is INCOMPLETE. It names internal/worker/registry.go and scripts/collect-handover.py. It misses adapters/human-inbox/src/`human_inbox_bridge`/tracker.py, whose `verify_bridge_serves_actor` reads registered.`endpoint_ref` at startup and RAISES BridgeIdentityError when it does not match the tracker's own bridge URL (issue #72's guard). Dropping the column stops culture-nodes-human-inbox-tracker.service from starting at all.
  - instruction: Design the tracker's replacement identity check BEFORE 0036 — dial-in presence for its own actor key, or a locally-configured assertion. Add it to transport-inversion.md's consumer list, which is the document that missed it.
  - honesty: culture-nodes-human-inbox-tracker.service starts and confirms its bridge identity with actors.`endpoint_ref` gone.
- Applying migration 0036 is a ONE-WAY DOOR for the whole fleet. transport-inversion.md's 'Rollback at 03:00' is per-bridge precisely because step 4 confirms the actor's latest registration still carries its pre-cutover `endpoint_ref` and step 5 restarts that bridge without dial-in configuration. Once the column is dropped, no bridge can be rolled back that way — the graceful per-bridge path is replaced by a schema restore.
  - instruction: Order the plan so 0036 is the final task, gated on the all-five simultaneity demonstration, not merely on the code being written.
  - honesty: Every step before the column drop is demonstrated live while the per-bridge rollback still exists, and the drop is the last action taken.
- The cutover carries an UNRESOLVED liveness risk forward rather than fixing it. transport-inversion.md states plainly that a dropped poll does not revoke a claim, that a different worker may meanwhile believe it owns the attempt, and that fencing tokens refuse stale writes but the dial-in liveness signal is not consulted by the lease model — so duplicate execution or delayed completion is possible in that window.
  - instruction: Do not attempt to solve the lease/liveness interaction in this cycle. Decide which of the three it gets, and write it down.
  - honesty: The duplicate-execution window is either bounded, instrumented, or recorded as accepted-and-time-bounded with the reason — never left silent.
- Retiring the address does not retire the QUESTION the address answered. 'Is this bridge reachable?' becomes 'is this bridge dialled in right now?', and nothing surfaces that today — internal/worker/registry.go consults dial-in presence only at dispatch resolution, so an un-dialled bridge is still discovered by a failing dispatch. The rejected reachability detector returns in this form, as a presence surface rather than a probe.
  - instruction: Surface dial-in presence as a read-only operator view. This is the rejected reachability detector in its correct form — presence, not a probe.
  - honesty: An operator can ask which bridges are dialled in right now and get an answer without dispatching anything.
- There is a window where nothing can dispatch: the cutover's second-to-last step disables the outbound fallback, and any bridge not yet dialled in at that instant is unreachable by either path. transport-inversion.md's conversion is deliberately one-bridge-at-a-time, but disabling the fallback is fleet-wide and single-step.
  - instruction: Make the fallback-disable step check live presence for all five at that moment, not a checklist of conversions.
  - honesty: Disabling the outbound fallback is refused, or loudly warned, while any of the five actors has no current dial-in presence.
- The gate node cannot gate its own delivery. This cycle's merges are verified by the operator-typed gate that the cycle exists to replace, so the gate node's first real verdict is on work merged after it. Claiming the cycle's own packages were gate-node-verified would be exactly the false green #101 names.
  - instruction: State the bootstrap asymmetry in the delivery summary explicitly. Do not claim this cycle's merges were gate-node-verified.
  - honesty: The gate node's first verdict is on a package merged AFTER this cycle, named with its run id — and the delivery summary says so rather than implying the cycle gated itself.
- The repository-identity mapping needs a collision and miss rule: two bridges on one host may allowlist overlapping checkouts, and an identity the registry supplies may map to nothing the bridge is permitted to touch. Both must be named refusals, not a silently-picked first match — which is the same failure `only_allowed_repo`() already fails closed on.
  - instruction: Mirror `only_allowed_repo`()'s fail-closed shape: ambiguity refuses. Propagate to all five adapters.
  - honesty: A repository identity that maps to two permitted paths, or to none, produces a named refusal with a hint — never a silently-picked first match.
- Migration 0036's ADR 0002 bypass premise no longer describes the fleet. It reads 'exactly two workers and one API, and deploy/prod/deploy.sh restarts all three together'. Measured: deploy.sh takes ONE host argument (deploy.sh <thor|orin>), so thor and orin are TWO operations; thor runs api, scheduler, worker, notifier and backup containers while orin runs a worker of its own; and the migrate service lives in compose.thor.yml, so migrating on thor leaves orin's worker on the previous binary — the exact N-1 window ADR 0002 exists to prevent. The migration's own text says the exception lapses in that case.
  - instruction: Resolve this BEFORE the cutover's last step. Re-justifying means amending the migration's own premise text, not asserting it elsewhere.
  - honesty: Either the ADR 0002 bypass is re-justified in writing against the measured fleet, or 0036 follows the full expand-contract sequence.

## Honesty conditions

- The pr-upkeep example still loads into a second deployment with no edit to workflow.yaml and no new field on the event payload.
- Every gate record in the ledger carries authority=derived with a validator origin; no proposed agent claim is read as a gate result anywhere in the merge path.
- An operator who did not run this cycle can read a merged package's ledger and tell, without asking anyone, which suites ran and at which commit.
- The hand-turn count for the next cycle's merge gates is lower than the fifteen this one and the #87 batch recorded, and the difference is attributable to gate records rather than to fewer packages.
- Each of the three halves ends with a query that answers the question a human currently answers from memory: why was this refused, is this actor connected, was this merge verified.
- All three signals are read from live surfaces after deploy, each naming a run id, and none is satisfied by a passing test alone.
- GET /v1alpha1/actors returns no `endpoint_ref`, every one of the five bridges shows as dialled in, and a dispatch to a spark actor succeeds with no address recorded anywhere in PostgreSQL.
- grep shows no third-party import in any adapter's dial-in path, all five pyprojects still declare dependencies = \[\], and tests/lint's byte-identity check passes for the shared module.
- All three halves are demonstrated LIVE, not by fixture: one triggered run dispatches with a multi-entry allowlist, one dispatch lands on a bridge the control plane holds no address for, and one merge is gated on a derived record — each with a run id in the ledger.
- The issued per-bridge token is distributed without acquiring #133's rotation-split hazard or #134's credential-relay hazard, or those hazards are fixed first.

## Success signals

- One triggered pr-upkeep run reaches its approval node without an operator edit; GET /v1alpha1/actors returns no `endpoint_ref` column at all and every bridge is dialled in; and one package merges on a derived gate record whose commit sha was read back from the worktree the suites ran in.

## Scope / boundaries

- sweep.py stays a pure emitter. Widening the event payload to carry instruction or repo is the thing task t17 deliberately stopped it doing, and examples/pr-upkeep/README.md forbids making workflow.yaml deployment-specific.
- No agent gets to author the gate verdict. PRD §10.4 reserves derived records for deterministic validators; an agent node running the suites and reporting a result is the actor grading itself, which is the exact substitution #101 exists to prevent.
- The adapters keep dependencies = \[\]. Whatever the dial-in client becomes, it stays stdlib-only and byte-identical across all five packages, because tests/lint enforces the byte-identity and the fleet's installability on thor and orin depends on the zero-dep property.
- The issued credential inherits the rotation hazards already filed against the secrets lane: #133 (a `FORCE_PROD` rotation can leave two different values) and #134 (probes relay live operator credentials into throwaway files). A newly-minted per-bridge token distributed by install-secrets.sh acquires both unless the issuance path is designed around them.
  - instruction: Do not route the new credential through install-secrets.sh's rotation-guarded block until #133 is resolved; a rotation that updates one copy and not another is exactly what would silently un-dial a bridge.

## Non-goals

- This cycle does not enable unattended repair dispatch (#102's execution half stays a deliberate step while the bridge write path is unproven, #18), and does not widen coverage/complexity instruments to Go, adapters or web (#88).
- This cycle does not give spark a DHCP reservation or static lease, does not remove either wireless interface, does not enable unattended repair dispatch (#102's execution half, while #18 keeps the bridge write path unproven), and does not widen coverage/complexity instruments to Go, adapters or web (#88).

## Assumptions

- The #125 code fix already SHIPPED in commit 261524f (v0.30.0): examples/pr-upkeep/workflow.yaml binds instruction as a node literal, and every bridge's Config.`only_allowed_repo`() falls back to the single allowlisted repo. The issue body still describes the pre-fix state and is stale.
- Issue #136's headline is no longer true: the five spark actors are REACHABLE right now. From thor, <http://192.168.1.157:8088/v1/capabilities> answers 401 (auth required, i.e. connected), as does 192.168.1.118.
- \#101's MECHANICAL half already shipped: POST /v1alpha1/runs/{id}/suite-verdicts records a suite's exit code, command and commit sha as a derived validator record, and scripts/collect-handover.py --gate runs the suite in a detached worktree at the collected commit and posts it. Task t32 added the repair routing on a rejecting verdict.
- Dial-in already exists as a bounded HTTP LONG POLL, not a websocket: dialin.py is byte-identical in all five adapters (90 lines, stdlib urllib) and drives POST /v1alpha1/inbound/poll + POST /v1alpha1/inbound/{id}/complete against a durable mailbox, with inbound rate-limit, lockout and revocation already in internal/actors/`inbound_authentication.go`.
- Control-plane token ISSUANCE is smaller than #111 implies: migrations 0031 and 0032 already give a per-party credential record keyed (`party_kind`, `party_key`), storing only a SHA-256 verifier or an env var NAME, with `revoked_at`, `failure_count`, `locked_until` and rate-window columns. What is missing is minting (generate, digest, reveal once) and an authorization model — not the storage, the revocation, or the admission controls.

## Scope exploration

- `s1` — `examples/pr-upkeep/workflow.yaml + adapters/*/src/*/config.py:only_allowed_repo`: workflow.yaml already carries the literal-instruction binding with an 'issue #125' comment, committed in 261524f; the bridges already fall back to the sole allowlisted repo. The issue body describes a state the tree left behind.
  - seeds: `c2`
- `s2` — `~/.config/culture-nodes-bridges/developer.json (deployed, spark)`: `repo_allowlist` has two leftover merged-worktree entries, so the documented single-repo inference cannot fire and every triggered run fails closed. Verified by reading the deployed config against config.py's exactly-one rule.
  - seeds: `c3`
- `s3` — `adapters/claude-code/src/claude_code_bridge/config.py:only_allowed_repo docstring`: the fallback rule is deliberately cardinality-based and fails closed on ambiguity; the docstring names #125 as the gap it half-closes. It leaves open where repo comes from when a bridge is legitimately multi-lane.
  - seeds: `c4`
- `s4` — `actor.metadata.handover_remote (scripts/collect-handover.py)`: the registry ALREADY holds per-actor deployment facts as actor.metadata (`handover_remote`), which is the shipped precedent for putting a deployment-specific repo somewhere other than the graph or the event.
- `s5` — `GET /v1alpha1/runs/01M04Q26ZPNKTXVNBGSDS1YR9F (live, thor)`: the failed attempt's result.error.detail stops at 'actor answered Bad Request' — the bridge's own error/class body is dropped, so a fail-closed dispatch is unreadable from the run view alone.
  - seeds: `c5`
- `s6` — `examples/pr-upkeep/README.md + workflow.yaml header comments`: the README states loading the workflow into another deployment must never mean editing workflow.yaml; the graph comments repeat it for repo specifically. Both options 1 and 2 in issue #125 are therefore already constrained by shipped documentation.
  - seeds: `c6`
- `s7` — `ssh thor -> curl spark:8086-8090 (live probe)`: thor reaches spark on BOTH .157 and .118 today; every bridge binds 0.0.0.0. The outage #136 recorded has cleared on its own, which is exactly why an address-decay class needs detection rather than a one-off re-registration.
  - seeds: `c7`
- `s8` — `ip -4 addr show (spark)`: two dynamic wireless interfaces hold 192.168.1.157 and 192.168.1.118 simultaneously; neither is static. Issue #136's option 1 (re-register at .118) therefore does not reduce the failure rate, it relocates it.
  - seeds: `c8`
- `s9` — `ssh thor -> curl 100.127.105.72:{8086..8090} + tailscale status`: every spark bridge is reachable over the tailnet from thor today, and deploy/prod/register-actor.sh requires a NUMERIC IPv4 host — which the tailnet address satisfies and a hostname would not. This is a durable re-registration target needing no router access.
  - seeds: `c9` (rejected)
- `s10` — `GET /v1alpha1/actors + deploy/prod/install-secrets.sh incident`: actor rows carry `endpoint_ref` with no liveness relation to anything; the registry is append-only so a stale row simply stays newest. No health check, audit or alarm reads it. That is the detection gap #136 is evidence for.
  - seeds: `c10` (rejected)
- `s11` — `migrations/pending/0036_retire_stored_participant_addresses.sql + docs/decisions/transport-inversion.md`: the migration is written and deliberately held behind the dial-in cutover. #136 is evidence for that priority, and the issue itself says so — it is explicitly not a request to shortcut the precondition.
  - seeds: `c11` (rejected)
- `s12` — `issue #121 + issue #136 options 1/2/3`: options 2 (DHCP reservation) and 3 (dial-in cutover) are both out of this cycle's reach — one needs router access nobody has scripted, the other is a multi-bridge conversion. The tailnet re-registration plus a detector is the part that is both durable and in-repo.
  - seeds: `c12` (rejected)
- `s13` — `internal/api/suiteverdicts.go + internal/api/repairroute.go + scripts/collect-handover.py --gate`: the derived-evidence recorder, the exit-code-must-be-present refusal, and the bounded repair routing are all live. What is NOT shipped is the gate as a NODE: the operator still types the command and the run id.
  - seeds: `c13`
- `s14` — `internal/compiler/vocabulary.go`: the node-kind enum is closed and contains no validator kind; KindCode is the only deterministic executor. Any gate node this cycle ships is therefore a code node, or the enum grows — and growing it touches the compiler, the authoring schema and the worker.
  - seeds: `c14`
- `s15` — `docs/baselines/toolchains/{spark,thor,orin}.json + live ssh command -v probes`: the committed baselines measure uv/go/gh/git/codex/claude/colleague and do NOT measure node or npm at all, while the gate's web suite needs npm. thor: go present-off-path. orin: no node, no npm. spark alone has all four. Making the gate a node moves it out of the operator's SESSION, not off the operator's HOST.
  - seeds: `c15`
- `s16` — `scripts/toolchain-baseline.sh TOOLS array`: TOOLS=(uv go gh git codex claude colleague) — node and npm are absent, so no baseline or capability surface can report them. #101's acceptance criterion 'chosen from the capability surface (#96) rather than assumed' is unmeetable for the web suite until they are added.
  - seeds: `c16`
- `s17` — `PRD §10.4 via CLAUDE.md ledger authority model + internal/api/suiteverdicts.go doc comment`: the endpoint's own doc comment says an operator reading a green tick is not evidence; the same argument forbids an agent node reporting a suite result as anything but a proposed claim. The gate node must be deterministic.
  - seeds: `c17`
- `s18` — `internal/repair package doc + issues #18/#88/#119`: routing is a decision and nothing is dispatched, by design and for stated reasons; and the t18 applicability matrix leaves internal/ and adapters/ as tests-plus-file-length only until #88. Both are deliberately outside this cycle.
  - seeds: `c18`
- `s19` — `adapters/*/src/*/dialin.py + internal/actors/inbound_authentication.go`: the reverse transport, the durable mailbox, the per-bridge bearer token (`PREFIX_DIAL_TOKEN`) and the admission controls are all shipped. The owner's decision changes the TRANSPORT (websocket) and the token's ISSUER (control plane, not operator config) — it does not start from nothing.
  - seeds: `c19`
- `s20` — `adapters/*/pyproject.toml + scripts/check-zero-runtime-deps.sh + go.mod`: every adapter declares dependencies = \[\] (CI gates only the ROOT pyproject, so the adapters' zero-dep property is convention, not enforcement). Go has no websocket library either — golang.org/x/net is present but only as an indirect dependency. The transport change therefore adds a dependency on both sides or hand-rolls framing on one.
  - seeds: `c20` (rejected)
- `s21` — `issue #121 + migrations/pending/README.md + docs/decisions/transport-inversion.md`: the cutover's steps, its rollback procedure and its two non-dispatch consumers (internal/worker/registry.go, scripts/collect-handover.py) are already written down. #121 also records that NO actor currently carries metadata.`handover_remote`, so handover collection breaks when `endpoint_ref` goes unless that key is registered first.
  - seeds: `c21`
- `s22` — `docs/decisions/transport-inversion.md 'Authentication clock started by this change' + issue #111`: the precondition is recorded and unmet; the owner's answer supplies the decision it was waiting for. #111 is therefore pulled into this cycle rather than being a separate later item.
  - seeds: `c22`
- `s23` — `CLAUDE.md 'Record deviations from the PRD explicitly (ADR / devague deviation record), don't drift silently' + docs/decisions/transport-inversion.md`: transport-inversion.md is status 'proposed, decided before any bridge implementation changed (t23)' and explicitly reasons for a bounded long poll. Replacing it needs the reasoning superseded in writing.
  - seeds: `c23` (rejected)
- `s24` — `challenge pass / adjacent-systems lens: adapters/human-inbox/src/human_inbox_bridge/tracker.py + deploy/prod/culture-nodes-human-inbox-tracker.service`: a THIRD `endpoint_ref` consumer the recorded cutover decision does not list, and it fails closed by design — the tracker refuses to run rather than risk two bridges sharing one idempotency store. Its replacement identity check must be designed before 0036, not discovered after.
  - seeds: `c44`
- `s25` — `challenge pass / reversibility lens: docs/decisions/transport-inversion.md 'Rollback at 03:00' steps 4-5 + migrations/pending/0036`: the cutover's own rollback procedure depends on the column the cutover's last step drops. That is intended, but it means the final step deletes the escape hatch, so everything before it must be demonstrated while the hatch still exists.
  - seeds: `c45`
- `s26` — `challenge pass / concurrency lens: docs/decisions/transport-inversion.md 'Routing and reconnect semantics'`: the duplicate-execution window is named in the recorded decision and explicitly NOT solved by transport inversion. A full cutover makes it the steady state rather than a transitional exposure.
  - seeds: `c46`
- `s27` — `challenge pass / observability lens: internal/worker/registry.go DialIn resolution + internal/actors/client.go`: presence is consulted at resolution time and nowhere else; there is no operator-facing 'who is connected' view. Without one the cutover trades a silently-decaying address for a silently-absent connection, which is the same blind spot wearing different clothes.
  - seeds: `c47`
- `s28` — `challenge pass / security lens: migrations/0031_inbound_authentication.sql + 0032_inbound_authentication_controls.sql + internal/actors/inbound_credential.go`: the schema is already per-actor with no-plaintext-at-rest and explicit revocation; the 'simple record' 0031's expiry comment refers to is that the verifier is operator-PROVISIONED and unauthorized, not that it is shared. Issuance is the gap, and it lands on an existing table rather than a new one.
  - seeds: `c48`
- `s29` — `challenge pass / failure-mode lens: transport-inversion.md conversion sequence + the five bridges on three hosts`: converting is per-bridge and reversible; disabling the fallback is neither. The plan needs an explicit check that all five are dialled in AT THAT MOMENT, not merely that all five were converted at some point.
  - seeds: `c49`
- `s30` — `challenge pass / lifecycle lens: issue #101 acceptance criteria + this cycle's own merge path`: a bootstrap asymmetry the acceptance criteria do not address — 'a package cannot merge without a derived gate record' cannot hold for the package that introduces the gate. The honest form is a first post-delivery run, named with its run id.
  - seeds: `c50`
- `s31` — `challenge pass / failure-mode lens: adapters/*/src/*/config.py repo_allowed + repo_allowlist_prefixes`: `repo_allowed` already accepts an exact entry OR a strict child of a scoped prefix, so an identity could plausibly resolve to more than one permitted path. The mapping must be exact and refuse ambiguity the way the cardinality rule does today.
  - seeds: `c51`
- `s32` — `challenge pass / security lens: deploy/prod/install-secrets.sh + issues #133/#134`: the distribution lane for a new secret is the same script those two open issues are about. The pass does not re-diagnose them; it records that this cycle adds a fifth credential to a lane with two known unfixed hazards.
  - seeds: `c52`
- `s33` — `challenge pass / adjacent-systems lens: web/src/api/types.ts + web/src/fixtures/mesh-fixture.ts`: clean-ish: `endpoint_ref` is typed OPTIONAL in the web client and appears in no component, only in types and fixtures, so dropping the column does not break the UI build. The mesh fixtures still assert addresses and will need updating, and the UI silently loses any 'where is this actor' affordance.
- `s34` — `challenge pass / data-flow lens: migration 0036's second statement (runner_invocations.endpoint)`: clean pass: transport-inversion.md states runner sampling resolves `runner_ref` through the configured runner-service registry on every poll, so dropping the COPIED `runner_invocations`.endpoint does not touch runner addressing. Verified against the migration's two statements; runner service endpoints remain operator configuration.
- `s35` — `challenge pass / operations lens: deploy/prod/deploy.sh restart scope + ADR 0002 bypass in 0036`: NOT examined in depth. The bypass rests on 'exactly two workers and one API, restarted together by deploy.sh'. This pass did not verify that the deployed fleet still matches that shape, and the migration itself says the exception lapses if it does not.
- `s36` — `challenge pass / operations lens: deploy/prod/deploy.sh argv + compose.thor.yml/compose.orin.yml service lists + live docker ps on both hosts`: the bypass's factual premise is stale. Two deploy operations, six culture-nodes containers across two hosts, and the migrator on only one of them. Either the bypass is re-justified against the real fleet or 0036 needs the full expand-contract sequence — this is a precondition on applying it, not a detail.
  - seeds: `c53`

## Decisions

- \#125: the registry supplies a repository IDENTITY on the actor (actor.metadata), the bridge maps that identity to its own local checkout, and the `repo_allowlist` reverts to being a pure permission surface that may hold many entries.
- \#101: the gate is a code node executed through the headspace-cli runner boundary, and a merge node routes on its domain outcome. The compiler's node-kind enum does not grow.
- \#136: no participant address is stored. Addressing is solved by DIRECTION, not by framing — the bridge initiates the connection, so the control plane never learns an address. The shipped bounded long poll already does this, so the transport is unchanged and docs/decisions/transport-inversion.md (t23) stands rather than being superseded.
- Identity is a control-plane-ISSUED per-bridge token, replacing migration 0031's single simple verifier record and the operator-invented `PREFIX_DIAL_TOKEN`. This is #111's dial-in half, which transport-inversion.md names as the precondition for enabling the first production bridge.
- The FULL address-retirement cutover lands this cycle: control-plane-issued per-bridge tokens, all five bridges converted to dial-in, actor.metadata.`handover_remote` registered, the outbound fallback disabled, and migration 0036 moved into the applied sequence. Closes #136, #121, and #111's dial-in half.
  - instruction: Sequence: build token issuance first (nothing may dial with an operator-invented credential), then convert the five bridges one at a time as transport-inversion.md's rollback procedure requires, then register `handover_remote`, then disable the fallback, then apply 0036.
- No websocket in this cycle — DEFERRED to a new issue for exploration, not dropped. It was separated from the address problem once it became clear that framing is not what removes a stored address: direction of initiation is. What a websocket would add (full-duplex push, dispatch without a poll cycle, server-initiated cancellation) is a latency and liveness question worth its own scope pass, and it carries a real cost — a dependency in five zero-dependency adapters plus a websocket library in Go, or hand-rolled RFC 6455 framing in a byte-identical shared module.
- The cycle's second item is therefore narrowed to one mechanism: WHO INITIATES. Every bridge dials out to the control plane and identifies itself with a control-plane-issued token, so no address is ever stored or needed. The shipped bounded long poll already provides that direction, so the transport is unchanged and transport-inversion.md (t23) stands.

## Open parks

- [unknown_nonblocking] The two leftover worktree entries in spark's developer bridge allowlist are both merged branches. Cleaning them would incidentally make `only_allowed_repo`() fire again and mask whichever answer question 1 gets.
- [unknown_nonblocking] Residual surprise risk that survives this pass: the duplicate-execution window between a dropped dial-in poll and a lease transition is described but never measured. Nobody has observed how often it happens, so the cutover's steady-state exposure is unquantified.

## Resolved vagueness

- [unknown_blocking] Only spark has the full gate toolchain, so a gate node moves the work off the operator's SESSION but not off the operator's HOST. Whether that satisfies #101's intent is the owner's call. — resolved: Accepted and stated as a limit, not hidden: the gate node runs on spark because spark is the only host with Go, npm and uv together. It moves the gate out of the operator's SESSION (it becomes a ledgered derived record instead of a typed command and a read tick) but not off the operator's HOST. Widening the eligible hosts is #96/#88 work, not this cycle's.
- [unknown_blocking] Whether the deployed fleet still matches the two-workers-one-API shape the ADR 0002 bypass in migration 0036 depends on. The migration states the exception lapses if the fleet grew past one deploy.sh restart, and this pass did not verify it. — resolved: Answered by finding c53: the bypass premise is stale. deploy.sh takes one host argument, thor and orin are two operations, and the migrate service runs only in compose.thor.yml. The bypass must be re-justified in writing against the measured fleet, or 0036 follows the full expand-contract sequence.
- [unknown_blocking] The five bridges have never been demonstrated dialled in SIMULTANEOUSLY. Issue #121 records that the cross-fleet simultaneity demonstration (one converted and one unconverted bridge live together) was never run; a full cutover needs the all-five version of that evidence. — resolved: Plan-side gate: the all-five simultaneous dial-in demonstration is a required plan task that BLOCKS the fallback-disable and 0036 steps, evidenced by a run id rather than a checkbox. It stops blocking the spec and starts blocking the build.
