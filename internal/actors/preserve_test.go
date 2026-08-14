package actors_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// Preserve carriage (task t25/t26, issue #49, spec claim c32 / honesty h21).
//
// Task t25's bridges attach an additive "preserve" key to a failed
// event/error body, the same way ADR 0008/0009/0010 attached usage/
// termination-reason/continuation-ref. These tests pin the seam task t26
// adds on top of that: Preserve.ToEngine's gating (a branch is trusted onto
// the attempt row only when the bridge reports it actually committed), and
// both terminal paths (the synchronous error body via PreserveOf, and the
// asynchronous `failed` callback) threading it through.

func strPtr(v string) *string { return &v }

// ToEngine on a nil Preserve is nil — the same nil-in/nil-out shape
// Usage.ToEngine has.
func TestPreserveToEngineConvertsNil(t *testing.T) {
	var p *actors.Preserve
	if got := p.ToEngine(); got != nil {
		t.Errorf("ToEngine() on a nil Preserve = %+v, want nil", got)
	}
}

// A minted-but-never-committed branch (Committed: false — a plumbing
// failure) names nothing that exists in any repository. ToEngine must
// refuse it rather than carry a name onto the attempt row that no `git
// update-ref` ever backed.
func TestPreserveToEngineRefusesUncommittedBranch(t *testing.T) {
	p := &actors.Preserve{
		Attempted: true,
		Committed: false,
		Branch:    strPtr("preserve/would-be-branch"),
		Reason:    strPtr("git commit-tree failed"),
	}
	if got := p.ToEngine(); got != nil {
		t.Errorf("ToEngine() on an uncommitted Preserve = %+v, want nil", got)
	}
}

// Committed with no branch name (should never happen in practice, but the
// gate must not trust it) also yields nil rather than a zero-value branch.
func TestPreserveToEngineRefusesMissingBranch(t *testing.T) {
	p := &actors.Preserve{Attempted: true, Committed: true, Branch: nil}
	if got := p.ToEngine(); got != nil {
		t.Errorf("ToEngine() with Committed but no Branch = %+v, want nil", got)
	}
}

// The honest common path: a committed, local-only branch. Pushed stays
// false and Remote still carries the remote the bridge attempted the push
// against — the distinction the run detail page renders.
func TestPreserveToEngineCopiesLocalOnlyBranch(t *testing.T) {
	p := &actors.Preserve{
		Attempted: true,
		Committed: true,
		Branch:    strPtr("preserve/run-01J-att-01K-20260813T120000Z-ab12cd"),
		Commit:    strPtr("deadbeef"),
		Pushed:    false,
		LocalOnly: true,
		Remote:    strPtr("origin"),
		Reason:    strPtr("git push to remote 'origin' failed: authentication required — the local preserve commit still exists"),
	}
	got := p.ToEngine()
	if got == nil {
		t.Fatal("ToEngine() = nil, want a converted block")
	}
	if got.Branch != "preserve/run-01J-att-01K-20260813T120000Z-ab12cd" {
		t.Errorf("Branch = %q, want the minted branch name", got.Branch)
	}
	if got.Pushed {
		t.Error("Pushed = true, want false for a local-only outcome")
	}
	if got.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", got.Remote)
	}
}

// The pushed path: Pushed true, Remote carried through.
func TestPreserveToEngineCopiesPushedBranch(t *testing.T) {
	p := &actors.Preserve{
		Attempted: true,
		Committed: true,
		Branch:    strPtr("preserve/run-01J-att-01M-20260813T130000Z-cd34ef"),
		Commit:    strPtr("cafef00d"),
		Pushed:    true,
		LocalOnly: false,
		Remote:    strPtr("origin"),
	}
	got := p.ToEngine()
	if got == nil {
		t.Fatal("ToEngine() = nil, want a converted block")
	}
	if !got.Pushed {
		t.Error("Pushed = false, want true")
	}
	if got.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", got.Remote)
	}
}

