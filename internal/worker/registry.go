package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
		// The store-pull mapping step (task t8, issue #192): a pulled flow's
		// graph pins refs minted on the SOURCE plane, and the binding that
		// makes it runnable here lives outside the graph document — so this
		// is exactly where it applies. When no local registration answers
		// for the ref's own key, the current binding for the VERBATIM ref
		// supplies the local key to resolve instead — and only when every
		// entry that binds the ref agrees on it: entries disagreeing is a
		// named refusal, never a newest-entry-wins pick that would dispatch
		// this flow on another flow's mapping (PR #208 finding 4). A binding
		// never overrides a direct registration, and an unbound foreign ref
		// stays ErrUnknownActor.
		boundKey, bindErr := r.store.ResolveStoreBoundActorKey(ctx, r.namespaceID, ref)
		if bindErr != nil {
			if errors.Is(bindErr, postgres.ErrStoreBindingConflict) {
				return actors.Endpoint{}, fmt.Errorf("worker: resolve %q: %w: %v", ref, ErrUnknownActor, bindErr)
			}
			return actors.Endpoint{}, fmt.Errorf("worker: resolve %q: %w: %v", ref, ErrUnknownActor, err)
		}
		key = boundKey
		err = r.store.Pool().QueryRow(ctx, `
			SELECT endpoint_ref, metadata
			FROM actors
			WHERE namespace_id = $1 AND actor_key = $2
			ORDER BY revision DESC
			LIMIT 1
		`, r.namespaceID, key).Scan(&endpointRef, &metadata)
		if err != nil {
			return actors.Endpoint{}, fmt.Errorf("worker: resolve %q via binding to %q: %w: %v", ref, key, ErrUnknownActor, err)
		}
	}
	url := endpointRef.String
	endpoint := actors.Endpoint{URL: url}
	// The freshness window is actors.DialInPresenceCutoff, not a local
	// literal: the read-only presence view (GET /v1alpha1/dial-in-presence,
	// task t6) answers "is this bridge dialled in right now" and must give
	// the same answer this line gives, or an operator would read "connected"
	// for an actor dispatch had already decided was absent.
	cutoff := actors.DialInPresenceCutoff(time.Now().UTC())
	if available, availErr := r.store.InboundActorAvailable(ctx, r.namespaceID, key, cutoff); availErr == nil && available {
		endpoint.DialIn = r.store
		endpoint.DialInNamespace = r.namespaceID
		endpoint.DialInActorKey = key
	}
	if endpoint.DialIn == nil && (!endpointRef.Valid || url == "") {
		return actors.Endpoint{}, fmt.Errorf("worker: actor %q registers no endpoint_ref and has no current dial-in: %w", key, ErrUnknownActor)
	}
	// The repository this deployment registered the actor to work in (issue
	// #125), read from the same document and the same revision the
	// credential is. It is optional by construction: an actor that declares
	// none dispatches exactly as it did before this existed.
	endpoint.RepositoryIdentity = repositoryIdentityOf(metadata)
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

// repositoryIdentityOf reads metadata.repository_identity the same partial
// way authTokenEnvOf reads its own key, and for the same reason: the
// metadata column is deliberately open, so a typed decode of the whole
// document would fail on a key this code does not care about.
//
// The key is a per-actor deployment fact, exactly like `handover_remote`
// (scripts/collect-handover.py) — the shipped precedent for a fact that
// belongs to neither the graph nor the event that started the run. It is
// registry-only on purpose: nothing here consults a run input, an event
// payload, or anything an agent reported, because a repository the agent
// could name is a repository the agent chose.
func repositoryIdentityOf(metadata []byte) string {
	if len(metadata) == 0 {
		return ""
	}
	var fields struct {
		RepositoryIdentity string `json:"repository_identity"`
	}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return ""
	}
	return fields.RepositoryIdentity
}

