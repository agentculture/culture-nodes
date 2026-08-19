package api

// The flow-store registry surface (plan task t7, issue #192): a catalog
// entry is a graph PLUS its evidence — the workflow's content digest with
// the source embedded verbatim, the proving prod run ids, the deviation
// records recorded against it, and the actor/runner capability
// requirements the graph pins. The registry is an internal, private
// surface: it serves everyone on the mesh read-side, and its two write
// routes are bearer-gated (the t15 rule: every mutating surface ships
// authenticated). The registry address a client pulls from is
// configuration on the client and data on the row (source_registry) —
// never a hardcoded peer list (SCRUM-3 q1).
//
// Capability requirements live in the evidence manifest, never as graph
// rewrites: the later import step (WP-F, t8) binds them to local
// registrations without touching the graph document, which is what keeps
// "pull a flow proven elsewhere and publish it without hand-editing
// digests" true — a rewritten graph would digest differently.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// storeCapabilityOut is components.schemas.StoreCapabilityRequirement.
type storeCapabilityOut struct {
	Kind         string   `json:"kind"`
	Ref          string   `json:"ref"`
	Capabilities []string `json:"capabilities"`
}

// storeDeviationOut is components.schemas.StoreDeviationRecordRef.
type storeDeviationOut struct {
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

// storeEvidenceOut is components.schemas.StoreEvidenceManifest.
type storeEvidenceOut struct {
	ProvingRunIDs        []string             `json:"proving_run_ids"`
	DeviationRecords     []storeDeviationOut  `json:"deviation_records"`
	RequiredCapabilities []storeCapabilityOut `json:"required_capabilities"`
}

// storeEntryGraphOut is components.schemas.StoreEntryGraph. Source is
// omitted on the listing surface (the summary carries the digest, which IS
// the graph's identity) and always present on the single-entry document,
// which must be self-contained enough to republish elsewhere.
type storeEntryGraphOut struct {
	Digest       string `json:"digest"`
	SourceFormat string `json:"source_format"`
	Source       string `json:"source,omitempty"`
}

// storeEntryOut is components.schemas.StoreEntry.
type storeEntryOut struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Origin         string             `json:"origin"`
	SourceRegistry string             `json:"source_registry,omitempty"`
	Graph          storeEntryGraphOut `json:"graph"`
	Evidence       storeEvidenceOut   `json:"evidence"`
	EntryDigest    string             `json:"entry_digest"`
	CreatedAt      time.Time          `json:"created_at"`
}

// storeEntryListOut is components.schemas.StoreEntryList.
type storeEntryListOut struct {
	Items []storeEntryOut `json:"items"`
}

func storeEvidenceOutFrom(m postgres.EvidenceManifest) storeEvidenceOut {
	out := storeEvidenceOut{
		ProvingRunIDs:        nonNilJSONStrings(m.ProvingRunIDs),
		DeviationRecords:     make([]storeDeviationOut, 0, len(m.DeviationRecords)),
		RequiredCapabilities: make([]storeCapabilityOut, 0, len(m.RequiredCapabilities)),
	}
	for _, d := range m.DeviationRecords {
		out.DeviationRecords = append(out.DeviationRecords, storeDeviationOut{Ref: d.Ref, Note: d.Note})
	}
	for _, c := range m.RequiredCapabilities {
		out.RequiredCapabilities = append(out.RequiredCapabilities, storeCapabilityOut{
			Kind: c.Kind, Ref: c.Ref, Capabilities: nonNilJSONStrings(c.Capabilities)})
	}
	return out
}

func storeEntryOutFrom(e postgres.StoreEntry, withSource bool) storeEntryOut {
	out := storeEntryOut{
		ID:             e.ID,
		Name:           e.Name,
		Origin:         e.Origin,
		SourceRegistry: e.SourceRegistry,
		Graph:          storeEntryGraphOut{Digest: e.GraphDigest, SourceFormat: e.GraphSourceFormat},
		Evidence:       storeEvidenceOutFrom(e.Evidence),
		EntryDigest:    e.EntryDigest,
		CreatedAt:      e.CreatedAt,
	}
	if withSource {
		out.Graph.Source = e.GraphSource
	}
	return out
}

