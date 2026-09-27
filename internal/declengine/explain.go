// explain.go answers spec c88 / honesty h61: "why did (or didn't) declaration
// X fire for event E" from a store/engine query, independent of the API
// route (internal/api/declexplain.go) built on top of it. engine.go's
// evaluate() already appends one declaration_evaluations row per pipeline
// step for every declaration whose trigger matched; Explain reads the
// newest (terminal) row for one (event, declaration) pair by name, which is
// exactly the outcome + reason c88 asks every trigger match to carry.
//
// A declaration whose trigger never matched event E in the first place
// leaves no declaration_evaluations row at all (engine.go's Handle simply
// `continue`s past a non-match) -- Explain reports "not found" for that
// case, the same as for an unknown event or declaration name, because there
// is no match to explain.
package declengine

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExplainResult is the terminal declaration_evaluations row for one
// (event, declaration) pair: the last outcome the firing loop recorded, and
// why. FiringID is set once a firing was claimed -- for OutcomeFired and
// OutcomeShadow always, and for any later terminal outcome that reused an
// already-claimed firing (OutcomeDuplicate, OutcomeDispatchFailed) -- and
// empty for every outcome decided before Claim ran (OutcomeConditionFalse,
// OutcomeLineageMissing, OutcomeLoopLimited, OutcomeDeferred, and the two
// outcomes this task defines but does not yet produce,
// OutcomeBudgetBlocked and OutcomeOverlapSuppressed).
type ExplainResult struct {
	NamespaceID, EventID           string
	DeclarationID, DeclarationName string
	DeclarationVersion             string
	Outcome, Reason                string
	FiringID                       string
	CreatedAt                      time.Time
}

// Explain resolves declarationName to its stable declaration id (the same
// identity every version of a declaration shares -- see declstore.go's
// PublishDeclaration) and returns the newest declaration_evaluations row
// recorded for it against eventID: the terminal step of that one
// declaration's evaluation of that one event. found is false when no such
// row exists, whether because the declaration name is unknown in this
// namespace or its trigger never matched the event.
//
// "Newest" is ORDER BY created_at DESC, id DESC -- the same tie-break
// PostgresBackend.Lineage already uses for its own recency ordering.
// declaration_evaluations rows are appended by separate, unbatched Record
// calls (each its own statement, so each gets its own now()) except for the
// single terminal row Finish appends inside a transaction, so ties are rare
// in practice; the ULID id remains a monotonic secondary order regardless.
func (p PostgresBackend) Explain(ctx context.Context, namespaceID, eventID, declarationName string) (ExplainResult, bool, error) {
	if namespaceID == "" || eventID == "" || declarationName == "" {
		return ExplainResult{}, false, errors.New("declengine: namespace id, event id and declaration name are required")
	}
	var r ExplainResult
	var firingID *string
	err := p.Store.Pool().QueryRow(ctx, `SELECT e.declaration_id,d.name,e.declaration_version,e.outcome,e.reason,e.firing_id,e.created_at
 FROM declaration_evaluations e JOIN declarations d ON d.id=e.declaration_id AND d.namespace_id=e.namespace_id
 WHERE e.namespace_id=$1 AND e.event_id=$2 AND d.name=$3
 ORDER BY e.created_at DESC, e.id DESC LIMIT 1`,
		namespaceID, eventID, declarationName).
		Scan(&r.DeclarationID, &r.DeclarationName, &r.DeclarationVersion, &r.Outcome, &r.Reason, &firingID, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExplainResult{}, false, nil
		}
		return ExplainResult{}, false, err
	}
	r.NamespaceID, r.EventID = namespaceID, eventID
	if firingID != nil {
		r.FiringID = *firingID
	}
	return r, true, nil
}
