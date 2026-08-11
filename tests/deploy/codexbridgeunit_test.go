// Package deploytest -- this file is task t2's (codex-bridges-thor-orin
// plan): definition tests for deploy/prod/codex-bridge.service and
// deploy/prod/codex-bridge.json.template, the systemd user unit + per-host
// config template that run a managed Codex actor bridge beside the
// containerized worker on thor/orin. Modeled on nodes-runner.service and
// proven the same way TestNoServiceMountsTheDockerSocket in compose_test.go
// proves its own prose claims: parse the real files, assert the properties
// t2's acceptance criteria promise, fail loudly if either file drifts.
//
// codex-bridge.service is parsed as a plain systemd unit file -- INI-shaped
// (`[Section]` headers, `Key=Value` lines, `#`/`;` comments) but not
// standard INI (repeated keys are meaningful, e.g. multiple Environment=
// lines), so this file hand-rolls a minimal line scanner rather than
// pulling in an INI library for a handful of directives.
//
// codex-bridge.json.template is parsed with encoding/json as committed --
// the __HOME__ placeholder tokens are left untouched, not substituted.
// This test checks the *shape* of the template (the placeholder marker
// itself, not a resolved filesystem path), and __HOME__/... is a
// syntactically valid JSON string either way, so decoding the file as-is
// is sufficient.
//
// The auth_token precedence this template relies on -- env
// (CODEX_BRIDGE_AUTH_TOKEN, wired through EnvironmentFile in the .service)
// overrides file config, and Config.auth_token defaults to None when the
// file omits the key entirely -- is adapters/codex/src/codex_bridge/
// config.py's own documented contract (see its module docstring:
// "environment variables (CODEX_BRIDGE_*) override individual fields on
// top of it", and _ENV_STRING_FIELDS mapping CODEX_BRIDGE_AUTH_TOKEN ->
// auth_token). JSON has no comment syntax to cite that inline in the
// template itself -- _coerce_file_fields in config.py rejects any key
// outside its known _FILE_FIELDS table, so a "_comment" pseudo-key would
// itself fail to load -- which is exactly why this test, not a JSON
// comment, is the citation: TestBridgeUnitJSONTemplateCarriesNoAuthTokenKey below
// pins the absence, and this file's own header pins the reason why that is
// safe rather than an oversight.
package deploytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// codexBridgeDir locates deploy/prod from this test file's own path
// (tests/deploy/codexbridgeunit_test.go -> tests/deploy -> tests -> repo
// root -> deploy/prod), the same runtime.Caller(0) technique
// composeFilePath in compose_test.go and chartDir in helm_test.go both use.
func codexBridgeDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "deploy", "prod")
}

// unitFile is a minimal parse of a systemd unit file: every "Key=Value"
// line found anywhere in the file (section headers are not tracked --
// this unit has exactly one [Service] block, and directive names like
// ExecStartPre/EnvironmentFile/Restart/RestartSec are unambiguous without
// section scoping). Repeated keys are kept in encounter order since
// systemd itself treats some directives (e.g. multiple ExecStartPre=) as
// cumulative rather than last-wins.
type unitFile struct {
	raw    string
	values map[string][]string
}

func loadUnitFile(t *testing.T) unitFile {
	t.Helper()
	path := filepath.Join(codexBridgeDir(t), "codex-bridge.service")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	uf := unitFile{raw: string(raw), values: map[string][]string{}}
	for _, line := range strings.Split(uf.raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		uf.values[key] = append(uf.values[key], value)
	}
	if len(uf.values) == 0 {
		t.Fatalf("%s: no Key=Value directives parsed; this test is not proving anything", path)
	}
	return uf
}

