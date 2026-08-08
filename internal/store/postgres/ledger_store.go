package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// LedgerStore is the PostgreSQL implementation of ledger.Store: the durable
// backing for the append-only work ledger (prd-spec §10, §14).
//
// It is scoped to one namespace. The ledger envelope has no namespace field
// -- a namespace is a deployment boundary, not something a record asserts
// about itself -- so the binding is made here, once, rather than being
// carried in and out of every record.
//
// Two invariants live in the database rather than in this type. Records are
// immutable: migration 0003's ledger_records_no_update/_no_delete triggers
// refuse any UPDATE or DELETE, so this store has no code path that could
// rewrite one. And origin_actor_id references actors(id), so a record can
// only name a producer that has been registered (prd-spec §9.5) -- an
// unregistered actor is a foreign-key violation, not a silently accepted
// string.
type LedgerStore struct {
	pool        *pgxpool.Pool
	q           ledgerQuerier
	namespaceID string
	inTx        bool
}

// ledgerQuerier is the subset of pgx both *pgxpool.Pool and pgx.Tx provide,
// so every query below is written once and runs either directly or inside a
// transaction.
type ledgerQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewLedgerStore returns a ledger store over s, scoped to namespaceID.
func NewLedgerStore(s *Store, namespaceID string) (*LedgerStore, error) {
	if s == nil {
		return nil, errors.New("postgres: NewLedgerStore requires a store")
	}
	if namespaceID == "" {
		return nil, errors.New("postgres: NewLedgerStore requires a namespace id")
	}
	return &LedgerStore{pool: s.pool, q: s.pool, namespaceID: namespaceID}, nil
}

// NewLedger returns a ledger runtime backed by s and scoped to namespaceID.
// It is the one-line path callers want: the store is an implementation
// detail of the runtime, not something most callers hold separately.
func NewLedger(s *Store, namespaceID string, opts ...ledger.Option) (*ledger.Ledger, error) {
	store, err := NewLedgerStore(s, namespaceID)
	if err != nil {
		return nil, err
	}
	return ledger.New(store, opts...)
}

// NamespaceID reports the namespace this store is bound to.
func (ls *LedgerStore) NamespaceID() string { return ls.namespaceID }

const ledgerRecordColumns = `id, schema_version, record_type, run_id, node_run_id, attempt_id,
	origin_kind, origin_actor_id, origin_actor_revision, authority, subject_ref,
	data, provenance_refs, supersedes, content_digest, created_at`

