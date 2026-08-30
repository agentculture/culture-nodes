package deploytest

// Task t16 (SCRUM-6, spec c10 / finding s10): NODES_UI_BASE_URL is the one
// value that decides whether the link culture-nodes posts on a Jira ticket
// is clickable. It was set NOWHERE on prod, so every page-link comment read
// `/tickets/SCRUM-N` -- a path with no origin, which Jira renders as text.
//
// The variable has to arrive in two places at once, and both halves are
// asserted here because either alone leaves the link broken:
//
//  1. every compose service that can MINT a run declares it (api, scheduler
//     and worker on thor; the worker on orin). The comment is rendered
//     inside the engine's run-creation transaction, so it is whichever
//     process claimed that work that reads the variable -- and thor and orin
//     run workers against the SAME namespace, so a value present on one
//     machine only makes the link's correctness a race, exactly the #224
//     shape.
//  2. both hosts' prod.env carry a value, because compose reaches the
//     container through the env-file and an undeclared key is simply absent
//     at run time.

import (
	"path/filepath"
	"strings"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// uiBaseURLRoles maps each production compose file to the services that can
// create a run and therefore render the page-link comment. Deliberately the
// same shape as otelcollector_test.go's exportingRoles, and for the same
// reason: a role that never sees the variable cannot be fixed by any value.
var uiBaseURLRoles = map[string][]string{
	filepath.Join("prod", "compose.thor.yml"): {"api", "scheduler", "worker"},
	filepath.Join("prod", "compose.orin.yml"): {"worker"},
}

// uiBaseURLComposeDefault is the origin both compose files fall back to. A
// NON-EMPTY default is load-bearing twice over: a host that has not yet
// re-run install-secrets.sh still posts an absolute link rather than
// silently reverting to the bare path, and audit-credentials.sh classifies
// `${KEY:-value}` as `defaulted` -> optional by construction, where `${KEY:-}`
// would land in its fail-closed `unclassified` bucket.
const uiBaseURLComposeDefault = "http://thor:18080"

// The two phrases install-secrets.sh's log uses to say WHERE the origin came
// from. They are asserted rather than merely printed because the defaulted
// and exported cases produce identically-shaped prod.env lines, and only one
// of them is reachable from outside the LAN.
const (
	uiBaseURLDefaultedMarker = "defaulted to the control-plane API origin"
	uiBaseURLExportedMarker  = "exported for this run"
)

// TestEveryRunMintingRoleCarriesTheUIBaseURL is half one.
func TestEveryRunMintingRoleCarriesTheUIBaseURL(t *testing.T) {
	for file, roles := range uiBaseURLRoles {
		doc := loadOtelCompose(t, file)
		for _, role := range roles {
			svc, ok := doc.Services[role]
			if !ok {
				t.Errorf("%s declares no %q service", file, role)
				continue
			}
			value, ok := svc.Environment[storepg.UIBaseURLEnv]
			if !ok {
				t.Errorf("%s's %q service does not declare %s, so a run it mints renders the page link as the bare path '/tickets/<KEY>' however the host's prod.env is set",
					file, role, storepg.UIBaseURLEnv)
				continue
			}
			want := "${" + storepg.UIBaseURLEnv + ":-" + uiBaseURLComposeDefault + "}"
			if value != want {
				t.Errorf("%s's %q service sets %s to %q, want %q — the value must come from prod.env, and its default must be a non-empty origin",
					file, role, storepg.UIBaseURLEnv, value, want)
			}
		}
	}
}

// TestInstallSecretsDeliversTheUIBaseURLToBothHosts is half two, and pins
// where the default comes from: the control-plane host THIS deploy was told
// about, not a literal. The hosts are named `thor-lan` / `orin-lan` here so a
// hardcoded `http://thor:18080` in the script cannot pass.
//
// Both hosts get THOR's origin. orin runs a worker and serves no API, so a
// link to orin would 404 for every reader; the machine that renders the
// comment is not the machine that serves the page.
func TestInstallSecretsDeliversTheUIBaseURLToBothHosts(t *testing.T) {
	if strings.Contains(accretedProdEnv, storepg.UIBaseURLEnv) {
		t.Fatalf("accretedProdEnv already carries %s; the seed no longer reproduces a provisioned host MISSING the key", storepg.UIBaseURLEnv)
	}

	c := newFakeCluster(t)
	hosts := []string{"thor-lan", "orin-lan"}
	for _, host := range hosts {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	out, code := c.run(t, installSecretsPath(t), hosts)
	if code != 0 {
		t.Fatalf("unforced re-run exited %d; output:\n%s", code, out)
	}

	const want = "http://thor-lan:18080"
	for _, host := range hosts {
		path := c.prodEnvPath(t, host)
		env := readEnvFile(t, path)
		env.assertNoDuplicateKeys(t, path)
		got, present := env.values[storepg.UIBaseURLEnv]
		if !present {
			t.Errorf("%s: prod.env carries no %s after a deploy, so the container sees no value and the page link stays the bare path", host, storepg.UIBaseURLEnv)
			continue
		}
		if got != want {
			t.Errorf("%s: %s = %q, want %q — the default is the control-plane origin this deploy was given, and orin gets thor's because orin serves no API", host, storepg.UIBaseURLEnv, got, want)
		}
	}
	if !strings.Contains(out, want) || !strings.Contains(out, uiBaseURLDefaultedMarker) {
		t.Errorf("the install log does not report the ticket-page origin as defaulted (want %q and %q); an operator reading the deploy log cannot otherwise tell a defaulted LAN address from one they exported\noutput:\n%s", want, uiBaseURLDefaultedMarker, out)
	}
}

// TestAnOperatorExportedUIBaseURLWinsAndIsAnnounced covers the other branch.
// Once the OAuth cycle gives the deployment a name a Jira reader can resolve,
// pointing every ticket link at it must be one exported variable — and the
// log has to distinguish that from the defaulted LAN address, because the two
// produce identically-shaped prod.env lines and only one of them is reachable
// from outside the network.
func TestAnOperatorExportedUIBaseURLWinsAndIsAnnounced(t *testing.T) {
	c := newFakeCluster(t)
	hosts := []string{"thor", "orin"}
	for _, host := range hosts {
		c.seedProdEnv(t, host, accretedProdEnv)
	}

	// The trailing slash is what an operator types; the value that lands in
	// prod.env is normalised, so the rendered URL cannot acquire a `//`.
	out, code := c.run(t, installSecretsPath(t), hosts,
		storepg.UIBaseURLEnv+"=https://nodes.example.net/")
	if code != 0 {
		t.Fatalf("run exited %d; output:\n%s", code, out)
	}

	const want = "https://nodes.example.net"
	for _, host := range hosts {
		env := readEnvFile(t, c.prodEnvPath(t, host))
		if got := env.values[storepg.UIBaseURLEnv]; got != want {
			t.Errorf("%s: %s = %q, want the exported %q — an exported origin must reach BOTH hosts, or which worker claims the node decides which link Jira gets", host, storepg.UIBaseURLEnv, got, want)
		}
	}
	if !strings.Contains(out, want) || !strings.Contains(out, uiBaseURLExportedMarker) {
		t.Errorf("the install log does not name the exported ticket-page origin as exported (want %q and %q)\noutput:\n%s", want, uiBaseURLExportedMarker, out)
	}
	if strings.Contains(out, uiBaseURLDefaultedMarker) {
		t.Errorf("the install log calls an exported origin defaulted\noutput:\n%s", out)
	}
}
