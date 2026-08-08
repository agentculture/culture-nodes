package postgres

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Namespace is the installation/tenant boundary row (prd-spec §14).
type Namespace struct {
	ID          string
	Slug        string
	DisplayName string
	CreatedAt   time.Time
}

// WorkflowVersion is an immutable published workflow definition: exact
// submitted source plus the normalized IR and content digest the runtime
// executes (prd-spec §11.3).
type WorkflowVersion struct {
	ID                 string
	NamespaceID        string
	WorkflowKey        string
	Version            int32
	DraftID            string
	OwnerID            string
	SourceFormat       string
	Source             string
	NormalizedIR       json.RawMessage
	ContentDigest      string
	PublishedByActorID string
	CreatedAt          time.Time
}

// CreateWorkflowVersionInput is the input to Store.CreateWorkflowVersion.
// DraftID, OwnerID, and PublishedByActorID are optional (empty string means
// NULL).
type CreateWorkflowVersionInput struct {
	NamespaceID        string
	WorkflowKey        string
	Version            int32
	DraftID            string
	OwnerID            string
	SourceFormat       string
	Source             string
	NormalizedIR       json.RawMessage
	ContentDigest      string
	PublishedByActorID string
}

// Event is an append-only audit event row (prd-spec §15.1). Sequence is
// monotonic per AggregateID.
type Event struct {
	ID            string
	NamespaceID   string
	AggregateType string
	AggregateID   string
	Sequence      int64
	EventType     string
	Source        string
	Data          json.RawMessage
	OccurredAt    time.Time
}

// InsertEventInput is the input to Store.InsertEvent. Source defaults to
// "nodes" and Data defaults to "{}" when left zero.
type InsertEventInput struct {
	NamespaceID   string
	AggregateType string
	AggregateID   string
	EventType     string
	Source        string
	Data          json.RawMessage
}

// OutboxRecord is a transactional outbox row (prd-spec §12.5, §12.3).
type OutboxRecord struct {
	ID          string
	NamespaceID string
	Topic       string
	Payload     json.RawMessage
	Status      string
	AvailableAt time.Time
	PublishedAt *time.Time
	Attempts    int32
	CreatedAt   time.Time
}

// InsertOutboxInput is the input to Store.InsertOutbox. AvailableAt
// defaults to now() when zero.
type InsertOutboxInput struct {
	NamespaceID string
	Topic       string
	Payload     json.RawMessage
	AvailableAt time.Time
}

// textOrNull converts an empty string to a NULL pgtype.Text and any other
// string to a valid one. Every optional TEXT foreign key/column in this
// package's inputs uses "" to mean NULL.
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// textOrEmpty is the inverse of textOrNull: a NULL column reads back as "".
func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func tsOrNow(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func tsValue(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// jsonOrEmptyObject returns data unchanged, or a JSON "{}" when data is nil
// or empty, so JSONB columns declared NOT NULL never receive a Go nil.
func jsonOrEmptyObject(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage(`{}`)
	}
	return data
}
