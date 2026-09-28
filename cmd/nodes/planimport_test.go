package main

// Tests for the `nodes plan-import` verb (task t22, issue #45). Like
// run_test.go, the flag/help scenarios run everywhere; the end-to-end
// scenarios exec the built binary against a real API server over the
// pgtest-provided PostgreSQL and skip when none is available.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// devagueTestdataPath resolves an absolute path into
// internal/devague/testdata — the same real `devague` fixtures
// internal/devague's own tests exercise MapPlanShow/MapDeviations against
// (see internal/devague/testdata/README.md). Absolute, because runNodes
// execs the built binary with cmd.Dir set to a fresh t.TempDir(), so a
// relative path would resolve against the wrong directory.
func devagueTestdataPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "internal", "devague", "testdata", name))
	if err != nil {
		t.Fatalf("resolve testdata path for %s: %v", name, err)
	}
	return abs
}

func TestPlanImportHelpDocumentsTheVerb(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import", "--help")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 for --help\nstderr=%s", r.ExitCode, r.Stderr)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty (--help output is a result)", r.Stderr)
	}
	for _, want := range []string{"--plan", "--deviations", "devague"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("--help output does not mention %q:\n%s", want, r.Stdout)
		}
	}
}

func TestPlanImportMissingPlanFlagIsUserError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
	if !strings.Contains(r.Stderr, "--plan") {
		t.Fatalf("stderr = %q, want it to point at the missing --plan flag", r.Stderr)
	}
}

func TestPlanImportUnreadablePlanFileIsEnvError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import", "--plan", filepath.Join(dir, "does-not-exist.json"))

	assertNeverMixed(t, r)
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (unreadable file)\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
}

// TestPlanImportEndToEndAgainstTestServer is the t22 acceptance test,
// exercised through the CLI verb end to end: real per-task status and real
// dependency edges round-trip, deviations carry their origin, and the
// created import is readable back from the ordinary plan-imports API.
func TestPlanImportEndToEndAgainstTestServer(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	r := runNodes(t, dir, "plan-import",
		"--api", ts.URL,
		"--plan", devagueTestdataPath(t, "plan-show.json"),
		"--deviations", devagueTestdataPath(t, "deviations.json"),
		"--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr=%s", r.ExitCode, r.Stderr)
	}
	var payload planImportResultPayload
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.ID == "" {
		t.Fatalf("id missing from payload: %s", r.Stdout)
	}
	if payload.Slug != "t22fixture" || payload.SourceSlug != "t22fixture" {
		t.Fatalf("payload = %+v, want slug/source_slug t22fixture", payload)
	}
	if payload.TaskCount != 5 {
		t.Fatalf("task_count = %d, want 5", payload.TaskCount)
	}
	if payload.DeviationCount != 3 {
		t.Fatalf("deviation_count = %d, want 3", payload.DeviationCount)
	}

	// The created import is a normal plan import readable from the
	// ordinary API.
	resp, err := http.Get(ts.URL + "/v1alpha1/plan-imports/" + payload.ID)
	if err != nil {
		t.Fatalf("GET plan import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET plan import: status = %d, want 200", resp.StatusCode)
	}
}

// TestPlanImportMalformedPlanIsRefusedWithAHint is the malformed-input half
// of the t22 acceptance, exercised through the CLI: refused with a hint on
// stderr, exit 1, never a panic.
func TestPlanImportMalformedPlanIsRefusedWithAHint(t *testing.T) {
	ts := runAPIServer(t)
	dir := t.TempDir()

	malformed := filepath.Join(dir, "malformed-plan.json")
	malformedJSON := `{"slug": "p", "tasks": [
		{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed", "deps": ["t99"]}
	]}`
	if err := os.WriteFile(malformed, []byte(malformedJSON), 0o600); err != nil {
		t.Fatalf("write malformed plan fixture: %v", err)
	}

	r := runNodes(t, dir, "plan-import", "--api", ts.URL, "--plan", malformed)

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (the control plane refused a malformed plan)\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
}

