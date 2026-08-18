package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/artifacts"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
)

const artifactTestSecret = "0123456789abcdef0123456789abcdef"

type fakeArtifactInvocationStore struct {
	inv actors.PendingInvocation
}

func (s fakeArtifactInvocationStore) Invocation(_ context.Context, attemptID string) (actors.PendingInvocation, error) {
	if attemptID != s.inv.AttemptID {
		return actors.PendingInvocation{}, actors.ErrUnknownAttempt
	}
	return s.inv, nil
}

// fakeRunnerOpSource serves the runner-attempt fallback: it knows exactly one
// runner attempt, att_runner, dispatched for run_runner in ns_runner.
type fakeRunnerOpSource struct{}

// The parked (in-flight) row: att_runner_parked is known here and NOT in the
// audit table, mirroring a mid-execution upload.
func (fakeRunnerOpSource) RunnerOperation(_ context.Context, _, attemptID string) (pgstore.RunnerOperation, error) {
	if attemptID != "att_runner_parked" {
		return pgstore.RunnerOperation{}, pgstore.ErrNotFound
	}
	return pgstore.RunnerOperation{AttemptID: "att_runner_parked", NamespaceID: "ns_runner", RunID: "run_runner"}, nil
}

func (fakeRunnerOpSource) ListRunnerOperationsByAttempt(_ context.Context, attemptID string) ([]pgstore.RunnerOperationRecord, error) {
	if attemptID != "att_runner" {
		return nil, nil
	}
	return []pgstore.RunnerOperationRecord{{
		ID:          "runop_1",
		NamespaceID: "ns_runner",
		AttemptID:   "att_runner",
		Request:     []byte(`{"context":{"run_id":"run_runner"}}`),
	}}, nil
}

type memoryArtifactStore struct {
	meta artifacts.ArtifactMeta
	body []byte
	ref  artifacts.Ref
}

func (s *memoryArtifactStore) Put(_ context.Context, meta artifacts.ArtifactMeta, r io.Reader) (artifacts.Ref, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	meta.SizeBytes = int64(len(body))
	meta.Digest = artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	meta.Backend = artifacts.BackendPostgres
	meta.CreatedAt = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	s.meta, s.body = meta, body
	s.ref = artifacts.NewRef(meta.NamespaceID, "01K2TESTARTIFACT0000000000")
	return s.ref, nil
}

func (s *memoryArtifactStore) Stat(_ context.Context, _ artifacts.Ref) (artifacts.ArtifactMeta, error) {
	return s.meta, nil
}

func (s *memoryArtifactStore) Get(_ context.Context, ref artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	if ref != s.ref || s.ref == "" {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(s.body)), s.meta, nil
}

// ListByAttempt makes the fake usable by the read-back routes (issue #189):
// it lists the one artifact the fake holds when the attempt matches.
func (s *memoryArtifactStore) ListByAttempt(_ context.Context, attemptID string) ([]artifacts.Listed, error) {
	if s.ref == "" || s.meta.AttemptID != attemptID {
		return nil, nil
	}
	return []artifacts.Listed{{Ref: s.ref, Meta: s.meta}}, nil
}

func (*memoryArtifactStore) Delete(context.Context, artifacts.Ref) error {
	panic("Delete must not be exposed by the write route")
}

func (*memoryArtifactStore) Reap(context.Context, artifacts.Ref, string, time.Time) (artifacts.Tombstone, error) {
	panic("Reap must not be exposed by the write route")
}

