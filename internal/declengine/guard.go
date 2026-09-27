// Task t10 (#328): the c46/c93 loop backstops -- hop limit, no-self-retrigger
// and the rate ceiling -- and the c84/h57 per-subject concurrency ceiling.
// Both live on top of engine.go's evaluate() pipeline: the re-entry count
// stayed inline there (it is one loop over the lineage already in hand and
// needs no backend read); everything here either reads only that same
// resolved lineage (hop limit, self-retrigger) or needs exactly one backend
// call (rate ceiling, subject concurrency). Every check here stops the
// chain with a visible record -- h32/h57 both say nothing is silently
// dropped -- and evaluate() is what turns "not ok" into that record; this
// file only decides.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/store"
)

// checkHopLimit is c93's second backstop: total lineage depth, regardless
// of which declarations appear in it (the re-entry limit above only counts
// occurrences of THIS declaration). HopLimit <= 0 cannot occur through decl
// parsing (schema minimum 1, default 20) but is treated as "no limit"
// rather than panicking on a hand-built ActiveDeclaration in a test.
func checkHopLimit(a ActiveDeclaration, lineage []Ancestor) (ok bool, reason string) {
	limit := a.Declaration.Trigger.HopLimit
	if limit <= 0 {
		return true, ""
	}
	if len(lineage) >= limit {
		return false, fmt.Sprintf("causal lineage is already %d hops deep; hop limit is %d", len(lineage), limit)
	}
	return true, ""
}

// checkSelfRetrigger is c93's third backstop: a declaration may not appear
// as its own immediate causal parent unless it opts in. This is distinct
// from the re-entry limit (which counts every appearance anywhere in the
// lineage, however far back) -- a declaration with reentry_limit 3 still
// cannot retrigger itself back-to-back on its very first re-entry unless
// allow_self_retrigger is set, because ancestors[0] is nearest-first: the
// immediate parent, not just any prior appearance.
func checkSelfRetrigger(a ActiveDeclaration, lineage []Ancestor) (ok bool, reason string) {
	if a.Declaration.Trigger.AllowSelfRetrigger || len(lineage) == 0 {
		return true, ""
	}
	if lineage[0].DeclarationID == a.ID {
		return false, fmt.Sprintf("declaration %s directly retriggered itself; self-retrigger is not opted in (allow_self_retrigger)", a.ID)
	}
	return true, ""
}

// rateCeilingLimit parses the schema-validated "N/h" form. A declaration
// that reached the engine with something else (hand-built, as tests do) is
// an evaluation error, not a silent pass.
func rateCeilingLimit(ceiling string) (int, error) {
	n, ok := strings.CutSuffix(ceiling, "/h")
	if !ok || n == "" {
		return 0, fmt.Errorf("declengine: invalid rate ceiling %q", ceiling)
	}
	limit, err := strconv.Atoi(n)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("declengine: invalid rate ceiling %q", ceiling)
	}
	return limit, nil
}

// checkRateCeiling is c93's last backstop: "N firings per declaration per
// hour", the same ceiling regardless of which subject or lineage a firing
// belongs to -- it is what catches a loop that keeps changing subject (or
// has none) to slip past every per-lineage guard above.
func (e *Engine) checkRateCeiling(ctx context.Context, namespaceID string, a ActiveDeclaration) (ok bool, reason string, err error) {
	limit, err := rateCeilingLimit(a.Declaration.Trigger.RateCeiling)
	if err != nil {
		return false, "", err
	}
	count, err := e.backend.RecentFirings(ctx, namespaceID, a.ID, time.Now().Add(-time.Hour))
	if err != nil {
		return false, "", err
	}
	if count >= limit {
		return false, fmt.Sprintf("declaration already fired %d times in the last hour; rate ceiling is %s", count, a.Declaration.Trigger.RateCeiling), nil
	}
	return true, "", nil
}

// DeferSubjectInput is what checkSubjectConcurrency asks a Backend to
// remember when a declaration's max_concurrent_subject cap left no room:
// the whole event, so DrainSubject can replay it through the ordinary
// Handle path later.
type DeferSubjectInput struct {
	NamespaceID, DeclarationID, Subject string
	Event                               Event
}

// DeferredSubject is one queued entry OldestDeferredSubject returns.
type DeferredSubject struct {
	ID    string
	Event Event
}

