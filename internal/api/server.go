package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// defaultEventPollInterval is how often the SSE handler polls the events
// table for rows newer than a client's resume point (PRD §15.1's stream,
// implemented as a poll rather than LISTEN/NOTIFY so a client's resume
// point is answered purely from durable state — see events.go).
const defaultEventPollInterval = 500 * time.Millisecond

// Server implements the Culture Nodes control-plane API
// (api/openapi/openapi.yaml). It is bound to one namespace at construction
// (see the package doc's "Single namespace" section).
type Server struct {
	Store  *postgres.Store
	Engine *engine.Engine
	Ledger *ledger.Ledger

	NamespaceID string

	// engineStore duplicates what Engine already wraps, but exposes the
	// engineQueries methods (EnsureWorkflowVersion, GetWorkflowVersion's
	// sibling lookups) that engine.Store's narrower interface does not —
	// see workflows.go's publish handler for why publication needs it
	// directly rather than only through Engine.
	engineStore *postgres.EngineStore

	// callbackStore backs the actor callback ingest route (see
	// callbackRoutePattern below). It is built unconditionally in
	// NewServer -- constructing it never fails once namespaceID has
	// already been validated by the lookups above -- so callbackSigner is
	// the only thing that decides whether the route is actually mounted.
	callbackStore *postgres.CallbackStore
	// callbackSigner verifies the attempt-scoped bearer token a callback
	// presents (internal/actors/token.go). Nil means this installation
	// offers no callback endpoint at all, and Handler leaves the route
	// unmounted (404) rather than mounting it to always answer 500 —
	// cmd/nodes/worker.go's callbackConfig applies the identical rule on
	// the dispatch side, and both must read the same
	// NODES_CALLBACK_TOKEN_SECRET for a token minted by a worker to verify
	// here.
	callbackSigner *actors.TokenSigner

	pollInterval time.Duration
}

// Option configures a Server.
type Option func(*Server)

// WithPollInterval replaces the SSE handler's events-table poll interval.
// It exists so tests do not have to wait out the 500ms production default.
func WithPollInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// WithCallbackSigner mounts the actor callback ingest route, verifying
// every token it receives against signer. Omitting this option (or passing
// a nil signer) leaves the route unmounted: a deployment that never
// dispatches to asynchronous actors has nothing to verify a callback token
// against, and mounting the route anyway would only ever answer 500.
func WithCallbackSigner(signer *actors.TokenSigner) Option {
	return func(s *Server) {
		if signer != nil {
			s.callbackSigner = signer
		}
	}
}

// NewServer builds a Server over store, scoped to namespaceID. It
// constructs its own Engine and Ledger runtimes bound to the same store and
// namespace, matching internal/store/postgres.NewEngine/NewLedger's own
// one-line construction path.
func NewServer(store *postgres.Store, namespaceID string, opts ...Option) (*Server, error) {
	engineStore, err := postgres.NewEngineStore(store, namespaceID)
	if err != nil {
		return nil, err
	}
	eng, err := postgres.NewEngine(store, namespaceID)
	if err != nil {
		return nil, err
	}
	led, err := postgres.NewLedger(store, namespaceID)
	if err != nil {
		return nil, err
	}
	callbackStore, err := postgres.NewCallbackStore(store, namespaceID)
	if err != nil {
		return nil, err
	}

	s := &Server{
		Store:         store,
		Engine:        eng,
		Ledger:        led,
		NamespaceID:   namespaceID,
		engineStore:   engineStore,
		callbackStore: callbackStore,
		pollInterval:  defaultEventPollInterval,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Handler builds the http.Handler serving every operation
// api/openapi/openapi.yaml declares, using the Go 1.22+ http.ServeMux
// method+pattern syntax. Every route not implemented directly wraps its
// handlerFunc in (*Server).wrap so error responses are rendered uniformly
// (see errors.go); streamRunEvents manages its own response lifecycle
// because it writes a streaming body rather than one JSON document.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1alpha1/workflows/validate", s.wrap(s.handleValidateWorkflow))
	mux.HandleFunc("POST /v1alpha1/workflows", s.wrap(s.handlePublishWorkflow))
	mux.HandleFunc("GET /v1alpha1/workflows", s.wrap(s.handleListWorkflows))
	mux.HandleFunc("GET /v1alpha1/workflows/{digest}", s.wrap(s.handleGetWorkflow))

	mux.HandleFunc("POST /v1alpha1/runs", s.wrap(s.handleCreateRun))
	mux.HandleFunc("GET /v1alpha1/runs", s.wrap(s.handleListRuns))
	mux.HandleFunc("GET /v1alpha1/runs/{id}", s.wrap(s.handleGetRun))
	mux.HandleFunc("POST /v1alpha1/runs/{id}/cancel", s.wrap(s.handleCancelRun))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/events", s.handleStreamRunEvents)

	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger", s.wrap(s.handleListLedgerRecords))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger/projections/{name}", s.wrap(s.handleGetLedgerProjection))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/reviews", s.wrap(s.handleCreateReview))
	mux.HandleFunc("POST /v1alpha1/reviews/{id}/commit", s.wrap(s.handleCommitReview))

	mux.HandleFunc("GET /v1alpha1/healthz", s.wrap(s.handleHealthz))
	mux.HandleFunc("GET /v1alpha1/readyz", s.wrap(s.handleReadyz))

	// The actor callback surface (PRD §13.1's callback.url, §13.4's event
	// ingest) is not part of the nodes.culture.dev/v1alpha1 group above: it
	// is the runner-agnostic wire contract internal/actors/protocol.go
	// fixes (CallbackEventsPathFormat), unversioned, which is what every
	// worker-minted callback.url already points at
	// (internal/worker/dispatch.go's callbackURL) — mounting it under
	// /v1alpha1 instead would silently break every real actor. It is
	// mounted only when this Server was built WithCallbackSigner; see that
	// option's doc for why an unconfigured installation leaves it absent
	// rather than mounted-but-always-failing.
	if s.callbackSigner != nil {
		mux.Handle("POST "+callbackRoutePattern, actors.NewCallbackHandler(actors.CallbackDeps{
			Store:  s.callbackStore,
			Engine: s.Engine,
			Signer: s.callbackSigner,
		}))
	}

	return mux
}

// callbackRoutePattern is the http.ServeMux pattern for the callback route,
// derived from actors.CallbackEventsPathFormat rather than hand-typed so
// the mux pattern and the URL every worker actually builds can never drift
// apart. Go's {id} wildcard matches one path segment, exactly what "%s"
// stands for in that format string.
var callbackRoutePattern = fmt.Sprintf(actors.CallbackEventsPathFormat, "{id}")
