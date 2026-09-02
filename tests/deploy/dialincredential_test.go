// Package deploytest -- this file is task t13's (upkeep-dispatch-stable-
// addresses-gate-as-node plan): DISTRIBUTING the dial-in credential task t5
// taught the control plane to issue, without acquiring the two hazards the
// existing secrets lane is already carrying.
//
// The three things these tests pin, one per acceptance criterion:
//
//  1. The dial-in credential is NOT in install-secrets.sh's rotation-guarded
//     block while issue #133 is open. It is not in install-secrets.sh at all:
//     prod.env is the CONTROL PLANE's environment file, and a dial-in
//     credential that landed there would be a second custody point for a
//     value that is supposed to have exactly one.
//
//  2. No probe or test path relays a live operator credential into a
//     throwaway file (issue #134). The mitigation is in the harness, not in
//     each brief: every deploy script that reads credentials out of its own
//     environment is executed with an environment built from scratch, and a
//     file-level guard test keeps it that way.
//
//  3. A rotation cannot leave one copy of the credential updated and another
//     stale. This is structural rather than procedural, and the structure is
//     the point of the whole task:
//
//     - the PLAINTEXT has exactly one custody point (the bridge's own
//     per-bridge file) and the DIGEST has exactly one (the control plane's
//     inbound_authentication row). Unlike every other bridge token in this
//     deployment, the control plane needs no copy of the plaintext: it
//     dials nothing and presents nothing, it only verifies.
//     - there is no lane that writes either copy alone. Minting always
//     replaces the verifier AND emits the plaintext, in one command, to one
//     destination. deploy/prod/issue-dialin-credential.sh is the only
//     writer of a *_DIAL_TOKEN anywhere in deploy/prod.
//     - the deliverer VERIFIES the pair end to end before it commits: it
//     recomputes the SHA-256 of the credential it received and refuses to
//     write unless it equals the digest the control plane stored.
//     - a delivery that fails is LOUD and leaves the previous credential
//     byte-intact, and the repair is to re-run the same command. #133's
//     complaint is that a loud documented failure became a silent
//     inconsistency; this lane keeps the failure loud.
//
// Behavioural, in prodenvmerge_test.go's shape: the real scripts run against
// a stub `ssh` that executes each remote command locally under a per-host
// HOME, and against a real HTTP server standing in for the control plane's
// issuance route. Asserting on script text would prove only that somebody
// wrote the words.
package deploytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// issueDialInPath locates deploy/prod/issue-dialin-credential.sh beside
// install-secrets.sh.
func issueDialInPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(installSecretsPath(t)), "issue-dialin-credential.sh")
}

// deployTestDir is this package's own directory, located the same way
// installSecretsPath locates the repo root.
func deployTestDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate tests/deploy")
	}
	return filepath.Dir(thisFile)
}

// --- the fake control plane ----------------------------------------------

// issuanceSecret is the bearer the fake control plane requires. In a real
// deployment it lives in thor's prod.env and is read there; these tests seed
// it into the fake thor's prod.env, so a script that used the OPERATOR's
// environment instead would present the wrong one and be refused.
const issuanceSecret = "fixture-issuance-bearer-secret"

