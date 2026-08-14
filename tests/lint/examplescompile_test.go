// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// Issue #73's recurrence half. `examples/pr-upkeep/workflow.yaml` shipped an
// authoring convention that had never compiled -- not because the convention
// was wrong, but because nothing ever compiled the examples. The defect
// survived a full task acceptance ("the convention is documented") precisely
// because "an author can copy this out of the example and it works" was never
// executed by anything.
//
// Two guards close that, and they are deliberately different in kind:
//
//   - TestEveryExampleWorkflowCompiles compiles every example in-process
//     through internal/compiler -- the same code path `nodes validate` runs --
//     so `go test ./...` is red the moment an example stops compiling.
//   - The gate-wiring tests below assert the CI job exists, runs the shared
//     script, and is *triggered* by a change under examples/. The path filter
//     is the subtle half: a job that never runs on the change that breaks an
//     example is indistinguishable from no job at all.
//
// Neither guard needs a control plane. Compilation is offline by construction
// (compiler.Compile takes bytes), which is what makes this gate cheap enough
// to be unconditional.

const (
	// exampleGateScript is the shared gate both CI and a human run.
	exampleGateScript = "scripts/validate-examples.sh"

	// goWorkflowFile is the CI workflow that hosts the gate.
	goWorkflowFile = ".github/workflows/go.yml"

	// exampleWorkflowFloor is the number of workflow files under examples/ at
	// the time this guard was written (task t5). It exists to catch a
	// *discovery* break: a walk that silently matches nothing, or nearly
	// nothing, would otherwise report a vacuous pass over zero files -- the
	// exact way a compile gate rots into decoration. Adding examples needs no
	// edit here; lowering it is a deliberate act that should accompany a
	// deliberate deletion.
	exampleWorkflowFloor = 11
)

// exampleTriggerPath is the path filter that must appear in go.yml's triggers
// for a change to an example to actually run the gate.
const exampleTriggerPath = "examples/**"

// discoverExampleWorkflows returns every workflow file under examples/, as
// repo-relative forward-slash paths, sorted. `.yaml` and `.yml` are both
// collected: the compiler reads anything that is not `.json` as YAML, so an
// example spelled either way is a workflow an author could copy.
func discoverExampleWorkflows(t *testing.T, root string) []string {
	t.Helper()

	var found []string
	examplesDir := filepath.Join(root, "examples")
	err := filepath.WalkDir(examplesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
		default:
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", examplesDir, err)
	}
	sort.Strings(found)

	if len(found) < exampleWorkflowFloor {
		t.Fatalf("found %d workflow file(s) under examples/, want at least %d.\n"+
			"Either discovery is broken (a gate over zero files passes vacuously, "+
			"which is what issue #73 is about) or examples were deleted -- if the "+
			"deletion was deliberate, lower exampleWorkflowFloor in the same commit.\nfound: %v",
			len(found), exampleWorkflowFloor, found)
	}
	return found
}

// TestEveryExampleWorkflowCompiles is the guard itself: every committed
// example must compile clean. It reports every failing example rather than
// stopping at the first, so one run tells an author the whole story.
func TestEveryExampleWorkflowCompiles(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range discoverExampleWorkflows(t, root) {
		source, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s: cannot read: %v", rel, err)
			continue
		}

		compiled, diagnostics, err := compiler.Compile(source, compiler.FormatForPath(rel))
		if err != nil {
			// A compiler fault, not a bad document -- `nodes validate` reports
			// this as an environment error (exit 2) for the same reason.
			t.Errorf("%s: compiler fault: %v", rel, err)
			continue
		}
		if compiled == nil {
			t.Errorf("%s: does not compile:\n%s", rel, renderDiagnostics(diagnostics))
		}
	}
}

