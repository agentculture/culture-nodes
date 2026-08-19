package api

// Store pull's actor mapping (plan task t8, issue #192's portability half):
// the explicit step that makes a pulled catalog entry RUNNABLE on this
// plane. A pulled entry's graph pins actor://…@sha256 / runner:// ids
// minted on the source plane; the caller binds each declared capability
// requirement (the evidence manifest's required_capabilities, WP-C) to a
// LOCAL registration here. Three rules are load-bearing:
//
//   - The binding lives OUTSIDE the graph document. Publishing an entry
//     re-publishes its embedded source verbatim, so the workflow digest on
//     this plane equals the exported original byte for byte — "pull a flow
//     proven elsewhere and publish it without hand-editing digests". The
//     mapping is applied where refs resolve to registrations: at publish
//     resolution here, and at dispatch resolution in the worker registry's
//     binding fallback (internal/worker/registry.go).
//   - A binding is refused unless the local registration ADVERTISES the
//     required capabilities: a required capability name must appear as a
//     top-level key of the registration's `capabilities` document (the
//     document is deliberately open — preflight advertises under its own
//     key the same way). Missing ones are refused BY NAME.
//   - Bindings are records: who bound what to what, when — append-only,
//     readable back in full (superseded rows included).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// storeBindingCreateRequest is components.schemas.StoreBindingCreateRequest.
type storeBindingCreateRequest struct {
	Ref      string `json:"ref"`
	ActorKey string `json:"actor_key"`
	BoundBy  string `json:"bound_by"`
}

// storeBindingOut is components.schemas.StoreBinding.
type storeBindingOut struct {
	ID        string    `json:"id"`
	EntryID   string    `json:"entry_id"`
	Ref       string    `json:"ref"`
	Kind      string    `json:"kind"`
	ActorID   string    `json:"actor_id"`
	ActorKey  string    `json:"actor_key"`
	BoundBy   string    `json:"bound_by"`
	CreatedAt time.Time `json:"created_at"`
}

// storeBindingListOut is components.schemas.StoreBindingList.
type storeBindingListOut struct {
	Items []storeBindingOut `json:"items"`
}

func storeBindingOutFrom(b postgres.StoreEntryBinding) storeBindingOut {
	return storeBindingOut{
		ID:        b.ID,
		EntryID:   b.EntryID,
		Ref:       b.RequiredRef,
		Kind:      b.RequiredKind,
		ActorID:   b.BoundActorID,
		ActorKey:  b.BoundActorKey,
		BoundBy:   b.BoundBy,
		CreatedAt: b.CreatedAt,
	}
}