func newArtifactRouteTestServer(t *testing.T, now time.Time) (*httptest.Server, *actors.TokenSigner, *memoryArtifactStore, actors.PendingInvocation) {
	t.Helper()
	signer, err := actors.NewTokenSigner([]byte(artifactTestSecret), actors.WithTokenClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	inv := actors.PendingInvocation{
		AttemptID:   "att_producer",
		NamespaceID: "ns_durable",
		RunID:       "run_durable",
	}
	store := &memoryArtifactStore{}
	router := artifacts.NewRouter(store, store, 1<<20)
	srv := &Server{
		callbackSigner:          signer,
		artifactRouter:          router,
		artifactInvocationStore: fakeArtifactInvocationStore{inv: inv},
		artifactRunnerOps:       fakeRunnerOpSource{},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, signer, store, inv
}

func artifactRequest(t *testing.T, method, url, token, mediaType, name string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	if name != "" {
		req.Header.Set("Artifact-Name", name)
	}
	return req
}

func TestArtifactWriteRouteStoresOpaqueBodyWithDurableAssociations(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, inv := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint(inv.AttemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// The body is opaque artifact content. Even when it happens to be JSON and
	// claims authority-bearing fields, the route never parses them; namespace,
	// run, and attempt come only from the durable PendingInvocation.
	body := []byte(`{"namespace_id":"ns_attacker","run_id":"run_attacker","attempt_id":"att_attacker"}`)
	req := artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts", token, "application/json", "claim.json", bytes.NewReader(body))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, got)
	}
	if !bytes.Equal(store.body, body) {
		t.Fatalf("stored body = %q, want opaque bytes %q", store.body, body)
	}
	if store.meta.NamespaceID != inv.NamespaceID || store.meta.RunID != inv.RunID || store.meta.AttemptID != inv.AttemptID {
		t.Fatalf("persisted associations = (%q, %q, %q), want durable invocation (%q, %q, %q)",
			store.meta.NamespaceID, store.meta.RunID, store.meta.AttemptID, inv.NamespaceID, inv.RunID, inv.AttemptID)
	}
	if store.meta.Name != "claim.json" || store.meta.MediaType != "application/json" {
		t.Fatalf("persisted descriptive metadata = (%q, %q)", store.meta.Name, store.meta.MediaType)
	}
	response, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(response, []byte(`"ref":"artifact://ns_durable/01K2TESTARTIFACT0000000000"`)) ||
		!bytes.Contains(response, []byte(`"size_bytes":`)) || !bytes.Contains(response, []byte(`"digest":`)) {
		t.Fatalf("response does not carry opaque ref and measured metadata: %s", response)
	}
}

func TestArtifactWriteRouteRefusesForeignAttemptToken(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, inv := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint("att_different")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	resp, err := ts.Client().Do(artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts", token, "text/plain", "x", strings.NewReader("secret")))
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if store.body != nil {
		t.Fatal("foreign-attempt request reached artifact Put")
	}
}

func TestArtifactWriteRouteRefusesMissingAndExpiredTokens(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, inv := newArtifactRouteTestServer(t, now)
	expired, err := signer.MintUntil(inv.AttemptID, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("MintUntil: %v", err)
	}
	for _, tc := range []struct{ name, token string }{{"missing", ""}, {"expired", expired}} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Do(artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts", tc.token, "text/plain", "x", strings.NewReader("secret")))
			if err != nil {
				t.Fatalf("POST artifact: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
	if store.body != nil {
		t.Fatal("unauthorized request reached artifact Put")
	}
}

func TestArtifactSurfaceExposesOnlyAuthenticatedPost(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, _, inv := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint(inv.AttemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for _, tc := range []struct {
		name, method, path, token string
		wantStatus                int
	}{
		// The listing GET became a real route with the #189 read-back half —
		// unauthenticated like every other read surface here (runs, ledger),
		// which already expose run outputs without a token. The pin is now
		// that it answers 200, not that it is refused.
		{"unauthenticated_get_lists", http.MethodGet, "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts", "", http.StatusOK},
		// 405 since the named path exists for GET; the refusal is the pin.
		{"no_delete", http.MethodDelete, "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts/artifact-id", token, http.StatusMethodNotAllowed},
		{"ref_is_not_authorization", http.MethodGet, "/v1alpha1/artifacts/artifact://ns_durable/01K2TESTARTIFACT0000000000", "", http.StatusNotFound},
		{"filesystem_paths_never_resolve", http.MethodGet, "/v1alpha1/artifacts/etc/passwd", token, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Do(artifactRequest(t, tc.method, ts.URL+tc.path, tc.token, "", "", nil))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want refusal %d", tc.method, tc.path, resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestArtifactWriteRouteBoundsTheBody pins the limit that authentication does
// NOT provide. Neither artifacts.Store nor its drivers bound what they accept,
// so without http.MaxBytesReader a legitimately dispatched attempt with a
// runaway loop -- or one whose token leaked -- streams until the volume fills.
// The refusal must be a 413 the caller can act on, not a 500.
func TestArtifactWriteRouteBoundsTheBody(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, inv := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint(inv.AttemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	oversized := bytes.Repeat([]byte("x"), MaxArtifactBytes+1)
	req := artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts",
		token, "application/octet-stream", "runaway.bin", bytes.NewReader(oversized))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, got)
	}
	if int64(len(store.body)) > int64(MaxArtifactBytes) {
		t.Fatalf("store received %d bytes, more than the %d-byte limit -- the reader was not bounded",
			len(store.body), MaxArtifactBytes)
	}
}

// TestArtifactWriteRouteAcceptsAnyCaseBearerScheme matches RFC 7235 and every
// other authenticated route in this package (actors.go, humantasks.go,
// adhoc.go, signalevents.go all use strings.EqualFold). A case-sensitive match
// would 401 a spec-compliant client and blame its token, which is the least
// debuggable failure this route can produce.
func TestArtifactWriteRouteAcceptsAnyCaseBearerScheme(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			ts, signer, _, inv := newArtifactRouteTestServer(t, now)
			token, err := signer.Mint(inv.AttemptID)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			req, err := http.NewRequest(http.MethodPost,
				ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts",
				bytes.NewReader([]byte("body")))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", scheme+" "+token)
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("Artifact-Name", "note.txt")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("POST artifact: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				got, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 201 for scheme %q; body=%s", resp.StatusCode, scheme, got)
			}
		})
	}
}

// TestArtifactReadBackRoundTrip pins the #189 read-back half: what an attempt
// published via the authenticated PUT is listable and fetchable by name —
// {"emitted": 0} and {"emitted": 7} become distinguishable by one GET.
func TestArtifactReadBackRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, _, inv := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint(inv.AttemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	body := []byte(`{"sweep": "pr-upkeep", "emitted": 7}`)
	req := artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/"+inv.AttemptID+"/artifacts", token, "text/plain; charset=utf-8", "stdout", bytes.NewReader(body))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201", resp.StatusCode)
	}

	listResp, err := ts.Client().Get(ts.URL + "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts")
	if err != nil {
		t.Fatalf("GET listing: %v", err)
	}
	defer listResp.Body.Close()
	listing, _ := io.ReadAll(listResp.Body)
	if listResp.StatusCode != http.StatusOK || !bytes.Contains(listing, []byte(`"name":"stdout"`)) {
		t.Fatalf("listing status=%d body=%s, want 200 naming stdout", listResp.StatusCode, listing)
	}

	getResp, err := ts.Client().Get(ts.URL + "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts/stdout")
	if err != nil {
		t.Fatalf("GET content: %v", err)
	}
	defer getResp.Body.Close()
	content, _ := io.ReadAll(getResp.Body)
	if getResp.StatusCode != http.StatusOK || !bytes.Equal(content, body) {
		t.Fatalf("content status=%d body=%q, want 200 with the published bytes", getResp.StatusCode, content)
	}
	if ct := getResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want the recorded media type", ct)
	}
	// Stored-XSS locks (PR #190 review): publisher-controlled bytes must
	// never render as active content on the API origin.
	if cd := getResp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q, want attachment", cd)
	}
	if xcto := getResp.Header.Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if csp := getResp.Header.Get("Content-Security-Policy"); csp != "sandbox" {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", csp)
	}

	missing, err := ts.Client().Get(ts.URL + "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts/no-such-name")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing-name status = %d, want 404", missing.StatusCode)
	}
}

