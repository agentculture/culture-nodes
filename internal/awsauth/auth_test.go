package awsauth_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/awsauth"
)

// clearAWSEnv resets every environment variable LoadConfig or the
// underlying SDK chain reads to "", so each subtest starts from a known,
// isolated state regardless of what the process (or a CI runner) already
// has set -- t.Setenv restores the previous value automatically at the end
// of the (sub)test.
func clearAWSEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"AWS_ROLE_ARN",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_PROFILE",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_ROLE_SESSION_NAME",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"AWS_CONFIG_FILE",
		"AWS_SHARED_CREDENTIALS_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(key, "")
	}
}

// capture returns a Logf-compatible func that appends every formatted
// message to *lines, so a test can assert on what LoadConfig reported.
func capture(lines *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*lines = append(*lines, fmt.Sprintf(format, args...))
	}
}

// writeFile writes content to path, failing the test on any error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requireConfigError asserts err is a *awsauth.ConfigError and returns it.
func requireConfigError(t *testing.T, err error) *awsauth.ConfigError {
	t.Helper()
	var cfgErr *awsauth.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("error %v (%T) is not a *awsauth.ConfigError", err, err)
	}
	return cfgErr
}

// TestLoadConfigAssumeRole proves link 1: an explicit RoleARN with no
// web-identity token file resolves via plain STS AssumeRole and reports
// SourceAssumeRole -- without ever calling STS (see the package doc's "No
// real AWS calls" section: constructing the AssumeRoleProvider is all
// LoadConfig does).
func TestLoadConfigAssumeRole(t *testing.T) {
	clearAWSEnv(t)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/nodes-worker")

	var logs []string
	cfg, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf:   capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceAssumeRole {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceAssumeRole)
	}
	if cfg.Credentials == nil {
		t.Fatal("resolved aws.Config has no Credentials provider")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "AssumeRole") {
		t.Fatalf("Logf calls = %v, want exactly one mentioning AssumeRole", logs)
	}
}

// TestLoadConfigWebIdentityIRSA proves link 2: RoleARN plus a web-identity
// token file resolves via STS AssumeRoleWithWebIdentity and reports
// SourceWebIdentity -- the IRSA path, explicitly named rather than falling
// through to SourceAmbient (see the package doc's "Why IRSA gets its own
// reported Source" section).
func TestLoadConfigWebIdentityIRSA(t *testing.T) {
	clearAWSEnv(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "fake-jwt-token")

	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/nodes-worker")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)

	var logs []string
	cfg, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf:   capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceWebIdentity {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceWebIdentity)
	}
	if cfg.Credentials == nil {
		t.Fatal("resolved aws.Config has no Credentials provider")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "WebIdentity") {
		t.Fatalf("Logf calls = %v, want exactly one mentioning WebIdentity", logs)
	}
}

// TestLoadConfigWebIdentityIRSAViaOptions proves the same link resolves when
// the caller passes RoleARN/WebIdentityTokenFile through Options directly
// rather than through the environment -- Options always takes precedence.
func TestLoadConfigWebIdentityIRSAViaOptions(t *testing.T) {
	clearAWSEnv(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "fake-jwt-token")

	_, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region:               "us-east-1",
		RoleARN:              "arn:aws:iam::123456789012:role/nodes-worker",
		WebIdentityTokenFile: tokenFile,
		RoleSessionName:      "explicit-session",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceWebIdentity {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceWebIdentity)
	}
}

// TestLoadConfigProfile proves link 3: AWS_PROFILE alone (no role) resolves
// via config.WithSharedConfigProfile and reports SourceProfile.
func TestLoadConfigProfile(t *testing.T) {
	clearAWSEnv(t)
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials")
	writeFile(t, credsPath, "[nodes-profile]\n"+
		"aws_access_key_id = AKIAFAKEEXAMPLE\n"+
		"aws_secret_access_key = fakeSecretExampleKey\n")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config-does-not-exist"))
	t.Setenv("AWS_PROFILE", "nodes-profile")

	var logs []string
	cfg, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf:   capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceProfile {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceProfile)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("Region = %q, want us-east-1", cfg.Region)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "nodes-profile") {
		t.Fatalf("Logf calls = %v, want exactly one naming the profile", logs)
	}
}

// TestLoadConfigProfileWithoutRegionWarns proves the profile link's region
// check is a soft warning (via Logf), not a *ConfigError -- unlike role
// assumption's hard requirement, a shared-config profile can carry its own
// region.
func TestLoadConfigProfileWithoutRegionWarns(t *testing.T) {
	clearAWSEnv(t)
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials")
	writeFile(t, credsPath, "[nodes-profile]\n"+
		"aws_access_key_id = AKIAFAKEEXAMPLE\n"+
		"aws_secret_access_key = fakeSecretExampleKey\n")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config-does-not-exist"))
	t.Setenv("AWS_PROFILE", "nodes-profile")

	var logs []string
	_, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Logf: capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v, want success (a missing region is a warning for a profile, not an error)", err)
	}
	if source != awsauth.SourceProfile {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceProfile)
	}
	if len(logs) != 2 {
		t.Fatalf("Logf calls = %v, want a region warning followed by the resolution log", logs)
	}
	if !strings.Contains(logs[0], "nodes-profile") || !strings.Contains(logs[0], "Region") {
		t.Fatalf("first Logf call = %q, want it to warn about the profile and the missing region", logs[0])
	}
}

