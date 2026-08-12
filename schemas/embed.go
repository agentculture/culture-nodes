// Package schemas embeds the Culture Nodes contract definitions so the control
// plane validates against the exact schema files that ship with the binary,
// never against whatever happens to be on disk at runtime.
//
// Three families live here:
//
//   - ledger/   the work-ledger record envelope (PRD §10.3), the ten MVP
//     record types (PRD §10.2), and the additively-registered `grade`
//     opinion record (issue #28 item 1), plus record.schema.json which
//     dispatches on record_type;
//   - workflow/ the workflow authoring document (PRD §9.1, §11.1);
//   - runner/   the runner-agnostic operation and result contracts (PRD §13.7).
//
// Every schema is JSON Schema Draft 2020-12 and carries an absolute $id under
// BaseURI, so cross-file $refs resolve without a network fetch. JSON is
// canonical; YAML is authoring sugar handled by the compiler, not here.
package schemas

import "embed"

// BaseURI is the $id prefix every embedded schema declares. A schema's
// embedded path appended to this prefix is its canonical identifier — the
// validator relies on that correspondence to register resources.
const BaseURI = "https://nodes.culture.dev/schemas/"

// FS holds the schema definitions, keyed by paths such as
// "ledger/envelope.schema.json".
//
//go:embed ledger workflow runner
var FS embed.FS

// ExamplesFS holds reference documents that are valid under those schemas —
// notably examples/deliver-change.workflow.json, the PRD §11.1 authoring
// example rendered as canonical JSON. They are validated by the test suite, so
// an example that drifts from the schemas fails the build rather than quietly
// misleading a reader.
//
//go:embed examples
var ExamplesFS embed.FS
