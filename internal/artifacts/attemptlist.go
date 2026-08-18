package artifacts

import (
	"context"
	"fmt"
)

// Listed is one artifact as returned by an attempt listing: the ref that
// resolves it plus the metadata recorded at Put time. It exists so a listing
// can be rendered (and its content fetched via Store.Get) without a second
// metadata round trip per artifact.
type Listed struct {
	Ref  Ref
	Meta ArtifactMeta
}

// AttemptLister is the optional read surface for "what artifacts does this
// attempt have?" (issue #189's read-back half). It is deliberately not part
// of Store: Put/Get/Stat operate on one artifact by ref, while listing is a
// metadata query -- only drivers that own the authoritative metadata table
// implement it.
type AttemptLister interface {
	ListByAttempt(ctx context.Context, attemptID string) ([]Listed, error)
}

// ListByAttempt lists the artifacts recorded for one attempt. Router
// delegates to its small driver: small and object share one authoritative
// metadata table (see Router's doc comment), so asking small alone sees
// every artifact regardless of which backend holds its bytes.
func (r *Router) ListByAttempt(ctx context.Context, attemptID string) ([]Listed, error) {
	lister, ok := r.small.(AttemptLister)
	if !ok {
		return nil, fmt.Errorf("artifacts: Router: ListByAttempt: the small driver (%T) does not implement AttemptLister", r.small)
	}
	return lister.ListByAttempt(ctx, attemptID)
}