// checkSubjectConcurrency is c84/h57: a declaration whose trigger declares
// max_concurrent_subject caps how many of ITS firings may be in flight --
// landing node still open -- for one subject at once. A declaration that
// never sets it, or an event that carries no subject, is unaffected: this
// is a no-op in either case, so every declaration written before t10 keeps
// its exact pre-t10 behavior.
func (e *Engine) checkSubjectConcurrency(ctx context.Context, event Event, a ActiveDeclaration) (deferred bool, err error) {
	ceiling := a.Declaration.Trigger.MaxConcurrentSubject
	if ceiling <= 0 || event.Subject == "" {
		return false, nil
	}
	count, err := e.backend.SubjectInFlight(ctx, event.NamespaceID, a.ID, event.Subject)
	if err != nil {
		return false, err
	}
	if count < ceiling {
		return false, nil
	}
	if err := e.backend.DeferSubject(ctx, DeferSubjectInput{NamespaceID: event.NamespaceID, DeclarationID: a.ID, Subject: event.Subject, Event: event}); err != nil {
		return false, err
	}
	return true, nil
}

// DrainSubject replays the oldest queued subject entry for one declaration
// as an ordinary Handle call over its stored event: whatever is currently
// active and whatever the event's marker now resolves decide its fate,
// exactly as a fresh delivery would -- nothing about a replay is
// privileged. It drains at most one entry, because the caller is expected
// to call this once per slot that actually freed (e.g. a node closing,
// task t12); draining more here would let the in-flight count run past the
// cap the same way skipping the check on entry would. A namespace or
// declaration with nothing queued is a no-op, not an error.
func (e *Engine) DrainSubject(ctx context.Context, namespaceID, declarationID string) error {
	deferred, found, err := e.backend.OldestDeferredSubject(ctx, namespaceID, declarationID)
	if err != nil || !found {
		return err
	}
	if err := e.backend.DeleteDeferredSubject(ctx, namespaceID, deferred.ID); err != nil {
		return err
	}
	return e.Handle(ctx, deferred.Event)
}

// RecentFirings is PostgresBackend's rate-ceiling read: distinct logical
// (non-remint) firings of one declaration created at or after since.
func (p PostgresBackend) RecentFirings(ctx context.Context, namespaceID, declarationID string, since time.Time) (int, error) {
	var count int
	err := p.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings
 WHERE namespace_id=$1 AND declaration_id=$2 AND remint_of_id IS NULL AND created_at>=$3`,
		namespaceID, declarationID, since).Scan(&count)
	return count, err
}

// SubjectInFlight is PostgresBackend's per-subject concurrency read: the
// declaration's canonical firings for one subject whose landing node
// (declaration_nodes, opened by Finish) is still open.
func (p PostgresBackend) SubjectInFlight(ctx context.Context, namespaceID, declarationID, subject string) (int, error) {
	var count int
	err := p.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_nodes n
 JOIN declaration_firings f ON f.namespace_id=n.namespace_id AND f.id=n.opening_firing_id
 WHERE n.namespace_id=$1 AND n.state='open' AND f.declaration_id=$2 AND f.subject=$3`,
		namespaceID, declarationID, subject).Scan(&count)
	return count, err
}

// DeferSubject upserts the one remembered entry for (namespace,
// declaration, subject): a second event for a subject already queued
// replaces it (attempts increments, created_at does not move) rather than
// adding a sibling row -- the same replace rule 0039's TouchDeferredTrigger
// uses for deferred_triggers.
func (p PostgresBackend) DeferSubject(ctx context.Context, in DeferSubjectInput) error {
	event, err := json.Marshal(in.Event)
	if err != nil {
		return err
	}
	_, err = p.Store.Pool().Exec(ctx, `INSERT INTO declaration_subject_deferrals(id,namespace_id,declaration_id,subject,event)
 VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (namespace_id,declaration_id,subject) DO UPDATE SET
 event=EXCLUDED.event, attempts=declaration_subject_deferrals.attempts+1, updated_at=now()`,
		store.NewULID(), in.NamespaceID, in.DeclarationID, in.Subject, event)
	return err
}

// OldestDeferredSubject is DrainSubject's read: the longest-queued entry
// for a declaration, across every subject -- "arrival order" (h57).
func (p PostgresBackend) OldestDeferredSubject(ctx context.Context, namespaceID, declarationID string) (DeferredSubject, bool, error) {
	var d DeferredSubject
	var event []byte
	err := p.Store.Pool().QueryRow(ctx, `SELECT id,event FROM declaration_subject_deferrals
 WHERE namespace_id=$1 AND declaration_id=$2 ORDER BY created_at,id LIMIT 1`,
		namespaceID, declarationID).Scan(&d.ID, &event)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeferredSubject{}, false, nil
		}
		return DeferredSubject{}, false, err
	}
	if err := json.Unmarshal(event, &d.Event); err != nil {
		return DeferredSubject{}, false, err
	}
	return d, true, nil
}

// DeleteDeferredSubject removes a drained entry.
func (p PostgresBackend) DeleteDeferredSubject(ctx context.Context, namespaceID, id string) error {
	_, err := p.Store.Pool().Exec(ctx, `DELETE FROM declaration_subject_deferrals WHERE namespace_id=$1 AND id=$2`, namespaceID, id)
	return err
}
