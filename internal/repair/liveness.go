package repair

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Lane liveness routing (plan loop-closure t10; spec c26/c33, decision c43).
//
// The dispatch site consults the persisted actor_liveness row before it
// resolves an endpoint (internal/worker/liveness.go). When the row says the
// lane's SESSION cannot start — a fresh session_ok=false, or a control-plane
// lock of any age — and the actor's registration names a fallback_actor, the
// dispatch goes to the fallback and this record says so. When it names none,
// the dispatch proceeds and this record is the warning that it did.
//
// It lives in this package rather than in the worker because it is a routing
// record in the same shape as the gate-failure routing above it: a derived
// `decision` with question/selected/options/reason/rationale/router. One
// shape means one way to read "the control plane decided where this went",
// and a distinct `router` value means PriorAttempts — which selects on
// RouterCollectionMethod — never counts a liveness reroute as a repair round.

// LivenessRouterMethod names how a liveness routing was produced, for the
// record's `router` field. Distinct from RouterCollectionMethod on purpose.
const LivenessRouterMethod = "lane_liveness"

// ReasonLaneNotLive is the one reason a liveness routing carries: the lane's
// session cannot start. What made it so (spent token, expired credential,
// failed probe) is the row's own reason, carried verbatim under `liveness`.
const ReasonLaneNotLive Reason = "lane_not_live"

// LivenessSelected is what the router chose.
type LivenessSelected string

const (
	// LivenessSelectedFallback: the dispatch went to the registered
	// fallback actor instead of the lane the node named.
	LivenessSelectedFallback LivenessSelected = "fallback"
	// LivenessSelectedProceed: the dispatch went to the lane the node named
	// even though it is not live, because no fallback is registered (or the
	// fallback is not live either). A warning, not a refusal: refusing would
	// turn one lane's login state into a dead node run.
	LivenessSelectedProceed LivenessSelected = "proceed"
)

// LivenessLane is the lane the node named and the liveness fact read for it.
type LivenessLane struct {
	ActorKey string
	ActorRef string
	// The persisted row, verbatim.
	SessionOK bool
	Reason    string
	Mode      string
	Locked    bool
	CheckedAt time.Time
	Source    string
}

// LivenessInput is everything a liveness routing record is a function of.
type LivenessInput struct {
	RunID     string
	NodeRunID string
	AttemptID string
	NodeID    string

	Lane LivenessLane
	// FallbackActorKey/Ref name the registered fallback; both empty when the
	// actor registers none.
	FallbackActorKey string
	FallbackActorRef string
	// FallbackNotLive, when non-empty, is why the registered fallback was
	// not taken either (its own row says it is not live).
	FallbackNotLive string
	// FreshnessWindow is the router's constant, carried so a reader knows
	// what "fresh" meant when this was decided.
	FreshnessWindow time.Duration

	RouterActorID  string
	RouterRevision string
	Now            time.Time
}

// LivenessRouting is the decision. Like Routing it is a value: composing it
// writes nothing.
type LivenessRouting struct {
	Selected LivenessSelected
}

