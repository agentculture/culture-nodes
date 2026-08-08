# api/openapi

`openapi.yaml` is the OpenAPI 3.1 definition of the Culture Nodes
control-plane API (PRD §12.1, §15.1), group `nodes.culture.dev/v1alpha1`.
It is spec-first: `internal/api` implements exactly the operations declared
here, `internal/api`'s contract-honesty tests parse this file (via
`sigs.k8s.io/yaml`) and sweep every documented path+method against the live
mux, and `tests/parity/parity_test.go` asserts every CLI product verb maps to
a documented operation here — so the spec and the code cannot drift silently.

Phase 1 is authless by design (spec decision c45): see the spec's top-level
`description` and `internal/api`'s package doc.