// renderDiagnostics formats diagnostics the way `nodes validate` does in text
// mode, so a failure here reads the same as the CI job's output.
func renderDiagnostics(diagnostics []compiler.Diagnostic) string {
	if len(diagnostics) == 0 {
		return "  (no diagnostics)"
	}
	var b strings.Builder
	for _, d := range diagnostics {
		path := d.Path
		if path == "" {
			path = "<document>"
		}
		fmt.Fprintf(&b, "  %s %s %s: %s", d.Level, path, d.Code, d.Message)
		if d.Hint != "" {
			fmt.Fprintf(&b, " | hint: %s", d.Hint)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// ghWorkflow is the slice of GitHub Actions workflow syntax these tests read.
//
// `on:` is a YAML 1.1 boolean literal, so sigs.k8s.io/yaml (which converts
// YAML to JSON before unmarshalling) renders the trigger key as "true" rather
// than "on". Both spellings are declared so the tests survive either
// resolution rather than silently reading an empty trigger block -- a false
// pass in exactly the tests whose job is to prevent false passes.
type ghWorkflow struct {
	Jobs      map[string]ghJob `json:"jobs"`
	OnLiteral *ghTriggers      `json:"on"`
	OnBoolean *ghTriggers      `json:"true"`
}

// triggers returns whichever spelling of the trigger block the parser produced.
func (w ghWorkflow) triggers() *ghTriggers {
	if w.OnLiteral != nil {
		return w.OnLiteral
	}
	return w.OnBoolean
}

type ghTriggers struct {
	PullRequest *ghTrigger `json:"pull_request"`
	Push        *ghTrigger `json:"push"`
}

type ghTrigger struct {
	Paths []string `json:"paths"`
}

type ghJob struct {
	Name     string         `json:"name"`
	Steps    []ghStep       `json:"steps"`
	Services map[string]any `json:"services"`
	Env      map[string]any `json:"env"`
}

type ghStep struct {
	Uses string         `json:"uses"`
	Run  string         `json:"run"`
	Env  map[string]any `json:"env"`
}

// loadGoWorkflow parses .github/workflows/go.yml.
func loadGoWorkflow(t *testing.T, root string) ghWorkflow {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, goWorkflowFile))
	if err != nil {
		t.Fatalf("cannot read %s: %v", goWorkflowFile, err)
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("cannot parse %s: %v", goWorkflowFile, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s declares no jobs", goWorkflowFile)
	}
	return wf
}

// findExampleGateJob returns the id of the job that runs the gate script, or
// fails with the job ids it did see.
func findExampleGateJob(t *testing.T, wf ghWorkflow) (string, ghJob) {
	t.Helper()

	for _, id := range sortedJobIDs(wf) {
		for _, step := range wf.Jobs[id].Steps {
			if strings.Contains(step.Run, exampleGateScript) {
				return id, wf.Jobs[id]
			}
		}
	}
	t.Fatalf("no job in %s runs %s -- nothing compiles the examples in CI (issue #73).\njobs: %v",
		goWorkflowFile, exampleGateScript, sortedJobIDs(wf))
	return "", ghJob{}
}

func sortedJobIDs(wf ghWorkflow) []string {
	ids := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestExampleCompileGateIsWiredIntoCI asserts a CI job runs the gate script,
// and that the script it names is actually present and executable. A job
// invoking a missing script fails loudly, but a job invoking a non-executable
// one fails for a reason no one reads as "the examples are fine".
func TestExampleCompileGateIsWiredIntoCI(t *testing.T) {
	root := repoRoot(t)
	wf := loadGoWorkflow(t, root)
	jobID, job := findExampleGateJob(t, wf)

	info, err := os.Stat(filepath.Join(root, exampleGateScript))
	if err != nil {
		t.Fatalf("job %q runs %s, but it is not present: %v", jobID, exampleGateScript, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v); job %q would fail on invocation, "+
			"not on a broken example", exampleGateScript, info.Mode().Perm(), jobID)
	}

	// The job must check out the tree and have a Go toolchain: the gate builds
	// the `nodes` binary it validates with.
	var checkout, setupGo bool
	for _, step := range job.Steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			checkout = true
		case strings.HasPrefix(step.Uses, "actions/setup-go@"):
			setupGo = true
		}
	}
	if !checkout {
		t.Errorf("job %q does not check out the repository", jobID)
	}
	if !setupGo {
		t.Errorf("job %q does not set up Go; %s builds the nodes binary it validates with",
			jobID, exampleGateScript)
	}
}

// controlPlaneEnv matches the environment a control plane would need. The gate
// must not need one: `nodes validate` compiles offline, and a gate that
// quietly grew a database dependency would stop running on forks and in any
// environment without one -- which is most of them.
var controlPlaneEnv = regexp.MustCompile(`(?i)database|postgres|dsn`)

// TestExampleCompileGateNeedsNoControlPlane locks acceptance criterion 1's
// "with no control plane": the gate job declares no service containers and no
// database configuration.
func TestExampleCompileGateNeedsNoControlPlane(t *testing.T) {
	root := repoRoot(t)
	jobID, job := findExampleGateJob(t, loadGoWorkflow(t, root))

	if len(job.Services) > 0 {
		t.Errorf("job %q declares service containers %v; compiling an example needs no control plane",
			jobID, sortedKeys(job.Services))
	}
	for _, key := range sortedKeys(job.Env) {
		if controlPlaneEnv.MatchString(key) {
			t.Errorf("job %q sets %s; compiling an example needs no control plane", jobID, key)
		}
	}
	for i, step := range job.Steps {
		for _, key := range sortedKeys(step.Env) {
			if controlPlaneEnv.MatchString(key) {
				t.Errorf("job %q step %d sets %s; compiling an example needs no control plane",
					jobID, i, key)
			}
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestExampleCompileGateTriggersOnExampleChanges is the half that makes the
// gate real. go.yml filters on paths, so a job that is never triggered by a
// change under examples/ would have let issue #73's non-compiling example
// through untouched. Both the pull_request and push triggers must list the
// examples tree and the gate script itself.
func TestExampleCompileGateTriggersOnExampleChanges(t *testing.T) {
	root := repoRoot(t)
	triggers := loadGoWorkflow(t, root).triggers()
	if triggers == nil {
		t.Fatalf("%s declares no `on:` triggers", goWorkflowFile)
	}

	for _, tc := range []struct {
		event   string
		trigger *ghTrigger
	}{
		{"pull_request", triggers.PullRequest},
		{"push", triggers.Push},
	} {
		if tc.trigger == nil {
			t.Errorf("%s has no %s trigger", goWorkflowFile, tc.event)
			continue
		}
		// No paths filter at all means every change runs the job, which
		// satisfies the requirement -- the filter is the only way to miss it.
		if len(tc.trigger.Paths) == 0 {
			continue
		}
		for _, want := range []string{exampleTriggerPath, exampleGateScript} {
			if !containsString(tc.trigger.Paths, want) {
				t.Errorf("%s's %s paths filter does not list %q, so a change to it "+
					"would not run the example compile gate (issue #73)\npaths: %v",
					goWorkflowFile, tc.event, want, tc.trigger.Paths)
			}
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
