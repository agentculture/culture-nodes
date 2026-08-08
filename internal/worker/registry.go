package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Actor resolution: turning a node's declared component reference into an
// endpoint to POST to.
//
// A node says `uses: actor://company/verifier@sha256:…`. That reference names
// an identity and pins a revision; it does not say where the actor lives,
// what credential it takes, or what runs behind it — and it must not, because
// a definition that hard-coded a URL would stop being portable across
// namespaces and environments the moment it was published (§9.5: "an actor is
// a registered identity and endpoint").
//
// Resolution is therefore a lookup, and it is an interface so a deployment
// can supply a registry the control plane does not own.

// ErrUnknownActor reports a component reference with no registered actor.
var ErrUnknownActor = errors.New("worker: no registered actor for this reference")

// Registry resolves a node's component reference to an actor endpoint.
type Registry interface {
	// Resolve returns the endpoint for ref, or an error matching
	// ErrUnknownActor when there is none.
	Resolve(ctx context.Context, ref string) (actors.Endpoint, error)
}

// StaticRegistry resolves references from an in-memory map. It is what a
// single-node development deployment and every test in this package use.
type StaticRegistry map[string]actors.Endpoint

// Resolve implements Registry.
func (r StaticRegistry) Resolve(_ context.Context, ref string) (actors.Endpoint, error) {
	if endpoint, ok := r[ref]; ok {
		return endpoint, nil
	}
	// A registry keyed by the full pinned reference should still answer for a
	// caller that asks by identity alone, and vice versa: the digest pins the
	// revision, it does not change who the actor is.
	if key, _, ok := strings.Cut(ref, "@"); ok {
		if endpoint, found := r[key]; found {
			return endpoint, nil
		}
	}
	return actors.Endpoint{}, fmt.Errorf("worker: reference %q: %w", ref, ErrUnknownActor)
}

// DBRegistry resolves references against the `actors` table.
//
// The endpoint URL comes from actors.endpoint_ref. The credential does NOT:
// §16.3 keeps secrets out of the definition and out of the state store, so
// the row names an environment variable (metadata.auth_token_env) and this
// registry reads the value from the process environment. A row that names no
// variable resolves to an unauthenticated endpoint, which is legitimate for
// an in-cluster actor and a deployment decision either way.
type DBRegistry struct {
	store       *postgres.Store
	namespaceID string
	// lookupEnv is os.LookupEnv, replaceable in tests.
	lookupEnv func(string) (string, bool)
}

// NewDBRegistry returns a registry over the actors table for a namespace.
func NewDBRegistry(s *postgres.Store, namespaceID string) (*DBRegistry, error) {
	if s == nil {
		return nil, errors.New("worker: NewDBRegistry requires a store")
	}
	if namespaceID == "" {
		return nil, errors.New("worker: NewDBRegistry requires a namespace id")
	}
	return &DBRegistry{store: s, namespaceID: namespaceID, lookupEnv: os.LookupEnv}, nil
}

// Resolve implements Registry.
//
// The lookup is by actor key at the highest recorded revision. Actor rows are
// append-only — "a new capability or endpoint change is a new row (revision),
// never an update" (migration 0001) — so "the current endpoint" is by
// definition the newest revision, and reading anything else would resolve to
// a deliberately superseded address.
func (r *DBRegistry) Resolve(ctx context.Context, ref string) (actors.Endpoint, error) {
	key := actorKeyOf(ref)
	if key == "" {
		return actors.Endpoint{}, fmt.Errorf("worker: reference %q names no actor key: %w", ref, ErrUnknownActor)
	}

	var (
		endpointRef pgtype.Text
		metadata    []byte
	)
	err := r.store.Pool().QueryRow(ctx, `
		SELECT endpoint_ref, metadata
		FROM actors
		WHERE namespace_id = $1 AND actor_key = $2
		ORDER BY revision DESC
		LIMIT 1
	`, r.namespaceID, key).Scan(&endpointRef, &metadata)
	if err != nil {
		return actors.Endpoint{}, fmt.Errorf("worker: resolve %q: %w: %v", ref, ErrUnknownActor, err)
	}
	url := endpointRef.String
	if !endpointRef.Valid || url == "" {
		return actors.Endpoint{}, fmt.Errorf("worker: actor %q registers no endpoint_ref: %w", key, ErrUnknownActor)
	}

	endpoint := actors.Endpoint{URL: url}
	if envName := authTokenEnvOf(metadata); envName != "" {
		if token, ok := r.lookupEnv(envName); ok {
			endpoint.AuthToken = token
		} else {
			return actors.Endpoint{}, fmt.Errorf(
				"worker: actor %q requires credential from environment variable %s, which is not set", key, envName)
		}
	}
	return endpoint, nil
}

// actorKeyOf extracts the identity from a component reference:
// "actor://company/verifier@sha256:…" yields "company/verifier". A bare key
// is returned unchanged, so a registry can be queried by either form.
func actorKeyOf(ref string) string {
	trimmed := ref
	if _, rest, ok := strings.Cut(trimmed, "://"); ok {
		trimmed = rest
	}
	if key, _, ok := strings.Cut(trimmed, "@"); ok {
		trimmed = key
	}
	return strings.Trim(trimmed, "/")
}

// authTokenEnvOf reads metadata.auth_token_env without decoding the whole
// document into a typed struct — the metadata column is deliberately open,
// and a typed decode would fail on a key this code does not care about.
func authTokenEnvOf(metadata []byte) string {
	if len(metadata) == 0 {
		return ""
	}
	var fields struct {
		AuthTokenEnv string `json:"auth_token_env"`
	}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return ""
	}
	return fields.AuthTokenEnv
}
