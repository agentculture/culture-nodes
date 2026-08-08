package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exec the real binary (built by TestMain in conformance_test.go),
// so what they assert is exactly what an agent parsing the CLI would see.

// writeWorkflow drops a workflow file into a temp dir and returns the dir and
// the file's path.
func writeWorkflow(t *testing.T, name, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return dir, path
}

// validWorkflow is the smallest workflow that compiles clean.
const validWorkflow = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: cli-fixture
  version: 1.0.0
  ownerRef: team/platform-ai
spec:
  entry: start
  contract:
    input:
      schemaRef: ./contracts/in.schema.json
    output:
      schemaRef: ./contracts/out.schema.json
  nodes:
    start:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/start@sha256:aaaaaa
      contract:
        outcomes:
          completed:
            schemaRef: ./contracts/start.schema.json
      input:
        from: /run/input
    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/start/output
  edges:
    - from: start.completed
      to: finish
`

// invalidWorkflow points its entry at a node that does not exist.
var invalidWorkflow = strings.Replace(validWorkflow, "  entry: start\n", "  entry: ghost\n", 1)

func TestValidateValidWorkflowText(t *testing.T) {
	dir, path := writeWorkflow(t, "workflow.yaml", validWorkflow)
	r := runNodes(t, dir, "validate", path)

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout=%s\nstderr=%s", r.ExitCode, r.Stdout, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "valid: cli-fixture 1.0.0") {
		t.Errorf("stdout = %q, want a valid summary naming the workflow", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "digest: sha256:") {
		t.Errorf("stdout = %q, want a digest line", r.Stdout)
	}
}

func TestValidateValidWorkflowJSON(t *testing.T) {
	dir, path := writeWorkflow(t, "workflow.yaml", validWorkflow)
	r := runNodes(t, dir, "validate", path, "--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout=%s\nstderr=%s", r.ExitCode, r.Stdout, r.Stderr)
	}

	var payload struct {
		Valid       bool   `json:"valid"`
		Digest      string `json:"digest"`
		Diagnostics []struct {
			Level   string `json:"level"`
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"diagnostics"`
	}
	assertSingleLineJSON(t, r.Stdout, &payload)
	if !payload.Valid {
		t.Error("valid = false for a clean workflow")
	}
	if !strings.HasPrefix(payload.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256: digest", payload.Digest)
	}
	if len(payload.Diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want none", payload.Diagnostics)
	}
	// A clean run must still emit a JSON array, never null.
	if !strings.Contains(r.Stdout, `"diagnostics":[]`) {
		t.Errorf("stdout = %q, want an empty diagnostics array rather than null", r.Stdout)
	}
}

// TestValidateInvalidWorkflowIsAResultNotAnError is the domain-outcome
// distinction: an invalid workflow is a verdict, so it prints to stdout and
// stderr stays empty even though the exit code is non-zero.
func TestValidateInvalidWorkflowIsAResultNotAnError(t *testing.T) {
	dir, path := writeWorkflow(t, "workflow.yaml", invalidWorkflow)
	r := runNodes(t, dir, "validate", path)

	assertNeverMixed(t, r)
	if r.Stderr != "" {
		t.Fatalf("stderr = %q, want empty — an invalid workflow is a result, not a CliError", r.Stderr)
	}
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "graph.entry_unknown") {
		t.Errorf("stdout = %q, want the precise diagnostic code", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "hint: ") {
		t.Errorf("stdout = %q, want each diagnostic to carry its hint", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "invalid: 1 error, 0 warnings") {
		t.Errorf("stdout = %q, want the summary line", r.Stdout)
	}
}

func TestValidateInvalidWorkflowJSON(t *testing.T) {
	dir, path := writeWorkflow(t, "workflow.yaml", invalidWorkflow)
	r := runNodes(t, dir, "validate", path, "--json")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}

	var payload struct {
		Valid       bool   `json:"valid"`
		Digest      string `json:"digest"`
		Diagnostics []struct {
			Level string `json:"level"`
			Path  string `json:"path"`
			Code  string `json:"code"`
			Hint  string `json:"hint"`
		} `json:"diagnostics"`
	}
	assertSingleLineJSON(t, r.Stdout, &payload)
	if payload.Valid {
		t.Error("valid = true for a workflow with an unknown entry node")
	}
	if payload.Digest != "" {
		t.Errorf("digest = %q, want empty — no digest is issued for a workflow that does not compile", payload.Digest)
	}
	if len(payload.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", payload.Diagnostics)
	}
	got := payload.Diagnostics[0]
	if got.Level != "error" || got.Path != "/spec/entry" || got.Code != "graph.entry_unknown" || got.Hint == "" {
		t.Errorf("diagnostic = %+v, want a /spec/entry graph.entry_unknown error with a hint", got)
	}
}

