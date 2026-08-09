// Package testsdeploy holds manifest-level tests for deploy/helm/culture-nodes
// -- the same "cheaper as a Go test than a separate tool" rationale
// tests/lint documents for its own checks. These tests shell out to a real
// `helm template` (skipping gracefully, like internal/store/postgres/pgtest,
// when the helm binary is not on PATH) and assert on the rendered
// manifests: no Docker socket ever mounted into a control-plane container
// (task t22's honesty condition h4/c4), exactly the expected Deployment/
// StatefulSet set, worker replicas default to 2, the migration Job carries
// the pre-install/pre-upgrade hook annotations, the api Deployment's probes
// point at the real health paths, and image.digest wins over image.tag when
// set.
package testsdeploy

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// chartDir is deploy/helm/culture-nodes, relative to this test file.
func chartDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "culture-nodes"))
	if err != nil {
		t.Fatalf("resolve chart dir: %v", err)
	}
	return dir
}

// requireHelm skips the test when the helm binary is not on PATH, mirroring
// pgtest.RequireStore's "report skipped rather than fail the whole suite
// when an external dependency is missing" contract.
func requireHelm(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("no helm binary on PATH: install helm to run this package's manifest tests")
	}
	return path
}

// manifest is one rendered Kubernetes object, decoded loosely enough to
// answer this package's questions without hand-modeling every resource
// kind's full schema.
type manifest map[string]any

func (m manifest) kind() string { return getString(m, "kind") }
func (m manifest) name() string { return getString(m, "metadata", "name") }
func (m manifest) component() string {
	return getString(m, "metadata", "labels", "app.kubernetes.io/component")
}

// getString walks m through the given nested keys, returning "" if any step
// is missing or not a string/map.
func getString(m map[string]any, path ...string) string {
	var cur any = map[string]any(m)
	for _, key := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = asMap[key]
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}

