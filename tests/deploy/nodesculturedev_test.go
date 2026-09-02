package deploytest

// Task t19 of the login-from-anywhere cycle (spec claims c2, c7, c23, c43,
// c44): the control plane gets a public name, https://nodes.culture.dev,
// behind Cloudflare Access, reached through a token-mode cloudflared user
// unit on thor whose ingress targets a LOOPBACK listener — never the LAN
// port. Three deployment facts follow from that and are pinned here:
//
//  1. every place NODES_UI_BASE_URL is written agrees on the public origin:
//     both compose files (every run-minting role) and install-secrets.sh's
//     default. A person follows that link from Jira or Discord, possibly off
//     the LAN, and whichever worker mints the run renders it — so two hosts
//     with two values makes the link's reachability a race (#224's shape).
//  2. the LAN publish `18080:8080` on thor's api is UNCHANGED. The sweep,
//     both workers and the bridges keep their LAN URLs (spec c26); the
//     tunnel does not replace that listener, it sits beside it.
//  3. the tunnel unit is the proven token-mode shape from spark, reads its
//     token from a mode-0600 file under the unit user's home, forwards to
//     127.0.0.1:18081 (the NODES_ACCESS_LISTEN port task t8 binds), and
//     carries no ingress config and no path into spark's home.
//
// The doc that turns these into operator steps is
// docs/operations/nodes-culture-dev.md; it is checked to exist and to name
// the same port and the same verification pair, so the recipe and the
// config cannot silently disagree about which port the tunnel targets.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// publicUIOrigin is the one value every writer of NODES_UI_BASE_URL agrees
// on. ticketpagelink_test.go's uiBaseURLComposeDefault aliases it so the two
// files cannot drift apart.
const publicUIOrigin = "https://nodes.culture.dev"

// lanAPIPublish is thor's LAN listener, host:container, as compose spells
// it. It is the line the tunnel deliberately does NOT front.
const lanAPIPublish = "18080:8080"

// accessLoopbackOrigin is the origin the tunnel's remote-managed ingress
// targets: the control plane's Access listener, loopback only. Task t8 binds
// it; this file and the doc fix the number so t8 has one to bind.
const accessLoopbackOrigin = "http://127.0.0.1:18081"

// tunnelUnit is the systemd user unit for the nodes.culture.dev tunnel.
const tunnelUnit = "cloudflared-nodes.service"

// tunnelTokenFile is where the unit reads the per-tunnel token from,
// %h-relative so the same unit file serves whichever account runs it on thor.
const tunnelTokenFile = "%h/.config/cloudflared/nodes-culture-dev.token"

// tunnelDoc is the operator recipe and hand-turn ledger.
const tunnelDoc = "docs/operations/nodes-culture-dev.md"

// declaredUIBaseURLs reads NODES_UI_BASE_URL out of every run-minting role in
// every production compose file, keyed by the value so the caller can ask the
// one question that matters: is there exactly one? A role that is missing, or
// that declares no value at all, is reported here and contributes nothing —
// the count then answers "the files disagree" without a second vocabulary for
// "one of them said nothing".
func declaredUIBaseURLs(t *testing.T) map[string][]string {
	t.Helper()
	seen := map[string][]string{}
	for file, roles := range uiBaseURLRoles {
		doc := loadOtelCompose(t, file)
		for _, role := range roles {
			if value, ok := declaredUIBaseURL(t, doc, file, role); ok {
				seen[value] = append(seen[value], file+":"+role)
			}
		}
	}
	return seen
}

// declaredUIBaseURL is one role's read, split out so the loop above stays a
// loop: it reports the absence itself and returns ok=false.
func declaredUIBaseURL(t *testing.T, doc otelComposeFile, file, role string) (string, bool) {
	t.Helper()
	svc, ok := doc.Services[role]
	if !ok {
		t.Errorf("%s declares no %q service", file, role)
		return "", false
	}
	value, ok := svc.Environment[storepg.UIBaseURLEnv]
	if !ok {
		t.Errorf("%s's %q service does not declare %s", file, role, storepg.UIBaseURLEnv)
		return "", false
	}
	return value, true
}

