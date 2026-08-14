// Package deploytest (see compose_test.go's doc comment). This file is task
// t10's, and unlike its neighbours it is BEHAVIORAL, not a text assertion:
// it sources deploy/prod/actor-placement.sh and runs its functions for real.
//
// That library answers one question — WHICH HOST serves an actor — and
// refuses a deployment that would answer it two different ways.
//
// The defect it exists for (issue #72): company/human-ops was registered at
// one machine's address while deploy/prod/deploy.sh and install-secrets.sh
// both declared the bridge and tracker "thor only". The engine therefore
// parked human tasks on the bridge at the registered endpoint while thor's
// tracker watched thor's own empty state directory and logged pending=0
// forever. Two config values that had to agree were agreeing only by luck.
//
// Task t8 (commit 724b3ad) made the tracker REFUSE TO START when its bridge
// does not serve the actor it watches. That is the runtime half. This
// library is the deploy half: it derives the host from the registration
// instead of declaring one, and fails the deploy loudly if the pair would be
// installed anywhere else. Neither half is sufficient alone — a wrong deploy
// that never starts is still a wrong deploy.
package deploytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// placementLibPath locates deploy/prod/actor-placement.sh via the shared
// codexBridgeDir helper (codexbridgeunit_test.go), so these tests stay
// independent of the directory `go test` runs from.
func placementLibPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(codexBridgeDir(t), "actor-placement.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("deploy/prod/actor-placement.sh is missing: %v", err)
	}
	return path
}

// requirePlacementTools skips loudly rather than passing quietly when the
// three things the library actually shells out to are absent. A green run on
// a machine with no python3 would prove nothing about the parsing below.
func requirePlacementTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "python3", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH; actor-placement.sh cannot be exercised here", bin)
		}
	}
}

// runPlacement sources the library and runs one snippet against it,
// returning combined output and exit code. `set -u` is on (the library must
// not depend on unset variables) but `set -e` deliberately is not: several
// of these functions report by exit status, and the snippets check it.
func runPlacement(t *testing.T, env map[string]string, snippet string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "case.sh")
	body := "set -u\n. " + placementLibPath(t) + "\n" + snippet + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the case script: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running the case script: %v (output: %s)", err, out)
	}
	return string(out), code
}

// --- actor_registration: the registration is the single source of truth ---

