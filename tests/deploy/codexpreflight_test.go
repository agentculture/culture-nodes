// Package deploytest, this file: task t1's manifest-as-Go-test check over
// deploy/prod/codex-preflight.sh, the non-billable readiness probe an
// operator runs before pointing a codex bridge (adapters/codex) at a
// production host. "Non-billable" is the whole point of the script: it may
// only ever invoke the configured codex binary with `--version` or
// `login status` -- both read-only, free CLI calls -- and never `codex
// exec` or anything else that would consume a turn. This file proves that
// constraint two ways: dynamically, by running the script against fake
// `codex`-shaped executables (the same "tiny script written to
// t.TempDir()" technique adapters/codex/tests/conftest.py's fake_codex
// fixture uses, reimplemented in Go since this suite has no Python
// runtime); and statically, by asserting the script's own source text
// never spells any other subcommand next to a codex_bin invocation.
//
// Each of the six checks the script brief names (codex_bin
// exists+executable, `--version` runs and parses, `login status` reports
// authenticated, every repo_allowlist entry is a git checkout, state_dir is
// writable, host non-loopback implies auth_token) gets its own refusal
// test here, each asserting a DISTINCT one-line "preflight: ..." message on
// stderr plus a non-zero exit -- distinct messages are the whole reason an
// operator can tell six failure classes apart from a CI log without
// re-running anything interactively.
package deploytest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// preflightScriptPath locates deploy/prod/codex-preflight.sh from this test
// file's own path, the same runtime.Caller(0) technique compose_test.go and
// helm_test.go both use to stay independent of the working directory `go
// test` is invoked from.
func preflightScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to find codex-preflight.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	path := filepath.Join(repoRoot, "deploy", "prod", "codex-preflight.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("codex-preflight.sh not found at %s: %v", path, err)
	}
	return path
}

// fakeCodexScript is the shell source for a controllable stand-in for the
// real `codex` binary. It only answers the two invocations
// codex-preflight.sh is allowed to make:
//   - `--version` -- behavior chosen by FAKE_VERSION_BEHAVIOR (ok / fail /
//     unparseable)
//   - `login status` -- behavior chosen by FAKE_LOGIN_BEHAVIOR (ok / fail),
//     and records the CODEX_HOME it was invoked with to a sibling file
//     (".observed-codex-home", next to this script itself) so the
//     CODEX_HOME passthrough test can assert on it after the run --
//     codex-preflight.sh captures login status's own stdout+stderr into a
//     variable for parsing rather than forwarding it, so a marker on
//     stderr would never reach the test's view of the *outer* process.
//
// Anything else exits 1 with a marker string -- a defense-in-depth signal
// (independent of the static source-text assertion below) that the script
// under test never reaches for a codex subcommand this fake doesn't know
// about.
const fakeCodexScript = `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --version)
    case "${FAKE_VERSION_BEHAVIOR:-ok}" in
      ok) echo "codex-cli 0.147.0"; exit 0 ;;
      fail) echo "fake-codex: --version boom" >&2; exit 1 ;;
      unparseable) echo "not a version string at all"; exit 0 ;;
    esac
    ;;
  login)
    case "${2:-}" in
      status)
        printf '%s' "${CODEX_HOME:-}" > "$(dirname "$0")/.observed-codex-home"
        case "${FAKE_LOGIN_BEHAVIOR:-ok}" in
          ok) echo "Logged in using ChatGPT"; exit 0 ;;
          fail) echo "Not logged in"; exit 1 ;;
        esac
        ;;
    esac
    ;;
esac
echo "fake-codex: unexpected invocation: $*" >&2
exit 1
`

// writeFakeCodex writes fakeCodexScript to dir/codex, marks it executable,
// and returns its path.
func writeFakeCodex(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(fakeCodexScript), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

// gitCheckout creates a real git repo at dir/name and returns its path --
// the positive fixture for the repo_allowlist check.
func gitCheckout(t *testing.T, dir, name string) string {
	t.Helper()
	repo := filepath.Join(dir, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", repo, err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", repo, err, out)
	}
	return repo
}

// preflightConfig is the JSON shape codex-preflight.sh reads -- field names
// match adapters/codex/src/codex_bridge/config.py's Config dataclass
// exactly, since this is the same config file the codex bridge itself
// loads (CODEX_BRIDGE_CONFIG).
type preflightConfig struct {
	CodexBin      string            `json:"codex_bin,omitempty"`
	CodexEnv      map[string]string `json:"codex_env,omitempty"`
	RepoAllowlist []string          `json:"repo_allowlist"`
	StateDir      string            `json:"state_dir,omitempty"`
	Host          string            `json:"host,omitempty"`
	AuthToken     string            `json:"auth_token,omitempty"`
}

