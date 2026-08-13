// Package deploytest (see compose_test.go's doc comment for the package's
// purpose). This file is task t34's: deploy/prod wiring for the
// nodes-notifier Discord notifier daemon (economy-discord-graphs task
// t14, cmd/nodes-notifier + internal/notifier). Pins three things as
// checked facts rather than claims in prose:
//
//   - the Dockerfile's build stage compiles cmd/nodes-notifier and the
//     final stage ships the resulting /nodes-notifier binary alongside
//     /nodes, without changing the default ENTRYPOINT;
//   - deploy/prod/compose.thor.yml declares a `notifier` service running
//     the SAME image as the control plane (via an entrypoint override),
//     depending on `api`, carrying both webhook env names and the
//     required NODES_NOTIFIER_API_BASE/CURSOR_FILE settings, and — the
//     property the daemon's whole exactly-once-across-restarts guarantee
//     depends on (internal/notifier/cursor.go) — persisting its cursor
//     file on a named volume, not a bind mount and not container-local
//     storage;
//   - no service in the file writes a literal secret value (the webhook
//     URL rides ${VAR:-} substitution from thor's prod.env, installed by
//     install-secrets.sh, exactly like every other secret in this file).
package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// --- Dockerfile ------------------------------------------------------------

// dockerfilePath locates the repo-root Dockerfile from prodComposeDir
// (codexworkerenv_test.go, same package): deploy/prod -> deploy -> repo
// root.
func dockerfilePath(t *testing.T) string {
	t.Helper()
	dir := prodComposeDir(t)
	return filepath.Join(filepath.Dir(filepath.Dir(dir)), "Dockerfile")
}

