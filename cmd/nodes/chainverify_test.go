package main

// Tests for the `nodes chain-verify` verb (task t24, issue #45). Like
// run_test.go, the flag/help scenarios run everywhere; the end-to-end
// scenario creates a real run through the ad-hoc lane (`nodes run`, no
// worker needed since nothing here dispatches it), appends real ledger
// records directly against the same store/namespace (the same
// postgres.NewLedger seam successsignal_test.go's proposeSuccessSignal uses
// in the worker package), and drives the verb against the ordinary read-only
// GET /v1alpha1/runs/{id}/ledger endpoint the real control plane serves.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestChainVerifyHelpDocumentsTheVerb(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "chain-verify", "--help")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 for --help\nstderr=%s", r.ExitCode, r.Stderr)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (--help output is a result)", r.Stderr)
	}
	for _, want := range []string{"--run", "claim", "decompose"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("--help output does not mention %q:\n%s", want, r.Stdout)
		}
	}
}

func TestChainVerifyMissingRunFlagIsUserError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "chain-verify")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "--run") {
		t.Fatalf("stderr = %q, want it to point at the missing --run flag", r.Stderr)
	}
}

func TestChainVerifyUnknownRunIsRefused(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "chain-verify", "--api", ts.URL, "--run", "run_does_not_exist")

	assertNeverMixed(t, r)
	if r.ExitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero for an unknown run\nstdout=%s", r.ExitCode, r.Stdout)
	}
	assertErrorHintShape(t, r.Stderr)
}

// runAPIServerWithLedger mirrors runAPIServer but also hands back the
// underlying store's ledger, opened on the same namespace, so a test can
// append real ledger records the CLI never dispatched a run to produce —
// the same seam internal/worker/successsignal_test.go's proposeSuccessSignal
// uses on the store side of this exact package boundary.
func runAPIServerWithLedger(t *testing.T) (apiURL string, led *ledger.Ledger, actorID string) {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "cli-chain-verify")
	srv, err := api.NewServer(s, ns.ID, api.WithAdhocRunSecret(testAdhocToken))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	l, err := storepg.NewLedger(s, ns.ID)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	// A record's origin_actor_id is a real foreign key into actors(id) — the
	// test's own producer identity must be a registered row, mirroring
	// tests/e2e/harness_test.go's registerActors for the same constraint.
	actorID = "actor_" + idstore.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'agent', 'http')
	`, actorID, ns.ID, "cli-chain-verify-test-actor"); err != nil {
		t.Fatalf("register test actor: %v", err)
	}

	return ts.URL, l, actorID
}

// TestChainVerifyEndToEndAgainstTestServer is the t24 verification-surface
// acceptance, exercised through the CLI verb: a run whose claims are all
// sourced and whose decisions all trace to one of them reports passed=true;
// the same claim, stripped of its sources by a second run, reports
// passed=false, with the specific unsourced claim named.
func TestChainVerifyEndToEndAgainstTestServer(t *testing.T) {
	apiURL, led, actorID := runAPIServerWithLedger(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "run", "--token", testAdhocToken,
		"--api", apiURL,
		"--instruction", "chain-verify fixture run",
		"--actor", testRunActorRef,
		"--repo", "/tmp/culture-nodes",
		"--json")
	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("create run: exit code = %d\nstderr=%s", r.ExitCode, r.Stderr)
	}
	var created runResultPayload
	assertSingleLineJSON(t, r.Stdout, &created)

	ctx := context.Background()
	claimData, _ := json.Marshal(map[string]any{
		"statement": "a sourced claim",
		"sources":   []map[string]string{{"url": "https://example.com/a"}},
	})
	claim, err := led.Append(ctx, ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      created.RunID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       claimData,
	})
	if err != nil {
		t.Fatalf("append claim: %v", err)
	}
	decisionData, _ := json.Marshal(map[string]any{"selected": "do the thing"})
	if _, err := led.Append(ctx, ledger.Record{
		RecordType:     ledger.RecordDecision,
		RunID:          created.RunID,
		Origin:         ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:      ledger.AuthorityProposed,
		Data:           decisionData,
		ProvenanceRefs: []string{claim.ID},
	}); err != nil {
		t.Fatalf("append decision: %v", err)
	}

	pass := runNodes(t, dir, "chain-verify", "--api", apiURL, "--run", created.RunID, "--json")
	assertNeverMixed(t, pass)
	if pass.ExitCode != 0 {
		t.Fatalf("chain-verify (sourced): exit code = %d, want 0\nstdout=%s\nstderr=%s", pass.ExitCode, pass.Stdout, pass.Stderr)
	}
	var passPayload chainVerifyResultPayload
	assertSingleLineJSON(t, pass.Stdout, &passPayload)
	if !passPayload.Passed {
		t.Fatalf("payload = %+v, want passed=true", passPayload)
	}
	if len(passPayload.Claims) != 1 || !passPayload.Claims[0].Sourced {
		t.Fatalf("payload.claims = %+v, want one sourced claim", passPayload.Claims)
	}
	if len(passPayload.Motivated) != 1 || !passPayload.Motivated[0].Motivated {
		t.Fatalf("payload.motivated = %+v, want one motivated decision", passPayload.Motivated)
	}

	// A second, unsourced claim with an unmotivated decision flips the
	// verdict — and names exactly what is wrong.
	unsourcedData, _ := json.Marshal(map[string]any{"statement": "an unsourced claim"})
	if _, err := led.Append(ctx, ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      created.RunID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       unsourcedData,
	}); err != nil {
		t.Fatalf("append unsourced claim: %v", err)
	}

	fail := runNodes(t, dir, "chain-verify", "--api", apiURL, "--run", created.RunID, "--json")
	assertNeverMixed(t, fail)
	if fail.ExitCode != 1 {
		t.Fatalf("chain-verify (unsourced): exit code = %d, want 1 (domain outcome)\nstdout=%s\nstderr=%s", fail.ExitCode, fail.Stdout, fail.Stderr)
	}
	var failPayload chainVerifyResultPayload
	assertSingleLineJSON(t, fail.Stdout, &failPayload)
	if failPayload.Passed {
		t.Fatalf("payload = %+v, want passed=false", failPayload)
	}
	unsourced := 0
	for _, c := range failPayload.Claims {
		if !c.Sourced {
			unsourced++
		}
	}
	if unsourced != 1 {
		t.Fatalf("payload.claims = %+v, want exactly one unsourced claim", failPayload.Claims)
	}
}
