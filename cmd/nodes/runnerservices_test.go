package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// strconvQuote keeps the JSON fixtures readable on Windows-safe paths.
func strconvQuote(s string) string { return strconv.Quote(s) }

// secretFileName turns a registry name (which may contain "/", e.g.
// "delivery-loop/first") into a flat filename component, so a secret file
// beside the registry JSON never needs a directory of its own.
func secretFileName(name string) string {
	return strings.ReplaceAll(name, "/", "_") + ".secret"
}

// writeRunnerServiceEntry writes one runner-service registry entry to path,
// with a fresh secret file beside it, and returns the entry's secret_file
// path so a test can rewrite the secret independently of the registry.
func writeRunnerServiceEntry(t *testing.T, dir, cfgPath, name, endpoint, secret string) string {
	t.Helper()
	secretPath := filepath.Join(dir, secretFileName(name))
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `[{
		"name": ` + strconvQuote(name) + `,
		"endpoint": ` + strconvQuote(endpoint) + `,
		"image_digest": "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de",
		"secret_file": ` + strconvQuote(secretPath) + `,
		"allow_insecure_transport": true,
		"description": "test runner"
	}]`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return secretPath
}

// writeRunnerServiceEntries writes a two-entry registry file, reusing
// writeRunnerServiceEntry's fixture shape for each entry.
func writeRunnerServiceEntries(t *testing.T, dir, cfgPath string, entries [][3]string) {
	t.Helper()
	secretPaths := make([]string, len(entries))
	for i, e := range entries {
		name, _, secret := e[0], e[1], e[2]
		secretPath := filepath.Join(dir, secretFileName(name))
		if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		secretPaths[i] = secretPath
	}
	var body string
	for i, e := range entries {
		name, endpoint := e[0], e[1]
		if i > 0 {
			body += ","
		}
		body += `{
			"name": ` + strconvQuote(name) + `,
			"endpoint": ` + strconvQuote(endpoint) + `,
			"image_digest": "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de",
			"secret_file": ` + strconvQuote(secretPaths[i]) + `,
			"allow_insecure_transport": true
		}`
	}
	if err := os.WriteFile(cfgPath, []byte("["+body+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// touchForward advances path's mtime strictly past after, so a reload check
// keyed on "did mtime advance" observes the rewrite deterministically instead
// of racing the filesystem's timestamp resolution.
func touchForward(t *testing.T, path string, after time.Time) {
	t.Helper()
	next := after.Add(time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
}

// The worker dispatches code nodes to runner services only when the
// deployment says so: no NODES_RUNNER_SERVICES_FILE, no protocol path.
func TestRunnerServicesAbsentEnvDisablesTheProtocolPath(t *testing.T) {
	t.Setenv(envRunnerServicesFile, "")
	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("absent env must not error: %v", cliErr)
	}
	if opts.Registry != nil || opts.Client != nil {
		t.Fatalf("absent env must leave the protocol path disabled, got %+v", opts)
	}
	if reloader != nil {
		t.Fatal("absent env must not build a reloader: there is nothing to watch for changes")
	}
}

func TestRunnerServicesFileBuildsRegistryAndClient(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/test", "http://thor:17070", "s3cret-material")
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("valid config errored: %v", cliErr)
	}
	if opts.Registry == nil || opts.Client == nil {
		t.Fatalf("valid config must enable the protocol path, got %+v", opts)
	}
	if reloader == nil {
		t.Fatal("a valid config must build a reloader to watch the file it just loaded")
	}
	svc, err := opts.Registry.ResolveService("delivery-loop/test")
	if err != nil {
		t.Fatalf("registered service did not resolve: %v", err)
	}
	if svc.Endpoint != "http://thor:17070" {
		t.Fatalf("endpoint mismatch: %q", svc.Endpoint)
	}
	if svc.SecretRef != "runner-secret:delivery-loop/test" {
		t.Fatalf("secret ref should be the symbolic per-entry reference, got %q", svc.SecretRef)
	}
}

func TestRunnerServicesMissingSecretFileIsAnEnvError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	cfg := `[{
		"name": "x",
		"endpoint": "http://thor:17070",
		"image_digest": "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de",
		"secret_file": ` + strconvQuote(filepath.Join(dir, "absent.secret")) + `,
		"allow_insecure_transport": true
	}]`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRunnerServicesFile, cfgPath)
	if _, _, cliErr := runnerServiceConfig(); cliErr == nil {
		t.Fatal("a missing secret file must be an env error, not a silent skip")
	}
}

