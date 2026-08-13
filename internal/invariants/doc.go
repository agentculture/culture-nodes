// Package invariants holds cross-package invariant gates: CI-runnable
// source-level sweeps proving that batch-wide promises survived the batch.
//
// The package contains no runtime code — every gate is a test in
// invariants_test.go, run by the ordinary `go test ./...`. The two
// invariants gated today, and the allowlists they encode, are documented
// in docs/invariants.md:
//
//  1. Provider neutrality (spec 2026-08-13 c16/h14): the dispatch path
//     never branches on an actor's kind, and internal/actors'
//     neutrality_test.go stays byte-identical and green.
//  2. The ledger authority ladder (spec 2026-08-13 c17/h15): observed
//     authority is constructed only at the runner boundary, confirmed
//     authority only by the ledger's review transaction and the sanctioned
//     human-grade path, derived authority only by deterministic producers.
package invariants