// fakeIssuer stands in for POST /v1alpha1/inbound/credentials. It mints a
// value, keeps only the digest -- exactly what internal/actors.
// MintInboundCredential and migration 0031 do -- and records what it minted
// so a test can ask whether the two copies agree.
type fakeIssuer struct {
	*httptest.Server

	mu       sync.Mutex
	minted   []string          // plaintext, in issue order (the test's oracle)
	stored   map[string]string // party key -> digest hex: the control plane's ONLY copy
	revoked  map[string]bool
	bearers  []string // every Authorization header presented
	requests []string // every party key requested
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{stored: map[string]string{}, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1alpha1/inbound/credentials", func(w http.ResponseWriter, r *http.Request) {
		f.handle(w, r, false)
	})
	mux.HandleFunc("/v1alpha1/inbound/credentials/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.handle(w, r, true)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeIssuer) handle(w http.ResponseWriter, r *http.Request, revoke bool) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	var req struct {
		PartyKind string `json:"party_kind"`
		PartyKey  string `json:"party_key"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.bearers = append(f.bearers, r.Header.Get("Authorization"))
	f.requests = append(f.requests, req.PartyKey)
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+issuanceSecret {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if req.PartyKey == "" {
		http.Error(w, `{"error":"party_key required"}`, http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if revoke {
		f.revoked[req.PartyKey] = true
		delete(f.stored, req.PartyKey)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"party_kind":"actor","party_key":"` + req.PartyKey +
			`","revoked_at":"2026-08-16T00:00:00Z"}`))
		return
	}

	// A fresh value every mint, in the shipped prefix, with the digest the
	// control plane would keep. Distinguishable per issuance so a test can
	// say WHICH credential a file holds.
	plaintext := "cnd_fixture-credential-" + hex.EncodeToString([]byte{byte(len(f.minted) + 1)}) +
		"-abcdefghijklmnopqrstuvwxyz0123456789"
	sum := sha256.Sum256([]byte(plaintext))
	digest := hex.EncodeToString(sum[:])
	f.minted = append(f.minted, plaintext)
	f.stored[req.PartyKey] = digest

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"party_kind":"actor","party_key":"` + req.PartyKey +
		`","credential":"` + plaintext + `","digest_sha256":"` + digest +
		`","issued_at":"2026-08-16T00:00:00Z","revealed_once":true}`))
}

func (f *fakeIssuer) mintedAt(t *testing.T, index int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.minted) {
		t.Fatalf("the control plane minted %d credentials, wanted at least %d", len(f.minted), index+1)
	}
	return f.minted[index]
}

func (f *fakeIssuer) digestFor(t *testing.T, party string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	digest, ok := f.stored[party]
	if !ok {
		t.Fatalf("the control plane holds no verifier for %s", party)
	}
	return digest
}

func (f *fakeIssuer) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.minted)
}

// --- running the lane ------------------------------------------------------

// dialInCluster is a fake cluster with the control plane's issuance bearer
// already in thor's prod.env -- the state install-secrets.sh's issuance lane
// leaves a host in, and the state this script requires.
func dialInCluster(t *testing.T, issuer *fakeIssuer) *fakeCluster {
	t.Helper()
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", "POSTGRES_PASSWORD=fixture-postgres-password\n"+
		"NODES_INBOUND_ISSUANCE_TOKEN_SECRET="+issuanceSecret+"\n")
	// The bridge hosts exist as homes but hold nothing.
	c.hostHome(t, "spark")
	c.hostHome(t, "orin")
	_ = issuer
	return c
}

func runIssue(t *testing.T, c *fakeCluster, issuer *fakeIssuer, args []string, extraEnv ...string) (string, string, int) {
	t.Helper()
	env := append([]string{"NODES_API_URL=" + issuer.URL}, extraEnv...)
	return c.runSplit(t, issueDialInPath(t), args, env...)
}

// dialFilePath is where the lane puts one bridge's credential: a per-bridge,
// single-purpose, mode-0600 file under the bridge's own home.
func dialFilePath(t *testing.T, c *fakeCluster, host, slug string) string {
	t.Helper()
	return filepath.Join(c.hostHome(t, host), ".culture-nodes", "dialin", slug+".env")
}

