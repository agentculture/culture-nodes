// Package deploytest (see compose_test.go's doc comment). This file is
// task t34's: static checks over deploy/prod/install-secrets.sh's two new
// secret lanes --
//
//   - the nodes-notifier webhook (CULTURE_NODES_WEBHOOK_URL /
//     DISCORD_WEBHOOK_URL), relayed into thor's prod.env only when the
//     operator already exported one into this script's own environment
//     (an externally issued credential the script must never fabricate);
//   - the human-inbox bridge/tracker secrets (HUMAN_INBOX_BRIDGE_AUTH_TOKEN,
//     generated locally like the codex-bridge tokens; GITHUB_TOKEN,
//     relayed the same way the webhook is, never fabricated).
//
// Mirrors codexsecrets_test.go's own style and discipline checks: text
// assertions over the script itself, not a live ssh run.
package deploytest

import (
	"regexp"
	"strings"
	"testing"
)

// --- nodes-notifier webhook -------------------------------------------

// TestInstallSecretsRelaysWebhookURLWithoutFabricating asserts the script
// only relays CULTURE_NODES_WEBHOOK_URL/DISCORD_WEBHOOK_URL from its own
// environment (an operator-supplied, externally issued credential) and
// never invents a value with openssl or any other generator the way it
// does for POSTGRES_PASSWORD or the runner/codex-bridge tokens.
func TestInstallSecretsRelaysWebhookURLWithoutFabricating(t *testing.T) {
	script := readInstallSecrets(t)

	for _, key := range []string{"CULTURE_NODES_WEBHOOK_URL", "DISCORD_WEBHOOK_URL"} {
		if !strings.Contains(script, key) {
			t.Errorf("install-secrets.sh does not reference %s", key)
		}
	}

	webhookIdx := strings.Index(script, "nodes-notifier webhook")
	if webhookIdx == -1 {
		t.Fatal("install-secrets.sh has no nodes-notifier webhook lane")
	}
	// Bound the lane to before the next lane comment header.
	rest := script[webhookIdx:]
	end := len(rest)
	if idx := strings.Index(rest, "human-inbox bridge"); idx != -1 {
		end = idx
	}
	lane := rest[:end]

	if strings.Contains(lane, "openssl rand") {
		t.Error("the webhook lane generates secret material with openssl; a webhook URL is externally issued and must never be fabricated")
	}
	if !strings.Contains(lane, "${CULTURE_NODES_WEBHOOK_URL:-}") && !strings.Contains(lane, `"${CULTURE_NODES_WEBHOOK_URL:-}"`) {
		t.Error("the webhook lane does not read CULTURE_NODES_WEBHOOK_URL from this script's own environment with an empty-safe default")
	}
}

// TestInstallSecretsWebhookOnlyReachesThor asserts the webhook is only
// ever installed into thor's prod.env: nodes-notifier is declared as a
// compose.thor.yml-only service, so orin has no consumer for it.
func TestInstallSecretsWebhookOnlyReachesThor(t *testing.T) {
	script := readInstallSecrets(t)

	webhookIdx := strings.Index(script, "nodes-notifier webhook")
	if webhookIdx == -1 {
		t.Fatal("install-secrets.sh has no nodes-notifier webhook lane")
	}
	rest := script[webhookIdx:]
	end := len(rest)
	if idx := strings.Index(rest, "human-inbox bridge"); idx != -1 {
		end = idx
	}
	lane := rest[:end]

	if !strings.Contains(lane, `update_env_line_on_host "$THOR"`) {
		t.Error(`the webhook lane does not call update_env_line_on_host "$THOR" ...`)
	}
	if strings.Contains(lane, `"$ORIN"`) {
		t.Error("the webhook lane references $ORIN; nodes-notifier is a thor-only compose service")
	}
}

// TestInstallSecretsWebhookLaneNeverRidesSSHArgv mirrors
// TestCodexSecretsTokensNeverRideSSHArgv for the new update_env_line_on_host
// helper: the webhook value must ride ssh stdin, never argv.
func TestInstallSecretsWebhookLaneNeverRidesSSHArgv(t *testing.T) {
	script := readInstallSecrets(t)

	fnIdx := strings.Index(script, "update_env_line_on_host() {")
	if fnIdx == -1 {
		t.Fatal("install-secrets.sh has no update_env_line_on_host() helper")
	}
	// Bound to the function body (next blank-line-preceded closing brace at
	// column 0, i.e. the next "\n}\n").
	closeIdx := strings.Index(script[fnIdx:], "\n}\n")
	if closeIdx == -1 {
		t.Fatal("could not find the end of update_env_line_on_host()")
	}
	body := script[fnIdx : fnIdx+closeIdx]

	for i, line := range strings.Split(body, "\n") {
		idx := strings.Index(line, "ssh ")
		if idx == -1 {
			idx = strings.Index(line, `ssh "`)
		}
		if idx == -1 {
			continue
		}
		argvPortion := line[idx:]
		if strings.Contains(argvPortion, "$value") || strings.Contains(argvPortion, "${value}") {
			t.Errorf("line %d of update_env_line_on_host: %q interpolates $value at/after the ssh keyword; secret material must ride stdin only", i+1, line)
		}
	}
}

// --- human-inbox bridge + tracker secrets -------------------------------