// TestLoadConfigBadProfileIsConfigError proves a profile absent from the
// shared config/credentials files is a synchronous *ConfigError (detected by
// awsconfig.LoadDefaultConfig itself with no network access -- see the
// package doc), not a failure deferred to first use.
func TestLoadConfigBadProfileIsConfigError(t *testing.T) {
	clearAWSEnv(t)
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials-does-not-exist"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config-does-not-exist"))
	t.Setenv("AWS_PROFILE", "no-such-profile")

	_, _, err := awsauth.LoadConfig(t.Context(), awsauth.Options{Region: "us-east-1"})
	if err == nil {
		t.Fatal("LoadConfig: got nil error, want a *ConfigError for a nonexistent profile")
	}
	cfgErr := requireConfigError(t, err)
	if cfgErr.Remediation == "" {
		t.Fatal("ConfigError.Remediation is empty, want an actionable remediation")
	}
	if !strings.Contains(cfgErr.Message, "no-such-profile") {
		t.Fatalf("ConfigError.Message = %q, want it to name the profile", cfgErr.Message)
	}
}

// TestLoadConfigStaticKeys proves link 4: explicit AWS_ACCESS_KEY_ID/
// AWS_SECRET_ACCESS_KEY (with no role or profile) resolves via a static
// credentials provider and reports SourceStaticKeys.
func TestLoadConfigStaticKeys(t *testing.T) {
	clearAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKEEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fakeSecretExampleKey")

	var logs []string
	cfg, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf:   capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceStaticKeys {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceStaticKeys)
	}
	creds, err := cfg.Credentials.Retrieve(t.Context())
	if err != nil {
		t.Fatalf("Retrieve static credentials: %v", err)
	}
	if creds.AccessKeyID != "AKIAFAKEEXAMPLE" {
		t.Fatalf("AccessKeyID = %q, want AKIAFAKEEXAMPLE", creds.AccessKeyID)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "static") {
		t.Fatalf("Logf calls = %v, want exactly one mentioning static keys", logs)
	}
}

// TestLoadConfigAmbient proves link 5: with none of the above configured,
// LoadConfig falls back to the SDK's own default chain and reports
// SourceAmbient.
func TestLoadConfigAmbient(t *testing.T) {
	clearAWSEnv(t)

	var logs []string
	cfg, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf:   capture(&logs),
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceAmbient {
		t.Fatalf("Source = %q, want %q", source, awsauth.SourceAmbient)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("Region = %q, want us-east-1", cfg.Region)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "ambient") {
		t.Fatalf("Logf calls = %v, want exactly one mentioning the ambient chain", logs)
	}
}

// TestLoadConfigDefaultLogfIsLogPrintf proves a nil Options.Logf does not
// panic and does not silently drop the diagnostic -- LoadConfig falls back
// to log.Printf, matching internal/queue/sqs.Config.Logf's documented
// "never silently drop the diagnostic" convention.
func TestLoadConfigDefaultLogfIsLogPrintf(t *testing.T) {
	clearAWSEnv(t)
	if _, _, err := awsauth.LoadConfig(t.Context(), awsauth.Options{Region: "us-east-1"}); err != nil {
		t.Fatalf("LoadConfig with nil Logf: %v", err)
	}
}

// TestLoadConfigRoleWithoutRegionIsConfigError proves the acceptance
// criterion directly: an explicit RoleARN with no region configured fails
// fast with a *ConfigError carrying an actionable Remediation, not a delayed
// failure the first time something tries to build an STS endpoint.
func TestLoadConfigRoleWithoutRegionIsConfigError(t *testing.T) {
	clearAWSEnv(t)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/nodes-worker")

	_, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{})
	if err == nil {
		t.Fatal("LoadConfig: got nil error, want a *ConfigError for role-without-region")
	}
	if source != "" {
		t.Fatalf("Source = %q, want empty on error", source)
	}
	cfgErr := requireConfigError(t, err)
	if cfgErr.Remediation == "" {
		t.Fatal("ConfigError.Remediation is empty, want an actionable remediation")
	}
	if !strings.Contains(cfgErr.Remediation, "Region") {
		t.Fatalf("ConfigError.Remediation = %q, want it to mention Region", cfgErr.Remediation)
	}
}

// TestLoadConfigWebIdentityWithoutRoleIsConfigError proves the "web-identity
// token file alone" edge: a token file set with no resolvable RoleARN
// anywhere refuses with a *ConfigError rather than silently falling through
// to the ambient chain (which has no use for a bare token file and would
// otherwise mask the misconfiguration as SourceAmbient).
func TestLoadConfigWebIdentityWithoutRoleIsConfigError(t *testing.T) {
	clearAWSEnv(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "fake-jwt-token")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)

	_, _, err := awsauth.LoadConfig(t.Context(), awsauth.Options{Region: "us-east-1"})
	if err == nil {
		t.Fatal("LoadConfig: got nil error, want a *ConfigError")
	}
	cfgErr := requireConfigError(t, err)
	if !strings.Contains(cfgErr.Message, "AWS_WEB_IDENTITY_TOKEN_FILE") {
		t.Fatalf("ConfigError.Message = %q, want it to name AWS_WEB_IDENTITY_TOKEN_FILE", cfgErr.Message)
	}
}

// TestLoadConfigOptionsOverrideEnvironment proves an explicit Options.RoleARN
// takes priority over an environment that would otherwise resolve via a
// different link (static keys), for every link that reads an environment
// fallback.
func TestLoadConfigOptionsOverrideEnvironment(t *testing.T) {
	clearAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENVEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envSecretExampleKey")

	_, source, err := awsauth.LoadConfig(t.Context(), awsauth.Options{
		Region:  "us-east-1",
		RoleARN: "arn:aws:iam::123456789012:role/nodes-worker",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if source != awsauth.SourceAssumeRole {
		t.Fatalf("Source = %q, want %q -- an explicit Options.RoleARN must take priority over env static keys", source, awsauth.SourceAssumeRole)
	}
}
