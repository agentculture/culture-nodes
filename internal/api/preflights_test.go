package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/preflight"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The clarify-then-commit gate's confirm verb (issue #67, task t14):
// GET /v1alpha1/preflights, GET /v1alpha1/preflights/{id}, and
// POST /v1alpha1/preflights/{id}/acknowledge.
//
// What these pin:
//
//   - acknowledging appends a PROPOSED, agent-origin ledger record naming
//     the briefing by id AND digest, and marks the row;
//   - it is single-use and windowed, and both refusals are 409s that say so;
//   - registering an actor whose gate is enabled without a capability
//     surface is a 400 at CONFIGURATION time, with a remediation.

// seedPreflight creates a real run and node run, appends a derived preflight
// record for it, and records the durable row — the state a worker leaves
// behind when it defers a gated dispatch.
func seedPreflight(t *testing.T, f *fixture, actorID string, window time.Duration) storepg.Preflight {
	t.Helper()
	ctx := context.Background()

	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("run %s: got %d node runs, want 1", run.ID, len(view.NodeRuns))
	}
	nodeRunID := view.NodeRuns[0].ID

	now := time.Now().UTC()
	doc := preflight.Compose(
		preflight.Surface{
			ProtocolVersion: preflight.ProtocolVersion,
			Host:            json.RawMessage(`{"hostname":"test-host","sandbox_modes":["read-only"]}`),
		},
		preflight.Task{
			RunID:     run.ID,
			NodeRunID: nodeRunID,
			NodeID:    view.NodeRuns[0].NodeID,
			NodeKind:  "agent",
			ActorRef:  "actor://company/fixer",
			ActorKey:  "company/fixer",
			ActorID:   actorID,
			Outcomes:  []string{"completed"},
		}, now, window)

	record, err := preflight.NewPreflightRecord(doc, f.insertActorKind("engine-dispatch-gate", "engine"))
	if err != nil {
		t.Fatalf("NewPreflightRecord: %v", err)
	}
	appended, err := f.api.Ledger.Append(ctx, record)
	if err != nil {
		t.Fatalf("append preflight record: %v", err)
	}

	row, err := f.store.IssuePreflight(ctx, storepg.IssuePreflightInput{
		NamespaceID:  f.nsID,
		RunID:        run.ID,
		NodeRunID:    nodeRunID,
		NodeID:       view.NodeRuns[0].NodeID,
		ActorKey:     "company/fixer",
		ActorID:      actorID,
		RecordID:     appended.ID,
		RecordDigest: appended.ContentDigest,
		IssuedAt:     now,
		ExpiresAt:    doc.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("IssuePreflight: %v", err)
	}
	return row
}