func (uf unitFile) first(key string) (string, bool) {
	vals, ok := uf.values[key]
	if !ok || len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// TestBridgeUnitRunsPreflightAsExecStartPre asserts the preflight (t1's
// deploy/prod/codex-preflight.sh, installed at
// %h/.culture-nodes/bin/codex-preflight.sh) gates startup: an
// ExecStartPre directive naming that installed path, taking the bridge
// config path as its argument. A failing preflight makes systemd refuse
// to run ExecStart at all -- this is the "startup refused on failure"
// half of t2's acceptance criteria (t1 supplies the distinct-message
// failure behavior itself; this test only proves the unit wires it in).
func TestBridgeUnitRunsPreflightAsExecStartPre(t *testing.T) {
	uf := loadUnitFile(t)

	execStartPre, ok := uf.first("ExecStartPre")
	if !ok {
		t.Fatal("codex-bridge.service declares no ExecStartPre; the non-billable preflight (t1) must gate startup")
	}
	if !strings.Contains(execStartPre, ".culture-nodes/bin/codex-preflight.sh") {
		t.Errorf("ExecStartPre=%q does not reference the installed preflight path (.culture-nodes/bin/codex-preflight.sh)", execStartPre)
	}
	if !strings.Contains(execStartPre, "codex-bridge.json") {
		t.Errorf("ExecStartPre=%q does not pass the bridge config path to the preflight", execStartPre)
	}
}

// TestBridgeUnitAlwaysRestarts asserts Restart=always, per t2's acceptance
// criteria -- distinct from nodes-runner.service's Restart=on-failure,
// since the bridge is a long-running server process the operator wants
// back regardless of how it exited (auth expiry, OOM, etc.), not only on
// a nonzero exit.
func TestBridgeUnitAlwaysRestarts(t *testing.T) {
	uf := loadUnitFile(t)

	restart, ok := uf.first("Restart")
	if !ok {
		t.Fatal("codex-bridge.service declares no Restart=")
	}
	if restart != "always" {
		t.Errorf("Restart=%q, want %q", restart, "always")
	}
	if _, ok := uf.first("RestartSec"); !ok {
		t.Error("codex-bridge.service declares Restart= but no RestartSec=; a bare Restart=always with no backoff can hot-loop")
	}
}

// TestBridgeUnitCarriesTokenOnlyViaEnvironmentFile asserts the bearer token
// rides EnvironmentFile (%h/.culture-nodes/codex-bridge.env), never a
// literal Environment= line or any token-shaped value inline in the unit
// itself -- mirroring nodes-runner.service's own EnvironmentFile
// convention ("the secret file keeps the bearer out of the environment
// listing entirely").
func TestBridgeUnitCarriesTokenOnlyViaEnvironmentFile(t *testing.T) {
	uf := loadUnitFile(t)

	envFile, ok := uf.first("EnvironmentFile")
	if !ok {
		t.Fatal("codex-bridge.service declares no EnvironmentFile=")
	}
	if !strings.Contains(envFile, ".culture-nodes/codex-bridge.env") {
		t.Errorf("EnvironmentFile=%q does not point at %s", envFile, ".culture-nodes/codex-bridge.env")
	}
	if _, ok := uf.values["Environment"]; ok {
		t.Error("codex-bridge.service declares a literal Environment= line; the token must ride EnvironmentFile only")
	}
	assertNoTokenLiteral(t, "codex-bridge.service", uf.raw)
}

// TestBridgeUnitStartsTheBridgeBinary asserts ExecStart runs the uv-tool-installed
// codex-bridge with the installed config path, and that the unit is wired
// to come up after networking and be enabled by default -- the other
// install-time contract points t3's deploy lane depends on.
func TestBridgeUnitStartsTheBridgeBinary(t *testing.T) {
	uf := loadUnitFile(t)

	execStart, ok := uf.first("ExecStart")
	if !ok {
		t.Fatal("codex-bridge.service declares no ExecStart=")
	}
	if !strings.Contains(execStart, ".local/bin/codex-bridge") {
		t.Errorf("ExecStart=%q does not run the uv-tool-installed codex-bridge (.local/bin/codex-bridge)", execStart)
	}
	if !strings.Contains(execStart, "codex-bridge.json") {
		t.Errorf("ExecStart=%q does not pass --config codex-bridge.json", execStart)
	}

	after, ok := uf.first("After")
	if !ok || !strings.Contains(after, "network-online.target") {
		t.Errorf("After=%q does not include network-online.target", after)
	}

	wantedBy, ok := uf.first("WantedBy")
	if !ok || !strings.Contains(wantedBy, "default.target") {
		t.Errorf("WantedBy=%q does not include default.target", wantedBy)
	}
}

// --- codex-bridge.json.template -----------------------------------------

// bridgeConfigTemplate is the subset of the JSON template's shape this
// test cares about. RawAuthToken/RawFields captures the full decoded map
// too, so TestBridgeUnitJSONTemplateCarriesNoAuthTokenKey can assert the "auth_token"
// key is entirely absent rather than merely empty (an explicit
// `"auth_token": ""` would still satisfy a typed zero-value check but
// would defeat the point -- config.py's env override only matters when the
// file has no opinion on the field at all).
type bridgeConfigTemplate struct {
	CodexBin       string            `json:"codex_bin"`
	CodexEnv       map[string]string `json:"codex_env"`
	Host           string            `json:"host"`
	Port           int               `json:"port"`
	AlwaysAsync    bool              `json:"always_async"`
	DefaultSandbox string            `json:"default_sandbox"`
	StateDir       string            `json:"state_dir"`
	RepoAllowlist  []string          `json:"repo_allowlist"`
}

func loadBridgeConfigTemplate(t *testing.T) (bridgeConfigTemplate, map[string]json.RawMessage, string) {
	t.Helper()
	path := filepath.Join(codexBridgeDir(t), "codex-bridge.json.template")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var typed bridgeConfigTemplate
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatalf("parse %s as JSON: %v", path, err)
	}

	var loose map[string]json.RawMessage
	if err := json.Unmarshal(raw, &loose); err != nil {
		t.Fatalf("parse %s as a loose JSON object: %v", path, err)
	}

	return typed, loose, string(raw)
}