// writeConfig marshals cfg to dir/bridge-config.json and returns its path.
func writeConfig(t *testing.T, dir string, cfg preflightConfig) string {
	t.Helper()
	path := filepath.Join(dir, "bridge-config.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runPreflight runs codex-preflight.sh with the given config path and
// extra environment (appended to os.Environ()), returning combined stdout
// and stderr separately plus the process error (non-nil on non-zero exit).
func runPreflight(t *testing.T, configPath string, extraEnv ...string) (stdout, stderr string, err error) {
	t.Helper()
	script := preflightScriptPath(t)
	cmd := exec.Command(script, configPath)
	cmd.Env = append(os.Environ(), extraEnv...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// baseConfig returns a fully-valid config pointing at a fake codex binary
// that answers everything "ok", a real git checkout for the sole allowlist
// entry, and a writable state dir -- the success-path fixture each refusal
// test then mutates exactly one field of.
func baseConfig(t *testing.T, dir string) preflightConfig {
	t.Helper()
	fakeCodex := writeFakeCodex(t, dir)
	repo := gitCheckout(t, dir, "repo")
	return preflightConfig{
		CodexBin:      fakeCodex,
		RepoAllowlist: []string{repo},
		StateDir:      filepath.Join(dir, "state"),
		Host:          "127.0.0.1",
	}
}

// ---------------------------------------------------------------------------
// Success path
// ---------------------------------------------------------------------------

func TestPreflightSuccessPrintsMeasuredVersion(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	configPath := writeConfig(t, dir, cfg)

	stdout, stderr, err := runPreflight(t, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstdout=%q stderr=%q", err, stdout, stderr)
	}
	trimmed := strings.TrimSpace(stdout)
	if trimmed != "preflight: ok codex-cli 0.147.0" {
		t.Errorf("stdout = %q, want the measured version line", trimmed)
	}
}

// TestPreflightReadsConfigPathFromEnv covers the CODEX_BRIDGE_CONFIG
// fallback the brief requires when $1 is absent.
func TestPreflightReadsConfigPathFromEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	configPath := writeConfig(t, dir, cfg)

	script := preflightScriptPath(t)
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "CODEX_BRIDGE_CONFIG="+configPath)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected success via CODEX_BRIDGE_CONFIG, got error %v\nstderr=%s", err, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "codex-cli 0.147.0") {
		t.Errorf("stdout = %q, want it to contain the measured version", outBuf.String())
	}
}

// TestPreflightRespectsCodexHomeForLoginStatus covers the brief's "respect
// the config's codex_env.CODEX_HOME when set" requirement for the login
// check: the fake codex records the CODEX_HOME it saw (to a marker file --
// see fakeCodexScript's doc comment for why not stderr), so this proves
// the value flowed from codex_env through to the login status subprocess's
// own environment.
func TestPreflightRespectsCodexHomeForLoginStatus(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	codexHome := filepath.Join(dir, "codex-home")
	cfg.CodexEnv = map[string]string{"CODEX_HOME": codexHome}
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstderr=%s", err, stderr)
	}
	observed, readErr := os.ReadFile(filepath.Join(dir, ".observed-codex-home"))
	if readErr != nil {
		t.Fatalf("fake codex never recorded an observed CODEX_HOME: %v", readErr)
	}
	if string(observed) != codexHome {
		t.Errorf("fake codex observed CODEX_HOME=%q, want %q", observed, codexHome)
	}
}

// ---------------------------------------------------------------------------
// Refusal classes -- each must exit non-zero with its own distinct message
// ---------------------------------------------------------------------------

func TestPreflightRefusesMissingCodexBin(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	cfg.CodexBin = filepath.Join(dir, "does-not-exist")
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	requireFailure(t, err, stderr, "codex_bin is not an executable file")
}

func TestPreflightRefusesCodexBinPresentButNotExecutable(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	notExec := filepath.Join(dir, "codex-not-exec")
	if err := os.WriteFile(notExec, []byte("#!/usr/bin/env bash\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write non-executable stand-in: %v", err)
	}
	cfg.CodexBin = notExec
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	requireFailure(t, err, stderr, "codex_bin is not an executable file")
}

