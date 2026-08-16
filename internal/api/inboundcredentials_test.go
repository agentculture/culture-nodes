package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

const fixtureIssuanceSecret = "inbound-issuance-secret-long-enough"

// issuanceFixture is a live control plane with the dial-in credential
// issuance lane configured and its log sink captured, so a test can assert
// both what came back over HTTP and what was written to the logs.
type issuanceFixture struct {
	t     *testing.T
	url   func(string) string
	store *storepg.Store
	logs  *bytes.Buffer
	nsID  string
}

func newIssuanceFixture(t *testing.T) *issuanceFixture {
	t.Helper()
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "inbound-issuance").ID
	logs := &bytes.Buffer{}
	srv, err := api.NewServer(s, nsID,
		api.WithInboundIssuanceSecret(fixtureIssuanceSecret),
		api.WithLogger(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &issuanceFixture{
		t:     t,
		url:   func(path string) string { return ts.URL + path },
		store: s,
		logs:  logs,
		nsID:  nsID,
	}
}

func (f *issuanceFixture) post(path, bearer, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url(path), strings.NewReader(body))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// issue mints a credential for partyKey through the API and returns the
// decoded response body.
func (f *issuanceFixture) issue(partyKey string) map[string]any {
	f.t.Helper()
	resp := f.post("/v1alpha1/inbound/credentials", fixtureIssuanceSecret,
		`{"party_kind":"actor","party_key":"`+partyKey+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("issue %s: status = %d, want 201", partyKey, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.t.Fatalf("decode issuance response: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.store.Pool().Exec(context.Background(),
			`DELETE FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, partyKey)
	})
	return out
}

// dialAdmitted probes the admission decision for one bridge without waiting
// out a long poll: the completion route authenticates first and then refuses
// the deliberately empty body with 400, so 401 means "this bridge cannot
// dial in" and anything else means it can.
func (f *issuanceFixture) dialAdmitted(actorKey, credential string) bool {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url("/v1alpha1/inbound/probe-id/complete"), strings.NewReader(`{}`))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-Culture-Nodes-Actor-Key", actorKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("dial probe: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusUnauthorized
}

// TestIssueInboundCredentialRevealsOnceAndNeverStoresOrLogsIt covers
// acceptance criteria 1 and 2 end to end: the control plane chooses the
// value, hands it back exactly once, admits the bridge that presents it, and
// leaves neither the database nor the log sink holding it.
func TestIssueInboundCredentialRevealsOnceAndNeverStoresOrLogsIt(t *testing.T) {
	f := newIssuanceFixture(t)
	key := "test/issued-" + store.NewULID()

	issued := f.issue(key)
	credential, _ := issued["credential"].(string)
	if credential == "" {
		t.Fatal("issuance returned no credential")
	}
	digest := sha256.Sum256([]byte(credential))
	if got, _ := issued["digest_sha256"].(string); got != hex.EncodeToString(digest[:]) {
		t.Errorf("digest_sha256 = %q, want the SHA-256 of the issued credential", got)
	}
	if got, _ := issued["party_key"].(string); got != key {
		t.Errorf("party_key = %q, want %q", got, key)
	}
	if issued["issued_at"] == nil {
		t.Error("issuance reported no issued_at")
	}

	if !f.dialAdmitted(key, credential) {
		t.Fatal("the issued credential did not admit its own bridge")
	}

	// Re-reading the credential is impossible by construction: issuance is
	// the only surface that ever holds it, and issuing again mints a new one
	// rather than re-revealing the old.
	second, _ := f.issue(key)["credential"].(string)
	if second == credential {
		t.Fatal("a second issuance re-revealed the first credential")
	}
	if f.dialAdmitted(key, credential) {
		t.Error("the superseded credential still admitted a dial")
	}
	if !f.dialAdmitted(key, second) {
		t.Error("the reissued credential did not admit a dial")
	}

	var table string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT coalesce(string_agg(to_jsonb(t)::text, ' '), '') FROM inbound_authentication t`).Scan(&table); err != nil {
		t.Fatalf("dump inbound_authentication: %v", err)
	}
	for _, secret := range []string{credential, second} {
		if strings.Contains(table, secret) {
			t.Error("an issued credential reached the database in presentable form")
		}
		if strings.Contains(f.logs.String(), secret) {
			t.Error("an issued credential reached the log sink")
		}
	}
}

// TestIssueInboundCredentialRefusesAnOperatorSuppliedValue is acceptance
// criterion 1's refusal: a caller cannot register a credential it invented,
// under any of the names it might reach for.
func TestIssueInboundCredentialRefusesAnOperatorSuppliedValue(t *testing.T) {
	f := newIssuanceFixture(t)
	key := "test/invented-" + store.NewULID()

	for _, body := range []string{
		`{"party_kind":"actor","party_key":"` + key + `","credential":"i-invented-this"}`,
		`{"party_kind":"actor","party_key":"` + key + `","token":"i-invented-this"}`,
		`{"party_kind":"actor","party_key":"` + key + `","secret":"i-invented-this"}`,
		`{"party_kind":"actor","party_key":"` + key + `","verifier_sha256":"0f0f"}`,
		`{"party_kind":"actor","party_key":"` + key + `","verifier_env_name":"BRIDGE_DIAL_TOKEN"}`,
		`{"party_kind":"actor","party_key":"` + key + `","surprise":"value"}`,
	} {
		resp := f.post("/v1alpha1/inbound/credentials", fixtureIssuanceSecret, body)
		payload, _ := json.Marshal(json.RawMessage(body))
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("%s: status = %d, want 400", payload, resp.StatusCode)
		}
		resp.Body.Close()
	}

	var rows int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM inbound_authentication WHERE party_kind='actor' AND party_key=$1`, key).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a refused issuance still wrote %d row(s)", rows)
	}

	bad := f.post("/v1alpha1/inbound/credentials", fixtureIssuanceSecret, `{"party_kind":"actor","party_key":"192.168.1.157"}`)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("address-shaped party key: status = %d, want 400", bad.StatusCode)
	}
}

// TestInboundCredentialRoutesRefuseUnauthenticated keeps the issuance lane
// on the same closed-by-default posture every other mutating surface holds:
// no bearer, or the wrong one, mints and revokes nothing.
func TestInboundCredentialRoutesRefuseUnauthenticated(t *testing.T) {
	f := newIssuanceFixture(t)
	body := `{"party_kind":"actor","party_key":"test/unauth-` + store.NewULID() + `"}`
	for _, path := range []string{"/v1alpha1/inbound/credentials", "/v1alpha1/inbound/credentials/revoke"} {
		for name, bearer := range map[string]string{"no bearer": "", "wrong bearer": "not-the-secret-but-long-enough"} {
			resp := f.post(path, bearer, body)
			status := resp.StatusCode
			resp.Body.Close()
			if status != http.StatusUnauthorized {
				t.Errorf("%s with %s: status = %d, want 401", path, name, status)
			}
		}
	}

	unconfigured := newFixture(t)
	resp, err := http.Post(unconfigured.url("/v1alpha1/inbound/credentials"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST unconfigured issuance: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("issuance on a server with no issuance secret: status = %d, want 401", resp.StatusCode)
	}
}

// TestRevokeInboundCredentialUnDialsOneBridgeOnly is acceptance criterion 3
// over HTTP: one revocation takes effect on the next dial of that bridge
// alone, against the same running control plane, with nothing restarted.
func TestRevokeInboundCredentialUnDialsOneBridgeOnly(t *testing.T) {
	f := newIssuanceFixture(t)
	revokedKey := "test/revoked-" + store.NewULID()
	survivingKey := "test/surviving-" + store.NewULID()
	revoked, _ := f.issue(revokedKey)["credential"].(string)
	surviving, _ := f.issue(survivingKey)["credential"].(string)

	if !f.dialAdmitted(revokedKey, revoked) || !f.dialAdmitted(survivingKey, surviving) {
		t.Fatal("a freshly issued credential was refused before any revocation")
	}

	resp := f.post("/v1alpha1/inbound/credentials/revoke", fixtureIssuanceSecret,
		`{"party_kind":"actor","party_key":"`+revokedKey+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode revocation response: %v", err)
	}
	if out["revoked_at"] == nil {
		t.Error("revocation reported no revoked_at")
	}

	if f.dialAdmitted(revokedKey, revoked) {
		t.Error("the revoked bridge still dialled in")
	}
	if !f.dialAdmitted(survivingKey, surviving) {
		t.Error("revoking one bridge un-dialled another")
	}

	missing := f.post("/v1alpha1/inbound/credentials/revoke", fixtureIssuanceSecret,
		`{"party_kind":"actor","party_key":"test/never-issued-`+store.NewULID()+`"}`)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("revoking an unknown party: status = %d, want 404", missing.StatusCode)
	}
}