const insertLedgerRecordSQL = `
INSERT INTO ledger_records (
	id, namespace_id, schema_version, record_type, run_id, node_run_id, attempt_id,
	origin_kind, origin_actor_id, origin_actor_revision, authority, subject_ref,
	data, provenance_refs, supersedes, content_digest, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

// InsertRecord appends one record. A duplicate id is refused rather than
// overwritten: there is no update path to fall back on, by design.
func (ls *LedgerStore) InsertRecord(ctx context.Context, rec ledger.Record) error {
	provenance, err := json.Marshal(nonNilRefs(rec.ProvenanceRefs))
	if err != nil {
		return fmt.Errorf("postgres: ledger: encode provenance_refs of %s: %w", rec.ID, err)
	}

	_, err = ls.q.Exec(ctx, insertLedgerRecordSQL,
		rec.ID,
		ls.namespaceID,
		rec.SchemaVersion,
		string(rec.RecordType),
		rec.RunID,
		textOrNull(rec.NodeRunID.String()),
		textOrNull(rec.AttemptID.String()),
		string(rec.Origin.Kind),
		textOrNull(rec.Origin.ActorID),
		textOrNull(rec.Origin.ActorRevision),
		string(rec.Authority),
		textOrNull(rec.SubjectRef.String()),
		jsonOrEmptyObject(rec.Data),
		provenance,
		textOrNull(rec.Supersedes.String()),
		rec.ContentDigest,
		rec.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("postgres: ledger: record %s already exists; records are immutable and are never rewritten: %w", rec.ID, err)
		}
		return fmt.Errorf("postgres: ledger: insert record %s: %w", rec.ID, err)
	}
	return nil
}

// GetRecord returns one record, or ledger.ErrRecordNotFound.
func (ls *LedgerStore) GetRecord(ctx context.Context, id string) (ledger.Record, error) {
	row := ls.q.QueryRow(ctx,
		`SELECT `+ledgerRecordColumns+` FROM ledger_records WHERE namespace_id = $1 AND id = $2`,
		ls.namespaceID, id)

	rec, err := scanLedgerRecord(row)
	if err != nil {
		if isNoRows(err) {
			return ledger.Record{}, fmt.Errorf("postgres: ledger record %s: %w", id, ledger.ErrRecordNotFound)
		}
		return ledger.Record{}, fmt.Errorf("postgres: ledger: get record %s: %w", id, err)
	}
	return rec, nil
}

// RunRecords returns every record of a run, ordered by id -- which, for the
// ULIDs this schema uses as primary keys, is append order.
func (ls *LedgerStore) RunRecords(ctx context.Context, runID string) ([]ledger.Record, error) {
	rows, err := ls.q.Query(ctx,
		`SELECT `+ledgerRecordColumns+` FROM ledger_records
		 WHERE namespace_id = $1 AND run_id = $2 ORDER BY id`,
		ls.namespaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ledger: list records of run %s: %w", runID, err)
	}
	defer rows.Close()

	out := make([]ledger.Record, 0)
	for rows.Next() {
		rec, err := scanLedgerRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ledger: scan record of run %s: %w", runID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ledger: read records of run %s: %w", runID, err)
	}
	return out, nil
}

// LedgerVersion is the number of records appended to a run. The count is a
// usable optimistic-concurrency token precisely because ledger_records is
// append-only: nothing can ever remove a row and make it go backwards.
func (ls *LedgerStore) LedgerVersion(ctx context.Context, runID string) (int64, error) {
	var version int64
	err := ls.q.QueryRow(ctx,
		`SELECT count(*) FROM ledger_records WHERE namespace_id = $1 AND run_id = $2`,
		ls.namespaceID, runID).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("postgres: ledger: version of run %s: %w", runID, err)
	}
	return version, nil
}

// LiveSupersessors returns the records that replace recordID and have not
// themselves been replaced.
func (ls *LedgerStore) LiveSupersessors(ctx context.Context, recordID string) ([]ledger.Record, error) {
	rows, err := ls.q.Query(ctx,
		`SELECT `+prefixedLedgerRecordColumns("s")+` FROM ledger_records s
		 WHERE s.namespace_id = $1 AND s.supersedes = $2
		   AND NOT EXISTS (
			   SELECT 1 FROM ledger_records t
			   WHERE t.namespace_id = $1 AND t.supersedes = s.id
		   )
		 ORDER BY s.id`,
		ls.namespaceID, recordID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ledger: supersessors of %s: %w", recordID, err)
	}
	defer rows.Close()

	out := make([]ledger.Record, 0)
	for rows.Next() {
		rec, err := scanLedgerRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ledger: scan supersessor of %s: %w", recordID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ledger: read supersessors of %s: %w", recordID, err)
	}
	return out, nil
}

// Lock takes a transaction-scoped advisory lock, serialising the
// read-then-write sequences (supersession checks, review version checks)
// that would otherwise let two callers both act on the same state.
//
// It refuses outside a transaction: pg_advisory_xact_lock releases on commit
// or rollback, and taking one on a pooled connection with no transaction to
// scope it to would leak the lock onto whatever ran next.
func (ls *LedgerStore) Lock(ctx context.Context, key string) error {
	if !ls.inTx {
		return errors.New("postgres: ledger: Lock requires a transaction; call it inside InTx")
	}
	if _, err := ls.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("postgres: ledger: lock %s: %w", key, err)
	}
	return nil
}

const insertLedgerReviewSQL = `
INSERT INTO ledger_reviews (
	id, namespace_id, run_id, reviewer_actor_id, reviewed_ledger_version,
	frame_checksum, decision, record_ids, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// InsertReviewRequest stores an uncommitted review request.
//
// ledger_reviews.decision carries the request's lifecycle: 'requested' until
// CommitReview applies it, then 'committed'. The per-record verdicts are not
// stored here -- they are appended as review records, where they carry an
// authority, an origin, and a content digest like every other assertion in
// the ledger.
func (ls *LedgerStore) InsertReviewRequest(ctx context.Context, req ledger.ReviewRequest) error {
	recordIDs, err := json.Marshal(nonNilRefs(req.RecordIDs))
	if err != nil {
		return fmt.Errorf("postgres: ledger: encode record_ids of review %s: %w", req.ID, err)
	}

	_, err = ls.q.Exec(ctx, insertLedgerReviewSQL,
		req.ID,
		ls.namespaceID,
		req.RunID,
		textOrNull(req.ReviewerActorID),
		req.LedgerVersion,
		req.FrameChecksum,
		string(req.Status),
		recordIDs,
		req.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("postgres: ledger: review %s already exists: %w", req.ID, err)
		}
		return fmt.Errorf("postgres: ledger: insert review %s: %w", req.ID, err)
	}
	return nil
}

