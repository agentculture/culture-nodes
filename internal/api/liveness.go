package api

import (
	"context"
	"time"

	"github.com/agentculture/culture-nodes/internal/mesh"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Actor liveness on the read surfaces (plan loop-closure t10; spec c23/c33,
// decision c43). Three things live here and nowhere else in this package:
//
//   - ActorLivenessOut, the persisted actor_liveness row rendered for
//     GET /v1alpha1/actors, GET /v1alpha1/actors/{id}, the resume response,
//     and each GET /v1alpha1/mesh actor;
//   - CollectorLivenessSink, the adapter that lets the mesh collector (which
//     runs in this process and may not import the store's engine scope)
//     persist a bridge fact as a source=collector row;
//   - the resume semantics for the lock (unlockLiveness), called from
//     handleResumeActor.
//
// actors.go and mesh.go are both large; this file is where the liveness
// logic lives so they only gain a field and a call each.

// ActorLivenessOut is components.schemas.ActorLiveness: one actor_liveness
// row. Absent from an actor entirely when nothing has ever observed the
// actor's session — a different fact from a row that says "unmeasured".
type ActorLivenessOut struct {
	// SessionOK is three-valued: true, false, or null for "nobody measured
	// this". It is never omitted, so a reader can tell null from absent.
	SessionOK *bool `json:"session_ok"`
	// Reason is the bridge's closed vocabulary (ok, unmeasured,
	// refresh_token_spent, not_logged_in, credential_expired, probe_timeout,
	// probe_failed).
	Reason string `json:"reason"`
	// Mode is LOCK or CHECK as the bridge advertised, "" when the last
	// writer was not a bridge fact.
	Mode      string    `json:"mode"`
	CheckedAt time.Time `json:"checked_at"`
	// Source is who last wrote the fact: collector, worker, or resume.
	Source string `json:"source"`
	// Locked is the control-plane lock (decision c43): set when an attempt
	// failed with class credential_spent, cleared by resume. A locked lane
	// is not live regardless of CheckedAt.
	Locked            bool   `json:"locked"`
	LockedByRunID     string `json:"locked_by_run_id,omitempty"`
	LockedByAttemptID string `json:"locked_by_attempt_id,omitempty"`
	// Live is the router's own verdict on this row at render time, computed
	// with the same rule the dispatch site applies
	// (postgres.ActorLiveness.Live, window postgres.LivenessFreshness) so an
	// operator reading "live: false" is reading what the next lease will
	// decide, not deriving it from four fields and a clock.
	Live bool `json:"live"`
}

func actorLivenessOut(row postgres.ActorLiveness, now time.Time) *ActorLivenessOut {
	return &ActorLivenessOut{
		SessionOK:         row.SessionOK,
		Reason:            row.Reason,
		Mode:              row.Mode,
		CheckedAt:         row.CheckedAt,
		Source:            row.Source,
		Locked:            row.Locked,
		LockedByRunID:     row.LockedByRunID,
		LockedByAttemptID: row.LockedByAttemptID,
		Live:              row.Live(now),
	}
}

// withLiveness attaches the liveness row for this actor's key, when there
// is one. Keyed by actor_key like availability and dispatch_rate, for the
// same reason: a credential belongs to the identity, not to a revision.
func withLiveness(out ActorOut, row postgres.ActorLiveness, ok bool, now time.Time) ActorOut {
	if ok {
		out.Liveness = actorLivenessOut(row, now)
	}
	return out
}

// unlockLiveness is resume's half of decision c43's AND: clear the
// control-plane lock and nothing else. session_ok stays what was last
// observed, so a resume alone never reopens a lane whose last fact is false
// — the lane leases again only once a later collector write says
// session_ok=true. An actor with no row is a no-op, not an error.
func (s *Server) unlockLiveness(ctx context.Context, actorKey string) error {
	_, _, err := s.engineStore.UnlockActorLiveness(ctx, actorKey)
	return err
}

// CollectorLivenessSink adapts the store to mesh.LivenessSink: every fact
// the collector observes becomes a source=collector upsert on the actor's
// key. cmd/nodes/serve.go wires it into the collector it builds.
func CollectorLivenessSink(store *postgres.Store, namespaceID string) mesh.LivenessSink {
	return collectorLivenessSink{store: store, namespaceID: namespaceID}
}

type collectorLivenessSink struct {
	store       *postgres.Store
	namespaceID string
}

func (s collectorLivenessSink) RecordLiveness(ctx context.Context, actorKey string, fact mesh.Liveness) error {
	_, err := s.store.RecordActorLiveness(ctx, postgres.RecordActorLivenessInput{
		NamespaceID: s.namespaceID,
		ActorKey:    actorKey,
		SessionOK:   fact.SessionOK,
		Reason:      fact.Reason,
		Mode:        fact.Mode,
		CheckedAt:   fact.CheckedAt,
		Source:      postgres.LivenessSourceCollector,
	})
	return err
}