// fakeActorRegistry serves a GET /v1alpha1/actors payload shaped like the
// real one (internal/api/actors.go's ActorOut), so these tests exercise the
// same read the tracker's own startup check makes.
func fakeActorRegistry(t *testing.T, items []map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatalf("encoding the fake registry payload: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1alpha1/actors" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// humanOpsRegistry is the shape that matters: identity is append-only, so an
// endpoint move appends a NEW REVISION rather than updating the old row. The
// rows are deliberately ordered so that neither "first match" nor "last
// match" would produce the right answer — only "highest revision" does.
func humanOpsRegistry(t *testing.T) string {
	t.Helper()
	return fakeActorRegistry(t, []map[string]any{
		{
			"id":           "row-notify",
			"actor_key":    "company/notify-discord",
			"revision":     7,
			"endpoint_ref": "http://192.0.2.21:8088",
			"metadata":     map[string]any{"auth_token_env": "NODES_ACTOR_NOTIFY_TOKEN"},
		},
		{
			"id":           "row-human-new",
			"actor_key":    "company/human-ops",
			"revision":     2,
			"endpoint_ref": "http://192.0.2.20:8090",
			"metadata":     map[string]any{"auth_token_env": "NODES_ACTOR_HUMAN_TOKEN"},
		},
		{
			"id":           "row-human-old",
			"actor_key":    "company/human-ops",
			"revision":     1,
			"endpoint_ref": "http://198.51.100.7:8087",
			"metadata":     map[string]any{"auth_token_env": "OLD_TOKEN_ENV"},
		},
	})
}

func TestActorRegistrationResolvesTheNewestRevision(t *testing.T) {
	requirePlacementTools(t)

	out, code := runPlacement(t,
		map[string]string{"NODES_API_URL": humanOpsRegistry(t)},
		`actor_registration company/human-ops`)
	if code != 0 {
		t.Fatalf("actor_registration exited %d for a registered actor; output: %s", code, out)
	}

	got := strings.TrimSpace(out)
	want := "row-human-new|2|http://192.0.2.20:8090|NODES_ACTOR_HUMAN_TOKEN"
	if got != want {
		t.Errorf("actor_registration = %q, want %q (id|revision|endpoint_ref|auth_token_env of the newest revision)", got, want)
	}
}

// TestActorRegistrationFailsForAnUnregisteredActor pins the deploy-side
// posture: an actor with no registration produces a failure, never a guess.
// Nothing installed is always safer than a pair installed on a host chosen
// by default.
func TestActorRegistrationFailsForAnUnregisteredActor(t *testing.T) {
	requirePlacementTools(t)

	out, code := runPlacement(t,
		map[string]string{"NODES_API_URL": humanOpsRegistry(t)},
		`actor_registration company/nobody && echo UNEXPECTED_SUCCESS`)
	if code == 0 || strings.Contains(out, "UNEXPECTED_SUCCESS") {
		t.Errorf("actor_registration succeeded for an unregistered actor_key; output: %s", out)
	}
}

func TestActorRegistrationFailsWhenTheRegistryIsUnreachable(t *testing.T) {
	requirePlacementTools(t)

	out, code := runPlacement(t, map[string]string{
		"NODES_API_URL":             "http://127.0.0.1:1",
		"NODES_API_TIMEOUT_SECONDS": "2",
	}, `actor_registration company/human-ops && echo UNEXPECTED_SUCCESS`)
	if code == 0 || strings.Contains(out, "UNEXPECTED_SUCCESS") {
		t.Errorf("actor_registration succeeded against an unreachable registry; output: %s", out)
	}
}

// --- endpoint parsing ---------------------------------------------------

func TestEndpointAddressAndPort(t *testing.T) {
	requirePlacementTools(t)

	for _, tc := range []struct{ endpoint, address, port string }{
		{"http://192.0.2.20:8090", "192.0.2.20", "8090"},
		{"https://198.51.100.7:8087/", "198.51.100.7", "8087"},
		{"http://127.0.0.1:8087/submit", "127.0.0.1", "8087"},
	} {
		out, code := runPlacement(t, nil,
			fmt.Sprintf("endpoint_address %q; printf ' '; endpoint_port %q", tc.endpoint, tc.endpoint))
		if code != 0 {
			t.Errorf("parsing %q exited %d: %s", tc.endpoint, code, out)
			continue
		}
		want := tc.address + " " + tc.port
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%q parsed to %q, want %q", tc.endpoint, got, want)
		}
	}

	// A portless endpoint is not usable for this pairing: the bridge port is
	// derived from it, so silently defaulting one is exactly the invented
	// second config value this library exists to remove.
	out, code := runPlacement(t, nil, `endpoint_port http://192.0.2.20 && echo UNEXPECTED_SUCCESS`)
	if code == 0 || strings.Contains(out, "UNEXPECTED_SUCCESS") {
		t.Errorf("endpoint_port invented a port for a portless endpoint; output: %s", out)
	}
}

// --- assert_human_inbox_colocated: the deploy-time refusal ---------------

// placementEnvFiles writes the two env files the assertion reads back off
// the target host, into a temporary HOME. The assertion inspects what was
// ACTUALLY written rather than what the caller meant to write, which is why
// these tests can drive it with files alone.
type placementEnvFiles struct {
	bridgePort          string
	bridgeStateDir      string
	bridgeActorID       string
	trackerBridgeURL    string
	trackerStateDir     string
	trackerActorID      string
	trackerControlPlane string
}

func matchedEnvFiles() placementEnvFiles {
	return placementEnvFiles{
		bridgePort:          "8087",
		bridgeStateDir:      "/home/nodes/.culture-nodes/human-inbox-state",
		bridgeActorID:       "row-human-new",
		trackerBridgeURL:    "http://127.0.0.1:8087",
		trackerStateDir:     "/home/nodes/.culture-nodes/human-inbox-state",
		trackerActorID:      "company/human-ops",
		trackerControlPlane: "http://192.0.2.1:18080",
	}
}

func writePlacementEnv(t *testing.T, files placementEnvFiles) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".culture-nodes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the fake ~/.culture-nodes: %v", err)
	}
	bridge := fmt.Sprintf("HUMAN_INBOX_BRIDGE_HOST=0.0.0.0\nHUMAN_INBOX_BRIDGE_PORT=%s\nHUMAN_INBOX_BRIDGE_STATE_DIR=%s\nHUMAN_INBOX_BRIDGE_ACTOR_ID='%s'\n",
		files.bridgePort, files.bridgeStateDir, files.bridgeActorID)
	tracker := fmt.Sprintf("HUMAN_INBOX_TRACKER_STATE_DIR=%s\nHUMAN_INBOX_TRACKER_BRIDGE_URL=%s\nHUMAN_INBOX_BRIDGE_STATE_DIR=%s\nHUMAN_INBOX_BRIDGE_ACTOR_ID='%s'\nHUMAN_INBOX_TRACKER_CONTROL_PLANE_URL=%s\n",
		files.trackerStateDir, files.trackerBridgeURL, files.trackerStateDir, files.trackerActorID, files.trackerControlPlane)

	if err := os.WriteFile(filepath.Join(dir, "human-inbox-bridge.env"), []byte(bridge), 0o600); err != nil {
		t.Fatalf("writing the fake bridge env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "human-inbox-tracker.env"), []byte(tracker), 0o600); err != nil {
		t.Fatalf("writing the fake tracker env: %v", err)
	}
	return home
}