func TestRunnerServicesMalformedJSONIsAnEnvError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRunnerServicesFile, cfgPath)
	if _, _, cliErr := runnerServiceConfig(); cliErr == nil {
		t.Fatal("malformed JSON must be an env error")
	}
}

// TestRunnerServicesReloadTakesEffectWithoutRebuildingTheRegistry is task
// t19's acceptance criterion for issue #8's residual gap, proven directly: a
// runner-service registered AFTER a worker's registry and client already
// exist becomes resolvable through the SAME *runners.FunctionRegistry and
// dispatchable through the SAME *runners.ProtocolClient this worker was
// built with -- no new registry, no new client, no process restart, just a
// reload check against a changed file.
func TestRunnerServicesReloadTakesEffectWithoutRebuildingTheRegistry(t *testing.T) {
	// A fake runner service this test dispatches a real operation to after
	// the reload, so "the secret resolves" is proven the same way
	// runnerasync_test.go proves it -- an authenticated request the client
	// actually sends -- rather than by reaching into ProtocolClient's
	// unexported fields.
	var gotAuth string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(runners.Acceptance{OperationID: "op-reload-test"})
	}))
	t.Cleanup(fake.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/first", "http://thor:17070", "first-secret")
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("initial load errored: %v", cliErr)
	}
	registry, client := opts.Registry, opts.Client // the identity a worker would be built with

	if _, err := registry.ResolveService("delivery-loop/second"); err == nil {
		t.Fatal("a service not yet in the file must not resolve before the reload")
	}

	// The deployment change this whole gap is about: `nodes runner-services
	// register` appends a second entry to the SAME file, with no worker
	// restart in between.
	before, statErr := os.Stat(cfgPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	writeRunnerServiceEntries(t, dir, cfgPath, [][3]string{
		{"delivery-loop/first", "http://thor:17070", "first-secret"},
		{"delivery-loop/second", fake.URL, "second-secret"},
	})
	touchForward(t, cfgPath, before.ModTime())

	applied, cliErr := reloader.checkAndReload()
	if cliErr != nil {
		t.Fatalf("reload errored: %v", cliErr)
	}
	if !applied {
		t.Fatal("a changed file must report that the reload was applied")
	}

	// Resolved through the exact registry object obtained BEFORE the reload:
	// this is what makes it a live reload rather than a rebuild.
	svc, err := registry.ResolveService("delivery-loop/second")
	if err != nil {
		t.Fatalf("the newly registered service did not resolve after reload: %v", err)
	}
	if svc.Endpoint != fake.URL {
		t.Fatalf("endpoint mismatch: %q", svc.Endpoint)
	}

	// Dispatched through the exact client object obtained before the reload:
	// a registry-only reload that left the client's credentials stale would
	// resolve the identity and then fail authentication on every request.
	_, err = client.Dispatch(context.Background(), svc, runners.Operation{OperationID: "op-reload-test"}, runners.CallbackOffer{})
	if err != nil {
		t.Fatalf("dispatch to the newly reloaded service failed: %v", err)
	}
	if gotAuth != "Bearer second-secret" {
		t.Fatalf("the new entry's secret did not reach the client: got Authorization %q", gotAuth)
	}

	// The first entry survives the reload unchanged.
	first, err := registry.ResolveService("delivery-loop/first")
	if err != nil {
		t.Fatalf("the original service stopped resolving after reload: %v", err)
	}
	if first.Endpoint != "http://thor:17070" {
		t.Fatalf("original endpoint changed unexpectedly: %q", first.Endpoint)
	}
}