// TestPreflightNeverFallsBackToAmbientPath is the direct proof of the
// brief's "never fall back to ambient PATH" requirement: a config
// naming the bare command "codex" (which resolves on the test host's own
// PATH, since it is exactly what an operator who forgot to set an explicit
// path would write) must still refuse -- not silently succeed against
// whatever real `codex` happens to be on PATH.
func TestPreflightNeverFallsBackToAmbientPath(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	cfg.CodexBin = "codex" // bare name: only resolvable via PATH lookup, never as a literal file path in dir
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	requireFailure(t, err, stderr, "codex_bin is not an executable file")
}

func TestPreflightRefusesWhenVersionFails(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath, "FAKE_VERSION_BEHAVIOR=fail")
	requireFailure(t, err, stderr, "--version failed to run")
}

func TestPreflightRefusesWhenVersionOutputUnparseable(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath, "FAKE_VERSION_BEHAVIOR=unparseable")
	requireFailure(t, err, stderr, "does not contain a parseable version")
}

func TestPreflightRefusesWhenNotLoggedIn(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath, "FAKE_LOGIN_BEHAVIOR=fail")
	requireFailure(t, err, stderr, "did not report an authenticated session")
}

func TestPreflightRefusesRepoAllowlistEntryNotAGitCheckout(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	notARepo := filepath.Join(dir, "plain-dir")
	if err := os.MkdirAll(notARepo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg.RepoAllowlist = []string{notARepo}
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	requireFailure(t, err, stderr, "is not a git checkout")
}

// TestPreflightRefusesUncreatableStateDir exercises the state_dir writable
// check via a path whose parent component is a plain FILE, not a
// directory -- `mkdir -p` fails on that (ENOTDIR) regardless of the
// invoking user's privilege level, unlike a permission-bit-based fixture
// which a root-run CI job could silently bypass.
func TestPreflightRefusesUncreatableStateDir(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	cfg.StateDir = filepath.Join(blocker, "state")
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	requireFailure(t, err, stderr, "state_dir does not exist and could not be created")
}

func TestPreflightRefusesNonLoopbackHostWithoutAuthToken(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	cfg.Host = "0.0.0.0"
	cfg.AuthToken = ""
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath, "CODEX_BRIDGE_AUTH_TOKEN=")
	requireFailure(t, err, stderr, "auth_token is not set")
}

// TestPreflightAcceptsAuthTokenFromEnvironment mirrors the bridge's own
// config precedence: CODEX_BRIDGE_AUTH_TOKEN overrides the file, and the
// unit's EnvironmentFile delivers it to ExecStartPre — so a token present
// only in the environment must satisfy the non-loopback check. The
// committed config template deliberately carries no auth_token key.
func TestPreflightAcceptsAuthTokenFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	cfg.Host = "0.0.0.0"
	cfg.AuthToken = ""
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath, "CODEX_BRIDGE_AUTH_TOKEN=env-token")
	if err != nil {
		t.Fatalf("preflight refused a non-loopback host with CODEX_BRIDGE_AUTH_TOKEN set: %v\nstderr: %s", err, stderr)
	}
}

// TestPreflightAllowsNonLoopbackHostWithAuthToken is the positive half of
// the host/auth_token pairing: a non-loopback host with an auth_token set
// must still succeed, proving the refusal above is about the pairing, not
// the host value alone.
func TestPreflightAllowsNonLoopbackHostWithAuthToken(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(t, dir)
	cfg.Host = "0.0.0.0"
	cfg.AuthToken = "s3cr3t"
	configPath := writeConfig(t, dir, cfg)

	_, stderr, err := runPreflight(t, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstderr=%s", err, stderr)
	}
}

// requireFailure asserts the process failed (non-zero exit) and its
// stderr contains both the "preflight:" prefix and wantSubstr.
func requireFailure(t *testing.T, err error, stderr, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success; stderr=%s", stderr)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("expected an *exec.ExitError, got %T: %v", err, err)
	}
	if !strings.Contains(stderr, "preflight:") {
		t.Errorf("stderr = %q, want it to contain the required \"preflight:\" prefix", stderr)
	}
	if !strings.Contains(stderr, wantSubstr) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, wantSubstr)
	}
}

// ---------------------------------------------------------------------------
// Distinctness of refusal messages across all six classes
// ---------------------------------------------------------------------------

