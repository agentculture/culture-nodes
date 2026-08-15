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

type memoryArtifactStore struct {
	meta artifacts.ArtifactMeta
	body []byte
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
	return artifacts.NewRef(meta.NamespaceID, "01K2TESTARTIFACT0000000000"), nil
}

func (s *memoryArtifactStore) Stat(_ context.Context, _ artifacts.Ref) (artifacts.ArtifactMeta, error) {
	return s.meta, nil
}

func (*memoryArtifactStore) Get(context.Context, artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	panic("GET must not be exposed by the write route")
}

func (*memoryArtifactStore) Delete(context.Context, artifacts.Ref) error {
	panic("Delete must not be exposed by the write route")
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
		{"no_unauthenticated_get", http.MethodGet, "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts", "", http.StatusMethodNotAllowed},
		{"no_delete", http.MethodDelete, "/v1alpha1/attempts/" + inv.AttemptID + "/artifacts/artifact-id", token, http.StatusNotFound},
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
