// Package deploytest, this file: task t4's manifest-as-Go-test check over
// deploy/prod/pi-preflight.sh, the non-billable deploy-time and
// ExecStartPre readiness probe an operator runs before pointing a pi actor
// bridge (adapters/pi, added later) at a production host. It is the exact
// shape of codexpreflight_test.go: a controllable fake `pi` binary written
// to t.TempDir() answers only the one invocation the script is allowed to
// make (`--version`), a fake `id` drives the account-confinement and
// ownership checks, an in-process httptest server stands in for the local
// model endpoint, and a fake ~/.pi/agent/models.json is dropped under an
// overridden HOME. Every one of the six checks the brief names gets its own
// refusal test asserting a DISTINCT one-line "preflight: ..." message on
// stderr plus a non-zero exit.
//
// "Non-billable" is the whole contract, same as codex: the script may only
// ever invoke the configured pi binary with `--version`, never a prompt or
// anything that would consume a turn. A dynamic guard (the fake pi exits 1
// with a marker on any other invocation) and a static source-text guard
// below both hold that line.
//
// This file lives in the same package (deploytest) as
// codexpreflight_test.go and deliberately reuses that file's generic
// helpers -- gitCheckout, writeFakeId/fakeIdScript, requireFailure,
// firstLine -- rather than redeclaring them. Everything pi-specific is
// named with a pi* / Pi* prefix so the two preflight suites never collide.
package deploytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// piPreflightScriptPath locates deploy/prod/pi-preflight.sh from this test
// file's own path (the runtime.Caller(0) technique the sibling preflight
// suite uses to stay independent of `go test`'s working directory).
func piPreflightScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to find pi-preflight.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	path := filepath.Join(repoRoot, "deploy", "prod", "pi-preflight.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pi-preflight.sh not found at %s: %v", path, err)
	}
	return path
}

// readPinnedPiVersion reads UNIX_USER_PI_VERSION out of
// deploy/prod/lanes/unix-user.sh -- the single source of truth for the pin
// that check 1 asserts pi --version against. The test reads it the same way
// the script does (never hardcoding a literal), so bumping the pin in one
// place keeps both the script and this suite honest.
func readPinnedPiVersion(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate unix-user.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	lane := filepath.Join(repoRoot, "deploy", "prod", "lanes", "unix-user.sh")
	raw, err := os.ReadFile(lane)
	if err != nil {
		t.Fatalf("read unix-user.sh: %v", err)
	}
	m := regexp.MustCompile(`(?m)^UNIX_USER_PI_VERSION=(\S+)`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("UNIX_USER_PI_VERSION not found in unix-user.sh")
	}
	return strings.TrimSpace(m[1])
}

// fakePiScript is a controllable stand-in for the real `pi` binary. It only
// answers the ONE invocation pi-preflight.sh is allowed to make:
//   - `--version` -- prints FAKE_PI_VERSION (default 0.0.0) when
//     FAKE_PI_BEHAVIOR is "ok", or fails when it is "fail".
//
// Anything else exits 1 with a marker string -- a defense-in-depth signal
// (independent of the static source-text assertion below) that the script
// under test never reaches for a pi subcommand that would consume a turn.
const fakePiScript = `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  --version)
    case "${FAKE_PI_BEHAVIOR:-ok}" in
      ok) echo "${FAKE_PI_VERSION:-0.0.0}"; exit 0 ;;
      fail) echo "fake-pi: --version boom" >&2; exit 1 ;;
    esac
    ;;
esac
echo "fake-pi: unexpected invocation: $*" >&2
exit 1
`

// writeFakePi writes fakePiScript to dir/pi, marks it executable, and
// returns its path.
func writeFakePi(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "pi")
	if err := os.WriteFile(path, []byte(fakePiScript), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return path
}