// runDeclarationAPIServer boots a real API server whose Access listener
// verifies a human principal -- the declaration lane's closed-by-default
// posture needs one -- behind a stand-in for the Cloudflare edge that turns
// the CF_Authorization cookie the CLI sends into the Cf-Access-Jwt-Assertion
// header the control plane reads (what the real edge does in production).
func runDeclarationAPIServer(t *testing.T) (url, cookie, actorID string) {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "cli-plan-import-decl")
	actorID = idstore.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol) VALUES ($1,$2,$3,1,'human','http')`,
		actorID, ns.ID, "cli-plan-import-human-"+actorID); err != nil {
		t.Fatal(err)
	}
	cookie = "cli-plan-import-human"
	if _, err := s.BindIdentity(context.Background(), ns.ID, "cloudflare-access", cookie, actorID, []string{string(auth.RoleNamespaceAdministrator)}); err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, ns.ID, api.WithPrincipalVerifier(accessVerifier(func(_ context.Context, tok string) (auth.Principal, error) {
		return auth.Principal{Subject: tok, Email: tok + "@example.test", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	access := srv.AccessHandler()
	edge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("CF_Authorization"); err == nil {
			r.Header.Set("Cf-Access-Jwt-Assertion", c.Value)
		}
		access.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(edge)
	t.Cleanup(ts.Close)
	return ts.URL, cookie, actorID
}

type accessVerifier func(context.Context, string) (auth.Principal, error)

func (f accessVerifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	return f(ctx, token)
}

// TestPlanImportDeclarationsEndToEndAgainstTestServer is the declaration
// counterpart of TestPlanImportEndToEndAgainstTestServer (task t35, spec
// c86/h59): the same real devague fixture, imported with
// --output declarations, still imports the snapshot and also publishes one
// inactive declaration per active task with a must link per real
// dependency edge, authored by the authenticated principal.
func TestPlanImportDeclarationsEndToEndAgainstTestServer(t *testing.T) {
	url, cookie, actorID := runDeclarationAPIServer(t)
	t.Setenv("NODES_OP_COOKIE", cookie)
	dir := t.TempDir()

	r := runNodes(t, dir, "plan-import",
		"--api", url,
		"--plan", devagueTestdataPath(t, "plan-show.json"),
		"--deviations", devagueTestdataPath(t, "deviations.json"),
		"--output", "declarations",
		"--actor-ref", "actor://company/developer",
		"--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr=%s", r.ExitCode, r.Stderr)
	}
	var payload planImportResultPayload
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.ID == "" || payload.TaskCount != 5 || payload.DeviationCount != 3 {
		t.Fatalf("snapshot not imported alongside: %+v", payload)
	}
	d := payload.Declarations
	if payload.Output != "declarations" || d == nil || d.Count != 4 || d.Links != 3 || !d.Published {
		t.Fatalf("declarations = %+v, want 4 published declarations and 3 links", d)
	}
	if !strings.Contains(r.Stdout, `"warnings":[]`) {
		t.Fatalf("warnings must be surfaced as an array even when empty: %s", r.Stdout)
	}

	resp, err := http.Get(url + "/v1alpha1/declarations/plan-t22fixture-t3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var shown struct {
		Author string `json:"author"`
		Active bool   `json:"active"`
		Links  []struct {
			To, Kind string
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&shown); err != nil {
		t.Fatal(err)
	}
	if shown.Author != actorID || shown.Active || len(shown.Links) != 1 || shown.Links[0].To != "plan-t22fixture-t1" || shown.Links[0].Kind != "must" {
		t.Fatalf("t3 = %+v, want author %s, inactive, one must link to t1", shown, actorID)
	}

	// Text mode names the declarations and the fact that nothing is active.
	r = runNodes(t, dir, "plan-import", "--api", url,
		"--plan", devagueTestdataPath(t, "plan-show.json"),
		"--output", "declarations", "--actor-ref", "actor://company/developer")
	assertNeverMixed(t, r)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "declarations: 4 published (inactive") {
		t.Fatalf("text output = %q (exit %d, stderr %q)", r.Stdout, r.ExitCode, r.Stderr)
	}
}

// Declaration output without --actor-ref is a user error before any
// request; without a credential the control plane refuses it (401 -> exit 1).
func TestPlanImportDeclarationsRefusals(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "plan-import", "--plan", devagueTestdataPath(t, "plan-show.json"), "--output", "declarations")
	assertNeverMixed(t, r)
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "--actor-ref") {
		t.Fatalf("missing --actor-ref: exit %d stderr %q", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)

	r = runNodes(t, dir, "plan-import", "--plan", devagueTestdataPath(t, "plan-show.json"), "--output", "graph")
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "--output") {
		t.Fatalf("unknown --output: exit %d stderr %q", r.ExitCode, r.Stderr)
	}

	url, _, _ := runDeclarationAPIServer(t)
	t.Setenv("NODES_OP_COOKIE", "")
	r = runNodes(t, dir, "plan-import", "--api", url, "--plan", devagueTestdataPath(t, "plan-show.json"),
		"--output", "declarations", "--actor-ref", "actor://company/developer")
	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("unauthenticated declaration import: exit %d, want 1\nstderr=%s", r.ExitCode, r.Stderr)
	}
	assertErrorHintShape(t, r.Stderr)
}