// storeEntryDigestDoc is what an entry's own content digest is computed
// over — the portable identity of the entry: its name, its graph
// (digest + verbatim source), and its evidence manifest. Deliberately NOT
// the row identity (id, origin, source_registry, created_at), which is
// per-plane: the same flow pulled onto another plane keeps the same entry
// digest, which is both what makes re-pulls idempotent and what lets a
// pulling plane verify integrity end-to-end (handleStoreEntryPull).
type storeEntryDigestDoc struct {
	Name     string             `json:"name"`
	Graph    storeEntryGraphOut `json:"graph"`
	Evidence storeEvidenceOut   `json:"evidence"`
}

func storeEntryDigest(name string, graph storeEntryGraphOut, evidence storeEvidenceOut) (string, error) {
	return contracts.DigestValue(storeEntryDigestDoc{Name: name, Graph: graph, Evidence: evidence})
}

// storeEvidenceIn mirrors storeEvidenceOut on the request side.
type storeEvidenceIn = storeEvidenceOut

// validateStoreEvidence checks the manifest's typed half — capability
// requirements — and that the entry declares at least one proving run: the
// store is a catalog of PROVEN flows, and an entry with no proving run is
// a claim with no evidence.
func validateStoreEvidence(evidence storeEvidenceIn) *apiError {
	if len(evidence.ProvingRunIDs) == 0 {
		return badRequest(
			"list at least one proving run id in evidence.proving_run_ids — the store catalogs proven flows only",
			"evidence.proving_run_ids is empty")
	}
	for i, id := range evidence.ProvingRunIDs {
		if id == "" {
			return badRequest("remove the empty entry from evidence.proving_run_ids", "proving_run_ids[%d] is empty", i)
		}
	}
	for i, d := range evidence.DeviationRecords {
		if d.Ref == "" {
			return badRequest("every deviation record needs a ref (a tree path or URL)", "deviation_records[%d].ref is empty", i)
		}
	}
	for i, c := range evidence.RequiredCapabilities {
		if c.Kind != "actor" && c.Kind != "runner" {
			return badRequest(
				`required_capabilities[].kind must be "actor" or "runner"`,
				"required_capabilities[%d].kind is %q", i, c.Kind)
		}
		if c.Ref == "" {
			return badRequest(
				"every capability requirement needs the graph-pinned ref it stands in for (actor://…, runner://…)",
				"required_capabilities[%d].ref is empty", i)
		}
	}
	return nil
}

func evidenceManifestFrom(evidence storeEvidenceIn) postgres.EvidenceManifest {
	m := postgres.EvidenceManifest{
		ProvingRunIDs:        evidence.ProvingRunIDs,
		DeviationRecords:     make([]postgres.DeviationRecordRef, 0, len(evidence.DeviationRecords)),
		RequiredCapabilities: make([]postgres.CapabilityRequirement, 0, len(evidence.RequiredCapabilities)),
	}
	for _, d := range evidence.DeviationRecords {
		m.DeviationRecords = append(m.DeviationRecords, postgres.DeviationRecordRef{Ref: d.Ref, Note: d.Note})
	}
	for _, c := range evidence.RequiredCapabilities {
		m.RequiredCapabilities = append(m.RequiredCapabilities, postgres.CapabilityRequirement{
			Kind: c.Kind, Ref: c.Ref, Capabilities: c.Capabilities})
	}
	return m
}

// requireStoreWriteAuth is requireAdhocRunAuth's pattern applied to the
// store's two write routes (create + pull). Reads stay authless: the
// registry is an internal, mesh-private surface and "everyone on the mesh
// reads" is the q6 decision; writing the catalog is a distinct standing.
func (s *Server) requireStoreWriteAuth(r *http.Request) error {
	if len(s.storeWriteSecret) == 0 {
		return unauthorized(
			"configure the server with a store write secret (NODES_STORE_TOKEN_SECRET) to enable store writes",
			"store writes require a configured bearer secret and none is configured")
	}

	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}

	presented := sha256.Sum256([]byte(header[len(prefix):]))
	expected := sha256.Sum256(s.storeWriteSecret)
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
	return nil
}

// storeEntryCreateRequest is components.schemas.StoreEntryCreateRequest.
type storeEntryCreateRequest struct {
	Name        string          `json:"name"`
	GraphDigest string          `json:"graph_digest"`
	Evidence    storeEvidenceIn `json:"evidence"`
}

