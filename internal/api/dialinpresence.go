package api

import (
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// GET /v1alpha1/dial-in-presence — which bridges are connected right now
// (task t6, issues #136 and #121).
//
// # Why this is its own route rather than a block on GET /v1alpha1/actors
//
// The actors list renders EVERY REVISION of every actor row, because actor
// identity is append-only. Dial-in presence is keyed by actor_key: one
// identity has one connection no matter how many times it has been
// re-registered. Attaching a presence block to the actors list would render
// the same connection state once per revision, and the operator's question —
// "are all five bridges up?" — would have to be answered by de-duplicating a
// list whose length is a registration-history artifact. The availability and
// dispatch-rate blocks already live on the actors list under exactly that
// duplication, and they get away with it because nobody counts them; a
// fleet-presence check is a thing you count.
//
// The second reason is the one that matters at 03:00: this is the surface an
// operator reaches for when something is wrong, and it must not depend on the
// heaviest read in the API. GET /v1alpha1/actors already fans out into three
// queries and grows with registration history. This is one query, whose row
// count is the number of distinct identities.
//
// # Why it is never a probe
//
// Nothing here opens a connection to a participant. That is the entire point
// of the cutover this view belongs to: the control plane holds no address for
// a bridge, so there is nothing to probe, and reintroducing a probe would
// reintroduce the address. Presence is a fact PostgreSQL already holds — a
// bridge dialling POST /v1alpha1/inbound/poll writes its own last_seen_at —
// and this route reads it. See TestDialInPresenceIsServedWithoutProbing.

// DialInPresenceOut is components.schemas.DialInPresence: one registered
// actor identity's connection state.
type DialInPresenceOut struct {
	ActorKey string `json:"actor_key"`
	// ActorID/Revision/Kind describe the current (highest) registration
	// revision, so a row here can be followed to GET /v1alpha1/actors/{id}.
	ActorID  string `json:"actor_id"`
	Revision int32  `json:"revision"`
	Kind     string `json:"kind"`

	// Presence is `connected`, `disconnected`, or `never_dialled` — three
	// values on purpose (see actors.DialInPresenceState): a bridge nobody
	// ever configured and a bridge that died an hour ago are different
	// problems.
	Presence string `json:"presence"`
	// LastSeenAt is present for `connected` and `disconnected`, absent for
	// `never_dialled`. On a dropped connection it is the whole answer: it
	// says WHEN, which a boolean cannot.
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	// SecondsSinceLastSeen saves the reader arithmetic against a timestamp
	// in another timezone at 03:00. Absent, never 0, when nothing was ever
	// seen — a fabricated 0 there would read as "seen just now".
	SecondsSinceLastSeen *float64 `json:"seconds_since_last_seen,omitempty"`

	// Credential is the admission-control state for this actor key, absent
	// when no inbound_authentication record exists at all. An actor can look
	// absent for four unrelated reasons — its process is down, its
	// credential was revoked, it is locked out after repeated failures, or
	// it was never issued a control-plane credential — and only the first is
	// an outage.
	Credential *DialInCredentialOut `json:"credential,omitempty"`
}

// DialInCredentialOut carries no verifier material: not the digest, not the
// environment variable name. Neither is needed to answer "why is this bridge
// not dialling in", and a read surface that rendered either would be a
// standing invitation to leak one.
type DialInCredentialOut struct {
	// Issued is whether THIS control plane minted the credential (migration
	// 0037). False means the record was provisioned by hand, which the
	// shipped admission default refuses as `not_control_plane_issued` —
	// a bridge in that state never dials in successfully, however healthy
	// its process is.
	Issued        bool       `json:"issued"`
	IssuedAt      *time.Time `json:"issued_at,omitempty"`
	IssuanceCount int        `json:"issuance_count"`
	// Revoked/RevokedAt: revocation is positive durable evidence (migration
	// 0032), so it renders as an instant, not as an inference from absence.
	Revoked   bool       `json:"revoked"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// LockedOut is evaluated against this response's own observation instant
	// rather than left for a client to derive, the same way
	// ActorAvailabilityOut.Paused is. A lapsed lockout renders as
	// `locked_out: false` with locked_until still present — distinct from a
	// credential that was never locked out at all, which carries no
	// locked_until.
	LockedOut    bool       `json:"locked_out"`
	LockedUntil  *time.Time `json:"locked_until,omitempty"`
	FailureCount int        `json:"failure_count"`
}

// DialInPresenceListOut is components.schemas.DialInPresenceList.
//
// The counts are not a client-side convenience: the cutover's
// fallback-disable step has to establish that ALL bridges are dialled in AT
// THAT MOMENT (spec claim c49), and "connected == len(items)" is a check an
// operator or a script can make in one read.
type DialInPresenceListOut struct {
	// ObservedAt is the single instant every row in this response was
	// classified against, so two rows never disagree about "now".
	ObservedAt time.Time `json:"observed_at"`
	// WindowSeconds is actors.DialInFreshness — published because "connected"
	// is a claim with a definition, and a reader must be able to see it.
	WindowSeconds float64 `json:"window_seconds"`

	Connected    int `json:"connected"`
	Disconnected int `json:"disconnected"`
	NeverDialled int `json:"never_dialled"`

	Items []DialInPresenceOut `json:"items"`
}

func dialInPresenceOut(row postgres.DialInPresenceRow, now time.Time) DialInPresenceOut {
	out := DialInPresenceOut{
		ActorKey:   row.ActorKey,
		ActorID:    row.ActorID,
		Revision:   row.Revision,
		Kind:       row.Kind,
		Presence:   string(actors.ClassifyDialInPresence(row.LastSeenAt, now)),
		LastSeenAt: row.LastSeenAt,
	}
	if row.LastSeenAt != nil {
		since := now.Sub(*row.LastSeenAt).Seconds()
		out.SecondsSinceLastSeen = &since
	}
	if row.Credential != nil {
		out.Credential = &DialInCredentialOut{
			Issued:        row.Credential.Issued,
			IssuedAt:      row.Credential.IssuedAt,
			IssuanceCount: row.Credential.IssuanceCount,
			Revoked:       row.Credential.RevokedAt != nil,
			RevokedAt:     row.Credential.RevokedAt,
			LockedOut:     row.Credential.LockedUntil != nil && row.Credential.LockedUntil.After(now),
			LockedUntil:   row.Credential.LockedUntil,
			FailureCount:  row.Credential.FailureCount,
		}
	}
	return out
}

// handleListDialInPresence is GET /v1alpha1/dial-in-presence.
//
// Read-only and authless, like every other read in this phase-1 API (PRD
// spec decision c45). It reveals no credential material and no address —
// there is no address to reveal — so it carries the same exposure as
// GET /v1alpha1/actors, which already renders the whole registration row.
func (s *Server) handleListDialInPresence(w http.ResponseWriter, r *http.Request) error {
	// One instant for the whole response: the cutoff the query filters on
	// and the classification each row is rendered with must be the same
	// `now`, or a slow query could return a row the renderer then disagrees
	// with.
	now := time.Now().UTC()
	rows, err := s.Store.DialInPresence(r.Context(), s.NamespaceID, actors.DialInPresenceCutoff(now))
	if err != nil {
		return internalError(err)
	}

	out := DialInPresenceListOut{
		ObservedAt:    now,
		WindowSeconds: actors.DialInFreshness.Seconds(),
		Items:         make([]DialInPresenceOut, 0, len(rows)),
	}
	for _, row := range rows {
		item := dialInPresenceOut(row, now)
		switch actors.DialInPresenceState(item.Presence) {
		case actors.DialInConnected:
			out.Connected++
		case actors.DialInDisconnected:
			out.Disconnected++
		case actors.DialInNeverDialled:
			out.NeverDialled++
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
