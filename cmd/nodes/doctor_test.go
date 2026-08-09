package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckGoBinaryReportsCompiledRuntime(t *testing.T) {
	// The check spawns no subprocess (the control-plane no-external-exec
	// rule): it reports the runtime this binary was compiled with.
	check := checkGoBinary()
	if check.Status != doctorStatusOK {
		t.Fatalf("checkGoBinary() = %+v, want status %q", check, doctorStatusOK)
	}
	if !strings.Contains(check.Detail, runtime.Version()) {
		t.Fatalf("Detail %q does not carry runtime.Version() %q", check.Detail, runtime.Version())
	}
}

func TestCheckCultureYAMLOkWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "culture.yaml"), []byte("agents: []\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	withCwd(t, dir)

	check := checkCultureYAML()
	if check.Status != doctorStatusOK {
		t.Fatalf("checkCultureYAML() = %+v, want status %q", check, doctorStatusOK)
	}
}

func TestCheckCultureYAMLFailsWhenAbsent(t *testing.T) {
	withCwd(t, t.TempDir())

	check := checkCultureYAML()
	if check.Status != doctorStatusFail {
		t.Fatalf("checkCultureYAML() = %+v, want status %q", check, doctorStatusFail)
	}
}

func TestRunDoctorChecksHealthyRequiresEveryCheckOK(t *testing.T) {
	withCwd(t, t.TempDir())

	report := runDoctorChecks()
	if report.Healthy {
		t.Fatal("runDoctorChecks() reported healthy with no culture.yaml present")
	}
	if len(report.Checks) != 2 {
		t.Fatalf("len(Checks) = %d, want 2", len(report.Checks))
	}
}

func TestRenderDoctorTextIncludesEveryCheck(t *testing.T) {
	report := doctorReport{
		Healthy: false,
		Checks: []doctorCheck{
			{Check: "go_binary_version", Status: doctorStatusOK, Detail: "go1.26.5"},
			{Check: "culture_yaml_present", Status: doctorStatusFail, Detail: "not found"},
		},
	}
	text := renderDoctorText(report)
	if !strings.Contains(text, "unhealthy") {
		t.Errorf("text %q does not mention unhealthy", text)
	}
	for _, want := range []string{"go_binary_version", "culture_yaml_present", "go1.26.5", "not found"} {
		if !strings.Contains(text, want) {
			t.Errorf("text does not contain %q:\n%s", want, text)
		}
	}
}

// cmdDoctor's own exit-code-vs-health wiring (and the fact that an
// unhealthy verdict is a result, not a CliError) is exercised end-to-end by
// the subprocess-based conformance test, which can observe stdout/stderr
// without a unit test writing straight to the real process streams.
