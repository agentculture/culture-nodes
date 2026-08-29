package deploytest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBridgeUnitsReadWorkerPushCredentialFile(t *testing.T) {
	dir := prodComposeDir(t)
	units := []string{
		"culture-nodes-claude-intake.service",
		"culture-nodes-claude-planner.service",
		"culture-nodes-claude-developer.service",
		"culture-nodes-claude-verifier.service",
		"codex-bridge.service",
		"culture-nodes-qwen-developer.service",
	}
	for _, unit := range units {
		raw, err := os.ReadFile(filepath.Join(dir, unit))
		if err != nil {
			t.Errorf("read %s: %v", unit, err)
			continue
		}
		if !strings.Contains(string(raw), "%h/.culture-nodes/bridge-push.env") {
			t.Errorf("%s does not load the unattended bridge push credential", unit)
		}
	}
}

func TestWorkerPushCredentialIsRelayedToRegisteredActorHost(t *testing.T) {
	script := readInstallSecrets(t)
	if !strings.Contains(script, "GITHUB_TOKEN_WORKER") {
		t.Fatal("install-secrets.sh has no GITHUB_TOKEN_WORKER relay")
	}
	if !regexp.MustCompile(`actor_registration\s+[^\n]*CLAUDE`).MatchString(script) {
		t.Error("worker push credential target is not derived from the Claude actor registration")
	}
	if !strings.Contains(script, "bridge-push.env") || !strings.Contains(script, "chmod 600") {
		t.Error("worker push credential is not installed into a mode-600 bridge-push.env")
	}
	if strings.Contains(script, "GITHUB_TOKEN_WORKER=$(openssl") {
		t.Error("install-secrets.sh fabricates the externally issued worker credential")
	}
}

func TestSweepAndHandoverCredentialsRemainSeparate(t *testing.T) {
	script := readInstallSecrets(t)
	if !strings.Contains(script, "GITHUB_TOKEN=${GITHUB_TOKEN}") {
		t.Error("human-inbox sweep credential delivery path is missing")
	}
	if !strings.Contains(script, `printf 'GITHUB_TOKEN_WORKER=%s\n' "$GITHUB_TOKEN_WORKER"`) {
		t.Error("bridge handover credential delivery path is missing")
	}
	if strings.Contains(script, "GITHUB_TOKEN_WORKER=${GITHUB_TOKEN}") || strings.Contains(script, "GITHUB_TOKEN=${GITHUB_TOKEN_WORKER}") {
		t.Error("sweep and handover credentials are aliased")
	}
}
