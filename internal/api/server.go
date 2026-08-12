package api

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
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

	// decisionAuthSecret gates POST /v1alpha1/human-tasks/{id}/decision (see
	// (*Server).requireDecisionAuth in humantasks.go). Every other operation
	// in this API is authless by phase-1 design (PRD spec decision c45,
	// stated plainly in api/openapi/openapi.yaml's description) — this is a
	// narrow, deliberate tightening for the one endpoint that writes
	// human-authority review records into the ledger on whoever presents the
	// bearer token, not a general auth story for the rest of the surface.
	// Nil means no secret is configured, and every decision is refused with
	// 401 rather than left open by default (the same posture
	// WithCallbackSigner uses for the callback route).
	decisionAuthSecret []byte

	pollInterval time.Duration
	webAssets    fs.FS

	// telemetry instruments the engine's completion transaction (wired into
	// Engine at construction, below) and the actor callback ingest route
	// (wired into its CallbackDeps in Handler). See WithTelemetry.
	telemetry *telemetry.Provider

	// log is where every 5xx response ((*Server).writeAPIError, see
	// errors.go) and every terminal-commit callback failure
	// (logCallbackFailures, see logging.go) is logged — see the package
	// doc's "Logging" section. Set unconditionally in NewServer, so a
	// Server built through it never has a nil logger; WithLogger replaces
	// it.
	log *slog.Logger
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

// WithDecisionAuthSecret configures the bearer secret POST
// /v1alpha1/human-tasks/{id}/decision requires (see requireDecisionAuth in
// humantasks.go). Omitting it (or passing "") leaves every decision refused
// with 401 rather than mounted-but-authless — a deployment with no
// configured secret has nothing to authenticate a decider against.
func WithDecisionAuthSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.decisionAuthSecret = []byte(secret)
		}
	}
}

// WithTelemetry instruments the engine's §12.5 completion transaction and
// the actor callback ingest route (task t19) through p. Omitting this
// option (or passing nil) leaves both uninstrumented — p's zero value, a
// nil *telemetry.Provider, is a safe no-op — matching every other Option
// here that has a sensible do-nothing default.
func WithTelemetry(p *telemetry.Provider) Option {
	return func(s *Server) {
		if p != nil {
			s.telemetry = p
		}
	}
}

// WithLogger replaces the *slog.Logger every 5xx response and every
// terminal-commit callback failure is logged through (see the package doc's
// "Logging" section). Omitting it (or passing nil) leaves the default,
// slog.Default() — the same sensible-default-with-explicit-override shape
// WithPollInterval and the other options here use.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Server) {
		if logger != nil {
			s.log = logger
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
		log:           slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}

	// A telemetry Provider set by WithTelemetry above arrives after Engine
	// was already built, so the engine handed to instrumented callers is
	// rebuilt over the same store/namespace with the option wired in. This
	// never fails once the first postgres.NewEngine call above already
	// succeeded for the identical (store, namespaceID) pair.
	if s.telemetry != nil {
		eng, err := postgres.NewEngine(store, namespaceID, engine.WithTelemetry(s.telemetry))
		if err != nil {
			return nil, err
		}
		s.Engine = eng
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
	mux.HandleFunc("PATCH /v1alpha1/runs/{id}", s.wrap(s.handlePatchRun))
	mux.HandleFunc("POST /v1alpha1/runs/{id}/cancel", s.wrap(s.handleCancelRun))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/events", s.handleStreamRunEvents)

	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger", s.wrap(s.handleListLedgerRecords))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger/projections/{name}", s.wrap(s.handleGetLedgerProjection))

	mux.HandleFunc("GET /v1alpha1/node-runs", s.wrap(s.handleListNodeRuns))

	mux.HandleFunc("GET /v1alpha1/actors", s.wrap(s.handleListActors))
	mux.HandleFunc("GET /v1alpha1/actors/{id}", s.wrap(s.handleGetActor))
	mux.HandleFunc("GET /v1alpha1/actors/{id}/stats", s.wrap(s.handleGetActorStats))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/reviews", s.wrap(s.handleCreateReview))
	mux.HandleFunc("POST /v1alpha1/reviews/{id}/commit", s.wrap(s.handleCommitReview))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/grades", s.wrap(s.handleCreateGrade))

	mux.HandleFunc("GET /v1alpha1/human-tasks", s.wrap(s.handleListHumanTasks))
	mux.HandleFunc("GET /v1alpha1/human-tasks/{id}", s.wrap(s.handleGetHumanTask))
	mux.HandleFunc("POST /v1alpha1/human-tasks/{id}/decision", s.wrap(s.handleDecideHumanTask))

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
		mux.Handle("POST "+callbackRoutePattern, s.logCallbackFailures(actors.NewCallbackHandler(actors.CallbackDeps{
			Store:     s.callbackStore,
			Engine:    s.Engine,
			Signer:    s.callbackSigner,
			Telemetry: s.telemetry,
		})))
	}

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

// callbackRoutePattern is the http.ServeMux pattern for the callback route,
// derived from actors.CallbackEventsPathFormat rather than hand-typed so
// the mux pattern and the URL every worker actually builds can never drift
// apart. Go's {id} wildcard matches one path segment, exactly what "%s"
// stands for in that format string.
var callbackRoutePattern = fmt.Sprintf(actors.CallbackEventsPathFormat, "{id}")