// runColocationAssertion drives the assertion against 127.0.0.1 — a host
// this machine genuinely answers on, so `host_owns_address` runs its real
// local-address probe with no ssh and no network.
func runColocationAssertion(t *testing.T, files placementEnvFiles, endpoint string) (string, int) {
	t.Helper()
	home := writePlacementEnv(t, files)
	return runPlacement(t, map[string]string{"HOME": home},
		fmt.Sprintf("assert_human_inbox_colocated 127.0.0.1 company/human-ops %q; echo ASSERTION_PASSED", endpoint))
}

func TestColocationAssertionAcceptsAMatchedPair(t *testing.T) {
	requirePlacementTools(t)

	out, code := runColocationAssertion(t, matchedEnvFiles(), "http://127.0.0.1:8087")
	if code != 0 || !strings.Contains(out, "ASSERTION_PASSED") {
		t.Errorf("a bridge and tracker co-located on the actor's own host were refused (exit %d): %s", code, out)
	}
}

// refusalCases are the ways this deployment can be split. Each one used to be
// possible to ship silently; each one now has to fail the deploy loudly.
func TestColocationAssertionRefusesEverySplit(t *testing.T) {
	requirePlacementTools(t)

	for _, tc := range []struct {
		name     string
		endpoint string
		mutate   func(*placementEnvFiles)
		// names is what the refusal must actually print: an operator has to
		// be able to see BOTH sides of the disagreement, not just that one
		// exists.
		names []string
	}{
		{
			// The measured defect: the pair installed on a host that is not
			// the one the engine dispatches this actor's work to.
			name:     "host does not serve the actor",
			endpoint: "http://203.0.113.9:8087",
			names:    []string{"203.0.113.9", "127.0.0.1"},
		},
		{
			// The bridge port and the registered port are two values that
			// must agree. company/human-ops was registered on :8090 while
			// the deploy wrote :8087.
			name:     "bridge port disagrees with the registered endpoint",
			endpoint: "http://127.0.0.1:8090",
			names:    []string{"8090", "8087"},
		},
		{
			name:     "tracker submits to a different bridge",
			endpoint: "http://127.0.0.1:8087",
			mutate:   func(f *placementEnvFiles) { f.trackerBridgeURL = "http://127.0.0.1:9999" },
			names:    []string{"9999", "8087"},
		},
		{
			// Same host, same port, different state directory: the tracker
			// reads pending tasks off the filesystem, so a second directory
			// is a second inbox that stays empty.
			name:     "tracker watches a different state directory",
			endpoint: "http://127.0.0.1:8087",
			mutate:   func(f *placementEnvFiles) { f.trackerStateDir = "/home/nodes/.culture-nodes/other-state" },
			names:    []string{"other-state", "human-inbox-state"},
		},
		{
			// The tracker resolves its configured actor id as an actor_KEY
			// against the control plane, while the bridge stamps its own as
			// origin.actor_id — a ledger foreign key into actors(id). One
			// name, two required values; giving the tracker the row id makes
			// its startup check refuse every time.
			name:     "tracker carries a row id where it needs the actor key",
			endpoint: "http://127.0.0.1:8087",
			mutate:   func(f *placementEnvFiles) { f.trackerActorID = "row-human-new" },
			names:    []string{"company/human-ops", "row-human-new"},
		},
		{
			// The other half of the same duality.
			name:     "bridge carries the actor key where it needs a row id",
			endpoint: "http://127.0.0.1:8087",
			mutate:   func(f *placementEnvFiles) { f.bridgeActorID = "company/human-ops" },
			names:    []string{"company/human-ops"},
		},
		{
			// Task t8's refusal is the runtime half of this invariant, and
			// it is disabled when the tracker has no control plane to
			// resolve its actor against. Installing that combination means
			// installing a pair nothing checks.
			name:     "tracker startup identity check is left disarmed",
			endpoint: "http://127.0.0.1:8087",
			mutate:   func(f *placementEnvFiles) { f.trackerControlPlane = "" },
			names:    []string{"HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSplitIsRefused(t, tc.endpoint, tc.mutate, tc.names)
		})
	}
}