// writePiModelsJson writes a ~/.pi/agent/models.json under home naming
// exactly one provider with one model, the positive fixture for check 2.
// The shape mirrors the real file: {"providers": {"<name>": {"baseUrl":
// ..., "api": "openai-completions", "apiKey": ..., "models": [{"id":
// "<model>"}]}}}.
func writePiModelsJson(t *testing.T, home, provider, model string) {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	doc := map[string]any{
		"providers": map[string]any{
			provider: map[string]any{
				"baseUrl": "http://thor:8000/v1",
				"api":     "openai-completions",
				"apiKey":  "dummy",
				"models":  []any{map[string]any{"id": model}},
			},
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal models.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0o644); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
}

// newModelEndpoint starts an in-process HTTP server answering GET
// /v1/models with an OpenAI-shaped list containing exactly the given model
// ids. It returns the server (t.Cleanup handles Close) and its base URL,
// which the config's model_endpoint points at for check 3.
func newModelEndpoint(t *testing.T, status int, modelIDs ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, 0, len(modelIDs))
		for _, id := range modelIDs {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// piConfig is the JSON shape pi-preflight.sh reads -- the subset of
// pi-developer.json (added by t7) the preflight needs directly.
type piConfig struct {
	PiBin         string   `json:"pi_bin,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	ModelEndpoint string   `json:"model_endpoint,omitempty"`
	RepoAllowlist []string `json:"repo_allowlist"`
	StateDir      string   `json:"state_dir,omitempty"`
}

// writePiConfig marshals cfg to dir/pi-config.json and returns its path.
func writePiConfig(t *testing.T, dir string, cfg piConfig) string {
	t.Helper()
	path := filepath.Join(dir, "pi-config.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// piBaseConfig returns a fully-valid config plus the fixtures it points at:
// a fake pi answering --version with the pinned version, a fake models.json
// naming provider "lobe" with the configured model, a live model endpoint
// listing that model, a repo checkout owned by the test uid, and a
// creatable state dir. Each refusal test then mutates exactly one field (or
// flips one FAKE_* env) off this base. home is the HOME runPiPreflight
// overrides so the ~/.pi/agent/models.json read resolves under t.TempDir().
func piBaseConfig(t *testing.T, dir string) piConfig {
	t.Helper()
	fakePi := writeFakePi(t, dir)
	writeFakeId(t, dir)
	const provider = "lobe"
	const model = "unsloth/Qwen3.8-27B-NVFP4"
	writePiModelsJson(t, dir, provider, model)
	endpoint := newModelEndpoint(t, http.StatusOK, model)
	repo := gitCheckout(t, dir, "repo")
	return piConfig{
		PiBin:         fakePi,
		Provider:      provider,
		Model:         model,
		ModelEndpoint: endpoint,
		RepoAllowlist: []string{repo},
		StateDir:      filepath.Join(dir, "state"),
	}
}

// runPiPreflight runs pi-preflight.sh with the given config path and extra
// environment, returning stdout, stderr and the process error (non-nil on
// non-zero exit). Like the sibling codex runner it builds the child env as
// a KEY->value map so a FAKE_* override in extraEnv replaces rather than
// duplicates the default. dir (the config's own directory) is prepended to
// PATH so the fake `id` shadows the real one, and HOME is set to dir so the
// ~/.pi/agent/models.json read lands on the fake this suite wrote.
func runPiPreflight(t *testing.T, dir, configPath string, extraEnv ...string) (stdout, stderr string, err error) {
	t.Helper()
	script := piPreflightScriptPath(t)
	cmd := exec.Command(script, configPath)

	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			envMap[kv[:idx]] = kv[idx+1:]
		}
	}
	envMap["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
	envMap["HOME"] = dir
	// check 4/5 defaults: match the real test-process uid (so the ownership
	// half passes on the checkout piBaseConfig created) and a group list
	// with neither "sudo" nor "docker".
	envMap["FAKE_ID_UID"] = strconv.Itoa(os.Getuid())
	envMap["FAKE_ID_GROUPS"] = "users adm"
	// check 1 default: the fake pi reports the pin, so the happy path is a
	// statement about the config rather than about a literal baked here.
	envMap["FAKE_PI_VERSION"] = readPinnedPiVersion(t)
	for _, kv := range extraEnv {
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			envMap[kv[:idx]] = kv[idx+1:]
		}
	}

	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// ---------------------------------------------------------------------------
// Success path
// ---------------------------------------------------------------------------

func TestPiPreflightSuccessPrintsMeasuredVersion(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	stdout, stderr, err := runPiPreflight(t, dir, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstdout=%q stderr=%q", err, stdout, stderr)
	}
	want := "preflight: ok pi " + readPinnedPiVersion(t)
	if strings.TrimSpace(stdout) != want {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(stdout), want)
	}
}

// ---------------------------------------------------------------------------
// Check 1: pi_bin exists, is executable, and --version equals the pin
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesMissingPiBin(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.PiBin = filepath.Join(dir, "does-not-exist")
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "pi_bin is not an executable file")
}

// TestPiPreflightNeverFallsBackToAmbientPath proves check 1 tests pi_bin as
// a literal file path, never a PATH lookup: a config naming the bare command
// "pi" (which resolves against the fake on PATH runPiPreflight sets up) must
// still refuse rather than silently succeed.
func TestPiPreflightNeverFallsBackToAmbientPath(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.PiBin = "pi"
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "pi_bin is not an executable file")
}

func TestPiPreflightRefusesWhenVersionFails(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_PI_BEHAVIOR=fail")
	requireFailure(t, err, stderr, "pi --version failed to run")
}

func TestPiPreflightRefusesVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_PI_VERSION=0.0.1")
	requireFailure(t, err, stderr, "does not equal the pinned version")
}

// ---------------------------------------------------------------------------
// Check 2: ~/.pi/agent/models.json names the provider + model
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesMissingModelsJson(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	// Remove the fixture written by piBaseConfig.
	if err := os.Remove(filepath.Join(dir, ".pi", "agent", "models.json")); err != nil {
		t.Fatalf("remove models.json: %v", err)
	}
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "models.json")
}

func TestPiPreflightRefusesProviderNotInModelsJson(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.Provider = "no-such-provider"
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "does not name provider")
}

func TestPiPreflightRefusesModelNotUnderProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.Model = "some/other-model"
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "does not list model")
}

// ---------------------------------------------------------------------------
// Check 3: GET model_endpoint/v1/models returns 200 and lists the model
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesEndpointModelNotListed(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	// A 200 endpoint that lists a DIFFERENT model than the config's.
	cfg.ModelEndpoint = newModelEndpoint(t, http.StatusOK, "someone/else")
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "model endpoint")
}

func TestPiPreflightRefusesEndpointNon200(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.ModelEndpoint = newModelEndpoint(t, http.StatusInternalServerError, cfg.Model)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "model endpoint")
}

// TestPiPreflightSkipEndpointCheckDowngradesToWarning covers
// SKIP_PI_ENDPOINT_CHECK=1: the endpoint is unreachable (a dead URL), yet
// the deploy succeeds and the skip is announced on stderr as a warning --
// the bootstrap-ordering escape the brief requires.
func TestPiPreflightSkipEndpointCheckDowngradesToWarning(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.ModelEndpoint = "http://127.0.0.1:1/dead" // nothing listens on port 1
	configPath := writePiConfig(t, dir, cfg)

	stdout, stderr, err := runPiPreflight(t, dir, configPath, "SKIP_PI_ENDPOINT_CHECK=1")
	if err != nil {
		t.Fatalf("expected success with SKIP_PI_ENDPOINT_CHECK=1, got error %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stderr, "SKIP_PI_ENDPOINT_CHECK") {
		t.Errorf("stderr = %q, want it to announce the skipped endpoint check", stderr)
	}
	if !strings.Contains(stdout, "preflight: ok pi") {
		t.Errorf("stdout = %q, want the success line", stdout)
	}
}

// ---------------------------------------------------------------------------
// Check 4: every repo_allowlist path exists and is owned by the running uid
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesMissingAllowlistPath(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	cfg.RepoAllowlist = []string{filepath.Join(dir, "not-there")}
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "does not exist")
}

func TestPiPreflightRefusesRepoOwnedByAnotherUid(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_ID_UID=1")
	requireFailure(t, err, stderr, "not owned by the running user")
}

// ---------------------------------------------------------------------------
// Check 5: id -nG contains neither sudo nor docker (account confinement)
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesSudoGroupMembership(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_ID_GROUPS=sudo users")
	requireFailure(t, err, stderr, "sudo")
}

func TestPiPreflightRefusesDockerGroupMembership(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_ID_GROUPS=users docker adm")
	requireFailure(t, err, stderr, "docker")
}

// TestPiPreflightAllowsGroupNameThatOnlyContainsSudoAsSubstring proves the
// group check matches whole group names, not a substring.
func TestPiPreflightAllowsGroupNameThatOnlyContainsSudoAsSubstring(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath, "FAKE_ID_GROUPS=users sudoers-readonly dockerish")
	if err != nil {
		t.Fatalf("expected success (neither group is an exact match), got error %v\nstderr=%s", err, stderr)
	}
}

// ---------------------------------------------------------------------------
// Check 6: state_dir exists or can be created, mode 700
// ---------------------------------------------------------------------------

func TestPiPreflightRefusesUncreatableStateDir(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	cfg.StateDir = filepath.Join(blocker, "state")
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	requireFailure(t, err, stderr, "state_dir")
}

// TestPiPreflightCreatesStateDirMode700 proves a freshly-created state_dir
// lands at mode 700, the confinement mode the brief requires.
func TestPiPreflightCreatesStateDirMode700(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	stateDir := filepath.Join(dir, "fresh-state")
	cfg.StateDir = stateDir
	configPath := writePiConfig(t, dir, cfg)

	_, stderr, err := runPiPreflight(t, dir, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstderr=%s", err, stderr)
	}
	info, statErr := os.Stat(stateDir)
	if statErr != nil {
		t.Fatalf("state_dir was not created: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state_dir mode = %o, want 700", perm)
	}
}

// ---------------------------------------------------------------------------
// Distinctness of refusal messages across the check classes
// ---------------------------------------------------------------------------

func TestPiPreflightRefusalMessagesAreAllDistinct(t *testing.T) {
	type fixture struct {
		name   string
		mutate func(t *testing.T, dir string, cfg *piConfig)
		env    []string
	}
	fixtures := []fixture{
		{name: "missing pi_bin", mutate: func(_ *testing.T, dir string, cfg *piConfig) {
			cfg.PiBin = filepath.Join(dir, "does-not-exist")
		}},
		{name: "version fails", env: []string{"FAKE_PI_BEHAVIOR=fail"}},
		{name: "version mismatch", env: []string{"FAKE_PI_VERSION=0.0.1"}},
		{name: "missing models.json", mutate: func(t *testing.T, dir string, _ *piConfig) {
			if err := os.Remove(filepath.Join(dir, ".pi", "agent", "models.json")); err != nil {
				t.Fatalf("remove models.json: %v", err)
			}
		}},
		{name: "provider absent", mutate: func(_ *testing.T, _ string, cfg *piConfig) {
			cfg.Provider = "no-such-provider"
		}},
		{name: "model absent", mutate: func(_ *testing.T, _ string, cfg *piConfig) {
			cfg.Model = "some/other-model"
		}},
		{name: "endpoint model not listed", mutate: func(t *testing.T, _ string, cfg *piConfig) {
			cfg.ModelEndpoint = newModelEndpoint(t, http.StatusOK, "someone/else")
		}},
		{name: "allowlist path missing", mutate: func(_ *testing.T, dir string, cfg *piConfig) {
			cfg.RepoAllowlist = []string{filepath.Join(dir, "not-there")}
		}},
		{name: "repo not owned by uid", env: []string{"FAKE_ID_UID=1"}},
		{name: "sudo group", env: []string{"FAKE_ID_GROUPS=sudo users"}},
		{name: "uncreatable state_dir", mutate: func(t *testing.T, dir string, cfg *piConfig) {
			blocker := filepath.Join(dir, "blocker-x")
			if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
				t.Fatalf("write blocker: %v", err)
			}
			cfg.StateDir = filepath.Join(blocker, "state")
		}},
	}

	seen := map[string]string{}
	for _, fx := range fixtures {
		dir := t.TempDir()
		cfg := piBaseConfig(t, dir)
		if fx.mutate != nil {
			fx.mutate(t, dir, &cfg)
		}
		configPath := writePiConfig(t, dir, cfg)

		_, stderr, err := runPiPreflight(t, dir, configPath, fx.env...)
		if err == nil {
			t.Fatalf("fixture %q: expected non-zero exit, got success", fx.name)
		}
		line := firstLine(stderr)
		if prev, ok := seen[line]; ok {
			t.Errorf("fixture %q and %q both produced %q; each refusal class must have a distinct message", fx.name, prev, line)
		}
		seen[line] = fx.name
	}
	if len(seen) != len(fixtures) {
		t.Errorf("got %d distinct messages for %d refusal classes", len(seen), len(fixtures))
	}
}

// ---------------------------------------------------------------------------
// Non-billable guarantees: no billable pi call, no container references
// ---------------------------------------------------------------------------

// piInvocationPattern matches an actual command-substitution call site of
// the pi_bin shell variable -- "$PI_BIN" (quoted) immediately after a `$(`
// open, with the remainder captured. It does NOT match `[[ -x "$PI_BIN" ]]`
// existence checks or the variable inside an echoed message.
var piInvocationPattern = regexp.MustCompile(`\$\(\s*"\$PI_BIN"\s+([^)]*)\)`)

// TestPiPreflightScriptNeverInvokesPiBeyondVersion is the static half of the
// non-billable contract: every command-substitution call site of $PI_BIN in
// the script's own source must be exactly `--version`.
func TestPiPreflightScriptNeverInvokesPiBeyondVersion(t *testing.T) {
	raw, err := os.ReadFile(piPreflightScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	matches := piInvocationPattern.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("found no $PI_BIN command-substitution call sites at all; this test is not proving anything")
	}
	for _, m := range matches {
		if !strings.Contains(m[1], "--version") {
			t.Errorf("pi_bin invocation is not --version: %q", m[1])
		}
	}
}

// TestPiPreflightIsNonBillableAtRuntime holds the fake pi's marker line to
// the same contract dynamically: a happy-path run must never trip the fake
// pi's "unexpected invocation" guard.
func TestPiPreflightIsNonBillableAtRuntime(t *testing.T) {
	dir := t.TempDir()
	cfg := piBaseConfig(t, dir)
	configPath := writePiConfig(t, dir, cfg)

	stdout, stderr, err := runPiPreflight(t, dir, configPath)
	if err != nil {
		t.Fatalf("expected success, got error %v\nstderr=%s", err, stderr)
	}
	if strings.Contains(stdout+stderr, "fake-pi: unexpected invocation") {
		t.Errorf("the script invoked pi beyond --version; output = %q", stdout+stderr)
	}
}

// TestPiPreflightScriptHasNoContainerReferences enforces the brief's
// absolute rule: no Dockerfile, bwrap, or container reference anywhere in
// the pi preflight script or its comments. pi has no built-in sandbox
// (spec s6); the account is the confinement (check 5).
func TestPiPreflightScriptHasNoContainerReferences(t *testing.T) {
	raw, err := os.ReadFile(piPreflightScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"dockerfile", "bwrap", "bubblewrap", "container", "userns", "unshare"} {
		if strings.Contains(lower, banned) {
			t.Errorf("pi-preflight.sh must contain no %q reference (pi has no sandbox; the account is the confinement)", banned)
		}
	}
}
