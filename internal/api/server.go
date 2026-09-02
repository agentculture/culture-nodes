package api

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// defaultEventPollInterval is how often the SSE handler polls the events
// table for rows newer than a client's resume point (PRD §15.1's stream,
// implemented as a poll rather than LISTEN/NOTIFY so a client's resume
// point is answered purely from durable state — see events.go).
const defaultEventPollInterval = 500 * time.Millisecond

// DefaultSSEKeepaliveInterval is how often both SSE handlers (events.go)
// write an SSE comment line while the stream is idle, so a proxy in the
// path never mistakes a quiet-but-live stream for a dead connection.
// Cloudflare closes idle proxied connections at ~100 s; 25 s leaves margin
// for a missed tick and the write itself. Comment lines are invisible to
// every SSE consumer by specification (a browser EventSource never
// dispatches them), so this changes no event name or payload. Injectable
// through WithSSEKeepaliveInterval so tests do not sleep 25 s.
const DefaultSSEKeepaliveInterval = 25 * time.Second

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
	// runnerCallbackStore backs the runner-protocol completion hint route.
	// The callback can only bring the next authenticated status sample
	// forward; it cannot commit the advisory state in the notification.
	runnerCallbackStore runnerCallbackStore
	// handoverObserver measures a handed-over git ref reported on a
	// `completed` callback event (task t10). Nil in every deployment that
	// has configured no remote to fetch from — see WithHandoverObserver.
	handoverObserver *handover.Observer

	// repairRouterActor is the producer identity a gate-failure routing is
	// derived under (task t32). Empty means DefaultRepairRouterActorID —
	// see WithRepairRouterActorID.
	repairRouterActor string

	// buildVersion and buildRevision are what GET /v1alpha1/version reports
	// (task t32, issue #104). Both are supplied by cmd/nodes from its
	// -ldflags values; an empty revision falls back to the Go toolchain's
	// own vcs stamp, and failing that is reported as unknown rather than as
	// blank. See WithBuildInfo.
	buildVersion  string
	buildRevision string

	// artifactRouter is the only artifact content boundary exposed by this
	// server. artifactInvocationStore deliberately has the one read method the
	// write route needs: authority is derived from the durable invocation after
	// the path-bound callback token verifies.
	artifactRouter          *artifacts.Router
	artifactInvocationStore artifactInvocationStore
	artifactRunnerOps       artifactRunnerOpSource

	// actorTokenLookup resolves the environment variable a registered agent
	// actor's row names (metadata.auth_token_env) to the bearer the control
	// plane expects from that actor on the routes an agent may write
	// (actorbearer.go, login-from-anywhere task t11). os.LookupEnv unless a
	// test replaces it; it is the same lookup worker.DBRegistry uses for the
	// outbound credential, so one row names one variable for both directions.
	actorTokenLookup func(string) (string, bool)

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

	// actorRegistrationSecret gates POST /v1alpha1/actors (see
	// (*Server).requireActorRegistrationAuth in actors.go) — its own secret,
	// deliberately separate from decisionAuthSecret, with the same
	// closed-by-default posture: nil refuses every registration with 401.
	actorRegistrationSecret []byte

	// eventTokenSecret gates POST /v1alpha1/events (see
	// (*Server).requireEventAuth in signalevents.go) — its own secret
	// (NODES_EVENT_TOKEN_SECRET), separate from the two above so an
	// operator can grant an external system the standing to emit signal
	// events without also granting decision or registration power. Same
	// closed-by-default posture: nil refuses every delivery with 401.
	eventTokenSecret []byte

	// adhocRunSecret gates POST /v1alpha1/adhoc-runs (see
	// (*Server).requireAdhocRunAuth in adhoc.go) — its own secret
	// (NODES_ADHOC_RUN_TOKEN_SECRET), separate from the three above so an
	// operator can grant the ad-hoc lane (render + publish + create in one
	// call) without granting decision, registration, or event power. Same
	// closed-by-default posture: nil refuses every ad-hoc run with 401.
	// Added by the t15 auth-hardening gate (spec c27): every mutating
	// surface this batch ships is authenticated from day one.
	adhocRunSecret []byte

	// storeWriteSecret gates the flow store's two write routes — POST
	// /v1alpha1/store/entries and POST /v1alpha1/store/entries/pull (see
	// (*Server).requireStoreWriteAuth in storeentries.go) — its own secret
	// (NODES_STORE_TOKEN_SECRET), separate from the five above so an
	// operator can grant catalog-writing standing without granting
	// decision, registration, event, or ad-hoc power. Reads stay authless:
	// the registry is internal and everyone on the mesh reads (the flow
	// store's q6 decision). Same closed-by-default posture: nil refuses
	// every store write with 401.
	storeWriteSecret []byte

	// inboundAuthenticator gates every bridge poll and completion before any
	// mailbox state is exposed. The migration 0031 simple verifier is now on
	// issue #111's replacement clock because this is the first accepting path.
	inboundAuthenticator *actors.InboundAuthenticator

	// inboundIssuanceSecret gates the dial-in credential issuance and
	// revocation routes (see requireInboundIssuanceAuth in
	// inboundcredentials.go) — its own secret
	// (NODES_INBOUND_ISSUANCE_TOKEN_SECRET), on the same closed-by-default
	// posture as the four above: nil refuses every issuance with 401. This
	// is issue #111's dial-in half — the credential a bridge presents is
	// minted here, never invented by an operator.
	inboundIssuanceSecret []byte

	pollInterval      time.Duration
	keepaliveInterval time.Duration
	webAssets         fs.FS

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
	log               *slog.Logger
	principalVerifier principalVerifier
	jiraWebhook       jiraWebhookConfig
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

