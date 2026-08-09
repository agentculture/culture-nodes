// Package compiler compiles workflow definitions into validated,
// content-addressed graphs.
//
// A workflow is authored in YAML or JSON, and published as an immutable,
// content-addressed version (PRD §9.10). Compile is the gate between those two
// states: it runs the document through the PRD §11.4 validation levels and,
// if nothing blocks it, emits the normalized JSON intermediate representation
// the runtime executes plus the digest that addresses it.
//
// # Validation levels
//
// The levels run in the PRD's order and every level runs:
//
//	syntax     the bytes parse and the document is an object
//	structure  it validates against schemas/workflow/workflow.schema.json
//	graph      entry and edge endpoints exist, every node is reachable, a
//	           terminal path exists, loops are bounded by policy
//	contract   node kinds declare the outcomes they need, inline schemas
//	           compile, JSON Pointer bindings resolve, CEL guards compile
//	ledger     projections and record types are in the vocabulary, and no
//	           node claims an authority its kind cannot hold
//	policy     durations parse, retries are sane, components are pinned, and
//	           no code node exceeds a runner cap
//	owners     every node resolves to a concrete owner
//
// Two levels named in §11.4 are absent, deliberately rather than by oversight:
// *deployment* (are pinned components resolvable in the target environment)
// needs a component registry that does not exist yet, and the parts of
// *ledger validation* that depend on an authenticated actor identity — who may
// promote a proposal — are runtime authority checks that no document can prove
// about itself.
//
// Only a syntax failure or an undecodable document stops the pipeline. A
// structure violation does not: one mistake can be true at several levels at
// once (a node missing `ownerRef` violates the schema *and* leaves the owners
// level with nothing to resolve), and suppressing the deeper verdict would
// make it appear only after the shallower one was fixed.
//
// # Diagnostics
//
// Every finding is a Diagnostic with a stable Code, a JSON Pointer Path into
// the submitted document, and a Hint. Diagnostics are deduplicated and sorted
// by path then code, so the same bytes always produce the same sequence and a
// test — or an agent — can assert on it. An error blocks compilation; a
// warning does not.
//
// # Determinism
//
// The normalized IR is encoded with contracts.CanonicalJSON and digested with
// contracts.Digest, so identical sources yield an identical digest. Everything
// that could vary is pinned: node maps are emitted through canonical JSON's
// sorted keys, edges are sorted into one canonical order regardless of how the
// author listed them, and resolved outcome sets are sorted. A YAML document
// and its canonical JSON rendering compile to the same digest — which is what
// "JSON is authoritative, YAML is authoring sugar" has to mean to be worth
// saying.
//
// Compiled CEL programs are returned on the CompiledWorkflow rather than
// embedded in the IR: serializing an AST would tie the content digest to
// cel-go's internal representation, and a dependency upgrade would silently
// re-digest every published workflow.
package compiler