// ActorRowID resolves a reference to the actors-table row id of its current
// (highest) revision — the identity attempts.actor_id records so per-actor
// surfaces (GET /v1alpha1/actors/{id}/stats, the jobs view) can attribute
// work. Separate from Resolve on purpose: Resolve answers "where do I send
// the invocation", this answers "who, durably, was invoked" — and callers
// treat a miss as unattributed, never as a dispatch failure.
func (r *DBRegistry) ActorRowID(ctx context.Context, ref string) (string, error) {
	key := actorKeyOf(ref)
	if key == "" {
		return "", fmt.Errorf("worker: reference %q names no actor key: %w", ref, ErrUnknownActor)
	}
	var id string
	err := r.store.Pool().QueryRow(ctx, `
		SELECT id
		FROM actors
		WHERE namespace_id = $1 AND actor_key = $2
		ORDER BY revision DESC
		LIMIT 1
	`, r.namespaceID, key).Scan(&id)
	if err != nil {
		// The same store-binding fallback Resolve applies (task t8): a
		// dispatch that resolved through a binding must attribute to the
		// registration the binding named, not go unattributed — and the
		// same agreement rule holds, so attribution can never name a
		// different actor than Resolve dispatched to.
		boundKey, bindErr := r.store.ResolveStoreBoundActorKey(ctx, r.namespaceID, ref)
		if bindErr != nil {
			if errors.Is(bindErr, postgres.ErrStoreBindingConflict) {
				return "", fmt.Errorf("worker: actor row for %q: %w: %v", ref, ErrUnknownActor, bindErr)
			}
			return "", fmt.Errorf("worker: actor row for %q: %w: %v", ref, ErrUnknownActor, err)
		}
		err = r.store.Pool().QueryRow(ctx, `
			SELECT id
			FROM actors
			WHERE namespace_id = $1 AND actor_key = $2
			ORDER BY revision DESC
			LIMIT 1
		`, r.namespaceID, boundKey).Scan(&id)
		if err != nil {
			return "", fmt.Errorf("worker: actor row for %q via binding to %q: %w: %v", ref, boundKey, ErrUnknownActor, err)
		}
	}
	return id, nil
}

// PreflightConfig returns an actor's registered `capabilities` and
// `metadata` documents verbatim — the two halves the clarify-then-commit
// gate reads its per-actor configuration from (task t14, issue #67):
// `capabilities.preflight` is the surface the bridge advertises,
// `metadata.preflight_gate` is whether the deployment turned the gate on.
//
// It returns the raw documents rather than a parsed configuration because
// parsing belongs to internal/preflight: a registry that also parsed would
// be a second place for the same rules to live, and the two would drift.
// Like Resolve it reads the highest revision — the current registration is
// the one whose facts are current.
//
// A reference with no registered actor yields two empty documents and no
// error: an unresolvable reference is a dispatch failure Resolve reports a
// moment later with a much better diagnostic, and having the gate fail first
// with "no configuration" would replace that message with a worse one.
func (r *DBRegistry) PreflightConfig(ctx context.Context, ref string) (json.RawMessage, json.RawMessage, error) {
	key := actorKeyOf(ref)
	if key == "" {
		return nil, nil, nil
	}
	var capabilities, metadata []byte
	err := r.store.Pool().QueryRow(ctx, `
		SELECT capabilities, metadata
		FROM actors
		WHERE namespace_id = $1 AND actor_key = $2
		ORDER BY revision DESC
		LIMIT 1
	`, r.namespaceID, key).Scan(&capabilities, &metadata)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("worker: preflight configuration for %q: %w", ref, err)
	}
	return capabilities, metadata, nil
}

// actorRowIDResolver is the optional registry capability the worker uses
// for durable attribution — DBRegistry implements it; a registry that does
// not (StaticRegistry in tests) simply leaves attempts unattributed.
type actorRowIDResolver interface {
	ActorRowID(ctx context.Context, ref string) (string, error)
}