// The synchronous path: a bridge's non-2xx error body carries a "preserve"
// key exactly the way it carries "usage" (TestInvokeCapturesUsageFromErrorBody's
// twin).
func TestInvokeCapturesPreserveFromErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{
			"error": "claude exited non-zero",
			"class": "execution",
			"preserve": {
				"attempted": true,
				"committed": true,
				"branch": "preserve/run-01J-att-01N-20260813T140000Z-ef56gh",
				"commit": "0123456789abcdef",
				"pushed": true,
				"local_only": false,
				"remote": "origin",
				"reason": null
			}
		}`))
	}))
	defer server.Close()

	_, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassExecution)

	preserve := actors.PreserveOf(err)
	if preserve == nil {
		t.Fatal("PreserveOf = nil, want the error body's preserve block surfaced")
	}
	if !preserve.Committed || preserve.Branch == nil {
		t.Fatalf("preserve = %+v, want Committed true with a Branch", preserve)
	}
	if *preserve.Branch != "preserve/run-01J-att-01N-20260813T140000Z-ef56gh" {
		t.Errorf("Branch = %q, want the minted branch", *preserve.Branch)
	}
	if !preserve.Pushed {
		t.Error("Pushed = false, want true")
	}

	converted := preserve.ToEngine()
	if converted == nil || converted.Branch != *preserve.Branch || !converted.Pushed {
		t.Errorf("ToEngine() = %+v, want the same branch, pushed", converted)
	}
}

// An error body with no preserve key — the overwhelming common case, since
// preserve-on-failure only runs when a bridge's node actually left
// workspace changes behind — yields a nil Preserve, never a fabricated one.
func TestInvokeErrorBodyWithoutPreserveYieldsNilPreserve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"session crashed","class":"execution"}`))
	}))
	defer server.Close()

	_, err := newClient(t).Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())
	assertClass(t, err, actors.ClassExecution)
	if preserve := actors.PreserveOf(err); preserve != nil {
		t.Errorf("PreserveOf = %+v, want nil: absent preserve stays absent", preserve)
	}
}

// PreserveOf on an unclassified error is nil, matching UsageOf's shape.
func TestPreserveOfUnclassifiedErrorIsNil(t *testing.T) {
	if preserve := actors.PreserveOf(errors.New("plain")); preserve != nil {
		t.Errorf("PreserveOf(plain error) = %+v, want nil", preserve)
	}
}

// The asynchronous path: a `failed` callback's optional preserve block
// persists on the failed attempt row, the same way its Usage block does
// (TestCallbackFailedPersistsUsage's twin).
func TestCallbackFailedPersistsPreserveBranch(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassExecution,
		Message: "the session crashed after leaving workspace changes",
		Preserve: &actors.Preserve{
			Attempted: true,
			Committed: true,
			Branch:    strPtr("preserve/async-branch-01"),
			Commit:    strPtr("abc123"),
			Pushed:    true,
			Remote:    strPtr("origin"),
		},
	})

	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-preserve", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	preserve := attempts[0].Preserve
	if preserve == nil {
		t.Fatal("Preserve = nil, want the reported branch persisted on the failed attempt")
	}
	if preserve.Branch != "preserve/async-branch-01" {
		t.Errorf("Branch = %q, want preserve/async-branch-01", preserve.Branch)
	}
	if !preserve.Pushed {
		t.Error("Pushed = false, want true")
	}
	if preserve.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", preserve.Remote)
	}
}

// A `failed` callback that reports no preserve block leaves the attempt's
// Preserve nil — the common case for most failures (nothing to preserve, or
// preserve-on-failure disabled).
func TestCallbackFailedWithoutPreserveStaysNil(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassTimeout,
		Message: "no terminal result survived",
	})
	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-no-preserve", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts[0].Preserve != nil {
		t.Errorf("Preserve = %+v, want nil for a failed event that reported none", attempts[0].Preserve)
	}
}

// A `failed` callback whose preserve block reports a plumbing failure
// (Committed: false) must not persist a branch name onto the attempt row —
// the same ToEngine gate the synchronous path relies on, exercised end to
// end through the real completion commit.
func TestCallbackFailedWithUncommittedPreserveStaysNil(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassExecution,
		Message: "the session crashed",
		Preserve: &actors.Preserve{
			Attempted: true,
			Committed: false,
			Branch:    strPtr("preserve/never-committed"),
			Reason:    strPtr("git commit-tree failed"),
		},
	})
	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-preserve-uncommitted", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts[0].Preserve != nil {
		t.Errorf("Preserve = %+v, want nil: a branch that was never committed must never be persisted",
			attempts[0].Preserve)
	}
}