// TestArtifactWriteRouteResolvesRunnerAttempts pins the d1 production gap
// found live on the 2026-08-18 18:10Z sweep: a RUNNER attempt has no pending
// invocation, but its runner_operations row (written at dispatch) is just as
// durable — the PUT must resolve namespace/run from it instead of 404ing.
func TestArtifactWriteRouteResolvesRunnerAttempts(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, _ := newArtifactRouteTestServer(t, now)
	// A runner attempt: unknown to the invocation store, known to the
	// runner-op source the server falls back to.
	token, err := signer.Mint("att_runner")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	body := []byte(`{"sweep": "pr-upkeep", "emitted": 2}`)
	req := artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/att_runner/artifacts", token, "text/plain", "stdout", bytes.NewReader(body))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, got)
	}
	if store.meta.NamespaceID != "ns_runner" || store.meta.RunID != "run_runner" || store.meta.AttemptID != "att_runner" {
		t.Fatalf("associations = (%q, %q, %q), want the runner operation's (ns_runner, run_runner, att_runner)",
			store.meta.NamespaceID, store.meta.RunID, store.meta.AttemptID)
	}
}

// TestArtifactWriteRouteResolvesParkedRunnerAttempts pins the second half of
// the d1 lifecycle gap (18:36Z sweep): mid-execution, only the parked
// runner_invocations row exists — the audit table is written at completion.
func TestArtifactWriteRouteResolvesParkedRunnerAttempts(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	ts, signer, store, _ := newArtifactRouteTestServer(t, now)
	token, err := signer.Mint("att_runner_parked")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	req := artifactRequest(t, http.MethodPost, ts.URL+"/v1alpha1/attempts/att_runner_parked/artifacts", token, "text/plain", "stdout", bytes.NewReader([]byte(`{"emitted": 1}`)))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST artifact: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, got)
	}
	if store.meta.NamespaceID != "ns_runner" || store.meta.RunID != "run_runner" || store.meta.AttemptID != "att_runner_parked" {
		t.Fatalf("associations = (%q, %q, %q), want the parked invocation's", store.meta.NamespaceID, store.meta.RunID, store.meta.AttemptID)
	}
}
