package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/store"
)

// Durable flow-store catalog entries (migration 0042, plan task t7, issue
// #192). A store entry is a graph PLUS its evidence: the workflow content
// digest with the source bytes embedded verbatim, the proving prod run ids,
// the deviation records recorded against it, and the actor/runner
// capability requirements the graph pins. See the migration file's header
// for why the source is embedded and why capability requirements live in
// the manifest rather than the graph.

// CapabilityRequirement declares one actor/runner capability the graph
// pins. Ref is the graph's pinned identifier (actor://…, runner://…)
// carried verbatim; Capabilities is what a substitute registration on an
// importing plane must advertise. Requirements live HERE — in the evidence
// manifest — never as graph rewrites, so the later import step (WP-F, t8)
// can bind them to local registrations without touching the graph document
// or its digest.
type CapabilityRequirement struct {
	Kind         string   `json:"kind"` // "actor" | "runner"
	Ref          string   `json:"ref"`
	Capabilities []string `json:"capabilities"`
}

// DeviationRecordRef points at one deviation record recorded against the
// flow — a tree path (docs/deviations/…) or URL, carried verbatim.
type DeviationRecordRef struct {
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

// EvidenceManifest is the full-fidelity evidence half of a store entry.
type EvidenceManifest struct {
	ProvingRunIDs        []string                `json:"proving_run_ids"`
	DeviationRecords     []DeviationRecordRef    `json:"deviation_records"`
	RequiredCapabilities []CapabilityRequirement `json:"required_capabilities"`
}

// CreateStoreEntryInput is the input to Store.CreateStoreEntry /
// EngineStore.CreateStoreEntry.
type CreateStoreEntryInput struct {
	NamespaceID       string
	Name              string
	Origin            string // "local" | "pulled"
	SourceRegistry    string // "" for local; required for pulled
	GraphDigest       string
	GraphSourceFormat string
	GraphSource       string
	Evidence          EvidenceManifest
	EntryDigest       string
}

// StoreEntry is one store_entries row.
type StoreEntry struct {
	ID                string
	NamespaceID       string
	Name              string
	Origin            string
	SourceRegistry    string
	GraphDigest       string
	GraphSourceFormat string
	GraphSource       string
	Evidence          EvidenceManifest
	EntryDigest       string
	CreatedAt         time.Time
}

// CreateStoreEntry persists one catalog entry (Store-level entry point,
// explicit namespace — the internal/store/postgres test-suite convention;
// see EngineStore.CreateStoreEntry for the namespace-bound API-layer entry
// point). Rows are insert-only; identity is (namespace, origin,
// entry_digest), so re-adding identical content returns the existing row
// rather than duplicating it, and a pulled entry can never resolve to (or
// replace) a locally-authored one — the origins are distinct identities by
// construction.
func (s *Store) CreateStoreEntry(ctx context.Context, in CreateStoreEntryInput) (StoreEntry, error) {
	return createStoreEntry(ctx, s.pool, in)
}

// GetStoreEntry returns one catalog entry (Store-level entry point).
func (s *Store) GetStoreEntry(ctx context.Context, namespaceID, id string) (StoreEntry, error) {
	return getStoreEntry(ctx, s.pool, namespaceID, id)
}

// GetStoreEntryByDigest resolves an entry by its identity triple —
// (namespace, origin, entry_digest) — returning ErrNotFound when none
// matches. This is what lets the API's create path report
// idempotent-resolve (200) versus created (201), the same pre-check shape
// workflow publication uses.
func (s *Store) GetStoreEntryByDigest(ctx context.Context, namespaceID, origin, entryDigest string) (StoreEntry, error) {
	return getStoreEntryByDigest(ctx, s.pool, namespaceID, origin, entryDigest)
}

// ListStoreEntries lists catalog entries, newest first, optionally filtered
// to one name ("" lists all — unlike plan imports, "every entry in the
// store" is exactly what a browsing client asks for).
func (s *Store) ListStoreEntries(ctx context.Context, namespaceID, name string) ([]StoreEntry, error) {
	return listStoreEntries(ctx, s.pool, namespaceID, name)
}

// The namespace-bound mirrors for the API surface (internal/api holds an
// *EngineStore) — planimports.go's convention.

// CreateStoreEntry persists one catalog entry, scoped to es's namespace.
func (es *EngineStore) CreateStoreEntry(ctx context.Context, in CreateStoreEntryInput) (StoreEntry, error) {
	in.NamespaceID = es.namespaceID
	return createStoreEntry(ctx, es.pool, in)
}

// GetStoreEntry returns one catalog entry, scoped to es's namespace.
func (es *EngineStore) GetStoreEntry(ctx context.Context, id string) (StoreEntry, error) {
	return getStoreEntry(ctx, es.pool, es.namespaceID, id)
}

// GetStoreEntryByDigest resolves an entry by (origin, entry_digest), scoped
// to es's namespace.
func (es *EngineStore) GetStoreEntryByDigest(ctx context.Context, origin, entryDigest string) (StoreEntry, error) {
	return getStoreEntryByDigest(ctx, es.pool, es.namespaceID, origin, entryDigest)
}

// ListStoreEntries lists catalog entries, scoped to es's namespace.
func (es *EngineStore) ListStoreEntries(ctx context.Context, name string) ([]StoreEntry, error) {
	return listStoreEntries(ctx, es.pool, es.namespaceID, name)
}

const storeEntryColumns = `id, namespace_id, name, origin, source_registry,
	graph_digest, graph_source_format, graph_source, evidence_manifest, entry_digest, created_at`

func createStoreEntry(ctx context.Context, pool *pgxpool.Pool, in CreateStoreEntryInput) (StoreEntry, error) {
	switch {
	case in.NamespaceID == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: namespaceID is required")
	case in.Name == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: name is required")
	case in.Origin != "local" && in.Origin != "pulled":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: origin must be %q or %q, got %q", "local", "pulled", in.Origin)
	case in.Origin == "pulled" && in.SourceRegistry == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: a pulled entry must name its source registry")
	case in.Origin == "local" && in.SourceRegistry != "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: a local entry must not name a source registry")
	case in.GraphDigest == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: graphDigest is required")
	case in.GraphSourceFormat == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: graphSourceFormat is required")
	case in.GraphSource == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: graphSource is required")
	case in.EntryDigest == "":
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: entryDigest is required")
	}

	manifest, err := json.Marshal(normalizedManifest(in.Evidence))
	if err != nil {
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: encode evidence manifest: %w", err)
	}

	// Insert-or-resolve on the identity triple: ON CONFLICT DO NOTHING plus
	// a read-back keeps this idempotent under concurrent identical adds
	// without ever updating a row — rows are immutable, so there is nothing
	// a conflict could legitimately overwrite.
	id := store.NewULID()
	tag, err := pool.Exec(ctx, `
		INSERT INTO store_entries (
			id, namespace_id, name, origin, source_registry,
			graph_digest, graph_source_format, graph_source, evidence_manifest, entry_digest
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (namespace_id, origin, entry_digest) DO NOTHING`,
		id, in.NamespaceID, in.Name, in.Origin, textOrNull(in.SourceRegistry),
		in.GraphDigest, in.GraphSourceFormat, in.GraphSource, manifest, in.EntryDigest)
	if err != nil {
		return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Identical content already present in this origin: resolve to it.
		existing, err := getStoreEntryByDigest(ctx, pool, in.NamespaceID, in.Origin, in.EntryDigest)
		if err != nil {
			return StoreEntry{}, fmt.Errorf("postgres: CreateStoreEntry: resolve existing entry: %w", err)
		}
		return existing, nil
	}
	return getStoreEntry(ctx, pool, in.NamespaceID, id)
}

