package artifacts

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// Router is a composite Store that fans a Put out to whichever backing
// Store fits the payload's size, and fans a Get/Stat/Delete in by asking
// which backend the ref's metadata row says holds it.
//
// Router assumes small and object share one authoritative metadata table --
// true of internal/artifacts/postgres and internal/artifacts/s3, which both
// write and read the very same PostgreSQL artifacts table
// (migrations/0004_observability.sql) regardless of which one holds a given
// artifact's bytes. That is what lets Router.Get resolve a ref's backend by
// calling small.Stat even when the ref is actually held by object: Stat is
// a pure metadata read, not a claim that small holds the content (see
// Store's doc comment).
type Router struct {
	small     Store
	object    Store
	threshold int64
}

var _ Store = (*Router)(nil)

// NewRouter returns a Router that sends a Put's bytes to small when they
// total threshold bytes or fewer, and to object otherwise. threshold is a
// routing policy, not the only place an oversize payload is caught: small
// (e.g. internal/artifacts/postgres's Driver) still enforces its own
// configured cap independently, so passing a threshold larger than small's
// cap does not let an oversize Put slip through -- it just means small
// itself rejects it with ErrTooLarge instead of Router ever routing to
// object.
func NewRouter(small, object Store, threshold int64) *Router {
	return &Router{small: small, object: object, threshold: threshold}
}

// Put buffers the first threshold+1 bytes of in to decide -- without
// loading the whole payload into memory -- whether it fits at or under
// threshold: reading exactly threshold+1 bytes proves the payload is
// larger, so Put routes to object, replaying the buffered prefix ahead of
// the rest of in; reading fewer (an EOF before the buffer fills) proves the
// payload is threshold bytes or smaller and that it is already fully
// buffered, so Put routes the buffer straight to small with no replay
// needed.
func (r *Router) Put(ctx context.Context, meta ArtifactMeta, in io.Reader) (Ref, error) {
	if r.threshold < 0 {
		return "", fmt.Errorf("artifacts: Router: Put: negative threshold %d", r.threshold)
	}

	peek := make([]byte, r.threshold+1)
	n, err := io.ReadFull(in, peek)
	switch {
	case err == nil:
		// The whole peek buffer filled: in has at least threshold+1
		// bytes, i.e. strictly more than threshold.
		return r.object.Put(ctx, meta, io.MultiReader(bytes.NewReader(peek), in))
	case err == io.EOF || err == io.ErrUnexpectedEOF: //nolint:errorlint // io.ReadFull contract: exact sentinels
		// in was exhausted before the buffer filled: everything it had is
		// already sitting in peek[:n].
		return r.small.Put(ctx, meta, bytes.NewReader(peek[:n]))
	default:
		return "", fmt.Errorf("artifacts: Router: Put: read: %w", err)
	}
}

// Get resolves ref's backend via backendFor and delegates to it.
func (r *Router) Get(ctx context.Context, ref Ref) (io.ReadCloser, ArtifactMeta, error) {
	store, meta, err := r.backendFor(ctx, ref)
	if err != nil {
		return nil, ArtifactMeta{}, err
	}
	rc, _, err := store.Get(ctx, ref)
	if err != nil {
		return nil, ArtifactMeta{}, err
	}
	return rc, meta, nil
}

// Stat reads ref's metadata directly from small -- a backend-agnostic
// lookup, per the Router doc comment above -- without needing to know
// whether small or object actually holds the bytes.
func (r *Router) Stat(ctx context.Context, ref Ref) (ArtifactMeta, error) {
	return r.small.Stat(ctx, ref)
}

// Delete resolves ref's backend via backendFor and delegates to it.
func (r *Router) Delete(ctx context.Context, ref Ref) error {
	store, _, err := r.backendFor(ctx, ref)
	if err != nil {
		return err
	}
	return store.Delete(ctx, ref)
}

// backendFor looks up ref's metadata (a backend-agnostic read, see Stat)
// and returns whichever of small/object its recorded Backend names.
func (r *Router) backendFor(ctx context.Context, ref Ref) (Store, ArtifactMeta, error) {
	meta, err := r.small.Stat(ctx, ref)
	if err != nil {
		return nil, ArtifactMeta{}, err
	}
	switch meta.Backend {
	case BackendPostgres:
		return r.small, meta, nil
	case BackendS3:
		return r.object, meta, nil
	default:
		return nil, ArtifactMeta{}, fmt.Errorf("artifacts: Router: %s: unrecognized backend %q", ref, meta.Backend)
	}
}
