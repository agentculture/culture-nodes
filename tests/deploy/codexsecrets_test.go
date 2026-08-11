// Package deploytest (this file) statically checks
// deploy/prod/install-secrets.sh's codex-bridge token lane against the
// same argv-only-ssh, stdin-only-secret discipline the rest of that
// script already follows: secrets ride ssh stdin into umask-077 files,
// never an ssh argv, and a re-run refuses to overwrite an existing
// codex-bridge.env without FORCE=1.
//
// These are text assertions over the script itself, not a live ssh run
// (plan t4) — the same "cheaper and more honest as static checks" call
// compose_test.go and helm_test.go both make for their own manifests.
package deploytest

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// installSecretsPath locates deploy/prod/install-secrets.sh from this test
// file's own path (tests/deploy/codexsecrets_test.go -> tests/deploy ->
// tests -> repo root -> deploy/prod/install-secrets.sh), the same
// runtime.Caller(0) technique compose_test.go's composeFilePath uses to
// stay independent of the working directory `go test` is invoked from.
func installSecretsPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to load install-secrets.sh")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "deploy", "prod", "install-secrets.sh")
}

func readInstallSecrets(t *testing.T) string {
	t.Helper()
	path := installSecretsPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// codexTokenVars are the two per-host bridge tokens the codex-bridge lane
// generates locally. Neither name may ever appear at or after the "ssh"
// keyword on a line -- that's the portion of the line that becomes ssh's
// own argv. Before "ssh" (the printf/pipe that feeds its stdin) is exactly
// where secret material is expected, mirroring the pre-existing
// $content / $NODES_RUNNER_SECRET_* pattern earlier in the same script.
var codexTokenVars = []string{"CODEX_BRIDGE_TOKEN_THOR", "CODEX_BRIDGE_TOKEN_ORIN"}

// remoteBlockPattern captures the single-quoted remote command string of
// an `ssh "$host" '...'`-shaped invocation, used by several tests below to
// inspect what actually runs on the remote side.
// An optional double-quoted prefix ("FORCE=${FORCE:-0}; ") may precede the
// single-quoted block — that is how the local FORCE value reaches the
// remote guard, since ssh forwards no environment variables.
var remoteBlockPattern = regexp.MustCompile(`ssh\s+"\$\w+"\s+(?:"[^"]*"\s*)?'([^']*)'`)

// TestCodexSecretsTokensNeverRideSSHArgv is the core discipline check:
// scan every physical line of the script, and for any line that invokes
// ssh, assert the codex-bridge token variable names never appear in the
// portion of the line at or after the "ssh" keyword. That portion is what
// becomes ssh's own argv; a token appearing there would mean the secret
// rode argv instead of stdin.
func TestCodexSecretsTokensNeverRideSSHArgv(t *testing.T) {
	script := readInstallSecrets(t)

	referenced := false
	for _, tok := range codexTokenVars {
		if strings.Contains(script, tok) {
			referenced = true
		}
	}
	if !referenced {
		t.Fatal("install-secrets.sh never references a codex-bridge token variable; the codex-bridge lane appears to be missing")
	}

	lines := strings.Split(script, "\n")
	checked := 0
	for i, line := range lines {
		idx := strings.Index(line, "ssh")
		if idx == -1 {
			continue
		}
		checked++
		argvPortion := line[idx:]
		for _, tok := range codexTokenVars {
			if strings.Contains(argvPortion, tok) {
				t.Errorf("line %d: %q references %s at/after the \"ssh\" keyword; secret material must ride ssh stdin only, never argv", i+1, line, tok)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ssh invocation found in install-secrets.sh; this test is not proving anything")
	}
}

// TestCodexSecretsBridgeEnvSetsUmask077 asserts every remote command block
// that writes codex-bridge.env sets `umask 077` before it does, so the
// file lands mode 0600 regardless of the remote shell's default umask —
// the same guarantee install_env already gives prod.env.
func TestCodexSecretsBridgeEnvSetsUmask077(t *testing.T) {
	script := readInstallSecrets(t)
	blocks := codexBridgeEnvBlocks(t, script)
	for _, remote := range blocks {
		if !strings.Contains(remote, "umask 077") {
			t.Errorf("remote command writing codex-bridge.env does not set umask 077 before writing: %q", remote)
		}
		umaskIdx := strings.Index(remote, "umask 077")
		writeIdx := strings.Index(remote, "> ~/.culture-nodes/codex-bridge.env")
		if umaskIdx == -1 || writeIdx == -1 || umaskIdx > writeIdx {
			t.Errorf("umask 077 does not precede the codex-bridge.env write in remote command: %q", remote)
		}
	}
}

// TestCodexSecretsBridgeEnvRefusesOverwriteWithoutForce asserts the
// codex-bridge.env write carries the same FORCE=1 overwrite guard
// prod.env's install_env already carries, so a re-run never silently
// rotates a live bridge token.
func TestCodexSecretsBridgeEnvRefusesOverwriteWithoutForce(t *testing.T) {
	script := readInstallSecrets(t)
	blocks := codexBridgeEnvBlocks(t, script)
	for _, remote := range blocks {
		if !strings.Contains(remote, "FORCE") {
			t.Errorf("remote command writing codex-bridge.env carries no FORCE guard: %q", remote)
		}
		if !strings.Contains(remote, "codex-bridge.env") {
			t.Errorf("remote command does not reference codex-bridge.env at all: %q", remote)
		}
		// The guard must actually gate the write with an existence check,
		// not just mention FORCE in a comment somewhere else in the block.
		if !strings.Contains(remote, "-e ~/.culture-nodes/codex-bridge.env") {
			t.Errorf("remote command has no existence check on codex-bridge.env ahead of the FORCE guard: %q", remote)
		}
	}
}

// TestCodexSecretsActorTokensReachBothHostsProdEnv asserts both
// NODES_ACTOR_CODEX_THOR_TOKEN and NODES_ACTOR_CODEX_ORIN_TOKEN are wired
// into prod.env for both THOR and ORIN -- either worker may dispatch
// either host's codex actor, so both hosts need both tokens.
func TestCodexSecretsActorTokensReachBothHostsProdEnv(t *testing.T) {
	script := readInstallSecrets(t)

	for _, key := range []string{"NODES_ACTOR_CODEX_THOR_TOKEN", "NODES_ACTOR_CODEX_ORIN_TOKEN"} {
		if !strings.Contains(script, key) {
			t.Errorf("install-secrets.sh does not reference %s", key)
		}
	}

	// The prod.env update helper must iterate BOTH hosts for every key it
	// installs — either worker may dispatch either host's codex actor.
	loop := regexp.MustCompile(`for\s+host\s+in\s+"\$THOR"\s+"\$ORIN"`)
	if !loop.MatchString(script) {
		t.Error(`update_actor_token_line does not loop over both "$THOR" and "$ORIN"`)
	}
	// And each fresh bridge token must feed exactly that helper — a kept
	// (exit-3) token never reaches prod.env with a regenerated value.
	for _, key := range []string{"NODES_ACTOR_CODEX_THOR_TOKEN", "NODES_ACTOR_CODEX_ORIN_TOKEN"} {
		call := regexp.MustCompile(`update_actor_token_line\s+` + key)
		if !call.MatchString(script) {
			t.Errorf("no update_actor_token_line call for %s found", key)
		}
	}
}

// codexBridgeEnvBlocks returns the single-quoted remote command strings of
// every ssh invocation that writes codex-bridge.env.
func codexBridgeEnvBlocks(t *testing.T, script string) []string {
	t.Helper()
	var blocks []string
	for _, m := range remoteBlockPattern.FindAllStringSubmatch(script, -1) {
		if strings.Contains(m[1], "codex-bridge.env") {
			blocks = append(blocks, m[1])
		}
	}
	if len(blocks) == 0 {
		t.Fatal("no ssh remote-command block writing codex-bridge.env found")
	}
	return blocks
}