func getStoreEntry(ctx context.Context, pool *pgxpool.Pool, namespaceID, id string) (StoreEntry, error) {
	if namespaceID == "" {
		return StoreEntry{}, fmt.Errorf("postgres: GetStoreEntry: namespaceID is required")
	}
	if id == "" {
		return StoreEntry{}, fmt.Errorf("postgres: GetStoreEntry: id is required")
	}
	row := pool.QueryRow(ctx, `SELECT `+storeEntryColumns+`
		FROM store_entries WHERE namespace_id = $1 AND id = $2`, namespaceID, id)
	e, err := scanStoreEntry(row)
	if err != nil {
		if isNoRows(err) {
			return StoreEntry{}, fmt.Errorf("postgres: store entry %s: %w", id, ErrNotFound)
		}
		return StoreEntry{}, fmt.Errorf("postgres: GetStoreEntry %s: %w", id, err)
	}
	return e, nil
}

func getStoreEntryByDigest(ctx context.Context, pool *pgxpool.Pool, namespaceID, origin, entryDigest string) (StoreEntry, error) {
	if namespaceID == "" {
		return StoreEntry{}, fmt.Errorf("postgres: GetStoreEntryByDigest: namespaceID is required")
	}
	row := pool.QueryRow(ctx, `SELECT `+storeEntryColumns+`
		FROM store_entries WHERE namespace_id = $1 AND origin = $2 AND entry_digest = $3`,
		namespaceID, origin, entryDigest)
	e, err := scanStoreEntry(row)
	if err != nil {
		if isNoRows(err) {
			return StoreEntry{}, fmt.Errorf("postgres: store entry %s/%s: %w", origin, entryDigest, ErrNotFound)
		}
		return StoreEntry{}, fmt.Errorf("postgres: GetStoreEntryByDigest %s/%s: %w", origin, entryDigest, err)
	}
	return e, nil
}