// TestBothComposeFilesAgreeOnThePublicUIOrigin is fact 1's compose half. It
// reads the value from every run-minting role in both files and requires
// them all to be the SAME string, and that string to default to the public
// origin — a value that differs between thor and orin is exactly the
// divergence that makes a Jira link's reachability depend on which worker
// claimed the node.
func TestBothComposeFilesAgreeOnThePublicUIOrigin(t *testing.T) {
	want := "${" + storepg.UIBaseURLEnv + ":-" + publicUIOrigin + "}"
	seen := declaredUIBaseURLs(t)
	if len(seen) != 1 {
		t.Fatalf("the compose files do not agree on %s; a person's link depends on which worker minted the run: %v", storepg.UIBaseURLEnv, seen)
	}
	for value := range seen {
		if value != want {
			t.Errorf("%s = %q in every compose role, want %q — the link a person follows is the public SSO origin, not a LAN address (spec c44)", storepg.UIBaseURLEnv, value, want)
		}
		if strings.Contains(value, "18080") || strings.Contains(value, "thor") {
			t.Errorf("%s = %q still names the LAN listener or the host; that is the link t19 retired", storepg.UIBaseURLEnv, value)
		}
	}
}

// TestInstallSecretsDefaultsTheUIBaseURLToThePublicOrigin is fact 1's
// prod.env half, read statically: the literal the script falls back to must
// be the same public origin the compose files default to, and must not be
// built from the host argument any more. The behavioural half (what actually
// lands in prod.env on a fake cluster) is
// TestInstallSecretsDeliversTheUIBaseURLToBothHosts next door.
func TestInstallSecretsDefaultsTheUIBaseURLToThePublicOrigin(t *testing.T) {
	raw, err := os.ReadFile(installSecretsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if !strings.Contains(script, `UI_BASE_URL="`+publicUIOrigin+`"`) {
		t.Errorf("install-secrets.sh does not default UI_BASE_URL to %q; prod.env on a fresh host would send a person to a LAN link", publicUIOrigin)
	}
	if strings.Contains(script, `UI_BASE_URL="http://$THOR:18080"`) {
		t.Error("install-secrets.sh still derives the ticket-page origin from the host argument, which is a LAN address a reader off the network cannot open")
	}
	if !strings.Contains(script, uiBaseURLDefaultedMarker) {
		t.Errorf("install-secrets.sh no longer announces %q; the log is how an operator tells the defaulted public origin from an exported one", uiBaseURLDefaultedMarker)
	}
}

// TestTheLANPublishIsUnchangedBesideTheTunnel is fact 2. The tunnel fronts a
// loopback listener that task t8 adds; the LAN listener every machine on the
// network already uses must be exactly what it was, and nothing may publish
// the Access port on all interfaces.
func TestTheLANPublishIsUnchangedBesideTheTunnel(t *testing.T) {
	doc := loadOtelCompose(t, filepath.Join("prod", "compose.thor.yml"))
	api, ok := doc.Services["api"]
	if !ok {
		t.Fatal("compose.thor.yml declares no api service")
	}
	found := false
	for _, p := range api.Ports {
		switch {
		case p == lanAPIPublish:
			found = true
		case strings.HasPrefix(p, "18081:"):
			t.Errorf("compose.thor.yml publishes the Access listener on every interface (%q); it must be loopback-only, `127.0.0.1:18081:<port>`, or a JWT seen on the LAN can be replayed to it (spec c43)", p)
		}
	}
	if !found {
		t.Errorf("compose.thor.yml's api no longer publishes %q; the sweep, workers and bridges reach the API there and the tunnel does not replace it (spec c26). ports=%v", lanAPIPublish, api.Ports)
	}
	for _, p := range api.Ports {
		if strings.HasPrefix(p, "127.0.0.1:18081:") {
			return // t8 has landed its loopback publish; nothing more to check here.
		}
	}
	// Before t8 lands, nothing may publish 18081 at all: half a listener is
	// worse than none, because the tunnel would answer with a Cloudflare 502
	// that looks like a tunnel fault rather than an unbound origin.
	for _, p := range api.Ports {
		if strings.Contains(p, "18081") {
			t.Errorf("compose.thor.yml publishes %q, which is neither the LAN listener nor the loopback-only Access publish", p)
		}
	}
}

// TestTunnelUnitIsTokenModeLoopbackAndUnprivileged is fact 3, read from the
// unit file itself. Each assertion is a property the doc relies on rather
// than the unit's byte content, so a comment edit cannot fail it while a
// change to what the unit DOES cannot pass it.
func TestTunnelUnitIsTokenModeLoopbackAndUnprivileged(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(prodComposeDir(t), tunnelUnit))
	if err != nil {
		t.Fatalf("deploy/prod/%s is missing: %v", tunnelUnit, err)
	}
	unit := string(raw)
	directives := map[string]string{}
	var active []string // every non-comment, non-section line: what systemd reads
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		active = append(active, line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("unit line %q is not KEY=value", line)
			continue
		}
		directives[key] = value
	}
	effective := strings.Join(active, "\n")
	wantExact := map[string]string{
		"Type":            "simple",
		"Environment":     "TUNNEL_TOKEN_FILE=" + tunnelTokenFile,
		"Restart":         "on-failure",
		"NoNewPrivileges": "true",
		"WantedBy":        "default.target",
		"After":           "network-online.target",
		"Wants":           "network-online.target",
	}
	for key, want := range wantExact {
		if got := directives[key]; got != want {
			t.Errorf("%s: %s=%q, want %q (the proven token-mode shape from spark's cloudflared units)", tunnelUnit, key, got, want)
		}
	}
	exec := directives["ExecStart"]
	if !strings.HasSuffix(exec, "cloudflared tunnel --no-autoupdate run") {
		t.Errorf("%s: ExecStart=%q is not `cloudflared tunnel --no-autoupdate run` — token mode means the ingress is remote-managed and the unit runs the tunnel by token alone", tunnelUnit, exec)
	}
	if !strings.HasPrefix(exec, "/") {
		t.Errorf("%s: ExecStart=%q must be an absolute path; systemd does not consult PATH", tunnelUnit, exec)
	}
	// The next three scan the EFFECTIVE unit only — the comments above the
	// sections explain why config.yml and the LAN port are absent, and an
	// explanation must not trip the check that enforces it.
	if strings.Contains(effective, "/home/spark") {
		t.Errorf("%s names /home/spark; the unit runs on thor and must use %%h-relative or /usr/local/bin paths", tunnelUnit)
	}
	for _, forbidden := range []string{"--config", "config.yml", "ingress", "--url"} {
		if strings.Contains(effective, forbidden) {
			t.Errorf("%s carries %q; the ingress is remote-managed in Cloudflare and a local one would drift from it (spec c2)", tunnelUnit, forbidden)
		}
	}
	if strings.Contains(effective, "18080") {
		t.Errorf("%s names the LAN listener; the tunnel must target the loopback Access listener only (spec c43)", tunnelUnit)
	}
	// The unit's own header must say where it forwards to, because the
	// ingress that actually does so lives outside the repo: this comment is
	// the only in-tree statement of the origin, and it must match the doc.
	if !strings.Contains(unit, "127.0.0.1:18081") {
		t.Errorf("%s does not name the loopback origin 127.0.0.1:18081 the tunnel's ingress targets", tunnelUnit)
	}
}

