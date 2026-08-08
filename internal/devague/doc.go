// Package devague maps devague CLI JSON output (PRD §9.11) onto ledger
// records — pure, offline, deterministic mapping only.
//
// devague is planning/spec software: it owns claims, honesty conditions,
// plans, dependency waves, and a deliverables preview. None of that is the
// work ledger. This package translates devague's output into
// internal/ledger.Record values so a converged devague frame and plan can be
// read through the same projections (ReadyTasks, ConfirmedClaims,
// DecisionHistory, ...) as any other run — "integrate through schemas and a
// conformance adapter, never copy internals" (PRD §9.11).
//
// # No execution, no store
//
// This package never runs the devague binary and never imports a devague
// package (Go cannot import Python regardless). It also never touches
// internal/ledger's store or append path: every Map* function is a pure
// function from JSON bytes to []ledger.Record. Feeding the result through
// internal/ledger's Append/CommitReview lifecycle — or through a schema
// validator — is a caller concern, not this package's. See
// TestNoExecInNonTestSources for the enforced half of that rule; the devague
// CLI is used only offline, from a test/dev shell, to regenerate the fixtures
// under testdata/.
//
// # Authority honesty
//
// devague tracks claim status as proposed/confirmed/rejected, and origin as
// user/llm. The ledger's producer/authority matrix (PRD §10.4) is stricter:
// an agent-origin record may only ever be `proposed`, and `confirmed` /
// `rejected` authority is reachable only through a review record — never by
// an actor rewriting its own proposal's authority field. So a devague claim
// never maps to a single record whose authority mirrors its devague status.
// Instead:
//
//   - The claim's own content always maps to one record at authority
//     `proposed`, origin `agent` (devague origin `llm`) or `human` (devague
//     origin `user`) — matching devague's origin exactly.
//   - If devague recorded the claim as `confirmed` or `rejected`, a second,
//     separate record is emitted: a `review` record, origin always `human`
//     (confirmation is a human act regardless of who authored the claim),
//     authority `confirmed`/`rejected`, whose `subject_ref` names the first
//     record.
//
// A bare, non-transactional ledger.CheckAuthority call refuses that review
// record — by design; see internal/ledger's own
// TestAppendEnforcesProducerAuthorityMatrix ("human confirms directly" /
// RuleHumanReviewOnly) and TestAuthorityHonestyMatchesLedgerRules in this
// package. Confirmed authority is reachable only by actually running
// ledger.CreateReviewRequest + ledger.CommitReview, which this package's
// authority test also exercises to prove the emitted record has exactly the
// shape the ledger's own review transaction would produce.
//
// # Deterministic ids
//
// Record ids are derived from devague's own ids, never random ULIDs, so
// mapping the same fixture bytes twice yields byte-identical canonical JSON
// and identical digests (the t25 acceptance): "dv_" + a devague slug, plus a
// devague-native suffix — "dv_<frame>_<claim id>" for a claim,
// "dv_<frame>_<claim id>_review" for the review record confirming or
// rejecting it, "dv_<plan>_<task id>" for a plan task, and
// "dv_<plan>_signal_<n>" for the nth deliverables success signal. A devague
// plan's slug always equals its source frame's slug (the devague CLI assigns
// it that way), so ids from MapFrameClaims and MapPlanWaves/MapDeliverables
// naturally line up without the caller threading a shared slug through.
package devague