// handleCreateStoreEntry is POST /v1alpha1/store/entries: catalog a flow
// this control plane has published and proven. The graph digest must
// resolve to a published workflow version (its source is embedded
// verbatim, making the entry self-contained), and every proving run id
// must name a run this plane actually holds — an entry's evidence is
// checked where it claims to have been produced. Identical content is
// idempotent (200), matching workflow publication.
func (s *Server) handleCreateStoreEntry(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireStoreWriteAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	var req storeEntryCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send a JSON body matching StoreEntryCreateRequest: {name?, graph_digest, evidence}", "decode request body: %v", err)
	}
	if req.GraphDigest == "" {
		return badRequest("graph_digest must name a published workflow version", "graph_digest is required")
	}
	if apiErr := validateStoreEvidence(req.Evidence); apiErr != nil {
		return apiErr
	}

	version, err := s.workflowVersionByDigest(ctx, req.GraphDigest)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound(
				"publish the workflow first (POST /v1alpha1/workflows) — a store entry embeds the published source verbatim",
				"no workflow version with digest %s", req.GraphDigest)
		}
		return internalError(err)
	}

	for _, runID := range req.Evidence.ProvingRunIDs {
		exists, err := s.runExists(ctx, runID)
		if err != nil {
			return internalError(err)
		}
		if !exists {
			return badRequest(
				"every proving run id must be a run this control plane holds — check GET /v1alpha1/runs",
				"proving run %s does not exist", runID)
		}
	}

	name := req.Name
	if name == "" {
		name = version.WorkflowKey
	}
	graph := storeEntryGraphOut{Digest: version.ContentDigest, SourceFormat: version.SourceFormat, Source: version.Source}
	digest, err := storeEntryDigest(name, graph, req.Evidence)
	if err != nil {
		return internalError(fmt.Errorf("compute entry digest: %w", err))
	}

	// Idempotent-by-digest: the pre-check is what lets the response say 200
	// versus 201 — CreateStoreEntry below resolves to the same row either
	// way (workflows.go's publish pre-check, same benign race).
	if existing, err := s.engineStore.GetStoreEntryByDigest(ctx, "local", digest); err == nil {
		writeJSON(w, http.StatusOK, storeEntryOutFrom(existing, false))
		return nil
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return internalError(err)
	}

	created, err := s.engineStore.CreateStoreEntry(ctx, postgres.CreateStoreEntryInput{
		Name:              name,
		Origin:            "local",
		GraphDigest:       version.ContentDigest,
		GraphSourceFormat: version.SourceFormat,
		GraphSource:       version.Source,
		Evidence:          evidenceManifestFrom(req.Evidence),
		EntryDigest:       digest,
	})
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, storeEntryOutFrom(created, false))
	return nil
}

// storeEntryPullRequest is components.schemas.StoreEntryPullRequest. Entry
// is the full self-contained document GET /v1alpha1/store/entries/{id}
// serves on the SOURCE registry; the client's job is transport alone —
// which registry to pull from is client configuration, never server code.
type storeEntryPullRequest struct {
	SourceRegistry string        `json:"source_registry"`
	Entry          storeEntryOut `json:"entry"`
}

