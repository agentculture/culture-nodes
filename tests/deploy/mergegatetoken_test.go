// Package deploytest (see codexworkerenv_test.go's doc comment for the
// package's purpose).
//
// This file is task t11 of login-from-anywhere (spec c45 / h31): the
// merge-gate scripts stop carrying the human decision secret and
// authenticate as their own registered agent actor, `company/merge-gate`,
// whose bearer is NODES_ACTOR_MERGE_GATE_TOKEN. The deployment half of that
// is three facts, each asserted here against the file that holds it:
//
//   - install-secrets.sh MINTS the token beside the other NODES_ACTOR_*
//     tokens and installs it into prod.env on every host, keeping an
//     existing value unless forced (a silent rotation would desync the
//     operator's copy from the control plane's).
//   - compose.thor.yml's api service DECLARES the variable — the control
//     plane resolves the bearer through the actor row's auth_token_env, and
//     an undeclared variable is simply absent in the container (the same
//     lesson NODES_ACTOR_QWEN_TOKEN's comment records).
//   - audit-credentials.sh CLASSIFIES it, so an absent value is reported
//     rather than dropped.
package deploytest

import (
	"regexp"
	"strings"
	"testing"
)

const mergeGateTokenVar = "NODES_ACTOR_MERGE_GATE_TOKEN"

func TestInstallSecretsMintsAndInstallsTheMergeGateToken(t *testing.T) {
	script := readInstallSecrets(t)

	if !regexp.MustCompile(`(?m)^MERGE_GATE_TOKEN=\$\((gen|openssl rand -base64 32)\)`).MatchString(script) {
		t.Fatalf("install-secrets.sh does not mint MERGE_GATE_TOKEN locally beside the other actor tokens")
	}
	calls := regexp.MustCompile(`install_merge_gate_token\s+"\$([A-Z_]+)"`).FindAllStringSubmatch(script, -1)
	installed := map[string]bool{}
	for _, m := range calls {
		installed[m[1]] = true
	}
	for _, hostVar := range []string{"THOR", "ORIN"} {
		if !installed[hostVar] {
			t.Errorf("install-secrets.sh never calls install_merge_gate_token \"$%s\"; the merge-gate scripts "+
				"read %s from that host's prod.env (or the operator's copy of it) and would find nothing", hostVar, mergeGateTokenVar)
		}
	}
	if !strings.Contains(script, "FORCE_MERGE_GATE") {
		t.Errorf("install-secrets.sh has no FORCE_MERGE_GATE guard: a re-run would silently rotate %s and "+
			"desync the operator's copy from the control plane's", mergeGateTokenVar)
	}
	// The token rides ssh stdin, never ssh argv: no line may carry the
	// variable at or after the ssh keyword (the same discipline
	// codexsecrets_test.go enforces for the codex bridge tokens).
	for i, line := range strings.Split(script, "\n") {
		idx := strings.Index(line, "ssh ")
		if idx < 0 {
			continue
		}
		if strings.Contains(line[idx:], "$MERGE_GATE_TOKEN") || strings.Contains(line[idx:], "${MERGE_GATE_TOKEN") {
			t.Errorf("install-secrets.sh line %d substitutes MERGE_GATE_TOKEN into an ssh argv: %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestThorAPIServiceDeclaresTheMergeGateToken(t *testing.T) {
	doc := loadProdComposeFile(t, prodComposeDir(t)+"/compose.thor.yml")
	api, ok := doc.Services["api"]
	if !ok {
		t.Fatal("compose.thor.yml: expected an \"api\" service, found none")
	}
	if _, exists := api.Environment[mergeGateTokenVar]; !exists {
		t.Errorf("compose.thor.yml api service does not declare %s: the control plane resolves the merge-gate "+
			"bearer through the actor row's auth_token_env, and an undeclared variable is absent in the container",
			mergeGateTokenVar)
	}
}

func TestCredentialAuditClassifiesTheMergeGateToken(t *testing.T) {
	audit := readProdFile(t, "audit-credentials.sh")
	if !regexp.MustCompile(`(?m)^` + mergeGateTokenVar + `\s+(required|optional)\s*$`).MatchString(audit) {
		t.Errorf("audit-credentials.sh does not classify %s; an absent value would be dropped rather than reported", mergeGateTokenVar)
	}
}