// TestBridgeUnitJSONTemplatePinsExplicitCodexBinAndPort asserts codex_bin is the
// explicit __HOME__/.local/bin/codex path (never a bare "codex" that would
// resolve through PATH at whatever the unit's PATH happens to be), and
// port is 8086 -- codex-bridge's own default (config.py: "Different
// default than colleague-bridge's 8085 so both can run on one host
// without colliding"), pinned explicitly here so a future change to that
// Python default doesn't silently drift the deployed template out from
// under it.
func TestBridgeUnitJSONTemplatePinsExplicitCodexBinAndPort(t *testing.T) {
	cfg, _, _ := loadBridgeConfigTemplate(t)

	const wantCodexBin = "__HOME__/.local/bin/codex"
	if cfg.CodexBin != wantCodexBin {
		t.Errorf("codex_bin = %q, want %q (explicit path, never bare \"codex\")", cfg.CodexBin, wantCodexBin)
	}
	if cfg.Port != 8086 {
		t.Errorf("port = %d, want 8086", cfg.Port)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf(`host = %q, want "0.0.0.0"`, cfg.Host)
	}
}

// TestBridgeUnitJSONTemplatePinsCodexHomeEnv asserts codex_env.CODEX_HOME is set to
// the per-host __HOME__/.codex path -- the codex auth profile the bridge's
// subprocess must see (config.py's codex_env docstring: "Extra env vars
// merged onto the subprocess environment (e.g. CODEX_HOME to point at a
// specific auth profile)").
func TestBridgeUnitJSONTemplatePinsCodexHomeEnv(t *testing.T) {
	cfg, _, _ := loadBridgeConfigTemplate(t)

	const wantCodexHome = "__HOME__/.codex"
	got, ok := cfg.CodexEnv["CODEX_HOME"]
	if !ok {
		t.Fatal("codex_env has no CODEX_HOME entry")
	}
	if got != wantCodexHome {
		t.Errorf("codex_env.CODEX_HOME = %q, want %q", got, wantCodexHome)
	}
}