// WithSSEKeepaliveInterval replaces the SSE handlers' idle keepalive
// interval (DefaultSSEKeepaliveInterval). It exists so tests can observe
// keepalives in milliseconds rather than waiting out 25 s; a non-positive
// value keeps the default.
func WithSSEKeepaliveInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.keepaliveInterval = d
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

// WithHandoverObserver gives the actor callback route a handover observer
// (task t10, issue #13): when a `completed` event reports a ref the session
// handed over, the control plane fetches it and records what it measured as
// observed evidence.
//
// It is an Option — not a required dependency — for the same reason
// WithCallbackSigner is: a deployment that has configured no remote to fetch
// from cannot measure anything, and a control plane that cannot look must
// record nothing rather than record that it could not. Omitting it leaves the
// route behaving exactly as it did before this existed.
//
// The observer passed here is expected to be the SAME one the worker holds
// (worker.Options.Handover): one control plane, one remote, one measuring
// identity, whichever terminal path a dispatch happens to take.
func WithHandoverObserver(observer *handover.Observer) Option {
	return func(s *Server) {
		s.handoverObserver = observer
	}
}

// WithRepairRouterActorID names the producer identity a gate-failure routing
// is derived under (task t32, issue #102), overriding
// DefaultRepairRouterActorID.
//
// Unlike the secret-shaped options above, there is no closed-by-default
// posture to hold here: the identity is not an authorization, it is an
// attribution. What it must be is REGISTERED — ledger_records
// .origin_actor_id has a foreign key to actors(id) — and a deployment whose
// identity is not registered gets that said to it on every routed gate
// failure rather than silently recording none.
func WithRepairRouterActorID(actorID string) Option {
	return func(s *Server) {
		if actorID != "" {
			s.repairRouterActor = actorID
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

// WithArtifactRouter enables authenticated artifact publication. The route
// is always mounted because OpenAPI declares it, but fails closed unless
// this and WithCallbackSigner are both configured.
func WithArtifactRouter(router *artifacts.Router) Option {
	return func(s *Server) {
		if router != nil {
			s.artifactRouter = router
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

// WithActorRegistrationSecret configures the bearer secret POST
// /v1alpha1/actors requires (see requireActorRegistrationAuth in actors.go).
// Omitting it (or passing "") leaves every registration refused with 401
// rather than mounted-but-authless — the same closed-by-default rule
// WithDecisionAuthSecret applies to human-task decisions.
func WithActorRegistrationSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.actorRegistrationSecret = []byte(secret)
		}
	}
}

// WithEventTokenSecret configures the bearer secret POST /v1alpha1/events
// requires (see requireEventAuth in signalevents.go). Omitting it (or
// passing "") leaves every inbound event delivery refused with 401 rather
// than mounted-but-authless — the same closed-by-default rule the decision
// and registration secrets follow.
func WithEventTokenSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.eventTokenSecret = []byte(secret)
		}
	}
}

// WithJiraWebhook configures the loopback-only Jira system webhook wake-up.
func WithJiraWebhook(secret, token, apiBase, site, project, email, apiToken, botAccountID string) Option {
	return func(s *Server) {
		s.jiraWebhook = jiraWebhookConfig{secret: []byte(secret), token: []byte(token), apiBase: apiBase, site: site, project: project, email: email, apiToken: apiToken, botAccountID: botAccountID}
	}
}

