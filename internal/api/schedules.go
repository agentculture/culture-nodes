package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The declared-cadence surface (issue #107, task t33).
//
// A schedule is the one piece of this system that acts with nobody watching,
// so the read surface is not a nicety: "why has nothing run since Tuesday"
// has to be answerable from the API alone, which is why ScheduleOut carries
// next_fire_at, last_fired_at, the id of the event the last fire appended,
// and both counters rather than just the declaration that was posted.
//
// Enabling and disabling is a PATCH of one field rather than a pair of
// /enable and /disable routes: it is a mutable property of a declaration, not
// an action taken against a thing, and the actor-resume endpoint next door
// (POST /actors/{id}/resume) is a genuinely different case -- it clears an
// inference the control plane made on its own, which is an act.

// ScheduleOut is components.schemas.Schedule.
type ScheduleOut struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	EventName string          `json:"event_name"`
	Emitter   string          `json:"emitter"`
	Payload   json.RawMessage `json:"payload"`
	// IntervalSeconds is the declared cadence. Seconds rather than a duration
	// string because this is the wire, and a client that has to parse "24h0m0s"
	// to decide whether to show "daily" is a client this API made work for.
	IntervalSeconds int64 `json:"interval_seconds"`
	// CatchUp is "fire-once" or "skip": what happens to an occurrence that
	// came due while nothing was running. See postgres.ScheduleCatchUp.
	CatchUp    string    `json:"catch_up"`
	Enabled    bool      `json:"enabled"`
	NextFireAt time.Time `json:"next_fire_at"`
	// LastFiredAt and LastEventID are absent until the first fire. LastEventID
	// is the whole audit trail in one field: it names the signal_events fact
	// this schedule appended, from which the runs it triggered are reachable.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	LastEventID string     `json:"last_event_id,omitempty"`
	FireCount   int64      `json:"fire_count"`
	// SkipCount is how many occurrences were declined under catch_up=skip. It
	// is reported separately from FireCount because "declined 6 nightly runs
	// while we were down" and "never came due" must not look the same.
	SkipCount int64 `json:"skip_count"`
	// SuppressedCount is how many DUE occurrences were held back because this
	// schedule's last two runs failed with the same reason (task t9, issue
	// #253). It is separate from SkipCount for the same reason SkipCount is
	// separate from FireCount: "the operator declared a catch-up policy" and
	// "the environment is broken" are different facts about why nothing ran,
	// and an operator asking why has to be able to tell them apart.
	SuppressedCount int64 `json:"suppressed_count"`
	// LastFailureDetail is the repeated reason, verbatim — the last minted
	// run's last attempt's result.error.detail. Absent when the schedule is
	// not currently failing, which makes its PRESENCE the answer to "is this
	// schedule holding back, and why".
	LastFailureDetail string    `json:"last_failure_detail,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ScheduleListOut is components.schemas.ScheduleList.
type ScheduleListOut struct {
	Items []ScheduleOut `json:"items"`
}

func scheduleOut(sc postgres.Schedule) ScheduleOut {
	out := ScheduleOut{
		ID: sc.ID, Name: sc.Name, EventName: sc.EventName, Emitter: sc.Emitter,
		Payload:           nonNullJSON(sc.Payload),
		IntervalSeconds:   int64(sc.Interval / time.Second),
		CatchUp:           string(sc.CatchUp),
		Enabled:           sc.Enabled,
		NextFireAt:        sc.NextFireAt,
		LastEventID:       sc.LastEventID,
		FireCount:         sc.FireCount,
		SkipCount:         sc.SkipCount,
		SuppressedCount:   sc.SuppressedCount,
		LastFailureDetail: sc.LastFailureDetail,
		CreatedAt:         sc.CreatedAt,
		UpdatedAt:         sc.UpdatedAt,
	}
	if !sc.LastFiredAt.IsZero() {
		at := sc.LastFiredAt
		out.LastFiredAt = &at
	}
	return out
}

// createScheduleRequest is components.schemas.CreateScheduleRequest.
type createScheduleRequest struct {
	Name            string          `json:"name"`
	EventName       string          `json:"event_name"`
	Emitter         string          `json:"emitter"`
	Payload         json.RawMessage `json:"payload"`
	IntervalSeconds int64           `json:"interval_seconds"`
	CatchUp         string          `json:"catch_up"`
	// Enabled defaults to true when omitted. It is a pointer so "declared
	// disabled" and "did not mention it" stay distinguishable -- a plain bool
	// would make every client that omits the field create a paused schedule.
	Enabled *bool `json:"enabled"`
	// FirstFireAt is the first instant this schedule is due, and the phase
	// every later occurrence aligns to. Omitted means one interval from now.
	FirstFireAt *time.Time `json:"first_fire_at"`
}

// patchScheduleRequest is components.schemas.PatchScheduleRequest. Only
// `enabled` is patchable, deliberately: changing the cadence or the payload of
// a live schedule would leave already-fired runs referring to a declaration
// that no longer exists anywhere, so those are a delete and a create.
type patchScheduleRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleCreateSchedule is POST /v1alpha1/schedules.
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) error {
	var req createScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateScheduleRequest: {name, event_name, interval_seconds, payload?, catch_up?, enabled?, first_fire_at?}",
			"decode request body: %v", err)
	}
	if req.IntervalSeconds <= 0 {
		return badRequest("interval_seconds must be a positive whole number of seconds",
			"interval_seconds is %d", req.IntervalSeconds)
	}

	in := postgres.CreateScheduleInput{
		NamespaceID: s.NamespaceID,
		Name:        req.Name,
		EventName:   req.EventName,
		Emitter:     req.Emitter,
		Payload:     req.Payload,
		Interval:    time.Duration(req.IntervalSeconds) * time.Second,
		CatchUp:     postgres.ScheduleCatchUp(req.CatchUp),
		Disabled:    req.Enabled != nil && !*req.Enabled,
	}
	if req.FirstFireAt != nil {
		in.FirstFireAt = *req.FirstFireAt
	}

	sc, err := s.Store.CreateSchedule(r.Context(), in)
	if err != nil {
		// Every refusal CreateSchedule raises is about the declaration the
		// caller sent -- an interval it cannot honour, a policy it does not
		// know, a name already taken -- so it is a 400/409, not a 500.
		if isDuplicateKey(err) {
			return conflict("choose a schedule name not already used in this namespace",
				"a schedule named %q already exists", req.Name)
		}
		return badRequest("correct the schedule declaration and resend it", "%v", err)
	}
	writeJSON(w, http.StatusCreated, scheduleOut(sc))
	return nil
}

// handleListSchedules is GET /v1alpha1/schedules. Disabled schedules are
// included: a schedule that is not running is exactly what an operator asking
// "why has nothing happened" is looking for.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListSchedules(r.Context(), s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	out := ScheduleListOut{Items: make([]ScheduleOut, 0, len(list))}
	for _, sc := range list {
		out.Items = append(out.Items, scheduleOut(sc))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleGetSchedule is GET /v1alpha1/schedules/{id}.
func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) error {
	sc, err := s.Store.Schedule(r.Context(), s.NamespaceID, r.PathValue("id"))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, scheduleOut(sc))
	return nil
}

// handlePatchSchedule is PATCH /v1alpha1/schedules/{id} — the enable/disable
// control acceptance criterion 1 requires ("disabling the schedule stops it
// starting").
func (s *Server) handlePatchSchedule(w http.ResponseWriter, r *http.Request) error {
	var req patchScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching PatchScheduleRequest: {enabled}",
			"decode request body: %v", err)
	}
	if req.Enabled == nil {
		return badRequest("set enabled to true or false; it is the only patchable field",
			"no patchable field was supplied")
	}
	sc, err := s.Store.SetScheduleEnabled(r.Context(), s.NamespaceID, r.PathValue("id"), *req.Enabled)
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, scheduleOut(sc))
	return nil
}

// handleDeleteSchedule is DELETE /v1alpha1/schedules/{id}.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) error {
	if err := s.Store.DeleteSchedule(r.Context(), s.NamespaceID, r.PathValue("id")); err != nil {
		return classify(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// isDuplicateKey reports whether err is PostgreSQL's unique-violation, so a
// name collision reads as 409 rather than 500. It matches on SQLSTATE via the
// error text the pgx driver carries, which is what every other 23505 check in
// this package would need; there is only one such check today, and it lives
// here.
func isDuplicateKey(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