func listStoreEntries(ctx context.Context, pool *pgxpool.Pool, namespaceID, name string) ([]StoreEntry, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: ListStoreEntries: namespaceID is required")
	}
	rows, err := pool.Query(ctx, `SELECT `+storeEntryColumns+`
		FROM store_entries
		WHERE namespace_id = $1 AND ($2 = '' OR name = $2)
		ORDER BY created_at DESC, id DESC`, namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListStoreEntries: %w", err)
	}
	defer rows.Close()

	out := make([]StoreEntry, 0)
	for rows.Next() {
		e, err := scanStoreEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ListStoreEntries: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListStoreEntries: %w", err)
	}
	return out, nil
}

// scanRow is the Scan-only subset shared by pgx.Row and pgx.Rows.
type scanRow interface {
	Scan(dest ...any) error
}

func scanStoreEntry(row scanRow) (StoreEntry, error) {
	var (
		e              StoreEntry
		sourceRegistry pgtype.Text
		manifest       []byte
		createdAt      pgtype.Timestamptz
	)
	if err := row.Scan(&e.ID, &e.NamespaceID, &e.Name, &e.Origin, &sourceRegistry,
		&e.GraphDigest, &e.GraphSourceFormat, &e.GraphSource, &manifest, &e.EntryDigest, &createdAt); err != nil {
		return StoreEntry{}, err
	}
	e.SourceRegistry = textOrEmpty(sourceRegistry)
	if err := json.Unmarshal(manifest, &e.Evidence); err != nil {
		return StoreEntry{}, fmt.Errorf("decode evidence manifest of %s: %w", e.ID, err)
	}
	e.Evidence = normalizedManifest(e.Evidence)
	e.CreatedAt = tsValue(createdAt)
	return e, nil
}

// normalizedManifest keeps nil slices from encoding as JSON null (and from
// making a stored-then-read manifest compare unequal to its input): every
// list in the manifest means "no entries", which is an empty list, not an
// absence — planimports.go's nonNilStrings distinction, applied to the
// whole manifest.
func normalizedManifest(m EvidenceManifest) EvidenceManifest {
	m.ProvingRunIDs = nonNilStrings(m.ProvingRunIDs)
	if m.DeviationRecords == nil {
		m.DeviationRecords = []DeviationRecordRef{}
	}
	if m.RequiredCapabilities == nil {
		m.RequiredCapabilities = []CapabilityRequirement{}
	}
	for i := range m.RequiredCapabilities {
		m.RequiredCapabilities[i].Capabilities = nonNilStrings(m.RequiredCapabilities[i].Capabilities)
	}
	return m
}