// GetReviewRequest returns one review request, or ledger.ErrReviewNotFound.
func (ls *LedgerStore) GetReviewRequest(ctx context.Context, id string) (ledger.ReviewRequest, error) {
	var (
		req       ledger.ReviewRequest
		reviewer  pgtype.Text
		decision  string
		recordIDs []byte
		createdAt pgtype.Timestamptz
	)

	err := ls.q.QueryRow(ctx,
		`SELECT id, run_id, reviewer_actor_id, reviewed_ledger_version, frame_checksum,
			decision, record_ids, created_at
		 FROM ledger_reviews WHERE namespace_id = $1 AND id = $2`,
		ls.namespaceID, id).Scan(
		&req.ID, &req.RunID, &reviewer, &req.LedgerVersion, &req.FrameChecksum,
		&decision, &recordIDs, &createdAt)
	if err != nil {
		if isNoRows(err) {
			return ledger.ReviewRequest{}, fmt.Errorf("postgres: ledger review %s: %w", id, ledger.ErrReviewNotFound)
		}
		return ledger.ReviewRequest{}, fmt.Errorf("postgres: ledger: get review %s: %w", id, err)
	}

	req.ReviewerActorID = textOrEmpty(reviewer)
	req.Status = ledger.ReviewStatus(decision)
	req.CreatedAt = tsValue(createdAt).UTC()
	if len(recordIDs) > 0 {
		if err := json.Unmarshal(recordIDs, &req.RecordIDs); err != nil {
			return ledger.ReviewRequest{}, fmt.Errorf("postgres: ledger: decode record_ids of review %s: %w", id, err)
		}
	}
	if req.RecordIDs == nil {
		req.RecordIDs = []string{}
	}
	return req, nil
}