// TestRunnerServicesReloadIsANoOpWhenTheFileIsUnchanged is the mtime gate:
// checking a file nobody touched must not re-parse it, and must not report a
// reload happened.
func TestRunnerServicesReloadIsANoOpWhenTheFileIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/first", "http://thor:17070", "first-secret")
	t.Setenv(envRunnerServicesFile, cfgPath)

	_, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("initial load errored: %v", cliErr)
	}

	applied, cliErr := reloader.checkAndReload()
	if cliErr != nil {
		t.Fatalf("unchanged-file reload errored: %v", cliErr)
	}
	if applied {
		t.Fatal("an untouched file must not report a reload as applied")
	}
}

// TestRunnerServicesReloadRefusesAnInvalidFileWithoutDisturbingTheRegistry:
// a reload that finds the file broken must leave the already-validated
// registry exactly as it was, the same way a malformed file at worker
// startup refuses to start rather than half-apply.
func TestRunnerServicesReloadRefusesAnInvalidFileWithoutDisturbingTheRegistry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/first", "http://thor:17070", "first-secret")
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("initial load errored: %v", cliErr)
	}

	before, statErr := os.Stat(cfgPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	touchForward(t, cfgPath, before.ModTime())

	if applied, cliErr := reloader.checkAndReload(); cliErr == nil || applied {
		t.Fatalf("a malformed file must refuse the reload, got applied=%v err=%v", applied, cliErr)
	}

	// The original service still resolves: the broken rewrite changed
	// nothing about the live registry.
	if _, err := opts.Registry.ResolveService("delivery-loop/first"); err != nil {
		t.Fatalf("a refused reload must not disturb the already-loaded registry: %v", err)
	}
}

// TestRunnerServicesReloaderPollAppliesAFileChangeOnItsOwn drives poll's
// background loop end to end (short interval, real ticker) instead of
// calling checkAndReload directly, so the goroutine wiring cmdWorker and
// buildWorker both rely on is exercised too.
func TestRunnerServicesReloaderPollAppliesAFileChangeOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/first", "http://thor:17070", "first-secret")
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("initial load errored: %v", cliErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pollErrs []error
	go reloader.poll(ctx, 10*time.Millisecond, func(err error) { pollErrs = append(pollErrs, err) })

	before, statErr := os.Stat(cfgPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	writeRunnerServiceEntries(t, dir, cfgPath, [][3]string{
		{"delivery-loop/first", "http://thor:17070", "first-secret"},
		{"delivery-loop/second", "http://orin:17070", "second-secret"},
	})
	touchForward(t, cfgPath, before.ModTime())

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := opts.Registry.ResolveService("delivery-loop/second"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poll did not apply the file change within the deadline; reload errors: %v", pollErrs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

// A CONFIGURED registry that is currently empty still builds the reloadable
// plumbing: a worker started before its first service registration must
// observe later additions without a restart (PR #180 review finding).
func TestRunnerServicesEmptyConfiguredFileStillBuildsTheReloader(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "services.json")
	if err := os.WriteFile(cfgPath, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, reloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("empty configured file errored: %v", cliErr)
	}
	if reloader == nil {
		t.Fatal("an explicitly configured (empty) registry file must produce a reloader")
	}
	if opts.Registry == nil {
		t.Fatal("an explicitly configured (empty) registry file must produce a registry")
	}

	before, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// The first-ever registration lands with no restart.
	writeRunnerServiceEntry(t, dir, cfgPath, "delivery-loop/first", "http://thor:17070", "first-secret")
	touchForward(t, cfgPath, before.ModTime())
	reloaded, cliErr := reloader.checkAndReload()
	if cliErr != nil {
		t.Fatalf("reload errored: %v", cliErr)
	}
	if !reloaded {
		t.Fatal("the appended first entry was not observed")
	}
	if _, err := opts.Registry.ResolveService("delivery-loop/first"); err != nil {
		t.Fatalf("the first registered service must resolve after reload: %v", err)
	}
}
