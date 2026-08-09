package api

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

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

	pollInterval time.Duration
	webAssets    fs.FS
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

// WithWebAssets mounts an embedded SPA build (the repo root's
// WebAssets(), present only in -tags embedweb binaries) on every
// non-/v1alpha1 path, with an index.html fallback for client-side routes.
// Without it the mux serves the API alone, which is what the contract
// tests exercise: their undocumented-route 404 sweep is only meaningful
// when no SPA catch-all is mounted (prd-spec §19.1).
func WithWebAssets(assets fs.FS) Option {
	return func(s *Server) {
		s.webAssets = assets
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

	s := &Server{
		Store:        store,
		Engine:       eng,
		Ledger:       led,
		NamespaceID:  namespaceID,
		engineStore:  engineStore,
		pollInterval: defaultEventPollInterval,
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

	if s.webAssets != nil {
		mux.Handle("GET /", spaHandler(s.webAssets))
	}

	return mux
}

// spaHandler serves the embedded web build: real files as-is, everything
// else (client-side routes like /runs/abc) falls back to index.html. It
// never shadows /v1alpha1 — the mux's more-specific API patterns win.
func spaHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(assets, p); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