// assertAbsentEverywhere walks every file the fake cluster owns -- both
// prod.envs, both hosts' homes, the operator's home -- and fails naming the
// file that holds the value. "one custody point" is a claim about the whole
// filesystem, not about the one file a test remembered to look at.
func assertAbsentEverywhere(t *testing.T, c *fakeCluster, what, value string, allow ...string) {
	t.Helper()
	base := filepath.Dir(c.root)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry cannot hold the value
		}
		for _, allowed := range allow {
			if path == allowed {
				return nil
			}
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr
		}
		if strings.Contains(string(body), value) {
			t.Errorf("%s reached %s — it must exist in exactly one place", what, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", base, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// --- criterion 1: the credential is not in the rotation-guarded block -----

// TestNoDialInCredentialIsWrittenIntoProdEnv is criterion 1 in its strongest
// available form. The brief forbids putting the new credential in
// install-secrets.sh's rotation-guarded block while #133 is open; what this
// asserts is more than that -- a full install-secrets.sh run puts no dial-in
// credential in prod.env at ALL, guarded block or otherwise.
//
// That is not pedantry. prod.env is an EnvironmentFile for notify-bridge.
// service, so a *_DIAL_TOKEN written there would actually be READ by a
// bridge -- and would be a second copy of a value whose whole design is to
// have one. #133 is precisely the failure mode of a value with two copies.
func TestNoDialInCredentialIsWrittenIntoProdEnv(t *testing.T) {
	c := newFakeCluster(t)
	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d; output:\n%s", code, out)
	}
	for _, host := range []string{"thor", "orin"} {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		for _, key := range env.order {
			if strings.HasSuffix(key, "_DIAL_TOKEN") {
				t.Errorf("%s carries %s — the dial-in credential's only custody point is the "+
					"bridge's own per-bridge file, and prod.env would be a second one", path, key)
			}
		}
	}
}

// TestOnlyTheIssuanceLaneWritesADialInCredential is the "no second writer"
// half of criterion 3, checked across the whole deploy directory rather than
// against the one script a test remembered. A value with one custody point
// needs one writer; two lanes able to write it is how #133 happened.
func TestOnlyTheIssuanceLaneWritesADialInCredential(t *testing.T) {
	dir := filepath.Dir(installSecretsPath(t))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			continue
		}
		if entry.Name() == "issue-dialin-credential.sh" {
			continue
		}
		// An ASSIGNMENT, not a mention: audit-credentials.sh matches the same
		// keys by glob in order to REPORT a second copy, which is the opposite
		// of writing one.
		body := readFileString(t, filepath.Join(dir, entry.Name()))
		if strings.Contains(body, "_DIAL_TOKEN=") {
			t.Errorf("%s assigns a _DIAL_TOKEN; issue-dialin-credential.sh is the single writer "+
				"of a dial-in credential (other scripts may only DETECT one, by pattern)",
				entry.Name())
		}
	}
}

// TestIssuanceSecretReachesAProvisionedHostWithoutRotatingAnything is why the
// issuance BEARER (not the credential) may live in prod.env at all, and why it
// is not in the guarded block: a key added to that block after a host was
// provisioned can only reach the host by rotating every secret beside it,
// which is issue #124's defect. This is the same add-if-absent shape the
// deployment-settings lane uses, and it is asserted the same way -- against a
// host that already has a prod.env.
func TestIssuanceSecretReachesAProvisionedHostWithoutRotatingAnything(t *testing.T) {
	c := newFakeCluster(t)
	const seeded = `POSTGRES_PASSWORD=old-generated-postgres-password
MINIO_ROOT_PASSWORD=old-generated-minio-password
NODES_HUMAN_DECISION_TOKEN_SECRET=old-generated-human-decision-secret
NODES_CALLBACK_TOKEN_SECRET=old-generated-callback-secret
NODES_ACTOR_CLAUDE_TOKEN=externally-issued-claude-bridge-token
`
	c.seedProdEnv(t, "thor", seeded)
	c.seedProdEnv(t, "orin", seeded)

	out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"})
	if code != 0 {
		t.Fatalf("install-secrets.sh exited %d; output:\n%s", code, out)
	}

	thor := readEnvFile(t, c.prodEnvPath(t, "thor"))
	secret, present := thor.values["NODES_INBOUND_ISSUANCE_TOKEN_SECRET"]
	if !present {
		t.Fatalf("thor's prod.env has no NODES_INBOUND_ISSUANCE_TOKEN_SECRET after a re-run on a "+
			"provisioned host; the issuance route stays 401 and no credential can be issued.\nfile:\n%s",
			thor.raw)
	}
	if !hexSecret.MatchString(secret) {
		t.Errorf("NODES_INBOUND_ISSUANCE_TOKEN_SECRET = %q, want a generated hex secret", secret)
	}
	if thor.values["POSTGRES_PASSWORD"] != "old-generated-postgres-password" {
		t.Errorf("delivering the issuance secret rotated POSTGRES_PASSWORD to %q; the lane is "+
			"add-if-absent and mints nothing else", thor.values["POSTGRES_PASSWORD"])
	}
	if thor.values["NODES_ACTOR_CLAUDE_TOKEN"] != "externally-issued-claude-bridge-token" {
		t.Error("delivering the issuance secret disturbed an accreted key")
	}

	// A second run keeps the value it found: an issuance bearer that rotated
	// on every deploy would invalidate nothing already issued but would make
	// the operator's own lane fail with no signal saying why.
	if out, code := c.run(t, installSecretsPath(t), []string{"thor", "orin"}); code != 0 {
		t.Fatalf("second install-secrets.sh run exited %d; output:\n%s", code, out)
	}
	again := readEnvFile(t, c.prodEnvPath(t, "thor"))
	if again.values["NODES_INBOUND_ISSUANCE_TOKEN_SECRET"] != secret {
		t.Error("a re-run rotated NODES_INBOUND_ISSUANCE_TOKEN_SECRET; the lane is add-if-absent")
	}
}

