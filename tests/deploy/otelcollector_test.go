package deploytest

// Task t13 (close-the-backlog, issue #5): the OTLP collector is a
// DEPLOYMENT, not an instrumentation change.
//
// internal/telemetry already instruments the three seams #5 names
// (engine.transition_commit, worker.dispatch, actors.callback) and already
// gates every exporter on one variable being set. What was missing was a
// collector to export to and the variable that points at it. These tests
// assert the deployment properties that make #5's acceptance checkable:
//
//  1. every control-plane role that carries an instrumented seam can export
//     (api and worker on both machines, scheduler where it runs),
//  2. the export is OFF unless one variable is set -- the same variable
//     internal/telemetry.EndpointEnvVar reads, pinned across the language
//     boundary here so a rename in either half fails the build,
//  3. a collector ships with the deployment behind its own profile, so a
//     plain `docker compose up` starts nothing new, and
//  4. the shipped collector config carries BOTH a traces and a metrics
//     pipeline -- measured, not stylistic: telemetry.New builds an OTLP
//     metric exporter alongside the trace one, and against a collector with
//     no metrics pipeline every process's Shutdown returns "unknown service
//     opentelemetry.proto.collector.metrics.v1.MetricsService".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentculture/culture-nodes/internal/telemetry"
)

// otelComposeService is the subset of a service's shape this file reads.
// Profiles is the load-bearing one: a collector that is not behind a
// profile would start on every `docker compose up`, which is the opposite
// of "off by default".
type otelComposeService struct {
	Image       string            `json:"image"`
	Profiles    []string          `json:"profiles"`
	Environment map[string]string `json:"environment"`
	Volumes     []string          `json:"volumes"`
	Ports       []string          `json:"ports"`
}

type otelComposeFile struct {
	Services map[string]otelComposeService `json:"services"`
}

// telemetryProfile is the profile every collector service sits behind.
const telemetryProfile = "telemetry"

// collectorService is the service name the smoke script, the READMEs and
// the endpoint values all refer to. It is also the DNS name the control
// plane dials on the compose network, so a rename is a deployment change
// in three places at once.
const collectorService = "otel-collector"

// collectorConfig is the ONE collector config both profiles mount. It sits
// at deploy/ rather than inside either profile directory because the file
// is identical for both and a second copy is a second thing to forget.
const collectorConfig = "otel-collector.yaml"

// exportingRoles maps each compose file to the role services that carry an
// instrumented seam and must therefore be able to export.
//
// compose.orin.yml has only a worker: orin runs the second production
// worker against thor's Postgres, and worker.dispatch is instrumented, so a
// trace taken while both workers run needs orin exporting too or half the
// dispatches are invisible.
var exportingRoles = map[string][]string{
	filepath.Join("compose", "docker-compose.yml"): {"api", "scheduler", "worker"},
	filepath.Join("prod", "compose.thor.yml"):      {"api", "scheduler", "worker"},
	filepath.Join("prod", "compose.orin.yml"):      {"worker"},
}

// deployDir is deploy/, located from this test file's own path the same way
// composeFilePath and prodComposeDir locate theirs.
func deployDir(t *testing.T) string {
	t.Helper()
	return filepath.Dir(prodComposeDir(t))
}

func loadOtelCompose(t *testing.T, relPath string) otelComposeFile {
	t.Helper()
	path := filepath.Join(deployDir(t), relPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc otelComposeFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Services) == 0 {
		t.Fatalf("%s declared no services", path)
	}
	return doc
}

// TestEveryInstrumentedRoleCanExport is #5's first deployment property: the
// variable reaches every process that holds an instrumented seam. A role
// that never sees the variable exports nothing however the collector is
// configured, and its spans are simply missing from the trace -- the
// failure this test exists to make loud rather than puzzling.
func TestEveryInstrumentedRoleCanExport(t *testing.T) {
	for file, roles := range exportingRoles {
		doc := loadOtelCompose(t, file)
		for _, role := range roles {
			svc, ok := doc.Services[role]
			if !ok {
				t.Errorf("%s declares no %q service", file, role)
				continue
			}
			if _, ok := svc.Environment[telemetry.EndpointEnvVar]; !ok {
				t.Errorf("%s's %q service does not declare %s, so that process can never export "+
					"the seam it instruments", file, role, telemetry.EndpointEnvVar)
			}
		}
	}
}

// TestTelemetryIsOffUntilOneVariableIsSet is the second and third
// acceptance criteria of t13 held as a property of the manifests: the
// variable is interpolated with an EMPTY default everywhere, so
//
//   - unset means internal/telemetry.New returns NoOp() and nothing is
//     exported (which is what makes "the same query returns nothing when the
//     variable is unset" a real control rather than a claim), and
//   - pointing the deployment at ANY collector -- the bundled one, a
//     throwaway local one, a hosted backend -- is that one variable's value
//     and nothing else. A committed non-empty default would silently make
//     one collector special.
func TestTelemetryIsOffUntilOneVariableIsSet(t *testing.T) {
	for file, roles := range exportingRoles {
		doc := loadOtelCompose(t, file)
		for _, role := range roles {
			value := doc.Services[role].Environment[telemetry.EndpointEnvVar]
			want := "${" + telemetry.EndpointEnvVar + ":-}"
			if value != want {
				t.Errorf("%s's %q service sets %s to %q, want %q: an unset variable must reach the "+
					"container empty (telemetry off), and any collector must be reachable by setting "+
					"that one variable", file, role, telemetry.EndpointEnvVar, value, want)
			}
		}
	}
}