// handleCreateStoreBinding is POST /v1alpha1/store/entries/{id}/bindings:
// bind one of a pulled entry's declared capability requirements to a local
// registration. Local-origin entries are refused — their refs already
// resolve on this plane, so a binding could only shadow a real
// registration.
func (s *Server) handleCreateStoreBinding(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireStoreWriteAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	entry, err := s.engineStore.GetStoreEntry(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the id against GET /v1alpha1/store/entries", "no store entry with id %s", r.PathValue("id"))
		}
		return internalError(err)
	}

	var req storeBindingCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send a JSON body matching StoreBindingCreateRequest: {ref, actor_key, bound_by}", "decode request body: %v", err)
	}
	switch {
	case req.Ref == "":
		return badRequest("ref must name one of the entry's declared required_capabilities refs, verbatim", "ref is required")
	case req.ActorKey == "":
		return badRequest("actor_key must name a registration on this plane — check GET /v1alpha1/actors", "actor_key is required")
	case req.BoundBy == "":
		return badRequest("bound_by must identify who declared this mapping — a binding is a record", "bound_by is required")
	}

	if entry.Origin != "pulled" {
		return badRequest(
			"bindings map a pulled entry's requirements to local registrations; a local entry's refs already resolve on this plane",
			"store entry %s has origin %q, not %q", entry.ID, entry.Origin, "pulled")
	}

	requirement, ok := requirementByRef(entry.Evidence, req.Ref)
	if !ok {
		return badRequest(
			fmt.Sprintf("bind one of the refs the entry declares: %s", strings.Join(declaredRefs(entry.Evidence), ", ")),
			"store entry %s declares no capability requirement with ref %q", entry.ID, req.Ref)
	}

	actor, err := s.Store.CurrentActorByKey(ctx, s.NamespaceID, req.ActorKey)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return badRequest(
				"register the actor first (POST /v1alpha1/actors) — a binding may only point at a local registration",
				"no registered actor with key %q on this plane", req.ActorKey)
		}
		return internalError(err)
	}
	if requirement.Kind == "runner" && actor.Kind != "runner" {
		return badRequest(
			"bind a runner requirement to a registration of kind runner",
			"requirement %q needs a runner; %q is registered as kind %q", requirement.Ref, actor.ActorKey, actor.Kind)
	}
	if requirement.Kind == "actor" && actor.Kind == "runner" {
		return badRequest(
			"bind an actor requirement to an agent or human registration, not a runner",
			"requirement %q needs an actor; %q is registered as kind %q", requirement.Ref, actor.ActorKey, actor.Kind)
	}

	if missing := missingCapabilities(requirement.Capabilities, actor.Capabilities); len(missing) > 0 {
		return badRequest(
			fmt.Sprintf("register %q with the missing capabilities advertised as top-level keys of its capabilities document, or bind a registration that advertises them", actor.ActorKey),
			"registration %q does not advertise required capabilities: %s", actor.ActorKey, strings.Join(missing, ", "))
	}

	created, err := s.engineStore.CreateStoreEntryBinding(ctx, postgres.CreateStoreEntryBindingInput{
		EntryID:       entry.ID,
		RequiredRef:   requirement.Ref,
		RequiredKind:  requirement.Kind,
		BoundActorID:  actor.ID,
		BoundActorKey: actor.ActorKey,
		BoundBy:       req.BoundBy,
	})
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, storeBindingOutFrom(created))
	return nil
}

// handleListStoreBindings is GET /v1alpha1/store/entries/{id}/bindings: the
// full record trail, newest first — who bound what to what, when,
// superseded rows included. The current binding per ref is the newest.
func (s *Server) handleListStoreBindings(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	entry, err := s.engineStore.GetStoreEntry(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the id against GET /v1alpha1/store/entries", "no store entry with id %s", r.PathValue("id"))
		}
		return internalError(err)
	}
	records, err := s.engineStore.ListStoreEntryBindings(ctx, entry.ID)
	if err != nil {
		return internalError(err)
	}
	out := storeBindingListOut{Items: make([]storeBindingOut, 0, len(records))}
	for _, b := range records {
		out.Items = append(out.Items, storeBindingOutFrom(b))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handlePublishStoreEntry is POST /v1alpha1/store/entries/{id}/publish:
// publish the entry's embedded workflow source, verbatim, as a workflow
// version on this plane — refusing while any declared capability
// requirement is unbound, by name. A requirement is satisfied when a
// current binding maps its ref to a local registration, or when the ref's
// own key already resolves here (the local-origin case, and the
// same-fleet-pull case where both planes registered the same keys).
//
// The source is the exact bytes the exporting plane published, so the
// version this creates digests to entry.graph.digest by construction — no
// digest is hand-edited, and re-publishing is the same idempotent 200 the
// generic publish lane gives.
func (s *Server) handlePublishStoreEntry(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireStoreWriteAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	entry, err := s.engineStore.GetStoreEntry(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the id against GET /v1alpha1/store/entries", "no store entry with id %s", r.PathValue("id"))
		}
		return internalError(err)
	}

	unbound, err := s.unboundRequirements(r, entry)
	if err != nil {
		return internalError(err)
	}
	if len(unbound) > 0 {
		return conflict(
			"bind each listed ref to a local registration first: POST /v1alpha1/store/entries/{id}/bindings",
			"store entry %s has unbound capability requirements: %s", entry.ID, strings.Join(unbound, ", "))
	}

	compiled, _, err := compiler.Compile([]byte(entry.GraphSource), compiler.Format(entry.GraphSourceFormat))
	if err != nil {
		return internalError(fmt.Errorf("compiler: %w", err))
	}
	// Both cases below are corruption, not user error: the pull path sealed
	// this source against this digest before cataloging it.
	if compiled == nil {
		return internalError(fmt.Errorf("store entry %s: embedded workflow source no longer compiles", entry.ID))
	}
	if compiled.Digest != entry.GraphDigest {
		return internalError(fmt.Errorf("store entry %s: embedded source digests to %s, entry says %s", entry.ID, compiled.Digest, entry.GraphDigest))
	}

	if existing, err := s.workflowVersionByDigest(ctx, compiled.Digest); err == nil {
		writeJSON(w, http.StatusOK, workflowVersionOut(existing))
		return nil
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return internalError(err)
	}

	versionID, err := s.engineStore.EnsureWorkflowVersion(ctx, engine.WorkflowVersionInput{
		WorkflowKey:   compiled.Name,
		SourceFormat:  string(compiled.Format),
		Source:        string(compiled.Source),
		NormalizedIR:  compiled.Normalized,
		ContentDigest: compiled.Digest,
	})
	if err != nil {
		return internalError(fmt.Errorf("publish store entry: %w", err))
	}
	published, err := s.Store.GetWorkflowVersion(ctx, versionID)
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, workflowVersionOut(published))
	return nil
}