type acknowledgeReq struct {
	ActorID string `json:"actor_id,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	Note    string `json:"note,omitempty"`
}

func TestAcknowledgingAPreflightRecordsTheActorsOwnProposedClaim(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("fixer")
	row := seedPreflight(t, f, actorID, preflight.DefaultWindow)

	// It is visible before it is answered, so a bridge that was not watching
	// the event stream can still find it.
	var pending apipkg.PreflightListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/preflights"), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	if len(pending.Items) != 1 || pending.Items[0].ID != row.ID {
		t.Fatalf("pending preflights = %+v, want the one just issued (%s)", pending.Items, row.ID)
	}
	if pending.Items[0].Acknowledged {
		t.Error("a freshly issued preflight lists as acknowledged")
	}

	// And it is readable in full, briefing included: an actor cannot
	// acknowledge what it cannot read.
	var detail apipkg.PreflightOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/preflights/"+row.ID), nil, &detail)
	requireStatus(t, resp, body, http.StatusOK)
	if detail.Document == nil {
		t.Fatal("the preflight detail carries no document; there is nothing for an actor to read")
	}
	var doc preflight.Document
	if err := json.Unmarshal(detail.Document, &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if doc.Verdict != preflight.VerdictHold || doc.Refusal == "" {
		t.Errorf("document = %+v, want a held briefing stating what does not proceed", doc)
	}

	var out apipkg.PreflightOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{ActorID: actorID, Verdict: preflight.VerdictProceed, Note: "read it"}, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if !out.Acknowledged || out.AcknowledgementRecordID == "" {
		t.Fatalf("acknowledge returned %+v, want an acknowledged row naming its record", out)
	}

	rec, err := f.api.Ledger.Record(context.Background(), out.AcknowledgementRecordID)
	if err != nil {
		t.Fatalf("read acknowledgement record: %v", err)
	}
	if rec.RecordType != ledger.RecordDispatchAcknowledgement {
		t.Errorf("record type = %q, want %q", rec.RecordType, ledger.RecordDispatchAcknowledgement)
	}
	if rec.Authority != ledger.AuthorityProposed || rec.Origin.Kind != ledger.OriginAgent {
		t.Errorf("origin/authority = %s/%s, want agent/proposed: an actor saying it understood is a claim",
			rec.Origin.Kind, rec.Authority)
	}
	data, err := rec.DataMap()
	if err != nil {
		t.Fatalf("decode record data: %v", err)
	}
	if data["preflight_ref"] != row.RecordID || data["preflight_digest"] != row.RecordDigest {
		t.Errorf("record names %v/%v, want the briefing record %s and its digest",
			data["preflight_ref"], data["preflight_digest"], row.RecordID)
	}

	// Answered once is answered: a second acknowledgement is refused rather
	// than silently replacing which record the row points at.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{ActorID: actorID}, nil)
	requireStatus(t, resp, body, http.StatusConflict)

	// And it drops out of the pending list, because nothing is waiting on it.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/preflights"), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	if len(pending.Items) != 0 {
		t.Errorf("pending preflights = %+v, want none after it was answered", pending.Items)
	}
}

func TestAcknowledgingAnExpiredPreflightIsRefused(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("fixer")
	row := seedPreflight(t, f, actorID, preflight.MinWindow)

	if _, err := f.store.Pool().Exec(context.Background(),
		`UPDATE dispatch_preflights SET expires_at = now() - interval '1 second' WHERE id = $1`,
		row.ID); err != nil {
		t.Fatalf("expire the preflight: %v", err)
	}

	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{ActorID: actorID}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	if !strings.Contains(string(body), "window") {
		t.Errorf("refusal body = %s, want it to name the window that closed", body)
	}
}

// The acknowledgement is the ACTOR's claim, so it must name an actor the
// registry knows — an unattributable acknowledgement would be a record of
// nobody having understood anything.
func TestAcknowledgingRequiresAKnownActor(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("fixer")
	row := seedPreflight(t, f, actorID, preflight.DefaultWindow)

	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)

	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{ActorID: "actor_does_not_exist"}, nil)
	requireStatus(t, resp, body, http.StatusNotFound)

	// A verdict other than "proceed" is not an acknowledgement: an actor
	// that cannot proceed simply does not answer.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/preflights/"+row.ID+"/acknowledge"),
		acknowledgeReq{ActorID: actorID, Verdict: "hold"}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
}

// Acceptance criterion 3 at the API door: the refusal is a 400 naming what to
// do, not a 500 from a constraint violation.
func TestRegisteringAGatedActorWithoutASurfaceIsRefused(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)

	resp, body := authedRegisterActor(t, f, actorRegistrationSecret, registerActorReq{
		ActorKey: "company/gate-without-surface",
		Kind:     "agent",
		Protocol: "http",
		Metadata: json.RawMessage(`{"preflight_gate":{"enabled":true}}`),
	})
	requireStatus(t, resp, body, http.StatusBadRequest)
	if !strings.Contains(string(body), "preflight") {
		t.Errorf("refusal body = %s, want it to name the missing capability surface", body)
	}

	// With the surface advertised it registers normally...
	resp, body = authedRegisterActor(t, f, actorRegistrationSecret, registerActorReq{
		ActorKey:     "company/gated",
		Kind:         "agent",
		Protocol:     "http",
		EndpointRef:  "http://actor.invalid",
		Capabilities: json.RawMessage(`{"preflight":{"protocol_version":"1.0","host":{"hostname":"h"}}}`),
		Metadata:     json.RawMessage(`{"preflight_gate":{"enabled":true}}`),
	})
	requireStatus(t, resp, body, http.StatusCreated)

	// ...and an ordinary registration is untouched.
	resp, body = authedRegisterActor(t, f, actorRegistrationSecret, registerActorReq{
		ActorKey:    "company/plain",
		Kind:        "agent",
		Protocol:    "http",
		EndpointRef: "http://actor.invalid",
	})
	requireStatus(t, resp, body, http.StatusCreated)
}