// TestTunnelDocNamesTheSamePortAndVerification keeps the operator recipe
// and the config on the same facts: the loopback origin, the env var t8
// binds, the token file, the enable commands, and the acceptance check
// that cannot run in-repo (curl through the tunnel == curl on the LAN).
func TestTunnelDocNamesTheSamePortAndVerification(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRootDir(t), tunnelDoc))
	if err != nil {
		t.Fatalf("%s is missing: %v", tunnelDoc, err)
	}
	doc := string(raw)
	for _, want := range []string{
		accessLoopbackOrigin,
		"NODES_ACCESS_LISTEN",
		"cultureflare remote-login setup",
		"--service " + accessLoopbackOrigin,
		"nodes-culture-dev.token",
		"systemctl --user enable --now cloudflared-nodes",
		"loginctl enable-linger",
		"curl -s https://nodes.culture.dev/v1alpha1/version",
		"curl -s http://192.168.1.146:18080/v1alpha1/version",
		publicUIOrigin,
		"hand-turn",
		"AUD",
		"t13",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not mention %q", tunnelDoc, want)
		}
	}
	if strings.Contains(doc, "@gmail.com") || strings.Contains(doc, "@agentculture.org") {
		t.Errorf("%s carries a real email address; the allow policy entry is a placeholder in the tree", tunnelDoc)
	}
}