// unboundRequirements names every declared capability requirement that
// neither a current binding nor a direct local registration satisfies,
// sorted for a stable refusal message.
func (s *Server) unboundRequirements(r *http.Request, entry postgres.StoreEntry) ([]string, error) {
	ctx := r.Context()
	records, err := s.engineStore.ListStoreEntryBindings(ctx, entry.ID)
	if err != nil {
		return nil, err
	}
	current := postgres.CurrentBindings(records)

	unbound := make([]string, 0)
	for _, c := range entry.Evidence.RequiredCapabilities {
		if _, bound := current[c.Ref]; bound {
			continue
		}
		if _, err := s.Store.CurrentActorByKey(ctx, s.NamespaceID, refKeyOf(c.Ref)); err == nil {
			continue
		} else if !errors.Is(err, postgres.ErrNotFound) {
			return nil, err
		}
		unbound = append(unbound, c.Ref)
	}
	sort.Strings(unbound)
	return unbound, nil
}

// requirementByRef finds the declared requirement a binding names —
// verbatim ref match, because the manifest carries the graph's pinned
// identifier byte for byte and the resolver matches the same bytes.
func requirementByRef(m postgres.EvidenceManifest, ref string) (postgres.CapabilityRequirement, bool) {
	for _, c := range m.RequiredCapabilities {
		if c.Ref == ref {
			return c, true
		}
	}
	return postgres.CapabilityRequirement{}, false
}

func declaredRefs(m postgres.EvidenceManifest) []string {
	refs := make([]string, 0, len(m.RequiredCapabilities))
	for _, c := range m.RequiredCapabilities {
		refs = append(refs, c.Ref)
	}
	return refs
}

// missingCapabilities names the required capabilities the registration's
// `capabilities` document does not advertise. The document is deliberately
// open JSON, so "advertises c" is defined structurally: c is a top-level
// key whose value is neither null nor false. An empty or malformed
// document advertises nothing — "this registration told us nothing" and
// "this registration can do anything" are the same bytes, and treating
// them as the same capability is how a flow gets bound to a lane that
// cannot run it (internal/repair's LaneFromCapabilities reads absence the
// same way).
func missingCapabilities(required []string, capabilities json.RawMessage) []string {
	advertised := map[string]json.RawMessage{}
	if len(capabilities) > 0 {
		// A document that does not decode advertises nothing; the zero map
		// already says that.
		_ = json.Unmarshal(capabilities, &advertised)
	}
	missing := make([]string, 0)
	for _, name := range required {
		value, ok := advertised[name]
		if !ok || string(value) == "null" || string(value) == "false" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// refKeyOf extracts the registration key a component reference names —
// "actor://company/verifier@sha256:…" yields "company/verifier" — the same
// reading internal/worker's actorKeyOf applies at dispatch resolution.
func refKeyOf(ref string) string {
	trimmed := ref
	if _, rest, ok := strings.Cut(trimmed, "://"); ok {
		trimmed = rest
	}
	if key, _, ok := strings.Cut(trimmed, "@"); ok {
		trimmed = key
	}
	return strings.Trim(trimmed, "/")
}
