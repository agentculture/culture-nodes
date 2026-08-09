package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// doctorCheck is one row of `nodes doctor`'s {check,status,detail} table.
type doctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Healthy bool          `json:"healthy"`
	Checks  []doctorCheck `json:"checks"`
}

const (
	doctorStatusOK   = "ok"
	doctorStatusFail = "fail"
)

// checkGoBinary reports the Go runtime this binary was compiled with. It
// deliberately spawns no subprocess: doctor is a diagnostic verb in a
// control-plane binary whose ground rule is that nothing in it executes
// external programs — and the toolchain that built this binary is a more
// truthful fact about it than whatever `go` happens to be on PATH.
func checkGoBinary() doctorCheck {
	return doctorCheck{
		Check:  "go_runtime_version",
		Status: doctorStatusOK,
		Detail: runtime.Version() + " (compiled in; no toolchain probe)",
	}
}

// checkCultureYAML verifies culture.yaml is discoverable walking up from
// the current directory (the same lookup whoami uses for identity).
func checkCultureYAML() doctorCheck {
	path, ok := findCultureYAML()
	if !ok {
		cwd, _ := os.Getwd()
		return doctorCheck{
			Check:  "culture_yaml_present",
			Status: doctorStatusFail,
			Detail: fmt.Sprintf("no culture.yaml found walking up from %s", cwd),
		}
	}
	return doctorCheck{
		Check:  "culture_yaml_present",
		Status: doctorStatusOK,
		Detail: path,
	}
}

func runDoctorChecks() doctorReport {
	checks := []doctorCheck{checkGoBinary(), checkCultureYAML()}
	healthy := true
	for _, c := range checks {
		if c.Status != doctorStatusOK {
			healthy = false
		}
	}
	return doctorReport{Healthy: healthy, Checks: checks}
}

func renderDoctorText(report doctorReport) string {
	status := "healthy"
	if !report.Healthy {
		status = "unhealthy"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "nodes doctor: %s\n\n", status)

	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "check\tstatus\tdetail")
	for _, c := range report.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Check, c.Status, c.Detail)
	}
	_ = tw.Flush()

	return strings.TrimRight(b.String(), "\n")
}

func cmdDoctor(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("doctor", err)
	}

	report := runDoctorChecks()

	// Doctor's healthy/unhealthy verdict is a domain outcome carried by the
	// exit code and the result body, not a CliError — it always prints to
	// stdout via EmitResult/EmitResultJSON, never through EmitError, even
	// when the exit code is non-zero.
	if jsonMode {
		if err := clifmt.EmitResultJSON(report); err != nil {
			return 0, err
		}
	} else {
		clifmt.EmitResult(renderDoctorText(report))
	}

	if report.Healthy {
		return clifmt.ExitSuccess, nil
	}
	return clifmt.ExitEnvError, nil
}