// handleStoreEntryPull is POST /v1alpha1/store/entries/pull: ingest an
// entry fetched from another registry, verbatim, as origin "pulled". The
// entry's own digest is its integrity seal — it is recomputed here over
// the carried name/graph/evidence, so a document edited in transit is
// refused rather than cataloged; the embedded graph source is additionally
// compiled to prove it digests to the declared graph digest. Evidence is
// NOT re-validated against local runs: the proving runs live on the source
// plane, and rewriting or dropping them would be exactly the loss of
// fidelity the q6 decision rules out. Row identity is re-minted locally
// with origin "pulled", so ingesting can never overwrite or shadow a
// locally-authored entry (#192 acceptance (b)).
func (s *Server) handleStoreEntryPull(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireStoreWriteAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	var req storeEntryPullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest("send a JSON body matching StoreEntryPullRequest: {source_registry, entry}", "decode request body: %v", err)
	}
	if req.SourceRegistry == "" {
		return badRequest("source_registry must name the registry the entry was fetched from", "source_registry is required")
	}
	e := req.Entry
	switch {
	case e.Name == "":
		return badRequest("entry.name is missing — pass the full document GET /v1alpha1/store/entries/{id} served", "entry.name is required")
	case e.Graph.Digest == "" || e.Graph.SourceFormat == "" || e.Graph.Source == "":
		return badRequest("entry.graph must carry digest, source_format, and the verbatim source", "entry.graph is incomplete")
	case e.EntryDigest == "":
		return badRequest("entry.entry_digest is missing — it is the integrity seal a pull verifies", "entry.entry_digest is required")
	}
	if apiErr := validateStoreEvidence(e.Evidence); apiErr != nil {
		return apiErr
	}

	recomputed, err := storeEntryDigest(e.Name, e.Graph, e.Evidence)
	if err != nil {
		return internalError(fmt.Errorf("compute entry digest: %w", err))
	}
	if recomputed != e.EntryDigest {
		return badRequest(
			"the entry's content does not digest to its entry_digest — re-fetch it from the source registry rather than editing it",
			"entry digest mismatch: document says %s, content digests to %s", e.EntryDigest, recomputed)
	}

	// The inner seal: the embedded source must actually be the graph the
	// digest names. A digest-consistent envelope around a forged graph
	// would otherwise catalog a flow nobody proved.
	// A compile error here is the CALLER's document failing, not this
	// server failing (PR #208 review finding 6): the source is
	// caller-supplied, so a malformed graph is a 400 with the compiler's
	// reason, the same posture as the digest mismatches beside it.
	compiled, _, err := compiler.Compile([]byte(e.Graph.Source), compiler.Format(e.Graph.SourceFormat))
	if err != nil {
		return badRequest(
			"the entry's embedded workflow source does not compile — re-fetch it from the source registry",
			"embedded workflow source does not compile: %v", err)
	}
	if compiled == nil {
		return badRequest(
			"the entry's embedded workflow source does not compile — re-fetch it from the source registry",
			"embedded workflow source does not compile")
	}
	if compiled.Digest != e.Graph.Digest {
		return badRequest(
			"the embedded workflow source does not digest to entry.graph.digest — re-fetch it from the source registry",
			"graph digest mismatch: document says %s, source digests to %s", e.Graph.Digest, compiled.Digest)
	}

	if existing, err := s.engineStore.GetStoreEntryByDigest(ctx, "pulled", e.EntryDigest); err == nil {
		writeJSON(w, http.StatusOK, storeEntryOutFrom(existing, false))
		return nil
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return internalError(err)
	}

	created, err := s.engineStore.CreateStoreEntry(ctx, postgres.CreateStoreEntryInput{
		Name:              e.Name,
		Origin:            "pulled",
		SourceRegistry:    req.SourceRegistry,
		GraphDigest:       e.Graph.Digest,
		GraphSourceFormat: e.Graph.SourceFormat,
		GraphSource:       e.Graph.Source,
		Evidence:          evidenceManifestFrom(e.Evidence),
		EntryDigest:       e.EntryDigest,
	})
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, storeEntryOutFrom(created, false))
	return nil
}

// handleListStoreEntries is GET /v1alpha1/store/entries[?name=]: the
// browsing surface — every entry, newest first, each with its graph digest
// and full evidence manifest (the graph source is omitted here; GET
// /store/entries/{id} serves the self-contained document).
func (s *Server) handleListStoreEntries(w http.ResponseWriter, r *http.Request) error {
	entries, err := s.engineStore.ListStoreEntries(r.Context(), r.URL.Query().Get("name"))
	if err != nil {
		return internalError(err)
	}
	out := storeEntryListOut{Items: make([]storeEntryOut, 0, len(entries))}
	for _, e := range entries {
		out.Items = append(out.Items, storeEntryOutFrom(e, false))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleGetStoreEntry is GET /v1alpha1/store/entries/{id}: the full
// self-contained document, graph source included — exactly what a client
// of this registry hands to its own plane's pull route.
func (s *Server) handleGetStoreEntry(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	e, err := s.engineStore.GetStoreEntry(r.Context(), id)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the id against GET /v1alpha1/store/entries", "no store entry with id %s", id)
		}
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, storeEntryOutFrom(e, true))
	return nil
}

// runExists reports whether a run row exists in this namespace — the
// evidence check behind proving_run_ids (queries.go's raw-read idiom).
func (s *Server) runExists(ctx context.Context, runID string) (bool, error) {
	var one int
	err := s.Store.Pool().QueryRow(ctx,
		`SELECT 1 FROM runs WHERE namespace_id = $1 AND id = $2`, s.NamespaceID, runID).Scan(&one)
	if err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("api: run %s: %w", runID, err)
	}
	return true, nil
}