// TestBridgeUnitJSONTemplatePinsAlwaysAsyncAndReadOnlySandbox asserts the two policy
// knobs t2's task names explicitly: always_async=true (every dispatch
// goes through the async path regardless of step-budget threshold), and
// default_sandbox="read-only" -- the q3 decision that production codex
// nodes default read-only, with write tasks opting in explicitly per
// node.
func TestBridgeUnitJSONTemplatePinsAlwaysAsyncAndReadOnlySandbox(t *testing.T) {
	cfg, _, _ := loadBridgeConfigTemplate(t)

	if !cfg.AlwaysAsync {
		t.Error("always_async = false, want true")
	}
	if cfg.DefaultSandbox != "read-only" {
		t.Errorf(`default_sandbox = %q, want "read-only" (q3: production codex nodes default read-only)`, cfg.DefaultSandbox)
	}
}

// TestBridgeUnitJSONTemplatePinsStateDirAndAllowlist asserts state_dir is the
// durable per-host path and repo_allowlist is exactly the one agent
// checkout the bridge is scoped to -- config.py's repo_allowed() refuses
// any input.repo outside this list with 403, so an allowlist with more
// than the intended entry (or the wrong one) would silently widen what
// the bridge will touch.
func TestBridgeUnitJSONTemplatePinsStateDirAndAllowlist(t *testing.T) {
	cfg, _, _ := loadBridgeConfigTemplate(t)

	const wantStateDir = "__HOME__/.culture-nodes/codex-bridge-state"
	if cfg.StateDir != wantStateDir {
		t.Errorf("state_dir = %q, want %q", cfg.StateDir, wantStateDir)
	}

	wantAllowlist := []string{"__HOME__/git/culture-nodes-agent"}
	if len(cfg.RepoAllowlist) != len(wantAllowlist) {
		t.Fatalf("repo_allowlist = %v, want exactly %v", cfg.RepoAllowlist, wantAllowlist)
	}
	for i, want := range wantAllowlist {
		if cfg.RepoAllowlist[i] != want {
			t.Errorf("repo_allowlist[%d] = %q, want %q", i, cfg.RepoAllowlist[i], want)
		}
	}
}

// TestBridgeUnitJSONTemplateCarriesNoAuthTokenKey asserts the "auth_token" key is
// entirely absent from the committed template -- the token rides
// CODEX_BRIDGE_AUTH_TOKEN via the unit's EnvironmentFile instead (see this
// file's header comment for config.py's documented env-overrides-file
// precedence, which is exactly what makes this split safe: Config.load
// applies file fields first, then _apply_env_overrides sets auth_token
// from the env var unconditionally when present, so a file that never
// mentions the field cannot conflict with or shadow the env-supplied
// value).
func TestBridgeUnitJSONTemplateCarriesNoAuthTokenKey(t *testing.T) {
	_, loose, _ := loadBridgeConfigTemplate(t)

	if _, present := loose["auth_token"]; present {
		t.Error(`codex-bridge.json.template declares an "auth_token" key; the token must ride CODEX_BRIDGE_AUTH_TOKEN via EnvironmentFile only, never a committed template`)
	}
}

// TestBridgeUnitJSONTemplateHasNoTokenLiteral extends the structural
// no-auth_token-key check with a substring scan for anything token/secret
// shaped, matching TestNoServiceMountsTheDockerSocket's "strongest and
// simplest form of the guarantee" style in compose_test.go.
func TestBridgeUnitJSONTemplateHasNoTokenLiteral(t *testing.T) {
	_, _, raw := loadBridgeConfigTemplate(t)
	assertNoTokenLiteral(t, "codex-bridge.json.template", raw)
}

// --- shared helpers -------------------------------------------------------

// tokenLiteralPattern flags anything that looks like a secret/token
// key or a plausible bearer-token-shaped value (long base64/hex runs),
// so both the unit file and the JSON template are covered by the same
// scan regardless of which one might accidentally grow an inline secret.
var tokenLiteralPattern = regexp.MustCompile(`(?i)(auth[_-]?token|secret|bearer)\s*[:=]`)

func assertNoTokenLiteral(t *testing.T, filename, content string) {
	t.Helper()
	if loc := tokenLiteralPattern.FindString(content); loc != "" {
		t.Errorf("%s contains a token/secret-shaped literal (%q); the token must ride CODEX_BRIDGE_AUTH_TOKEN via EnvironmentFile only", filename, loc)
	}
}
