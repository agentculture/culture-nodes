package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// assertJSONEqual compares two JSON documents by decoded value, since
// PostgreSQL's JSONB column normalizes key order on the way back out.
func assertJSONEqual(t *testing.T, label string, got, want json.RawMessage) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: decode got %s: %v", label, got, err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("%s: decode want %s: %v", label, want, err)
	}
	gotCanon, _ := json.Marshal(gotVal)
	wantCanon, _ := json.Marshal(wantVal)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("%s = %s, want %s", label, got, want)
	}
}

// mustAttempt inserts one attempts row against nodeRunID, for tests that need
// a real attempt id to satisfy runner_operations.attempt_id's foreign key.
func mustAttempt(t *testing.T, s *postgres.Store, namespaceID, nodeRunID string) string {
	t.Helper()
	attemptID := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO attempts (id, namespace_id, node_run_id, attempt_number, status) VALUES ($1, $2, $3, 1, 'succeeded')`,
		attemptID, namespaceID, nodeRunID,
	); err != nil {
		t.Fatalf("mustAttempt: insert attempt: %v", err)
	}
	return attemptID
}

// TestInsertRunnerOperationRoundTrips is the acceptance case task t14 asks
// for: a runner_operations row for a pre_run/post_run hook execution, keyed
// to the attempt it ran against, round-trips through Insert/Get/List exactly
// as written.
func TestInsertRunnerOperationRoundTrips(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "runner-ops")
	nodeRunID := mustNodeRun(t, s, ns.ID)
	attemptID := mustAttempt(t, s, ns.ID, nodeRunID)

	request := json.RawMessage(`{"operation_id":"att_1:pre_run","runner":"lambda"}`)
	result := json.RawMessage(`{"operation_id":"att_1:pre_run","state":"completed"}`)
	completedAt := time.Now().UTC().Truncate(time.Microsecond)

	rec, err := s.InsertRunnerOperation(ctx, postgres.InsertRunnerOperationInput{
		ID:            "runnerop_" + store.NewULID(),
		NamespaceID:   ns.ID,
		AttemptID:     attemptID,
		OperationKind: "pre_run",
		PolicyDigest:  "sha256:" + store.NewULID(),
		Request:       request,
		Result:        result,
		Status:        "completed",
		CompletedAt:   completedAt,
	})
	if err != nil {
		t.Fatalf("InsertRunnerOperation: %v", err)
	}
	if rec.NamespaceID != ns.ID {
		t.Errorf("NamespaceID = %q, want %q", rec.NamespaceID, ns.ID)
	}
	if rec.AttemptID != attemptID {
		t.Errorf("AttemptID = %q, want %q", rec.AttemptID, attemptID)
	}
	if rec.OperationKind != "pre_run" {
		t.Errorf("OperationKind = %q, want pre_run", rec.OperationKind)
	}
	if rec.Status != "completed" {
		t.Errorf("Status = %q, want completed", rec.Status)
	}
	// JSONB round-trips through PostgreSQL key-order-normalized, so this
	// compares decoded values rather than raw bytes.
	assertJSONEqual(t, "Request", rec.Request, request)
	assertJSONEqual(t, "Result", rec.Result, result)
	if !rec.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %v, want %v", rec.CompletedAt, completedAt)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a stamped creation time")
	}

	got, err := s.GetRunnerOperation(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetRunnerOperation: %v", err)
	}
	if got.ID != rec.ID || got.AttemptID != attemptID {
		t.Errorf("GetRunnerOperation = %+v, want the inserted row", got)
	}

	list, err := s.ListRunnerOperationsByAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("ListRunnerOperationsByAttempt: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("ListRunnerOperationsByAttempt = %+v, want exactly the inserted row", list)
	}
}

// TestInsertRunnerOperationAllowsNoAttemptAndNoResult covers a dispatch
// refusal: the hook runner never produced a Result (no HookRunner
// configured, an unpinned image, a *runners.DispatchError), so there is no
// attempt yet to key the row to and no result to record. Both are NULL, not
// a fabricated stand-in.
func TestInsertRunnerOperationAllowsNoAttemptAndNoResult(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "runner-ops-refused")

	rec, err := s.InsertRunnerOperation(ctx, postgres.InsertRunnerOperationInput{
		ID:            "runnerop_" + store.NewULID(),
		NamespaceID:   ns.ID,
		OperationKind: "pre_run",
		PolicyDigest:  "sha256:" + store.NewULID(),
		Request:       json.RawMessage(`{"operation_id":"refused"}`),
		Status:        "dispatch_failed",
	})
	if err != nil {
		t.Fatalf("InsertRunnerOperation: %v", err)
	}
	if rec.AttemptID != "" {
		t.Errorf("AttemptID = %q, want empty (NULL) when no attempt was supplied", rec.AttemptID)
	}
	if rec.Result != nil {
		t.Errorf("Result = %s, want nil (NULL) when no result was recorded", rec.Result)
	}
	if !rec.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = %v, want zero (NULL) when not supplied", rec.CompletedAt)
	}
}

// TestInsertRunnerOperationRequiresCoreFields pins the required-field
// validation: an id, namespace, operation kind, policy digest, and request
// are all load-bearing, and a caller that omits one gets an error rather
// than a row with a hole in it.
func TestInsertRunnerOperationRequiresCoreFields(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "runner-ops-validation")

	base := postgres.InsertRunnerOperationInput{
		ID:            "runnerop_" + store.NewULID(),
		NamespaceID:   ns.ID,
		OperationKind: "pre_run",
		PolicyDigest:  "sha256:" + store.NewULID(),
		Request:       json.RawMessage(`{}`),
	}

	cases := []struct {
		name   string
		mutate func(postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput
	}{
		{"missing id", func(in postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput {
			in.ID = ""
			return in
		}},
		{"missing namespace", func(in postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput {
			in.NamespaceID = ""
			return in
		}},
		{"missing operation kind", func(in postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput {
			in.OperationKind = ""
			return in
		}},
		{"missing policy digest", func(in postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput {
			in.PolicyDigest = ""
			return in
		}},
		{"missing request", func(in postgres.InsertRunnerOperationInput) postgres.InsertRunnerOperationInput {
			in.Request = nil
			return in
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.InsertRunnerOperation(ctx, tc.mutate(base)); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}