// TestValidateJSONFileFormat checks the .json path: the same workflow written
// as JSON must compile to the same digest as its YAML form.
func TestValidateJSONFileFormat(t *testing.T) {
	yamlDir, yamlPath := writeWorkflow(t, "workflow.yaml", validWorkflow)
	fromYAML := runNodes(t, yamlDir, "validate", yamlPath, "--json")

	jsonBody := `{"apiVersion":"nodes.culture.dev/v1alpha1","kind":"Workflow",
"metadata":{"name":"cli-fixture","version":"1.0.0","ownerRef":"team/platform-ai"},
"spec":{"entry":"start",
"contract":{"input":{"schemaRef":"./contracts/in.schema.json"},"output":{"schemaRef":"./contracts/out.schema.json"}},
"nodes":{"start":{"kind":"agent","ownerRef":"team/platform-ai","uses":"actor://company/start@sha256:aaaaaa",
"contract":{"outcomes":{"completed":{"schemaRef":"./contracts/start.schema.json"}}},"input":{"from":"/run/input"}},
"finish":{"kind":"end","ownerRef":"team/platform-ai","output":{"from":"/nodes/start/output"}}},
"edges":[{"from":"start.completed","to":"finish"}]}}`
	jsonDir, jsonPath := writeWorkflow(t, "workflow.json", jsonBody)
	fromJSON := runNodes(t, jsonDir, "validate", jsonPath, "--json")

	if fromJSON.ExitCode != 0 {
		t.Fatalf("JSON form exit code = %d, want 0\nstdout=%s\nstderr=%s", fromJSON.ExitCode, fromJSON.Stdout, fromJSON.Stderr)
	}

	var yamlPayload, jsonPayload struct {
		Digest string `json:"digest"`
	}
	assertSingleLineJSON(t, fromYAML.Stdout, &yamlPayload)
	assertSingleLineJSON(t, fromJSON.Stdout, &jsonPayload)
	if yamlPayload.Digest != jsonPayload.Digest {
		t.Fatalf("YAML digest %q != JSON digest %q", yamlPayload.Digest, jsonPayload.Digest)
	}
}

func TestValidateUnreadableFileIsAnEnvironmentError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "validate", filepath.Join(dir, "does-not-exist.yaml"))

	assertNeverMixed(t, r)
	if r.Stdout != "" {
		t.Fatalf("stdout = %q, want empty", r.Stdout)
	}
	if r.ExitCode != 2 {
		t.Fatalf("exit code = %d, want 2", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
}

func TestValidateMissingArgumentIsAUserError(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "validate")

	assertNeverMixed(t, r)
	if r.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", r.ExitCode)
	}
	assertErrorHintShape(t, r.Stderr)
}

func TestValidateIsNoLongerAStubMode(t *testing.T) {
	dir, path := writeWorkflow(t, "workflow.yaml", validWorkflow)
	r := runNodes(t, dir, "validate", path)
	if strings.Contains(r.Stderr, "not implemented yet") {
		t.Fatal("validate still reports the stub-mode CliError")
	}
	if strings.Contains(runNodes(t, dir, "learn").Stdout, "all|validate|run") {
		t.Error("learn still lists validate among the unimplemented process modes")
	}
}

func TestExplainValidateDescribesTheRealVerb(t *testing.T) {
	dir := t.TempDir()
	r := runNodes(t, dir, "explain", "validate")

	assertNeverMixed(t, r)
	if r.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", r.ExitCode)
	}
	if strings.Contains(r.Stdout, "not implemented") {
		t.Errorf("explain validate still describes an unimplemented mode:\n%s", r.Stdout)
	}
	for _, want := range []string{"nodes validate", "diagnostics", "digest"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("explain validate does not mention %q:\n%s", want, r.Stdout)
		}
	}
}