// --- criterion 3: one plaintext, one digest, replaced together ------------

// TestIssuanceDeliversTheOnlyCopyOfTheCredential is criterion 3's premise.
// After one issuance there is exactly ONE plaintext in the world -- the
// bridge's file -- and the control plane holds a digest of that exact value.
func TestIssuanceDeliversTheOnlyCopyOfTheCredential(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"})
	if code != 0 {
		t.Fatalf("issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	path := dialFilePath(t, c, "thor", "codex-thor")
	body := readFileString(t, path)
	credential := issuer.mintedAt(t, 0)

	if !strings.Contains(body, "CODEX_BRIDGE_DIAL_TOKEN="+credential) {
		t.Fatalf("%s does not carry the issued credential under the prefix the bridge reads;\nfile:\n%s", path, body)
	}
	if !strings.Contains(body, "CODEX_BRIDGE_ACTOR_KEY=company/codex-thor") {
		t.Errorf("%s carries no actor key; dialin.configured() requires all three settings together", path)
	}
	if !strings.Contains(body, "CODEX_BRIDGE_CONTROL_PLANE_URL=") {
		t.Errorf("%s carries no control plane URL; dialin.configured() requires all three settings together", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s is mode %v, want 0600", path, info.Mode().Perm())
	}

	issuer.mu.Lock()
	requested := append([]string(nil), issuer.requests...)
	issuer.mu.Unlock()
	if len(requested) != 1 || requested[0] != "company/codex-thor" {
		t.Errorf("the control plane was asked to issue for %v, want exactly [company/codex-thor] — "+
			"issuance is per party, and one command issues for one", requested)
	}

	// The digest the control plane kept is a digest of the value the bridge
	// holds. This is the pair being consistent, checked across both copies.
	sum := sha256.Sum256([]byte(credential))
	if got, want := issuer.digestFor(t, "company/codex-thor"), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("control plane verifier = %s, want %s", got, want)
	}

	// ...and nowhere else. Not in prod.env, not on the other hosts, not in
	// the operator's home, not in any file the run left behind.
	assertAbsentEverywhere(t, c, "the issued dial-in credential", credential, path)

	// The operator's own stdout does not disclose it either: the lane reports
	// the digest, which is what the database already holds.
	if strings.Contains(stdout+stderr, credential) {
		t.Error("the lane printed the credential; only the digest may be reported")
	}
	if !strings.Contains(stdout, hex.EncodeToString(sum[:])) {
		t.Errorf("the lane did not report the digest it delivered;\nstdout: %s", stdout)
	}
}

// TestReissuingReplacesBothCopiesTogether is criterion 3 itself. A rotation
// is a re-issue, and a re-issue cannot update one copy without the other,
// because it is ONE command that replaces the verifier and writes the only
// plaintext.
func TestReissuingReplacesBothCopiesTogether(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	if stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"}); code != 0 {
		t.Fatalf("first issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"}); code != 0 {
		t.Fatalf("re-issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	first := issuer.mintedAt(t, 0)
	second := issuer.mintedAt(t, 1)
	path := dialFilePath(t, c, "thor", "codex-thor")
	body := readFileString(t, path)

	if !strings.Contains(body, "CODEX_BRIDGE_DIAL_TOKEN="+second) {
		t.Fatalf("after a re-issue %s does not hold the NEW credential;\nfile:\n%s", path, body)
	}
	if strings.Contains(body, first) {
		t.Errorf("%s still holds the superseded credential; the file is replaced, not appended to", path)
	}
	assertAbsentEverywhere(t, c, "the superseded dial-in credential", first)

	sum := sha256.Sum256([]byte(second))
	if got, want := issuer.digestFor(t, "company/codex-thor"), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("after a re-issue the control plane verifier = %s, want the digest of the "+
			"credential the bridge now holds (%s)", got, want)
	}
}

// TestDeliveryFailureIsLoudAndLeavesTheBridgeCopyIntact is the other half of
// criterion 3, and the half #133 is actually about. Two machines cannot be
// written atomically, so what must be structural is that a half-completed
// rotation is IMPOSSIBLE TO MISS: the command fails non-zero, says which
// party's copies now disagree and which way round, and the previous
// credential is still byte-intact on the bridge because the write is a
// prepare-then-replace.
//
// #133's complaint is that a loud, documented failure became a silent
// inconsistency. This keeps it loud.
func TestDeliveryFailureIsLoudAndLeavesTheBridgeCopyIntact(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	if stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"}); code != 0 {
		t.Fatalf("first issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	path := dialFilePath(t, c, "thor", "codex-thor")
	before := readFileString(t, path)

	// Make the destination directory unwritable: the deliverer's prepare step
	// fails, so the replace never happens.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		// Running as a user that ignores the mode (root) would make this test
		// prove nothing; say so instead of passing vacuously.
		_ = f.Close()
		_ = os.Remove(f.Name())
		t.Skip("this user can write an unwritable directory (root?); the delivery-failure path cannot be provoked")
	}

	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"})
	if code == 0 {
		t.Fatalf("a failed delivery exited 0 — a rotation the bridge never received must fail loudly\nstdout: %s", stdout)
	}
	if issuer.mintCount() != 2 {
		t.Fatalf("the control plane minted %d times, want 2 (the failure must be on the delivery side)", issuer.mintCount())
	}
	for _, want := range []string{"company/codex-thor", "re-run"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the failure message does not mention %q; it must name the party whose copies "+
				"disagree and the repair.\nstderr: %s", want, stderr)
		}
	}
	// Compared, never printed: the contents are a live credential, so the
	// message names the file and not what it now holds.
	if readFileString(t, path) != before {
		t.Errorf("a failed delivery modified %s; the write prepares then replaces, so the previous "+
			"credential survives intact", path)
	}
	if strings.Contains(stdout+stderr, issuer.mintedAt(t, 1)) {
		t.Error("the failure path printed the undelivered credential")
	}
}