// Record composes the derived decision record, or refuses.
func (r LivenessRouting) Record(in LivenessInput) (ledger.Record, error) {
	if strings.TrimSpace(in.RunID) == "" {
		return ledger.Record{}, fmt.Errorf("repair: a liveness routing must name the run it routed")
	}
	if strings.TrimSpace(in.RouterActorID) == "" {
		return ledger.Record{}, fmt.Errorf(
			"repair: a derived record needs an identified deterministic producer (PRD §10.4); " +
				"an anonymous router attests to nothing")
	}
	if r.Selected != LivenessSelectedFallback && r.Selected != LivenessSelectedProceed {
		return ledger.Record{}, fmt.Errorf("repair: liveness routing selected %q, want fallback or proceed", r.Selected)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	lane := in.Lane.ActorKey
	if lane == "" {
		lane = in.Lane.ActorRef
	}
	fact := fmt.Sprintf("session_ok=%t reason=%s", in.Lane.SessionOK, in.Lane.Reason)
	if in.Lane.Locked {
		fact += " locked=true (a control-plane lock does not expire by time; it clears on resume AND a healthy bridge fact)"
	} else {
		fact += fmt.Sprintf(" checked_at=%s (%s old, window %s)",
			in.Lane.CheckedAt.UTC().Format(time.RFC3339), now.Sub(in.Lane.CheckedAt).Round(time.Second), in.FreshnessWindow)
	}

	var rationale string
	switch r.Selected {
	case LivenessSelectedFallback:
		rationale = fmt.Sprintf(
			"node %q names %s, whose persisted liveness row says its session cannot start: %s. Its registration "+
				"names %s as fallback_actor, so this dispatch goes there instead; the node's own outcome and retry "+
				"policy are unchanged, only the lane is",
			in.NodeID, lane, fact, in.FallbackActorKey)
	case LivenessSelectedProceed:
		why := "its registration names no fallback_actor"
		if in.FallbackNotLive != "" {
			why = fmt.Sprintf("its registered fallback %s is not live either (%s)", in.FallbackActorKey, in.FallbackNotLive)
		}
		rationale = fmt.Sprintf(
			"node %q names %s, whose persisted liveness row says its session cannot start: %s. %s, so the dispatch "+
				"proceeds into the lane anyway and this record is the warning that it did — a refusal here would turn "+
				"one lane's login state into a dead node run. Expect the attempt to fail with class credential_spent "+
				"until a human logs the lane back in",
			in.NodeID, lane, fact, why)
	}

	data := map[string]any{
		"question":  fmt.Sprintf("node %q is addressed to %s, whose session cannot start — where does the dispatch go?", in.NodeID, lane),
		"selected":  string(r.Selected),
		"options":   []string{string(LivenessSelectedFallback), string(LivenessSelectedProceed)},
		"reason":    string(ReasonLaneNotLive),
		"rationale": rationale,
		"router":    LivenessRouterMethod,
		"node_id":   in.NodeID,
		// The attempt is named in the payload, not the envelope: a routing
		// is decided BEFORE the attempt row exists (attempts commit with
		// their completion), and ledger_records.attempt_id is a foreign key.
		"attempt_id":     in.AttemptID,
		"lane_actor_ref": in.Lane.ActorRef,
		"liveness": map[string]any{
			"session_ok": in.Lane.SessionOK,
			"reason":     in.Lane.Reason,
			"mode":       in.Lane.Mode,
			"locked":     in.Lane.Locked,
			"checked_at": in.Lane.CheckedAt.UTC().Format(time.RFC3339),
			"source":     in.Lane.Source,
		},
		"freshness_window_seconds": int(in.FreshnessWindow / time.Second),
		// Unlike a gate-failure routing, the party composing this record IS
		// the dispatcher, and it dispatched what it selected.
		"dispatched": true,
	}
	if in.Lane.ActorKey != "" {
		data["lane_actor_key"] = in.Lane.ActorKey
	}
	if in.FallbackActorKey != "" {
		data["fallback_actor_key"] = in.FallbackActorKey
		data["fallback_actor_ref"] = in.FallbackActorRef
	}
	if in.FallbackNotLive != "" {
		data["fallback_not_live"] = in.FallbackNotLive
	}
	if r.Selected == LivenessSelectedProceed {
		data["severity"] = "warning"
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("repair: encode liveness routing payload: %w", err)
	}
	return ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      in.RunID,
		NodeRunID:  ledger.NullableID(in.NodeRunID),
		Origin: ledger.Origin{
			Kind:          ledger.OriginValidator,
			ActorID:       in.RouterActorID,
			ActorRevision: in.RouterRevision,
		},
		Authority:      ledger.AuthorityDerived,
		ProvenanceRefs: []string{},
		Data:           payload,
	}, nil
}
