package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
)

// specVersion is the CloudEvents context attribute version this envelope
// shape implements. It is not one of the fields task t10 names explicitly,
// but CloudEvents 1.0 requires it on every event, so New always stamps it --
// an envelope without it is not actually CloudEvents-compatible, only
// CloudEvents-shaped.
const specVersion = "1.0"

// typePrefix is the required prefix for every event Type this system emits
// (prd-spec §15.1).
const typePrefix = "dev.culture.nodes."

// defaultSource is used when NewInput.Source is left empty, matching
// internal/store/postgres.Store.InsertEvent's own "nodes" default so the
// events table and this envelope agree on what an unqualified source means.
const defaultSource = "nodes"

// Type* are the documented event types from prd-spec §15.1. This is not a
// closed set -- it is the illustrative list the PRD gives, and later tasks
// (the engine, ledger runtime, actor protocol, runner boundary) are
// expected to add more "dev.culture.nodes.*" types as they need audit
// events for state changes these constants don't cover yet.
const (
	TypeRunCreated               = typePrefix + "run.created"
	TypeTokenEntered             = typePrefix + "token.entered"
	TypeNodeRunReady             = typePrefix + "node-run.ready"
	TypeAttemptStarted           = typePrefix + "attempt.started"
	TypeActorAccepted            = typePrefix + "actor.accepted"
	TypeAttemptCompleted         = typePrefix + "attempt.completed"
	TypeLedgerRecordAppended     = typePrefix + "ledger.record-appended"
	TypeLedgerReviewCommitted    = typePrefix + "ledger.review-committed"
	TypeRunnerOperationCompleted = typePrefix + "runner.operation-completed"
	TypeContractRejected         = typePrefix + "contract.rejected"
	TypeTokenTransitioned        = typePrefix + "token.transitioned"
	TypeRunWaiting               = typePrefix + "run.waiting"
	TypeRunCompleted             = typePrefix + "run.completed"
)

// Envelope is a CloudEvents-1.0-compatible event (prd-spec §15.1). Data
// must carry IDs and safe metadata only -- see the package doc.
type Envelope struct {
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	SpecVersion     string          `json:"specversion"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

// NewInput is the input to New.
type NewInput struct {
	// Type must start with "dev.culture.nodes." -- one of the Type*
	// constants, or a new type a caller is introducing.
	Type string
	// Source defaults to "nodes" when empty.
	Source string
	// Subject is optional context (e.g. a run or node-run id) narrowing
	// what within Source the event is about.
	Subject string
	// Time defaults to time.Now().UTC() when zero.
	Time time.Time
	// Data is marshaled to JSON. It must carry IDs and safe metadata only
	// -- see the package doc's "Event data carries IDs and safe metadata
	// only" section. Passing nil produces a data-less event ("{}").
	Data any
}

// New builds an Envelope from in, stamping a fresh ULID as ID and
// DataContentType "application/json". It returns an error if in.Type does
// not carry the required "dev.culture.nodes." prefix or in.Data cannot be
// marshaled to JSON.
//
// New always mints a new ID. Callers that must keep an event's ID stable
// across a retry (the outbox relay, so a re-publish after a crash reuses
// the same ID rather than minting a new one -- see relay.go) construct an
// Envelope directly rather than calling New.
func New(in NewInput) (Envelope, error) {
	data, err := marshalData(in.Data)
	if err != nil {
		return Envelope{}, fmt.Errorf("events: New: %w", err)
	}

	env := Envelope{
		ID:              store.NewULID(),
		Source:          in.Source,
		SpecVersion:     specVersion,
		Type:            in.Type,
		Subject:         in.Subject,
		Time:            in.Time,
		DataContentType: "application/json",
		Data:            data,
	}
	env.applyDefaults()

	if err := env.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("events: New: %w", err)
	}
	return env, nil
}

// applyDefaults fills Source and Time when left zero. It is exported
// behavior only through New and Relay (which call it directly) -- an
// Envelope built purely by struct literal for a test does not get defaults
// applied automatically, matching how the CloudEvents attributes are
// meant to be explicit at the point an envelope is built.
func (e *Envelope) applyDefaults() {
	if e.Source == "" {
		e.Source = defaultSource
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.SpecVersion == "" {
		e.SpecVersion = specVersion
	}
	if e.DataContentType == "" {
		e.DataContentType = "application/json"
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	}
}

// Validate reports whether e carries every CloudEvents-required attribute
// this system stamps, and whether Type has the required
// "dev.culture.nodes." prefix.
func (e Envelope) Validate() error {
	switch {
	case e.ID == "":
		return fmt.Errorf("events: envelope: id is required")
	case e.Source == "":
		return fmt.Errorf("events: envelope: source is required")
	case e.SpecVersion == "":
		return fmt.Errorf("events: envelope: specversion is required")
	case e.Type == "":
		return fmt.Errorf("events: envelope: type is required")
	case !strings.HasPrefix(e.Type, typePrefix):
		return fmt.Errorf("events: envelope: type %q must start with %q", e.Type, typePrefix)
	case e.DataContentType == "":
		return fmt.Errorf("events: envelope: datacontenttype is required")
	}
	return nil
}

func marshalData(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal data: %w", err)
	}
	return b, nil
}