// TestDeliveryRefusesACredentialWhoseDigestDoesNotMatch pins the end-to-end
// verification: the deliverer recomputes the SHA-256 of what it received and
// refuses to write anything unless it equals the digest the control plane
// stored. A response that lost or altered the value in transit cannot install
// a credential the control plane will not accept.
func TestDeliveryRefusesACredentialWhoseDigestDoesNotMatch(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", "NODES_INBOUND_ISSUANCE_TOKEN_SECRET="+issuanceSecret+"\n")

	// A control plane that answers with a digest of something else.
	lying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"party_kind":"actor","party_key":"company/codex-thor",` +
			`"credential":"cnd_delivered-value","digest_sha256":"` + strings.Repeat("0", 64) +
			`","issued_at":"2026-08-16T00:00:00Z","revealed_once":true}`))
	}))
	t.Cleanup(lying.Close)

	stdout, stderr, code := c.runSplit(t, issueDialInPath(t), []string{"company/codex-thor"},
		"NODES_API_URL="+lying.URL)
	if code == 0 {
		t.Fatalf("a credential whose digest does not match its value was accepted\nstdout: %s", stdout)
	}
	if !strings.Contains(stderr, "digest") {
		t.Errorf("the refusal does not name the digest mismatch\nstderr: %s", stderr)
	}
	if _, err := os.Stat(dialFilePath(t, c, "thor", "codex-thor")); err == nil {
		t.Error("a credential that failed verification was written anyway")
	}
}

