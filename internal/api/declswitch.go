package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The declaration engine's control-plane wiring (task t38, #328):
//
//   - WithDeclarationEngine gives this server the running engine, so every
//     delivery that offers its fact to graph triggers (POST /v1alpha1/events
//     and the GitHub and Jira webhooks) also offers it to declarations, after
//     the delivery commits (postgres.DeliveredEventHandler).
//   - GET/POST /v1alpha1/declaration-engine/switch read and flip the
//     namespace's before/shadow/after switch (c80). A flip is a human
//     decision, never an agent's: the allow-list classifier the declaration
//     routes already use (declarationPrincipal) decides, and an agent
//     principal is refused. A flip to 'before' freezes open declaration
//     nodes inside the flip's own transaction (FreezeHook); a flip to
//     'after' thaws and replays them once the flip has committed
//     (ThawAndReplay cannot be a hook -- see freeze.go). Both run through
//     declengine.FlipSwitch, under the namespace's exclusive switch lock,
//     so a flip never lands underneath an in-flight evaluation (t38b).

// WithDeclarationEngine wires the declaration engine into this server's
// event deliveries and lets the switch route flip to 'shadow' or 'after'.
// Omitting it (or passing nil) leaves every delivery exactly as it was
// before t38, and the switch route then refuses any flip but 'before' --
// flipping to 'after' with no engine running would drain the graph engine
// (DrainGate) while nothing evaluates declarations.
func WithDeclarationEngine(e *declengine.Engine) Option {
	return func(s *Server) {
		if e != nil {
			s.declEngine = e
		}
	}
}

// declarationHandler is the post-commit handler deliveries pass as
// DeliverSignalEventInput.Declarations. A nil *Engine must become a nil
// interface, not a typed nil, so the store sees "no handler".
func (s *Server) declarationHandler() postgres.DeliveredEventHandler {
	if s.declEngine == nil {
		return nil
	}
	return declengine.Router{Engine: s.declEngine, Switch: declengine.PostgresSwitchStore{Store: s.Store}}
}

// logDeclarationErr surfaces a post-commit declaration failure. It never
// changes the delivery's response: the fact committed and the graph engine
// acted on it; the declaration engine recorded its own evaluation failures.
func (s *Server) logDeclarationErr(d postgres.SignalDelivery) {
	if d.DeclarationErr != nil {
		s.log.Warn("declaration engine: delivered event not fully evaluated", "event_id", d.Event.ID, "name", d.Event.Name, "error", d.DeclarationErr)
	}
}

// DeclarationEngineSwitchOut is GET/POST /v1alpha1/declaration-engine/switch's
// payload.
type DeclarationEngineSwitchOut struct {
	NamespaceID string `json:"namespace_id"`
	Mode        string `json:"mode"`
	// Previous is set on a flip: the mode it replaced.
	Previous string `json:"previous,omitempty"`
	// EngineEnabled reports whether this control plane runs the declaration
	// engine at all (WithDeclarationEngine).
	EngineEnabled bool `json:"engine_enabled"`
	// ReplayError is set when a flip to 'after' committed but thawing and
	// replaying frozen nodes failed. The flip stands; the scheduler's
	// declaration pass re-runs the replay on its next tick.
	ReplayError string `json:"replay_error,omitempty"`
}

type flipDeclarationSwitchRequest struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason,omitempty"`
}

func forbidden(remediation, message string) *apiError {
	return newAPIError(http.StatusForbidden, clifmt.ExitUserError, message, remediation)
}

func (s *Server) handleGetDeclarationSwitch(w http.ResponseWriter, r *http.Request) error {
	mode, err := (declengine.PostgresSwitchStore{Store: s.Store}).Mode(r.Context(), s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, DeclarationEngineSwitchOut{NamespaceID: s.NamespaceID, Mode: mode, EngineEnabled: s.declEngine != nil})
	return nil
}

func (s *Server) handleFlipDeclarationSwitch(w http.ResponseWriter, r *http.Request) error {
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	if principal.Kind != declengine.PrincipalHuman {
		return forbidden("ask a person to flip the switch through Cloudflare Access",
			"the engine switch is a human decision; an agent principal may not flip it")
	}
	var req flipDeclarationSwitchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest(`send a JSON body {"mode": "before"|"shadow"|"after", "reason": "..."}`, "decode request body: %v", err)
	}
	if !declengine.ValidMode(req.Mode) {
		return badRequest(`mode must be "before", "shadow" or "after"`, "invalid engine switch mode %q", req.Mode)
	}
	if p, ok := PrincipalFromContext(r.Context()); ok && p.Provider == principalProviderInboundCredential && req.Mode != declengine.ModeBefore {
		return forbidden("use Cloudflare Access to flip the engine switch to shadow or after",
			"a break-glass credential may only flip the engine switch to before")
	}
	if req.Mode != declengine.ModeBefore && s.declEngine == nil {
		return conflict("enable the declaration engine on this control plane (NODES_DECLARATION_ENGINE=on and "+declengine.DefaultMarkerKeyEnv+") before flipping to shadow or after",
			"the declaration engine is not running here; flipping to %q would evaluate nothing", req.Mode)
	}
	// FlipSwitch holds the namespace's exclusive switch lock across the flip
	// and, on 'after', its thaw and replay: it waits for every declaration
	// evaluation already in flight, and none starts until it is done.
	previous, replayErr, err := declengine.FlipSwitch(r.Context(), declengine.PostgresSwitchStore{Store: s.Store}, s.declEngine,
		s.NamespaceID, req.Mode, principal.Author, req.Reason, time.Now)
	if err != nil {
		return internalError(err)
	}
	out := DeclarationEngineSwitchOut{NamespaceID: s.NamespaceID, Mode: req.Mode, Previous: previous, EngineEnabled: s.declEngine != nil}
	if replayErr != nil {
		out.ReplayError = replayErr.Error()
		s.log.Warn("declaration engine: thaw and replay after flip", "namespace_id", s.NamespaceID, "error", replayErr)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