// TestInstallSecretsGeneratesHumanInboxBridgeToken asserts
// HUMAN_INBOX_BRIDGE_AUTH_TOKEN is generated locally (openssl), exactly
// like the codex-bridge tokens -- it is a bearer credential this repo
// mints, not something externally issued.
func TestInstallSecretsGeneratesHumanInboxBridgeToken(t *testing.T) {
	script := readInstallSecrets(t)

	if !strings.Contains(script, "HUMAN_INBOX_BRIDGE_AUTH_TOKEN") {
		t.Fatal("install-secrets.sh does not reference HUMAN_INBOX_BRIDGE_AUTH_TOKEN")
	}
	genPattern := regexp.MustCompile(`HUMAN_INBOX_BRIDGE_AUTH_TOKEN=\$\(openssl rand`)
	if !genPattern.MatchString(script) {
		t.Error("install-secrets.sh does not generate HUMAN_INBOX_BRIDGE_AUTH_TOKEN locally with openssl rand")
	}
}

// TestInstallSecretsRelaysGitHubTokenWithoutFabricating asserts
// GITHUB_TOKEN is only ever relayed from this script's own environment,
// never generated -- there is no way to locally mint a valid GitHub
// credential.
func TestInstallSecretsRelaysGitHubTokenWithoutFabricating(t *testing.T) {
	script := readInstallSecrets(t)

	if !strings.Contains(script, "GITHUB_TOKEN") {
		t.Fatal("install-secrets.sh does not reference GITHUB_TOKEN")
	}
	genPattern := regexp.MustCompile(`GITHUB_TOKEN=\$\(openssl`)
	if genPattern.MatchString(script) {
		t.Error("install-secrets.sh generates GITHUB_TOKEN with openssl; a GitHub credential is externally issued and must never be fabricated")
	}
	if !strings.Contains(script, `${GITHUB_TOKEN:-}`) {
		t.Error("install-secrets.sh does not read GITHUB_TOKEN from its own environment with an empty-safe default")
	}
}

// humanInboxEnvBlocks returns the single-quoted remote command strings of
// every ssh invocation that writes human-inbox.env, mirroring
// codexsecrets_test.go's codexBridgeEnvBlocks helper.
func humanInboxEnvBlocks(t *testing.T, script string) []string {
	t.Helper()
	var blocks []string
	for _, m := range remoteBlockPattern.FindAllStringSubmatch(script, -1) {
		if strings.Contains(m[1], "human-inbox.env") {
			blocks = append(blocks, m[1])
		}
	}
	if len(blocks) == 0 {
		t.Fatal("no ssh remote-command block writing human-inbox.env found")
	}
	return blocks
}

// TestInstallSecretsHumanInboxEnvSetsUmask077 mirrors
// TestCodexSecretsBridgeEnvSetsUmask077 for human-inbox.env.
func TestInstallSecretsHumanInboxEnvSetsUmask077(t *testing.T) {
	script := readInstallSecrets(t)
	blocks := humanInboxEnvBlocks(t, script)
	for _, remote := range blocks {
		if !strings.Contains(remote, "umask 077") {
			t.Errorf("remote command writing human-inbox.env does not set umask 077 before writing: %q", remote)
		}
	}
}

// TestInstallSecretsHumanInboxEnvRefusesOverwriteWithoutForce mirrors
// TestCodexSecretsBridgeEnvRefusesOverwriteWithoutForce for
// human-inbox.env: a re-run must not silently rotate a live bridge token
// out from under a running unit.
func TestInstallSecretsHumanInboxEnvRefusesOverwriteWithoutForce(t *testing.T) {
	script := readInstallSecrets(t)
	blocks := humanInboxEnvBlocks(t, script)
	for _, remote := range blocks {
		if !strings.Contains(remote, "FORCE") {
			t.Errorf("remote command writing human-inbox.env carries no FORCE guard: %q", remote)
		}
		if !strings.Contains(remote, "-e ~/.culture-nodes/human-inbox.env") {
			t.Errorf("remote command has no existence check on human-inbox.env ahead of the FORCE guard: %q", remote)
		}
	}
}

// TestInstallSecretsHumanInboxOnlyReachesThor asserts install_human_inbox_env
// is only ever called for $THOR -- one logical human actor, thor-only,
// matching deploy.sh's own deploy_human_inbox gate.
func TestInstallSecretsHumanInboxOnlyReachesThor(t *testing.T) {
	script := readInstallSecrets(t)

	if !strings.Contains(script, `install_human_inbox_env "$THOR"`) {
		t.Error(`install-secrets.sh does not call install_human_inbox_env "$THOR"`)
	}
	if strings.Contains(script, `install_human_inbox_env "$ORIN"`) {
		t.Error(`install-secrets.sh calls install_human_inbox_env "$ORIN"; the human-inbox bridge is thor-only (one logical human actor)`)
	}
}

// TestInstallSecretsHumanInboxTokenNeverRidesSSHArgv mirrors
// TestCodexSecretsTokensNeverRideSSHArgv for the new
// HUMAN_INBOX_BRIDGE_AUTH_TOKEN/GITHUB_TOKEN variables.
func TestInstallSecretsHumanInboxTokenNeverRidesSSHArgv(t *testing.T) {
	script := readInstallSecrets(t)

	lines := strings.Split(script, "\n")
	checked := 0
	for i, line := range lines {
		idx := strings.Index(line, "ssh")
		if idx == -1 {
			continue
		}
		checked++
		argvPortion := line[idx:]
		for _, tok := range []string{"HUMAN_INBOX_BRIDGE_AUTH_TOKEN", "GITHUB_TOKEN"} {
			if strings.Contains(argvPortion, tok) {
				t.Errorf("line %d: %q references %s at/after the \"ssh\" keyword; secret material must ride ssh stdin only, never argv", i+1, line, tok)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ssh invocation found in install-secrets.sh; this test is not proving anything")
	}
}