// TestUnknownActorIsRefusedByName keeps the lane from guessing. A party this
// deployment does not run a bridge for gets a named refusal and a hint, not a
// credential minted into a file nothing reads.
func TestUnknownActorIsRefusedByName(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/not-a-bridge"})
	if code != 1 {
		t.Fatalf("unknown actor exited %d, want 1 (a user error)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "company/not-a-bridge") {
		t.Errorf("the refusal does not name the actor\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "hint:") {
		t.Errorf("the refusal carries no hint\nstderr: %s", stderr)
	}
	if issuer.mintCount() != 0 {
		t.Error("an unknown actor still caused a credential to be minted")
	}
}

// TestIssuanceIsPerParty is what makes revocation and rotation scoped: two
// bridges get two different credentials in two different files, and neither
// file mentions the other's.
func TestIssuanceIsPerParty(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	for _, actor := range []string{"company/codex-thor", "company/developer"} {
		if stdout, stderr, code := runIssue(t, c, issuer, []string{actor}); code != 0 {
			t.Fatalf("issue %s exited %d\nstdout: %s\nstderr: %s", actor, code, stdout, stderr)
		}
	}

	codex := readFileString(t, dialFilePath(t, c, "thor", "codex-thor"))
	developer := readFileString(t, dialFilePath(t, c, "spark", "developer"))

	if !strings.Contains(codex, "CODEX_BRIDGE_DIAL_TOKEN="+issuer.mintedAt(t, 0)) {
		t.Error("codex-thor's file does not hold its own credential")
	}
	if !strings.Contains(developer, "CLAUDE_CODE_BRIDGE_DIAL_TOKEN="+issuer.mintedAt(t, 1)) {
		t.Errorf("developer's file does not hold its own credential under the claude prefix;\nfile:\n%s", developer)
	}
	if strings.Contains(developer, issuer.mintedAt(t, 0)) || strings.Contains(codex, issuer.mintedAt(t, 1)) {
		t.Error("two parties' credentials landed in one file; issuance is per party")
	}
	// Issue #147: spark's four claude bridges share one EnvironmentFile, so a
	// per-BACKEND destination would give all four the same identity. The
	// destination is per BRIDGE.
	planner := dialFilePath(t, c, "spark", "planner")
	if planner == dialFilePath(t, c, "spark", "developer") {
		t.Error("two spark bridges resolve to one destination file (issue #147)")
	}
}

// TestJSONDestinationIsSupported keeps the distribution path compatible with
// #147's likely answer -- the per-bridge JSON config, where every other
// per-bridge setting already lives. Which one a bridge READS from is t8's
// decision; this lane can write either, so that decision does not need this
// script rewritten.
func TestJSONDestinationIsSupported(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	configDir := filepath.Join(c.hostHome(t, "spark"), ".config", "culture-nodes-bridges")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", configDir, err)
	}
	configPath := filepath.Join(configDir, "developer.json")
	if err := os.WriteFile(configPath, []byte(`{"port": 8090, "actor_id": "company/developer"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", configPath, err)
	}

	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/developer"},
		"DIALIN_DESTINATION=json:.config/culture-nodes-bridges/developer.json")
	if code != 0 {
		t.Fatalf("issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	var config map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, configPath)), &config); err != nil {
		t.Fatalf("the config is no longer valid JSON: %v", err)
	}
	if config["port"] != float64(8090) {
		t.Errorf("the existing config lost a setting: %v", config)
	}
	if config["dial_token"] != issuer.mintedAt(t, 0) {
		t.Errorf("dial_token = %v, want the issued credential", config["dial_token"])
	}
	if config["dial_actor_key"] != "company/developer" {
		t.Errorf("dial_actor_key = %v", config["dial_actor_key"])
	}
	assertAbsentEverywhere(t, c, "the issued dial-in credential", issuer.mintedAt(t, 0), configPath)
}

// TestRevokeEndsAuthorityAndRemovesTheOnlyPlaintext keeps "one custody point"
// true through the end of a credential's life: a revoked credential is dead at
// the control plane AND gone from the bridge, so no dead plaintext is left
// lying in a file for someone to later mistake for a working one.
func TestRevokeEndsAuthorityAndRemovesTheOnlyPlaintext(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	if stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"}); code != 0 {
		t.Fatalf("issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	credential := issuer.mintedAt(t, 0)

	stdout, stderr, code := runIssue(t, c, issuer, []string{"--revoke", "company/codex-thor"})
	if code != 0 {
		t.Fatalf("revoke exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	issuer.mu.Lock()
	revoked := issuer.revoked["company/codex-thor"]
	issuer.mu.Unlock()
	if !revoked {
		t.Error("revoke did not reach the control plane")
	}
	assertAbsentEverywhere(t, c, "the revoked dial-in credential", credential)
}

// --- criterion 3, detection: a second copy is reported, not tolerated -----

// TestAuditReportsADialInCredentialInProdEnv is the detector half. The single
// inconsistency prod.env can express about a single-copy credential is HOLDING
// ONE AT ALL, and the audit -- already the repo's detector for "whatever eats
// a key next" -- names it and fails, rather than filing it under `unknown`.
func TestAuditReportsADialInCredentialInProdEnv(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvComplete+"CODEX_BRIDGE_DIAL_TOKEN=leaked-second-copy\n")

	out, code := runAudit(t, c, "thor")
	if code != 1 {
		t.Fatalf("audit exited %d, want 1 (a configuration error)\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "CODEX_BRIDGE_DIAL_TOKEN") {
		t.Errorf("the audit does not name the second copy\noutput:\n%s", out)
	}
	if strings.Contains(out, "leaked-second-copy") {
		t.Error("the audit printed the credential's VALUE; it reports key names")
	}
	if !strings.Contains(out, "issue-dialin-credential.sh") {
		t.Errorf("the audit does not point at the single writer\noutput:\n%s", out)
	}
}

// TestAuditPassesWithoutADialInCredential keeps the detector from becoming a
// blanket failure: a prod.env with no dial-in credential in it is the normal,
// correct state and must still pass.
func TestAuditPassesWithoutADialInCredential(t *testing.T) {
	c := newFakeCluster(t)
	c.seedProdEnv(t, "thor", auditProdEnvComplete)
	out, code := runAudit(t, c, "thor")
	if code != 0 {
		t.Fatalf("audit exited %d on a correct prod.env\noutput:\n%s", code, out)
	}
}

// --- criterion 2 and the secret's path ------------------------------------

// TestIssuanceSecretNeverLeavesTheControlPlaneHost. The bearer that authorises
// minting is read where it already lives -- thor's prod.env -- by a command
// running on thor. It is never relayed from the operator's environment (which
// is issue #134's mechanism), never written to a bridge host, and never in an
// argv.
func TestIssuanceSecretNeverLeavesTheControlPlaneHost(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)
	argvLog := logSSHArgv(t, c)

	// Poison the operator's environment with a DIFFERENT issuance secret. If
	// the lane relayed its own environment the fake control plane would see
	// this value and refuse; it must see the one on thor.
	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/codex-thor"},
		"NODES_INBOUND_ISSUANCE_TOKEN_SECRET=operator-environment-issuance-secret")
	if code != 0 {
		t.Fatalf("issue exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	issuer.mu.Lock()
	bearers := append([]string(nil), issuer.bearers...)
	issuer.mu.Unlock()
	for _, bearer := range bearers {
		if bearer != "Bearer "+issuanceSecret {
			t.Errorf("the control plane was presented %q; the bearer is read from the control "+
				"plane host's own prod.env, never relayed from the operator's environment", bearer)
		}
	}

	argv := readFileString(t, argvLog)
	if strings.Contains(argv, issuanceSecret) {
		t.Error("the issuance bearer reached an ssh argv")
	}
	if strings.Contains(argv, issuer.mintedAt(t, 0)) {
		t.Error("the issued credential reached an ssh argv")
	}
	// The bearer stays on the control plane host: it must not appear in the
	// bridge host's files.
	sparkHome := c.hostHome(t, "spark")
	err := filepath.WalkDir(sparkHome, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr
		}
		if strings.Contains(readFileString(t, path), issuanceSecret) {
			t.Errorf("the issuance bearer reached %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", sparkHome, err)
	}
}

// --- the LAN break-glass (login-from-anywhere task t22, spec c48) ---------

// TestBreakGlassCredentialForAHumanActorUsesTheSameLane is the deploy half of
// t22. c48 requires an operator on the LAN to keep a break-glass path — "an
// issued service credential bound to the operator's actor" — so a
// misconfigured Access policy cannot lock every human out.
//
// The point of this test is that closing that gap needs NO new issuance
// surface. A dial-in credential is issued for a PARTY, and a party is any
// key shaped like an actor key (internal/actors.ValidateInboundParty); a
// person's registered human actor is one. So the break-glass credential is
// minted by the lane that already exists, through the three one-off
// destination overrides that already exist, with the same single-custody
// guarantees the rest of this file pins — and nothing is added to
// dialin_bridges(), because a person is not a bridge this deployment runs.
//
// What made the credential USEFUL is the control-plane half
// (internal/api/breakglass.go); this pins that the operator recipe in
// docs/operations/people.md is a command that works.
func TestBreakGlassCredentialForAHumanActorUsesTheSameLane(t *testing.T) {
	issuer := newFakeIssuer(t)
	c := dialInCluster(t, issuer)

	stdout, stderr, code := runIssue(t, c, issuer, []string{"company/operator"},
		"DIALIN_PREFIX=BREAK_GLASS",
		"DIALIN_HOST=thor",
		"DIALIN_DESTINATION=env:.culture-nodes/dialin/break-glass.env")
	if code != 0 {
		t.Fatalf("issue for a human actor exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	path := dialFilePath(t, c, "thor", "break-glass")
	body := readFileString(t, path)
	credential := issuer.mintedAt(t, 0)
	if !strings.Contains(body, "BREAK_GLASS_DIAL_TOKEN="+credential) {
		t.Fatalf("%s does not carry the issued credential;\nfile:\n%s", path, body)
	}
	if !strings.Contains(body, "BREAK_GLASS_ACTOR_KEY=company/operator") {
		t.Errorf("%s does not name the human actor the credential is bound to;\nfile:\n%s", path, body)
	}

	issuer.mu.Lock()
	requested := append([]string(nil), issuer.requests...)
	issuer.mu.Unlock()
	if len(requested) != 1 || requested[0] != "company/operator" {
		t.Errorf("the control plane was asked to issue for %v, want exactly [company/operator]", requested)
	}
	sum := sha256.Sum256([]byte(credential))
	if got, want := issuer.digestFor(t, "company/operator"), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("control plane verifier = %s, want %s", got, want)
	}

	// Single custody holds for a person's credential exactly as it does for a
	// bridge's: one plaintext, in the operator's own mode-0600 file, and
	// nowhere else — in particular not in prod.env, which is what
	// audit-credentials.sh fails on.
	assertAbsentEverywhere(t, c, "the break-glass credential", credential, path)
	if strings.Contains(stdout+stderr, credential) {
		t.Error("the lane printed the break-glass credential; only the digest may be reported")
	}

	// And it is retired the same way, by the same command.
	if stdout, stderr, code := runIssue(t, c, issuer, []string{"--revoke", "company/operator"},
		"DIALIN_PREFIX=BREAK_GLASS",
		"DIALIN_HOST=thor",
		"DIALIN_DESTINATION=env:.culture-nodes/dialin/break-glass.env"); code != 0 {
		t.Fatalf("revoke exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	issuer.mu.Lock()
	revoked := issuer.revoked["company/operator"]
	issuer.mu.Unlock()
	if !revoked {
		t.Error("revoking the break-glass credential did not reach the control plane")
	}
	assertAbsentEverywhere(t, c, "the revoked break-glass credential", credential)
}