// TestACollectorShipsBehindItsOwnProfile: the deployment carries a
// collector so #5 can be closed on a live trace without inventing
// infrastructure, and it starts only when asked for.
func TestACollectorShipsBehindItsOwnProfile(t *testing.T) {
	for _, file := range []string{
		filepath.Join("compose", "docker-compose.yml"),
		filepath.Join("prod", "compose.thor.yml"),
	} {
		doc := loadOtelCompose(t, file)
		svc, ok := doc.Services[collectorService]
		if !ok {
			t.Errorf("%s declares no %q service; there is nothing for %s to point at",
				file, collectorService, telemetry.EndpointEnvVar)
			continue
		}
		if !contains(svc.Profiles, telemetryProfile) {
			t.Errorf("%s's %q service is not behind the %q profile (profiles: %v); a plain "+
				"`docker compose up` would start it", file, collectorService, telemetryProfile, svc.Profiles)
		}
		assertConfigMountedReadOnly(t, file, svc.Volumes)
	}
}

// assertConfigMountedReadOnly is the per-file half of the profile test: the
// collector config must be mounted, and mounted read-only. It lives out here
// so the loop above stays one level of nesting deep -- the checks are
// unchanged, only their home is.
func assertConfigMountedReadOnly(t *testing.T, file string, volumes []string) {
	t.Helper()
	mounted := false
	for _, v := range volumes {
		if !strings.Contains(v, collectorConfig) {
			continue
		}
		mounted = true
		if !strings.HasSuffix(v, ":ro") {
			t.Errorf("%s's %q mounts its config %q writable; a collector never writes its own config",
				file, collectorService, v)
		}
	}
	if !mounted {
		t.Errorf("%s's %q service mounts no %s: the config both profiles share is the one place "+
			"the receiver and pipelines are declared", file, collectorService, collectorConfig)
	}
}

// TestTheProdCollectorImageIsPinnedByDigest holds the collector to the same
// rule every other image in deploy/prod already follows: a tag is a moving
// target, and a collector that silently changes version between deploys
// changes what a trace query means.
func TestTheProdCollectorImageIsPinnedByDigest(t *testing.T) {
	doc := loadOtelCompose(t, filepath.Join("prod", "compose.thor.yml"))
	svc, ok := doc.Services[collectorService]
	if !ok {
		t.Fatalf("compose.thor.yml declares no %q service", collectorService)
	}
	if !strings.Contains(svc.Image, "@sha256:") {
		t.Errorf("compose.thor.yml's %q image %q is not pinned by digest, unlike every other image "+
			"in that file", collectorService, svc.Image)
	}
}

// collectorConfigDoc is the subset of the collector config these tests
// read. Everything else in the file is the collector's own business.
type collectorConfigDoc struct {
	Receivers map[string]struct {
		Protocols map[string]struct {
			Endpoint string `json:"endpoint"`
		} `json:"protocols"`
	} `json:"receivers"`
	Exporters map[string]any `json:"exporters"`
	Service   struct {
		Pipelines map[string]struct {
			Receivers []string `json:"receivers"`
			Exporters []string `json:"exporters"`
		} `json:"pipelines"`
	} `json:"service"`
}

// TestTheCollectorAcceptsWhatTheControlPlaneSends is the fourth property,
// and the one that is a measurement rather than a preference.
//
// telemetry.New builds an OTLP trace exporter AND an OTLP metric exporter
// from the same variable. A collector configured with only a traces
// pipeline accepts the spans and then refuses the metrics -- every
// control-plane process's Shutdown fails with "unknown service
// opentelemetry.proto.collector.metrics.v1.MetricsService" (observed on
// spark against a traces-only collector while building this task). So both
// pipelines are part of the contract between this config and that package,
// not decoration.
func TestTheCollectorAcceptsWhatTheControlPlaneSends(t *testing.T) {
	path := filepath.Join(deployDir(t), collectorConfig)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc collectorConfigDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	otlp, ok := doc.Receivers["otlp"]
	if !ok {
		t.Fatalf("%s declares no otlp receiver; the control plane speaks OTLP and nothing else", path)
	}
	grpc, ok := otlp.Protocols["grpc"]
	if !ok || !strings.HasSuffix(grpc.Endpoint, ":4317") {
		t.Errorf("%s's otlp receiver has no grpc protocol on :4317 (got %q); internal/telemetry "+
			"builds otlptracegrpc/otlpmetricgrpc exporters, which speak OTLP/gRPC", path, grpc.Endpoint)
	}

	for _, signal := range []string{"traces", "metrics"} {
		assertSignalPipeline(t, path, doc, signal)
	}
}

// assertSignalPipeline checks one signal's pipeline: that it exists, reads
// from the otlp receiver, and exports somewhere the file actually declares.
// Extracted from the loop above so each half stays readable; the assertions
// and their messages are unchanged.
func assertSignalPipeline(t *testing.T, path string, doc collectorConfigDoc, signal string) {
	t.Helper()
	pipeline, ok := doc.Service.Pipelines[signal]
	if !ok {
		t.Errorf("%s declares no %q pipeline: telemetry.New builds an exporter for that signal "+
			"from the same variable, and a collector that does not accept it fails every "+
			"process's Shutdown", path, signal)
		return
	}
	if !contains(pipeline.Receivers, "otlp") {
		t.Errorf("%s's %q pipeline does not read from the otlp receiver", path, signal)
	}
	if len(pipeline.Exporters) == 0 {
		t.Errorf("%s's %q pipeline exports nowhere, so nothing it receives is queryable", path, signal)
	}
	for _, name := range pipeline.Exporters {
		if _, ok := doc.Exporters[name]; !ok {
			t.Errorf("%s's %q pipeline names exporter %q, which the file does not declare",
				path, signal, name)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