// assertSplitIsRefused drives the colocation assertion with one deliberately
// split configuration and requires it to refuse loudly, naming every value in
// names. Lifted out of the table's subtest closure so the checks are a flat
// list rather than nested inside both the table loop and the closure.
func assertSplitIsRefused(t *testing.T, endpoint string, mutate func(*placementEnvFiles), names []string) {
	t.Helper()

	files := matchedEnvFiles()
	if mutate != nil {
		mutate(&files)
	}
	out, code := runColocationAssertion(t, files, endpoint)
	if code == 0 || strings.Contains(out, "ASSERTION_PASSED") {
		t.Fatalf("the assertion accepted a split deployment (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "SPLIT DEPLOYMENT REFUSED") {
		t.Errorf("refusal is not loud: output names no SPLIT DEPLOYMENT REFUSED: %s", out)
	}
	for _, name := range names {
		if !strings.Contains(out, name) {
			t.Errorf("refusal never names %q — an operator cannot see both sides of the disagreement: %s", name, out)
		}
	}
}

// TestLaneWritesAConfigTheAssertionAccepts closes the loop between the two
// files this task changed. Every other test here drives the assertion with
// env files a Go helper wrote; this one runs deploy.sh's OWN env-writing
// block and then asserts against what it produced.
//
// Without it, deploy.sh and actor-placement.sh could each be internally
// consistent and still disagree — which is the exact shape of the bug being
// fixed, one level up.
func TestLaneWritesAConfigTheAssertionAccepts(t *testing.T) {
	requirePlacementTools(t)

	script := deployScriptText(t)
	const startMarker = `actor_host_exec "$host" 'umask 077`
	const endMarker = "} > ~/.culture-nodes/human-inbox-tracker.env'"
	start := strings.Index(script, startMarker)
	end := strings.Index(script, endMarker)
	if start == -1 || end == -1 || end < start {
		t.Fatal("could not locate deploy.sh's human-inbox env-writing block; this test must run the real thing, not a copy of it")
	}
	envBlock := script[start : end+len(endMarker)]
	if !strings.Contains(envBlock, "human-inbox-bridge.env") {
		t.Fatalf("the extracted block does not write the bridge env file:\n%s", envBlock)
	}

	home := t.TempDir()
	// The values deploy_human_inbox holds at this point, all of them read
	// from one actor_registration call.
	snippet := strings.Join([]string{
		`say() { printf '==> %s\n' "$*"; }`,
		`host=127.0.0.1`,
		`port=8090`,
		`row_id=row-human-new`,
		`HUMAN_INBOX_ACTOR_KEY=company/human-ops`,
		`NODES_API_URL=http://192.0.2.1:18080`,
		envBlock,
		`assert_human_inbox_colocated "$host" "$HUMAN_INBOX_ACTOR_KEY" "http://127.0.0.1:$port"`,
		`echo ASSERTION_PASSED`,
	}, "\n")

	out, code := runPlacement(t, map[string]string{"HOME": home}, snippet)
	if code != 0 || !strings.Contains(out, "ASSERTION_PASSED") {
		t.Fatalf("deploy.sh's own env block produced a configuration its own assertion rejects (exit %d):\n%s", code, out)
	}

	// And the port it wrote is the registered one, not a number of its own.
	bridgeEnv, err := os.ReadFile(filepath.Join(home, ".culture-nodes", "human-inbox-bridge.env"))
	if err != nil {
		t.Fatalf("the lane wrote no bridge env file: %v", err)
	}
	if !strings.Contains(string(bridgeEnv), "HUMAN_INBOX_BRIDGE_PORT=8090") {
		t.Errorf("the bridge env does not bind the registered port:\n%s", bridgeEnv)
	}
	trackerEnv, err := os.ReadFile(filepath.Join(home, ".culture-nodes", "human-inbox-tracker.env"))
	if err != nil {
		t.Fatalf("the lane wrote no tracker env file: %v", err)
	}
	if !strings.Contains(string(trackerEnv), "HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL=http://192.0.2.1:18080") {
		t.Errorf("the tracker env does not arm the startup identity check:\n%s", trackerEnv)
	}
}

// TestColocationAssertionRefusesAnUnusableEndpoint closes the last gap: an
// endpoint with no port cannot be checked against anything, and unverifiable
// is not verified.
func TestColocationAssertionRefusesAnUnusableEndpoint(t *testing.T) {
	requirePlacementTools(t)

	out, code := runColocationAssertion(t, matchedEnvFiles(), "http://127.0.0.1")
	if code == 0 || strings.Contains(out, "ASSERTION_PASSED") {
		t.Fatalf("the assertion accepted an endpoint it could not check (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "SPLIT DEPLOYMENT REFUSED") {
		t.Errorf("refusal is not loud: %s", out)
	}
}