// MarkReviewCommitted flips a request from requested to committed, and
// reports whether this call was the one that did it. The status is part of
// the WHERE clause, so two concurrent commits cannot both win.
func (ls *LedgerStore) MarkReviewCommitted(ctx context.Context, id string) (bool, error) {
	tag, err := ls.q.Exec(ctx,
		`UPDATE ledger_reviews SET decision = $3
		 WHERE namespace_id = $1 AND id = $2 AND decision = $4`,
		ls.namespaceID, id, string(ledger.ReviewCommitted), string(ledger.ReviewRequested))
	if err != nil {
		return false, fmt.Errorf("postgres: ledger: commit review %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// CheckpointProjection records the digest a projection produced for a run at
// a ledger version (prd-spec §10.9, the ledger_projection_versions table).
//
// It is idempotent: re-checkpointing the same projection at the same version
// succeeds if the digest matches. A different digest for the same inputs is
// not a conflict to resolve, it is a determinism failure, and it is reported
// as one -- the whole value of a projection digest is that it cannot come out
// two ways.
//
// Subject-scoped projections are keyed as "<kind>:<subject>", because the
// table's uniqueness is (run, kind, version) and two subjects are two
// different projections at the same version.
func (ls *LedgerStore) CheckpointProjection(ctx context.Context, runID string, version int64, p ledger.Projection) error {
	if runID == "" {
		return errors.New("postgres: ledger: CheckpointProjection requires a run id")
	}
	if p.Digest == "" {
		return errors.New("postgres: ledger: CheckpointProjection requires a projection carrying its digest")
	}
	if err := p.VerifyDigest(); err != nil {
		return fmt.Errorf("postgres: ledger: %w", err)
	}

	kind := string(p.Kind)
	if p.Subject != "" {
		kind += ":" + p.Subject
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("postgres: ledger: encode %s projection: %w", kind, err)
	}

	tag, err := ls.q.Exec(ctx, `
		INSERT INTO ledger_projection_versions (
			id, namespace_id, run_id, projection_kind, ledger_version, digest, data
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT ON CONSTRAINT ledger_projection_versions_run_kind_version_key DO NOTHING`,
		store.NewULID(), ls.namespaceID, runID, kind, version, p.Digest, body)
	if err != nil {
		return fmt.Errorf("postgres: ledger: checkpoint %s projection: %w", kind, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var existing string
	err = ls.q.QueryRow(ctx,
		`SELECT digest FROM ledger_projection_versions
		 WHERE run_id = $1 AND projection_kind = $2 AND ledger_version = $3`,
		runID, kind, version).Scan(&existing)
	if err != nil {
		return fmt.Errorf("postgres: ledger: read existing %s checkpoint: %w", kind, err)
	}
	if existing != p.Digest {
		return fmt.Errorf(
			"postgres: ledger: %s projection of run %s at ledger version %d digests as %s but was checkpointed as %s; identical ledger inputs must produce identical projections",
			kind, runID, version, p.Digest, existing)
	}
	return nil
}

// InTx runs fn inside one transaction: everything fn writes commits together
// or not at all. Calling it on a store that is already inside a transaction
// joins that transaction rather than opening a second one.
func (ls *LedgerStore) InTx(ctx context.Context, fn func(context.Context, ledger.Tx) error) error {
	if ls.inTx {
		return fn(ctx, ls)
	}

	tx, err := ls.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: ledger: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	inner := &LedgerStore{pool: ls.pool, q: tx, namespaceID: ls.namespaceID, inTx: true}
	if err := fn(ctx, inner); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: ledger: commit transaction: %w", err)
	}
	return nil
}

// scanLedgerRecord reads one row into the ledger envelope. NULL columns
// become the zero NullableID, which serialises back to JSON null -- the
// round trip is lossless, which is what lets a record's content digest still
// verify after it has been read back.
func scanLedgerRecord(row pgx.Row) (ledger.Record, error) {
	var (
		rec            ledger.Record
		recordType     string
		originKind     string
		authority      string
		originActor    pgtype.Text
		originRevision pgtype.Text
		nodeRunID      pgtype.Text
		attemptID      pgtype.Text
		subjectRef     pgtype.Text
		supersedes     pgtype.Text
		data           []byte
		provenance     []byte
		createdAt      pgtype.Timestamptz
	)

	if err := row.Scan(
		&rec.ID, &rec.SchemaVersion, &recordType, &rec.RunID, &nodeRunID, &attemptID,
		&originKind, &originActor, &originRevision, &authority, &subjectRef,
		&data, &provenance, &supersedes, &rec.ContentDigest, &createdAt,
	); err != nil {
		return ledger.Record{}, err
	}

	rec.RecordType = ledger.RecordType(recordType)
	rec.Authority = ledger.Authority(authority)
	rec.Origin = ledger.Origin{
		Kind:          ledger.OriginKind(originKind),
		ActorID:       textOrEmpty(originActor),
		ActorRevision: textOrEmpty(originRevision),
	}
	rec.NodeRunID = ledger.NullableID(textOrEmpty(nodeRunID))
	rec.AttemptID = ledger.NullableID(textOrEmpty(attemptID))
	rec.SubjectRef = ledger.NullableID(textOrEmpty(subjectRef))
	rec.Supersedes = ledger.NullableID(textOrEmpty(supersedes))
	rec.Data = json.RawMessage(data)
	rec.CreatedAt = tsValue(createdAt).UTC()

	rec.ProvenanceRefs = []string{}
	if len(provenance) > 0 {
		if err := json.Unmarshal(provenance, &rec.ProvenanceRefs); err != nil {
			return ledger.Record{}, fmt.Errorf("decode provenance_refs of %s: %w", rec.ID, err)
		}
		if rec.ProvenanceRefs == nil {
			rec.ProvenanceRefs = []string{}
		}
	}
	return rec, nil
}

// prefixedLedgerRecordColumns qualifies the column list with a table alias,
// for the queries that join ledger_records to itself.
func prefixedLedgerRecordColumns(alias string) string {
	return alias + `.id, ` + alias + `.schema_version, ` + alias + `.record_type, ` +
		alias + `.run_id, ` + alias + `.node_run_id, ` + alias + `.attempt_id, ` +
		alias + `.origin_kind, ` + alias + `.origin_actor_id, ` + alias + `.origin_actor_revision, ` +
		alias + `.authority, ` + alias + `.subject_ref, ` + alias + `.data, ` +
		alias + `.provenance_refs, ` + alias + `.supersedes, ` + alias + `.content_digest, ` +
		alias + `.created_at`
}

// nonNilRefs keeps a nil slice from being encoded as JSON null into a column
// that means "no references", which is a list, not an absence.
func nonNilRefs(refs []string) []string {
	if refs == nil {
		return []string{}
	}
	return refs
}
