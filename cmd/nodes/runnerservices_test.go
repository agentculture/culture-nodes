package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// strconvQuote keeps the JSON fixtures readable on Windows-safe paths.
func strconvQuote(s string) string { return strconv.Quote(s) }

// The worker dispatches code nodes to runner services only when the
// deployment says so: no NODES_RUNNER_SERVICES_FILE, no protocol path.
func TestRunnerServicesAbsentEnvDisablesTheProtocolPath(t *testing.T) {
	t.Setenv(envRunnerServicesFile, "")
	opts, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("absent env must not error: %v", cliErr)
	}
	if opts.Registry != nil || opts.Client != nil {
		t.Fatalf("absent env must leave the protocol path disabled, got %+v", opts)
	}
}

func TestRunnerServicesFileBuildsRegistryAndClient(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "runner.secret")
	if err := os.WriteFile(secretPath, []byte("s3cret-material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "services.json")
	cfg := `[{
		"name": "delivery-loop/test",
		"endpoint": "http://thor:17070",
		"image_digest": "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de",
		"secret_file": ` + strconvQuote(secretPath) + `,
		"allow_insecure_transport": true,
		"description": "thor headspace runner"
	}]`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRunnerServicesFile, cfgPath)

	opts, cliErr := runnerServiceConfig()
	if cliErr != nil {
		t.Fatalf("valid config errored: %v", cliErr)
	}
	if opts.Registry == nil || opts.Client == nil {
		t.Fatalf("valid config must enable the protocol path, got %+v", opts)
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
	if _, cliErr := runnerServiceConfig(); cliErr == nil {
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
	if _, cliErr := runnerServiceConfig(); cliErr == nil {
		t.Fatal("malformed JSON must be an env error")
	}
}