// WithAdhocRunSecret configures the bearer secret POST /v1alpha1/adhoc-runs
// requires (see requireAdhocRunAuth in adhoc.go). Omitting it (or passing
// "") leaves every ad-hoc run refused with 401 rather than
// mounted-but-authless — the same closed-by-default rule the decision,
// registration, and event secrets follow (t15 auth-hardening gate, c27).
func WithAdhocRunSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.adhocRunSecret = []byte(secret)
		}
	}
}

// WithStoreWriteSecret configures the bearer secret the flow store's two
// write routes require (see requireStoreWriteAuth in storeentries.go).
// Omitting it (or passing "") leaves every store write refused with 401
// rather than mounted-but-authless — the same closed-by-default rule the
// decision, registration, event, and ad-hoc secrets follow. Store reads
// need no secret at all: the registry is an internal, mesh-private surface
// whose read side serves everyone on the mesh.
func WithStoreWriteSecret(secret string) Option {
	return func(s *Server) {
		if secret != "" {
			s.storeWriteSecret = []byte(secret)
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
	// The callback store is given the engine so a mid-execution `signal`
	// emission (issue #43, design D11) fires this namespace's event routes as
	// well as its signal waits — an emission that could wake a parked wait but
	// not a standing route would be half a feature.
	callbackStore, err := postgres.NewCallbackStore(store, namespaceID, postgres.WithEventPickup(eng))
	if err != nil {
		return nil, err
	}

	s := &Server{
		Store:                   store,
		Engine:                  eng,
		Ledger:                  led,
		NamespaceID:             namespaceID,
		engineStore:             engineStore,
		callbackStore:           callbackStore,
		runnerCallbackStore:     store,
		artifactInvocationStore: callbackStore,
		artifactRunnerOps:       store,
		pollInterval:            defaultEventPollInterval,
		keepaliveInterval:       DefaultSSEKeepaliveInterval,
		actorTokenLookup:        os.LookupEnv,
		log:                     slog.Default(),
	}
	s.inboundAuthenticator, err = actors.NewInboundAuthenticator(store, actors.DefaultInboundAuthenticationConfig, nil)
	if err != nil {
		return nil, err
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
		// The callback store's pickup runner must be the SAME engine the rest
		// of the server uses, or an emission would pick up through an
		// uninstrumented one.
		callbackStore, err := postgres.NewCallbackStore(store, namespaceID, postgres.WithEventPickup(eng))
		if err != nil {
			return nil, err
		}
		s.callbackStore = callbackStore
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
	return s.principalMiddleware(false, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1alpha1/workflows/validate", s.wrap(s.handleValidateWorkflow))
	mux.HandleFunc("POST /v1alpha1/workflows", s.wrap(s.handlePublishWorkflow))
	mux.HandleFunc("GET /v1alpha1/workflows", s.wrap(s.handleListWorkflows))
	mux.HandleFunc("GET /v1alpha1/workflows/{digest}", s.wrap(s.handleGetWorkflow))
	mux.HandleFunc("POST /v1alpha1/workflow-generations", s.wrap(s.handleCreateWorkflowGeneration))
	mux.HandleFunc("GET /v1alpha1/workflow-generations/{id}", s.wrap(s.handleGetWorkflowGeneration))

	mux.HandleFunc("POST /v1alpha1/adhoc-runs", s.wrap(s.handleCreateAdhocRun))
	mux.HandleFunc("POST /v1alpha1/runs", s.wrap(s.handleCreateRun))
	mux.HandleFunc("GET /v1alpha1/runs", s.wrap(s.handleListRuns))
	mux.HandleFunc("GET /v1alpha1/tickets/{id}", s.wrap(s.handleGetTicket))
	mux.HandleFunc("POST /v1alpha1/tickets/{id}/frame", s.wrap(s.handlePostTicketFrame))
	mux.HandleFunc("POST /v1alpha1/tickets/{id}/replies", s.wrap(s.handlePostTicketReply))
	mux.HandleFunc("POST /v1alpha1/tickets/{id}/freeze", s.wrap(s.handleFreezeTicket))
	mux.HandleFunc("POST /v1alpha1/tickets/{id}/reviews", s.wrap(s.handleTicketReviews))
	mux.HandleFunc("GET /v1alpha1/runs/{id}", s.wrap(s.handleGetRun))
	mux.HandleFunc("PATCH /v1alpha1/runs/{id}", s.wrap(s.handlePatchRun))
	mux.HandleFunc("POST /v1alpha1/runs/{id}/cancel", s.wrap(s.handleCancelRun))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/events", s.handleStreamRunEvents)
	mux.HandleFunc("GET /v1alpha1/events", s.handleStreamEvents)
	mux.HandleFunc("POST /v1alpha1/events", s.wrap(s.handleDeliverEvent))

	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger", s.wrap(s.handleListLedgerRecords))
	mux.HandleFunc("GET /v1alpha1/runs/{id}/ledger/projections/{name}", s.wrap(s.handleGetLedgerProjection))

	mux.HandleFunc("GET /v1alpha1/node-runs", s.wrap(s.handleListNodeRuns))

	mux.HandleFunc("POST /v1alpha1/actors", s.wrap(s.handleRegisterActor))
	mux.HandleFunc("GET /v1alpha1/actors", s.wrap(s.handleListActors))
	mux.HandleFunc("GET /v1alpha1/actors/{id}", s.wrap(s.handleGetActor))
	mux.HandleFunc("GET /v1alpha1/actors/{id}/stats", s.wrap(s.handleGetActorStats))
	mux.HandleFunc("POST /v1alpha1/actors/{id}/resume", s.wrap(s.handleResumeActor))
	// Read-only dial-in presence (task t6): its own route rather than a
	// block on the actors list — see dialinpresence.go for the argument.
	mux.HandleFunc("GET /v1alpha1/dial-in-presence", s.wrap(s.handleListDialInPresence))
	mux.HandleFunc("POST /v1alpha1/inbound/poll", s.handleInboundPoll)
	mux.HandleFunc("POST /v1alpha1/inbound/{id}/complete", s.handleInboundComplete)
	// Issue #111's dial-in half: the control plane mints what a bridge
	// presents (see inboundcredentials.go). Registered before the {id}
	// wildcard route above would ever be consulted for these paths — Go's
	// mux prefers the more specific literal pattern.
	mux.HandleFunc("POST /v1alpha1/inbound/credentials", s.wrap(s.handleIssueInboundCredential))
	mux.HandleFunc("POST /v1alpha1/inbound/credentials/revoke", s.wrap(s.handleRevokeInboundCredential))

	mux.HandleFunc("POST /v1alpha1/schedules", s.wrap(s.handleCreateSchedule))
	mux.HandleFunc("GET /v1alpha1/schedules", s.wrap(s.handleListSchedules))
	mux.HandleFunc("GET /v1alpha1/schedules/{id}", s.wrap(s.handleGetSchedule))
	mux.HandleFunc("PATCH /v1alpha1/schedules/{id}", s.wrap(s.handlePatchSchedule))
	mux.HandleFunc("DELETE /v1alpha1/schedules/{id}", s.wrap(s.handleDeleteSchedule))

	mux.HandleFunc("GET /v1alpha1/dispatch-rates", s.wrap(s.handleListDispatchRates))
	mux.HandleFunc("GET /v1alpha1/namespaces", s.wrap(s.handleListNamespaces))
	mux.HandleFunc("POST /v1alpha1/namespaces", s.wrap(s.handleCreateNamespace))

	mux.HandleFunc("GET /v1alpha1/preflights", s.wrap(s.handleListPreflights))
	mux.HandleFunc("GET /v1alpha1/preflights/{id}", s.wrap(s.handleGetPreflight))
	mux.HandleFunc("POST /v1alpha1/preflights/{id}/acknowledge", s.wrap(s.handleAcknowledgePreflight))

	// The flow store's registry surface (task t7, issue #192). The literal
	// /pull route is registered alongside the {id} wildcard; Go's mux
	// prefers the more specific literal pattern, the same shape the inbound
	// credential routes rely on above.
	mux.HandleFunc("POST /v1alpha1/store/entries", s.wrap(s.handleCreateStoreEntry))
	mux.HandleFunc("POST /v1alpha1/store/entries/pull", s.wrap(s.handleStoreEntryPull))
	mux.HandleFunc("GET /v1alpha1/store/entries", s.wrap(s.handleListStoreEntries))
	mux.HandleFunc("GET /v1alpha1/store/entries/{id}", s.wrap(s.handleGetStoreEntry))
	// The import mapping step (task t8): bind a pulled entry's declared
	// capability requirements to local registrations, read the record trail
	// back, and publish the embedded graph verbatim once nothing is unbound.
	mux.HandleFunc("POST /v1alpha1/store/entries/{id}/bindings", s.wrap(s.handleCreateStoreBinding))
	mux.HandleFunc("GET /v1alpha1/store/entries/{id}/bindings", s.wrap(s.handleListStoreBindings))
	mux.HandleFunc("POST /v1alpha1/store/entries/{id}/publish", s.wrap(s.handlePublishStoreEntry))

	mux.HandleFunc("POST /v1alpha1/plan-imports", s.wrap(s.handleImportPlan))
	mux.HandleFunc("GET /v1alpha1/plan-imports", s.wrap(s.handleListPlanImports))
	mux.HandleFunc("GET /v1alpha1/plan-imports/{id}", s.wrap(s.handleGetPlanImport))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/reviews", s.wrap(s.handleCreateReview))
	mux.HandleFunc("POST /v1alpha1/reviews/{id}/commit", s.wrap(s.handleCommitReview))
	mux.HandleFunc("GET /v1alpha1/pending-decisions", s.wrap(s.handleListPendingDecisions))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/grades", s.wrap(s.handleCreateGrade))

	mux.HandleFunc("POST /v1alpha1/runs/{id}/suite-verdicts", s.wrap(s.handleCreateSuiteVerdict))
	mux.HandleFunc("POST /v1alpha1/runs/{id}/gate-reports", s.wrap(s.handleCreateGateReport))

	mux.HandleFunc("GET /v1alpha1/human-tasks", s.wrap(s.handleListHumanTasks))
	mux.HandleFunc("GET /v1alpha1/human-tasks/{id}", s.wrap(s.handleGetHumanTask))
	mux.HandleFunc("POST /v1alpha1/human-tasks/{id}/decision", s.wrap(s.handleDecideHumanTask))

	mux.HandleFunc("GET /v1alpha1/version", s.wrap(s.handleVersion))
	mux.HandleFunc("GET /v1alpha1/healthz", s.wrap(s.handleHealthz))
	mux.HandleFunc("GET /v1alpha1/readyz", s.wrap(s.handleReadyz))
	mux.HandleFunc("GET /v1alpha1/whoami", s.wrap(s.handleWhoami))

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
			// Task t10: a `completed` event that reports a handover ref gets
			// the ref fetched and measured. Nil unless the deployment
			// configured a remote (WithHandoverObserver), in which case
			// nothing is fetched and nothing is recorded.
			Handover: s.handoverObserver,
		})))
		mux.HandleFunc("POST "+runnerCallbackRoutePattern, s.handleRunnerOperationEvent)
	}
	// Unlike the unversioned actor callback protocol above, this documented
	// API route is always present. Missing signer/router configuration fails
	// closed in the handler; it must never turn a declared operation into an
	// accidental 404 or an authless write surface.
	mux.HandleFunc("POST /v1alpha1/attempts/{attemptID}/artifacts", s.wrap(s.handlePutArtifact))
	mux.HandleFunc("GET /v1alpha1/attempts/{attemptID}/artifacts", s.wrap(s.handleListAttemptArtifacts))
	mux.HandleFunc("GET /v1alpha1/attempts/{attemptID}/artifacts/{name}", s.wrap(s.handleGetAttemptArtifact))

	if s.webAssets != nil {
		mux.Handle("GET /", spaHandler(s.webAssets))
	}

	return mux
}

func (s *Server) accessRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1alpha1/webhooks/jira", s.wrap(s.handleJiraWebhook))
	mux.Handle("/", s.routes())
	return mux
}

// spaHandler serves the embedded web build: real files as-is, everything
// else (client-side routes like /runs/abc) falls back to index.html. It
// never shadows a DECLARED /v1alpha1 operation — the mux's more-specific API
// patterns win — but it is where an UNdeclared one lands, and that is the
// defect issue #8 records: `GET /v1alpha1/pending-decisions` against a
// binary that predates the endpoint answered 200 with index.html, so a
// client could not tell an absent endpoint from an empty one. Any path under
// the API group is refused here instead. It is deliberately NOT registered
// as a mux pattern (`mux.HandleFunc("/v1alpha1/", http.NotFound)`): a
// pattern that broad matches wrong-method requests too, which turns the
// mux's own 405 for a real operation into a 404 and breaks the route sweep
// that probes with DELETE.
func spaHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1alpha1/") {
			http.NotFound(w, r)
			return
		}
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

// runnerCallbackRoutePattern is derived from the runner client's URL format
// so the offered callback URL and the API mux cannot drift apart.
var runnerCallbackRoutePattern = fmt.Sprintf(runners.CallbackPathFormat, "{id}")
