// Package api implements the HTTP control-plane API for Culture Nodes: the
// OpenAPI 3.1 surface declared in api/openapi/openapi.yaml, group
// nodes.culture.dev/v1alpha1 (docs/initial-design/culture-nodes-prd-spec.md
// §12.1). It is the read/write path for workflow publication, run
// orchestration, the append-only work ledger, and human review — the same
// state internal/engine and internal/ledger already commit through
// internal/store/postgres, exposed over HTTP instead of a Go call.
//
// # Authless by design
//
// Phase 1 carries no authentication or authorization of its own (spec
// decision c45, PRD §16.1's "authless while the control plane runs behind a
// private network only"). Every operation here is reachable by anyone who
// can reach the listener. `nodes serve` and `nodes all` are meant to run
// behind a private network or an authenticating proxy; this package adds
// neither.
//
// # Spec-first
//
// api/openapi/openapi.yaml is written first; this package implements
// exactly its operations, and internal/api's own test suite parses that
// file (via sigs.k8s.io/yaml) and sweeps every documented path and method
// against the live mux, so the spec and the code cannot drift silently
// (the repo's "record deviations explicitly" ground rule, applied to
// the API surface itself).
//
// # Error shape
//
// Every non-2xx response, on every operation, is a JSON body matching
// internal/clifmt.CliError's own encoding: {"code", "message",
// "remediation"}. code follows the same exit-code-style bucket the CLI
// uses — 1 for a domain/user error (not found, conflict, a malformed
// request), 2 for an environment error (the database is unreachable) — and
// the HTTP status line carries the specific condition. Results, on 2xx
// responses, are the resource itself: results and errors are never
// interleaved in one body, mirroring the CLI's stdout/stderr discipline.
//
// # Logging
//
// Server carries a *log/slog.Logger (the log field; WithLogger replaces it,
// defaulting to slog.Default()). Two paths log at Error level, both added
// to close the invisibility half of issue #16 — prod-api-1 answered every
// failed terminal-commit callback with a 5xx and logged nothing about it:
//
//   - Every 5xx response, however a handler produced it, logs one line
//     carrying the request's method, path, and err's full unwrapped chain.
//     (*Server).writeAPIError (errors.go) is the single funnel this passes
//     through — (*Server).wrap's handlerFunc results and
//     handleStreamRunEvents' two pre-stream failures (events.go) all call
//     it, so the hook lives in one place rather than in every handler.
//   - The actor callback ingest route additionally logs a terminal-commit
//     failure with the attempt id, via logCallbackFailures (logging.go)
//     wrapping actors.NewCallbackHandler in Handler — that route answers
//     its own response body directly (§13.1's wire contract, not this
//     package's Error shape) rather than through a handlerFunc, so it does
//     not pass through writeAPIError and needed its own hook.
//
// A 4xx is a domain or user outcome (not found, a conflict, a malformed
// request) rather than a failure this process needs paged on, so neither
// path logs one.
//
// # Single namespace
//
// Server is bound to one namespace at construction (matching
// internal/store/postgres.EngineStore and internal/ledger's own scoping);
// PRD §14 lists namespace as a deployment boundary the schema always
// carries, but nothing in this phase exposes more than one over HTTP.
package api