// getPath walks m through the given nested keys, returning the raw value
// and whether every step resolved.
func getPath(m map[string]any, path ...string) (any, bool) {
	var cur any = map[string]any(m)
	for _, key := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = asMap[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// helmTemplate renders the chart with the given extra --set/-f style args
// and a baseline of pinned, non-random values (a fixed callback token and
// Postgres password) so the output is reproducible across runs -- without
// pinning them, culture-nodes.callbackTokenSecret/postgresPassword
// (templates/_helpers.tpl) fall back to `randAlphaNum`, since `helm
// template` has no live cluster for their `lookup`-existing-secret branch
// to find anything in.
func helmTemplate(t *testing.T, extraArgs ...string) []manifest {
	t.Helper()
	helmBin := requireHelm(t)

	args := []string{
		"template", "manifest-test", chartDir(t),
		"--set", "callback.tokenSecret=0123456789abcdef0123456789abcdef",
		"--set", "postgresql.auth.password=test-postgres-password",
	}
	args = append(args, extraArgs...)

	cmd := exec.Command(helmBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr:\n%s", err, stderr.String())
	}

	return parseManifests(t, stdout.String())
}

// parseManifests splits `helm template`'s "---"-separated multi-document
// output and decodes each into a manifest, skipping empty documents (a
// template that renders nothing between two separators, e.g. a
// {{- if ... -}} guard that evaluated false).
func parseManifests(t *testing.T, rendered string) []manifest {
	t.Helper()
	var out []manifest
	for _, doc := range strings.Split(rendered, "\n---\n") {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		var m manifest
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("decode manifest document: %v\n--- document ---\n%s", err, doc)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatalf("helm template produced no manifests")
	}
	return out
}

func byKind(manifests []manifest, kind string) []manifest {
	var out []manifest
	for _, m := range manifests {
		if m.kind() == kind {
			out = append(out, m)
		}
	}
	return out
}

// TestNoDockerSocketAnywhere renders the chart with every optional surface
// turned on (ingress, in-chart Postgres) and greps the raw text for any
// reference to docker.sock -- the bluntest, most reliable way to prove no
// control-plane container ever mounts one (spec claim c4, honesty
// condition h4; CLAUDE.md's "Runtime" ground rule: "Code executes only
// through the headspace-cli runner boundary — never a shell, script, or
// Docker socket in the control-plane process").
func TestNoDockerSocketAnywhere(t *testing.T) {
	helmBin := requireHelm(t)
	cmd := exec.Command(helmBin, "template", "manifest-test", chartDir(t),
		"--set", "callback.tokenSecret=0123456789abcdef0123456789abcdef",
		"--set", "postgresql.auth.password=test-postgres-password",
		"--set", "ingress.enabled=true",
		"--set", "ingress.host=nodes.example.test",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr:\n%s", err, stderr.String())
	}

	rendered := stdout.String()
	if strings.Contains(strings.ToLower(rendered), "docker.sock") {
		t.Fatalf("rendered manifests reference docker.sock -- no control-plane container may mount it:\n%s", rendered)
	}
}

// TestExpectedWorkloadSet proves the chart contains exactly the control-plane
// Deployments (api, scheduler, worker) plus, when postgresql.enabled, one
// Postgres StatefulSet -- never more, never fewer.
func TestExpectedWorkloadSet(t *testing.T) {
	t.Run("postgresql_enabled_default", func(t *testing.T) {
		manifests := helmTemplate(t)

		deployments := byKind(manifests, "Deployment")
		gotComponents := make(map[string]bool)
		for _, d := range deployments {
			gotComponents[d.component()] = true
		}
		wantComponents := []string{"api", "scheduler", "worker"}
		if len(deployments) != len(wantComponents) {
			t.Fatalf("got %d Deployments, want %d (one each for %v); components seen: %v",
				len(deployments), len(wantComponents), wantComponents, gotComponents)
		}
		for _, want := range wantComponents {
			if !gotComponents[want] {
				t.Errorf("no Deployment with app.kubernetes.io/component=%s", want)
			}
		}

		statefulSets := byKind(manifests, "StatefulSet")
		if len(statefulSets) != 1 {
			t.Fatalf("got %d StatefulSets with postgresql.enabled (default), want exactly 1 (postgres)", len(statefulSets))
		}
		if got := statefulSets[0].component(); got != "postgres" {
			t.Errorf("the StatefulSet's component label = %q, want %q", got, "postgres")
		}
	})

	t.Run("postgresql_disabled_external", func(t *testing.T) {
		manifests := helmTemplate(t,
			"--set", "postgresql.enabled=false",
			"--set", "postgresql.external.url=postgres://user:pass@external-host:5432/nodes",
		)

		deployments := byKind(manifests, "Deployment")
		if len(deployments) != 3 {
			t.Fatalf("got %d Deployments with postgresql.enabled=false, want 3 (postgresql.enabled must not change the control-plane workload set)", len(deployments))
		}

		if statefulSets := byKind(manifests, "StatefulSet"); len(statefulSets) != 0 {
			t.Fatalf("got %d StatefulSets with postgresql.enabled=false, want 0 (no in-chart postgres)", len(statefulSets))
		}
	})
}

// TestWorkerReplicasDefaultToTwo proves worker.replicas defaults to 2 --
// "multi-pod safety is built in" is a claim about the DEFAULT topology, not
// an opt-in.
func TestWorkerReplicasDefaultToTwo(t *testing.T) {
	manifests := helmTemplate(t)

	var worker manifest
	for _, d := range byKind(manifests, "Deployment") {
		if d.component() == "worker" {
			worker = d
		}
	}
	if worker == nil {
		t.Fatalf("no worker Deployment rendered")
	}

	replicas, ok := getPath(worker, "spec", "replicas")
	if !ok {
		t.Fatalf("worker Deployment has no spec.replicas")
	}
	// encoding/json (which sigs.k8s.io/yaml decodes through) always
	// produces float64 for a bare YAML/JSON number decoded into `any`.
	got, ok := replicas.(float64)
	if !ok || got != 2 {
		t.Fatalf("worker Deployment spec.replicas = %v (%T), want 2", replicas, replicas)
	}
}

// TestMigrationJobHasPreInstallPreUpgradeHooks proves the migration Job
// carries the exact hook annotations that make it a pre-install/pre-upgrade
// hook (docs/adr/0002-migration-policy.md's "migrations apply via a Job
// before new pods receive traffic").
func TestMigrationJobHasPreInstallPreUpgradeHooks(t *testing.T) {
	manifests := helmTemplate(t)

	jobs := byKind(manifests, "Job")
	if len(jobs) != 1 {
		t.Fatalf("got %d Jobs, want exactly 1 (the migration Job)", len(jobs))
	}
	job := jobs[0]

	hook := getString(job, "metadata", "annotations", "helm.sh/hook")
	for _, want := range []string{"pre-install", "pre-upgrade"} {
		if !strings.Contains(hook, want) {
			t.Errorf("migration Job's helm.sh/hook annotation = %q, want it to contain %q", hook, want)
		}
	}

	args, ok := getPath(job, "spec", "template", "spec", "containers")
	if !ok {
		t.Fatalf("migration Job has no spec.template.spec.containers")
	}
	containers, ok := args.([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("migration Job has %v containers, want exactly 1", args)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("migration Job's container is not a map: %#v", containers[0])
	}
	cmdArgs, ok := container["args"].([]any)
	if !ok || len(cmdArgs) != 1 || cmdArgs[0] != "migrate" {
		t.Errorf("migration Job container args = %v, want [\"migrate\"]", container["args"])
	}
}

// TestAPIProbesPointAtRealHealthPaths proves the api Deployment's liveness
// and readiness probes target the endpoints internal/api/health.go actually
// serves -- not a placeholder, and not each other's path.
func TestAPIProbesPointAtRealHealthPaths(t *testing.T) {
	manifests := helmTemplate(t)

	var api manifest
	for _, d := range byKind(manifests, "Deployment") {
		if d.component() == "api" {
			api = d
		}
	}
	if api == nil {
		t.Fatalf("no api Deployment rendered")
	}

	containers, ok := getPath(api, "spec", "template", "spec", "containers")
	if !ok {
		t.Fatalf("api Deployment has no containers")
	}
	container := containers.([]any)[0].(map[string]any)

	livenessPath := getString(container, "livenessProbe", "httpGet", "path")
	if livenessPath != "/v1alpha1/healthz" {
		t.Errorf("api livenessProbe path = %q, want %q", livenessPath, "/v1alpha1/healthz")
	}
	readinessPath := getString(container, "readinessProbe", "httpGet", "path")
	if readinessPath != "/v1alpha1/readyz" {
		t.Errorf("api readinessProbe path = %q, want %q", readinessPath, "/v1alpha1/readyz")
	}
}

// TestImageDigestWinsOverTag proves every nodes-binary container (api,
// scheduler, worker, migrate) references repo@digest, not repo:tag, once
// image.digest is set -- values.yaml's own documented precedence.
func TestImageDigestWinsOverTag(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	manifests := helmTemplate(t,
		"--set", "image.tag=this-tag-must-be-ignored",
		"--set", "image.digest="+digest,
	)

	wantImage := "ghcr.io/agentculture/culture-nodes@" + digest
	checked := 0
	for _, m := range manifests {
		if m.component() == "postgres" {
			continue // the postgres:17 container is not this chart's image.
		}
		var containerPaths [][]string
		switch m.kind() {
		case "Deployment":
			containerPaths = [][]string{{"spec", "template", "spec", "containers"}}
		case "Job":
			containerPaths = [][]string{{"spec", "template", "spec", "containers"}}
		default:
			continue
		}
		for _, path := range containerPaths {
			raw, ok := getPath(m, path...)
			if !ok {
				continue
			}
			for _, c := range raw.([]any) {
				container := c.(map[string]any)
				image, _ := container["image"].(string)
				if image != wantImage {
					t.Errorf("%s %q container %q image = %q, want %q", m.kind(), m.name(), container["name"], image, wantImage)
				}
				checked++
			}
		}
	}
	if checked != 4 {
		t.Fatalf("checked %d containers, want exactly 4 (api, scheduler, worker, migrate)", checked)
	}
}

// TestContainerSecurityContextHasNumericRunAsUser is a regression test for
// a bug this chart genuinely shipped and only a real `helm install`
// against a live kind cluster caught (`helm template`/`helm lint` render
// the securityContext as syntactically valid YAML either way): setting
// runAsNonRoot: true WITHOUT an explicit numeric runAsUser fails every pod
// at admission against gcr.io/distroless/static-debian12:nonroot, because
// that image's USER directive is the *named* user "nonroot", not a numeric
// UID, and the kubelet cannot verify a named user is non-root from image
// metadata alone. See values.yaml's containerSecurityContext comment for
// the exact admission error this produced.
func TestContainerSecurityContextHasNumericRunAsUser(t *testing.T) {
	manifests := helmTemplate(t)

	checked := 0
	for _, m := range manifests {
		if m.component() == "postgres" {
			continue // not this chart's image; not in scope of the bug above.
		}
		var containers []any
		switch m.kind() {
		case "Deployment", "Job":
			raw, ok := getPath(m, "spec", "template", "spec", "containers")
			if !ok {
				continue
			}
			containers = raw.([]any)
		default:
			continue
		}
		for _, c := range containers {
			container := c.(map[string]any)
			sc, _ := container["securityContext"].(map[string]any)
			if sc == nil {
				t.Errorf("%s %q container %q has no securityContext", m.kind(), m.name(), container["name"])
				continue
			}
			runAsNonRoot, _ := sc["runAsNonRoot"].(bool)
			_, hasRunAsUser := sc["runAsUser"]
			if runAsNonRoot && !hasRunAsUser {
				t.Errorf("%s %q container %q sets runAsNonRoot without runAsUser -- fails admission against a distroless:nonroot image (named, not numeric, USER)", m.kind(), m.name(), container["name"])
			}
			checked++
		}
	}
	if checked != 4 {
		t.Fatalf("checked %d containers, want exactly 4 (api, scheduler, worker, migrate)", checked)
	}
}

// TestPostgresPasswordMatchesDatabaseURL is a regression test for a second
// bug this chart shipped alongside the one above: with no
// postgresql.auth.password pinned, the in-chart Postgres password is
// randomly generated (culture-nodes.postgresPassword's fallback,
// templates/_helpers.tpl) -- and calling that helper from two different
// template files independently mints two DIFFERENT random values, so the
// Postgres container's own POSTGRES_PASSWORD and NODES_DATABASE_URL's
// embedded password silently stopped matching. This chart now computes the
// password exactly once, in templates/secret-database.yaml, and threads it
// to both keys -- this test renders with the password left to generate
// randomly (deliberately NOT pinning postgresql.auth.password, unlike
// every other test in this file) and checks the two keys of the same
// rendered Secret agree.
func TestPostgresPasswordMatchesDatabaseURL(t *testing.T) {
	helmBin := requireHelm(t)
	cmd := exec.Command(helmBin, "template", "manifest-test", chartDir(t),
		"--set", "callback.tokenSecret=0123456789abcdef0123456789abcdef",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr:\n%s", err, stderr.String())
	}
	manifests := parseManifests(t, stdout.String())

	var dbSecret manifest
	for _, m := range byKind(manifests, "Secret") {
		if strings.HasSuffix(m.name(), "-db") {
			dbSecret = m
		}
	}
	if dbSecret == nil {
		t.Fatalf("no <fullname>-db Secret rendered")
	}

	password := getString(dbSecret, "stringData", "POSTGRES_PASSWORD")
	dbURL := getString(dbSecret, "stringData", "NODES_DATABASE_URL")
	if password == "" {
		t.Fatalf("the -db Secret has no POSTGRES_PASSWORD")
	}
	if !strings.Contains(dbURL, password) {
		t.Fatalf("NODES_DATABASE_URL %q does not contain POSTGRES_PASSWORD %q -- Postgres and the app would authenticate with different passwords", dbURL, password)
	}
}