func dockerfileText(t *testing.T) string {
	t.Helper()
	path := dockerfilePath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestDockerfileBuildsNotifierBinary asserts the build stage compiles
// cmd/nodes-notifier into /out/nodes-notifier -- the smallest honest way
// to give the notifier a deployable artifact without a second Dockerfile
// or a second image (see the Dockerfile's own header comment).
func TestDockerfileBuildsNotifierBinary(t *testing.T) {
	text := dockerfileText(t)

	if !strings.Contains(text, "./cmd/nodes-notifier") {
		t.Fatal("Dockerfile never builds ./cmd/nodes-notifier")
	}
	if !strings.Contains(text, "-o /out/nodes-notifier") {
		t.Error("Dockerfile does not build cmd/nodes-notifier to /out/nodes-notifier")
	}
}

// TestDockerfileShipsBothBinariesFromDistroless asserts the final
// distroless stage COPYs both /nodes and /nodes-notifier from the build
// stage, and that the default ENTRYPOINT is left unchanged at /nodes --
// every existing compose service (migrate/api/scheduler/worker) must stay
// unaffected by the notifier addition.
func TestDockerfileShipsBothBinariesFromDistroless(t *testing.T) {
	text := dockerfileText(t)

	distrolessIdx := strings.Index(text, "FROM gcr.io/distroless")
	if distrolessIdx == -1 {
		t.Fatal("Dockerfile has no distroless final stage")
	}
	finalStage := text[distrolessIdx:]

	if !strings.Contains(finalStage, "COPY --from=build /out/nodes /nodes") {
		t.Error("final stage does not copy /out/nodes to /nodes")
	}
	if !strings.Contains(finalStage, "COPY --from=build /out/nodes-notifier /nodes-notifier") {
		t.Error("final stage does not copy /out/nodes-notifier to /nodes-notifier")
	}
	if !strings.Contains(finalStage, `ENTRYPOINT ["/nodes"]`) {
		t.Error(`final stage's ENTRYPOINT is not ["/nodes"]; existing compose services must be unaffected by the notifier addition`)
	}
}

// --- compose.thor.yml: notifier service ------------------------------------

// notifierComposeService is the subset of the notifier service's shape
// this file cares about; it mirrors prodComposeService (codexworkerenv_
// test.go) but adds the fields specific to this check (Entrypoint,
// DependsOn, and the long-form Volumes this service's comment calls out
// as intentionally a named volume, not a bind mount).
type notifierComposeService struct {
	Image       string            `json:"image"`
	Entrypoint  []string          `json:"entrypoint"`
	Environment map[string]string `json:"environment"`
	Volumes     []string          `json:"volumes"`
	DependsOn   map[string]struct {
		Condition string `json:"condition"`
	} `json:"depends_on"`
	Restart string `json:"restart"`
}

type notifierComposeFile struct {
	Services map[string]notifierComposeService `json:"services"`
	Volumes  map[string]any                    `json:"volumes"`
}

func loadNotifierComposeFile(t *testing.T) notifierComposeFile {
	t.Helper()
	path := filepath.Join(prodComposeDir(t), "compose.thor.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc notifierComposeFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func notifierService(t *testing.T) notifierComposeService {
	t.Helper()
	doc := loadNotifierComposeFile(t)
	svc, ok := doc.Services["notifier"]
	if !ok {
		t.Fatal(`compose.thor.yml declares no "notifier" service`)
	}
	return svc
}

// TestNotifierServiceUsesControlPlaneImage asserts the notifier service
// runs the same culture-nodes:prod image every other role container does
// -- there is exactly one image built on thor, per compose_test.go's
// TestControlPlaneServicesShareOneImage for the dev profile.
func TestNotifierServiceUsesControlPlaneImage(t *testing.T) {
	svc := notifierService(t)
	if svc.Image != "culture-nodes:prod" {
		t.Errorf("notifier service image = %q, want %q", svc.Image, "culture-nodes:prod")
	}
}

// TestNotifierServiceSelectsNotifierBinaryViaEntrypoint asserts the
// service selects the second binary the Dockerfile now ships
// (/nodes-notifier) through an entrypoint override, rather than the
// default /nodes control-plane entrypoint.
func TestNotifierServiceSelectsNotifierBinaryViaEntrypoint(t *testing.T) {
	svc := notifierService(t)
	if len(svc.Entrypoint) == 0 {
		t.Fatal("notifier service declares no entrypoint override; it would run /nodes, the control-plane binary")
	}
	if svc.Entrypoint[0] != "/nodes-notifier" {
		t.Errorf("notifier service entrypoint = %v, want [\"/nodes-notifier\", ...]", svc.Entrypoint)
	}
}

// TestNotifierServiceDependsOnAPI asserts the notifier service declares a
// depends_on entry for the api service -- the daemon's whole job is
// consuming GET /v1alpha1/events from the control plane.
func TestNotifierServiceDependsOnAPI(t *testing.T) {
	svc := notifierService(t)
	if _, ok := svc.DependsOn["api"]; !ok {
		t.Errorf("notifier service has no depends_on entry for \"api\"; depends_on = %v", svc.DependsOn)
	}
}

// TestNotifierServiceCarriesRequiredEnv asserts the notifier service's
// environment block carries every env var cmd/nodes-notifier's main.go
// documents as required or load-bearing: the two REQUIRED settings
// (API_BASE, CURSOR_FILE) and both webhook URL names
// internal/notify.ResolveWebhook reads (CULTURE_NODES_WEBHOOK_URL first,
// DISCORD_WEBHOOK_URL fallback).
func TestNotifierServiceCarriesRequiredEnv(t *testing.T) {
	svc := notifierService(t)
	if len(svc.Environment) == 0 {
		t.Fatal("notifier service has no environment variables")
	}
	for _, key := range []string{
		"NODES_NOTIFIER_API_BASE",
		"NODES_NOTIFIER_CURSOR_FILE",
		"CULTURE_NODES_WEBHOOK_URL",
		"DISCORD_WEBHOOK_URL",
	} {
		if _, ok := svc.Environment[key]; !ok {
			t.Errorf("notifier environment missing %q", key)
		}
	}
}

// TestNotifierCursorFileOnNamedVolume asserts the cursor file's directory
// is mounted from a compose top-level named volume, not a host bind
// mount and not left on the container's own (ephemeral) filesystem --
// the property that makes "restarts resume exactly-once"
// (internal/notifier/cursor.go) actually true across a container
// recreate, not just a process restart.
func TestNotifierCursorFileOnNamedVolume(t *testing.T) {
	doc := loadNotifierComposeFile(t)
	svc := notifierService(t)

	cursorFile, ok := svc.Environment["NODES_NOTIFIER_CURSOR_FILE"]
	if !ok {
		t.Fatal("notifier environment has no NODES_NOTIFIER_CURSOR_FILE; cannot check its volume")
	}
	cursorDir := filepath.Dir(cursorFile)

	if len(svc.Volumes) == 0 {
		t.Fatal("notifier service declares no volumes; the cursor file has no durable storage")
	}

	var matched string
	for _, v := range svc.Volumes {
		name, mount, found := strings.Cut(v, ":")
		if !found {
			continue
		}
		if mount == cursorDir || strings.HasPrefix(cursorDir, mount+"/") || mount == cursorDir+"/" {
			matched = name
		}
		// A host path (bind mount) starts with "/" or "~" or "." -- a
		// compose named volume is a bare identifier with none of those.
		if (mount == cursorDir) && (strings.HasPrefix(name, "/") || strings.HasPrefix(name, "~") || strings.HasPrefix(name, ".")) {
			t.Errorf("notifier's cursor volume %q is a bind mount (host path), not a named volume; the daemon's exactly-once guarantee would not survive a container recreate cleanly separated from the host filesystem", v)
		}
	}
	if matched == "" {
		t.Fatalf("no volume entry in %v mounts the cursor directory %q", svc.Volumes, cursorDir)
	}
	if _, declared := doc.Volumes[matched]; !declared {
		t.Errorf("notifier mounts volume %q but compose.thor.yml's top-level volumes: section does not declare it", matched)
	}
}

// TestNotifierServiceRestartsUnlessStopped asserts the notifier service
// carries the same restart policy every other long-running role service
// in this file does (api/scheduler/worker/backup all use
// "unless-stopped").
func TestNotifierServiceRestartsUnlessStopped(t *testing.T) {
	svc := notifierService(t)
	if svc.Restart != "unless-stopped" {
		t.Errorf("notifier restart = %q, want %q", svc.Restart, "unless-stopped")
	}
}

// TestNotifierServiceHasNoLiteralSecret extends
// compose_test.go/codexbridgeunit_test.go's "no literal secret" style
// check onto the notifier service specifically: the webhook envs must be
// ${VAR:-...} substitutions from the operator's env file, never a
// hardcoded URL or bearer token.
func TestNotifierServiceHasNoLiteralSecret(t *testing.T) {
	path := filepath.Join(prodComposeDir(t), "compose.thor.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(raw)

	notifierIdx := strings.Index(text, "\n  notifier:")
	if notifierIdx == -1 {
		t.Fatal("could not locate the notifier: service block in compose.thor.yml")
	}
	// Bound the scan to this service's block: up to the next top-level
	// (two-space-indented) service key, or the top-level "volumes:" key.
	rest := text[notifierIdx+1:]
	end := len(rest)
	for _, marker := range []string{"\n  backup:", "\nvolumes:"} {
		if idx := strings.Index(rest, marker); idx != -1 && idx < end {
			end = idx
		}
	}
	block := rest[:end]

	for _, want := range []string{"CULTURE_NODES_WEBHOOK_URL: ${CULTURE_NODES_WEBHOOK_URL:-}", "DISCORD_WEBHOOK_URL: ${DISCORD_WEBHOOK_URL:-}"} {
		if !strings.Contains(block, want) {
			t.Errorf("notifier service block does not contain %q; the webhook must ride ${VAR:-} substitution from prod.env, never a literal", want)
		}
	}
	if strings.Contains(block, "https://discord.com/api/webhooks/") {
		t.Error("notifier service block contains a literal Discord webhook URL; secrets must never be committed to compose.thor.yml")
	}
}
