package main

import (
	"path/filepath"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{envAPIBase, envCursorFile, envRuns, envDashboardBase, envReconnectMin, envReconnectMax, envHTTPTimeout} {
		t.Setenv(name, "")
	}
}

func TestResolveRefusesWithNoAPIBase(t *testing.T) {
	clearEnv(t)
	_, err := resolve([]string{"--cursor-file", filepath.Join(t.TempDir(), "cursor.json")})
	if err == nil {
		t.Fatal("resolve accepted a config with no api-base")
	}
	if err.Remediation == "" {
		t.Error("the refusal carries no hint")
	}
}

func TestResolveRefusesWithNoCursorFile(t *testing.T) {
	clearEnv(t)
	_, err := resolve([]string{"--api-base", "http://localhost:8080"})
	if err == nil {
		t.Fatal("resolve accepted a config with no cursor file")
	}
	if err.Remediation == "" {
		t.Error("the refusal carries no hint")
	}
}

func TestResolveAcceptsFlags(t *testing.T) {
	clearEnv(t)
	resolved, err := resolve([]string{
		"--api-base", "http://localhost:8080",
		"--cursor-file", filepath.Join(t.TempDir(), "cursor.json"),
		"--runs", "run-1, run-2",
		"--dashboard-base", "http://dashboard.example",
		"--reconnect-min", "1s",
		"--reconnect-max", "10s",
		"--http-timeout", "5s",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.apiBase != "http://localhost:8080" {
		t.Errorf("apiBase = %q", resolved.apiBase)
	}
	if len(resolved.runs) != 2 || resolved.runs[0] != "run-1" || resolved.runs[1] != "run-2" {
		t.Errorf("runs = %v, want [run-1 run-2] (whitespace around commas must be trimmed)", resolved.runs)
	}
	if resolved.dashboardBase != "http://dashboard.example" {
		t.Errorf("dashboardBase = %q", resolved.dashboardBase)
	}
	if resolved.reconnectMin != time.Second {
		t.Errorf("reconnectMin = %v, want 1s", resolved.reconnectMin)
	}
	if resolved.reconnectMax != 10*time.Second {
		t.Errorf("reconnectMax = %v, want 10s", resolved.reconnectMax)
	}
	if resolved.httpTimeout != 5*time.Second {
		t.Errorf("httpTimeout = %v, want 5s", resolved.httpTimeout)
	}
}

func TestResolveFallsBackToEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(envAPIBase, "http://from-env:8080")
	t.Setenv(envCursorFile, filepath.Join(t.TempDir(), "cursor.json"))
	t.Setenv(envRuns, "run-a,run-b")
	t.Setenv(envReconnectMin, "2s")

	resolved, err := resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.apiBase != "http://from-env:8080" {
		t.Errorf("apiBase = %q, want the environment value", resolved.apiBase)
	}
	if len(resolved.runs) != 2 {
		t.Errorf("runs = %v, want 2 entries from the environment", resolved.runs)
	}
	if resolved.reconnectMin != 2*time.Second {
		t.Errorf("reconnectMin = %v, want 2s from the environment", resolved.reconnectMin)
	}
}

func TestResolveFlagWinsOverEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv(envAPIBase, "http://from-env:8080")
	resolved, err := resolve([]string{
		"--api-base", "http://from-flag:9090",
		"--cursor-file", filepath.Join(t.TempDir(), "cursor.json"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.apiBase != "http://from-flag:9090" {
		t.Errorf("apiBase = %q, want the flag's value to win", resolved.apiBase)
	}
}

func TestResolveRejectsANonPositiveDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv(envReconnectMin, "not-a-duration")
	_, err := resolve([]string{
		"--api-base", "http://localhost:8080",
		"--cursor-file", filepath.Join(t.TempDir(), "cursor.json"),
	})
	if err == nil {
		t.Fatal("resolve accepted a malformed duration from the environment")
	}
}

func TestRunHelpAndVersionSucceed(t *testing.T) {
	if code := run([]string{"--help"}); code != 0 {
		t.Errorf("run(--help) = %d, want 0", code)
	}
	if code := run([]string{"--version"}); code != 0 {
		t.Errorf("run(--version) = %d, want 0", code)
	}
}

func TestRunWithNoConfigurationExitsUserError(t *testing.T) {
	clearEnv(t)
	if code := run([]string{}); code != 1 {
		t.Errorf("run([]) = %d, want 1 (user error: no api-base/cursor-file configured)", code)
	}
}