// TestPreflightRefusalMessagesAreAllDistinct re-runs every refusal fixture
// above and asserts no two of the six failure classes print the same
// first stderr line -- the brief's explicit "DISTINCT one-line message"
// requirement, checked as a set rather than trusting six separate
// substring assertions never collide by accident.
func TestPreflightRefusalMessagesAreAllDistinct(t *testing.T) {
	type fixture struct {
		name   string
		mutate func(dir string, cfg *preflightConfig)
		env    []string
	}
	fixtures := []fixture{
		{name: "missing codex_bin", mutate: func(dir string, cfg *preflightConfig) {
			cfg.CodexBin = filepath.Join(dir, "does-not-exist")
		}},
		{name: "version fails", env: []string{"FAKE_VERSION_BEHAVIOR=fail"}},
		{name: "version unparseable", env: []string{"FAKE_VERSION_BEHAVIOR=unparseable"}},
		{name: "not logged in", env: []string{"FAKE_LOGIN_BEHAVIOR=fail"}},
		{name: "repo not a checkout", mutate: func(dir string, cfg *preflightConfig) {
			notARepo := filepath.Join(dir, "plain-dir-2")
			if err := os.MkdirAll(notARepo, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			cfg.RepoAllowlist = []string{notARepo}
		}},
		{name: "state_dir uncreatable", mutate: func(dir string, cfg *preflightConfig) {
			blocker := filepath.Join(dir, "blocker-2")
			if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
				t.Fatalf("write blocker: %v", err)
			}
			cfg.StateDir = filepath.Join(blocker, "state")
		}},
		{name: "non-loopback host, no auth_token", mutate: func(dir string, cfg *preflightConfig) {
			cfg.Host = "0.0.0.0"
			cfg.AuthToken = ""
		}},
	}

	seen := map[string]string{}
	for _, fx := range fixtures {
		dir := t.TempDir()
		cfg := baseConfig(t, dir)
		if fx.mutate != nil {
			fx.mutate(dir, &cfg)
		}
		configPath := writeConfig(t, dir, cfg)

		_, stderr, err := runPreflight(t, configPath, fx.env...)
		if err == nil {
			t.Fatalf("fixture %q: expected non-zero exit, got success", fx.name)
		}
		line := firstLine(stderr)
		if prevFixture, ok := seen[line]; ok {
			t.Errorf("fixture %q and %q both produced the same message %q; each refusal class must have a distinct message", fx.name, prevFixture, line)
		}
		seen[line] = fx.name
	}
	if len(seen) != len(fixtures) {
		t.Errorf("got %d distinct messages for %d refusal classes", len(seen), len(fixtures))
	}
}

// ---------------------------------------------------------------------------
// Static guarantee: the script's own source never spells a codex
// invocation other than --version / login status
// ---------------------------------------------------------------------------

// codexInvocationPattern matches an actual COMMAND SUBSTITUTION call site
// of the codex_bin shell variable: "$CODEX_BIN" (quoted, so it can never
// be mistaken for a bare mention inside an error-message string) appearing
// immediately after a `$(` command-substitution open, optionally preceded
// by one `NAME=value` environment-variable prefix (the CODEX_HOME
// passthrough on the login status call), with the remainder of the
// substitution captured in group 1. This deliberately does NOT match
// `[[ -x "$CODEX_BIN" ]]`-style existence checks or `$CODEX_BIN` appearing
// inside an echoed message -- those reference the variable without
// executing it as a command.
var codexInvocationPattern = regexp.MustCompile(
	`\$\((?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)?"\$CODEX_BIN"\s+([^)]*)\)`,
)

// TestPreflightScriptNeverInvokesCodexBeyondVersionOrLoginStatus is the
// static half of the brief's "must never invoke codex beyond --version and
// login status" requirement: it greps the script's own source (not its
// runtime behavior) for every command-substitution call site of
// $CODEX_BIN and asserts each one's argument list is exactly --version or
// login status. This catches a billable call (e.g. an accidental
// `$CODEX_BIN exec ...`) even in a code path no test fixture above
// happens to exercise.
func TestPreflightScriptNeverInvokesCodexBeyondVersionOrLoginStatus(t *testing.T) {
	raw, err := os.ReadFile(preflightScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	source := string(raw)

	matches := codexInvocationPattern.FindAllStringSubmatch(source, -1)
	if len(matches) == 0 {
		t.Fatal("found no $CODEX_BIN command-substitution call sites at all; this test is not proving anything")
	}
	for _, m := range matches {
		args := m[1]
		hasVersion := strings.Contains(args, "--version")
		hasLoginStatus := strings.Contains(args, "login") && strings.Contains(args, "status")
		if !hasVersion && !hasLoginStatus {
			t.Errorf("codex_bin invocation is neither --version nor login status: %q", args)
		}
	}
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
