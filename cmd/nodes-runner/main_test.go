package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// setSecret gives every case a usable credential, so a test about state
// directories is not really a test about secrets.
func setSecret(t *testing.T) {
	t.Helper()
	t.Setenv(envSecret, "a-runner-service-bearer-secret")
}

func TestResolveRefusesAServiceWithNoSecret(t *testing.T) {
	t.Setenv(envSecret, "")
	t.Setenv(envSecretFile, "")
	_, err := resolve([]string{"--state-dir", t.TempDir(), "--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err == nil {
		t.Fatal("resolve accepted a runner service with no bearer secret")
	}
	if err.Code != clifmt.ExitUserError {
		t.Errorf("exit code = %d, want %d", err.Code, clifmt.ExitUserError)
	}
	if err.Remediation == "" {
		t.Error("the refusal carries no hint")
	}
}

func TestResolveReadsTheSecretFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := writeFile(path, "  file-held-secret\n"); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}
	t.Setenv(envSecret, "")
	t.Setenv(envSecretFile, path)

	resolved, err := resolve([]string{"--state-dir", t.TempDir(), "--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.secret != "file-held-secret" {
		t.Errorf("secret = %q, want the trimmed file contents", resolved.secret)
	}
}

// Durability is a promise, so the weaker posture is a named opt-in rather
// than what happens when nobody says anything.
func TestResolveRefusesToRunWithNeitherAStateDirectoryNorTheExplicitOptIn(t *testing.T) {
	setSecret(t)
	t.Setenv(envStateDir, "")
	t.Setenv(envEphemeral, "")

	_, err := resolve([]string{"--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err == nil {
		t.Fatal("resolve defaulted to a status store that forgets everything on restart")
	}
	if err.Remediation == "" {
		t.Error("the refusal carries no hint")
	}
}

func TestResolveAcceptsTheExplicitEphemeralOptInAndRecordsItsLimit(t *testing.T) {
	setSecret(t)
	resolved, err := resolve([]string{"--ephemeral-state", "--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !resolved.ephemeral {
		t.Fatal("the opt-in did not take effect")
	}
	if resolved.durabilityLimit == "" {
		t.Error("the ephemeral posture records no stated limit; a weaker guarantee should say what it gave up")
	}
}

func TestResolveRefusesBothDurabilityPosturesAtOnce(t *testing.T) {
	setSecret(t)
	_, err := resolve([]string{
		"--state-dir", t.TempDir(),
		"--ephemeral-state",
		"--profile", testDigest + "=" + headspace.DefaultProfilePython312,
	})
	if err == nil {
		t.Fatal("resolve accepted a state directory and --ephemeral-state together")
	}
}

func TestResolveRefusesAnEmptyProfileMap(t *testing.T) {
	setSecret(t)
	t.Setenv(envProfiles, "")
	_, err := resolve([]string{"--state-dir", t.TempDir()})
	if err == nil {
		t.Fatal("resolve accepted a bridge with no digest-to-profile mapping; it would refuse every operation")
	}
}

func TestResolveMergesProfilesFromFlagsAndEnvironment(t *testing.T) {
	setSecret(t)
	other := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	t.Setenv(envProfiles, other+"=python3.11")

	resolved, err := resolve([]string{
		"--state-dir", t.TempDir(),
		"--profile", testDigest + "=" + headspace.DefaultProfilePython312,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := resolved.profiles[testDigest]; got != headspace.DefaultProfilePython312 {
		t.Errorf("flag profile = %q, want %q", got, headspace.DefaultProfilePython312)
	}
	if got := resolved.profiles[other]; got != "python3.11" {
		t.Errorf("environment profile = %q, want python3.11", got)
	}
}

func TestAnExplicitProfileFlagWinsOverTheEnvironment(t *testing.T) {
	setSecret(t)
	t.Setenv(envProfiles, testDigest+"=from-environment")

	resolved, err := resolve([]string{
		"--state-dir", t.TempDir(),
		"--profile", testDigest + "=from-flag",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := resolved.profiles[testDigest]; got != "from-flag" {
		t.Errorf("profile = %q, want the flag's value", got)
	}
}

func TestResolveReadsDurationsAndCountsFromTheEnvironment(t *testing.T) {
	setSecret(t)
	t.Setenv(envRetention, "48h")
	t.Setenv(envConcurrency, "3")

	resolved, err := resolve([]string{"--state-dir", t.TempDir(), "--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.retention != 48*time.Hour {
		t.Errorf("retention = %s, want 48h", resolved.retention)
	}
	if resolved.concurrency != 3 {
		t.Errorf("concurrency = %d, want 3", resolved.concurrency)
	}
}

func TestResolveRefusesAMalformedDuration(t *testing.T) {
	setSecret(t)
	t.Setenv(envRetention, "a while")
	_, err := resolve([]string{"--state-dir", t.TempDir(), "--profile", testDigest + "=" + headspace.DefaultProfilePython312})
	if err == nil {
		t.Fatal("resolve accepted a malformed retention duration")
	}
}

func TestHelpAndVersionExitCleanly(t *testing.T) {
	if code := run([]string{"--help"}); code != clifmt.ExitSuccess {
		t.Errorf("--help exit code = %d, want %d", code, clifmt.ExitSuccess)
	}
	if code := run([]string{"--version"}); code != clifmt.ExitSuccess {
		t.Errorf("--version exit code = %d, want %d", code, clifmt.ExitSuccess)
	}
}

func TestAnUnknownFlagIsAUserErrorWithAHint(t *testing.T) {
	setSecret(t)
	if code := run([]string{"--not-a-flag"}); code != clifmt.ExitUserError {
		t.Errorf("exit code = %d, want %d", code, clifmt.ExitUserError)
	}
}

// writeFile is a tiny helper so the secret-file case does not need os in the
// test's own import list twice over.
func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
