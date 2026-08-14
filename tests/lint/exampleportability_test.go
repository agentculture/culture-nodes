// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Task t16: a committed demo must be loadable by a deployment that is not
// this one. The audit that opened the task, in the order of how sharp it was:
//
//   - examples/pr-upkeep/workflow.yaml's sweep node fetched its script from a
//     raw.githubusercontent URL pinned to one org, one commit and one path.
//     A third party who loaded that demo got a graph that silently ran OUR
//     code -- not a portability wart, a supply-chain one.
//   - examples/codex-smoke-pair/smoke.workflow.yaml named this deployment's
//     machines in its node ids (`codex-thor`) and its run-input properties
//     (`thor_repo`), so the graph's own vocabulary assumed our fleet.
//   - six of the eleven committed workflows mentioned thor/orin/spark or a
//     192.168.1.x address somewhere in the file. (The task brief counted
//     nine; that count was inflated by a substring false positive -- "author"
//     contains "thor", and these files talk about authoring constantly. The
//     word-boundary count is six, and only the two above were load-bearing.)
//
// The doctrine these guards enforce. An environment-specific value reaches a
// graph through exactly three named sources, and nothing else:
//
//  1. RUN INPUT -- a `/run/input/...` pointer whose property is declared in
//     spec.contract.input. A reviewer traces it without leaving the document,
//     and a loader supplies it in the run's input JSON.
//  2. THE ACTOR/RUNNER REGISTRY -- an `actor://` or `runner://` id in `uses:`.
//     This is a registry KEY, not a host: internal/worker/registry.go resolves
//     the identity through the actors table (digest suffix stripped), so a
//     different deployment registers the same id against its own endpoint.
//  3. GRANTED ENVIRONMENT VALUES -- an `environmentRefs` name on a code
//     operation. The graph names the value; the deployment supplies it, and
//     the runner boundary refuses the operation BY NAME when it is missing
//     (internal/runners/headspace/bridge.go's resolveEnv).
//
// Everything else in a graph is deployment-independent. A hostname, an
// address, a filesystem path or a source URL may appear in a COMMENT, where
// it is provenance -- "this deployment observed the 403 on thor" is a fact
// worth keeping -- and never in a VALUE, where it is a requirement the loader
// cannot satisfy.
//
// Like crosshosthandoff_test.go, these assertions read the committed
// DOCUMENT rather than the compiled IR: what a third party copies is the
// document, and a value that only becomes portable after compilation is not
// one a reader can point at.

// deploymentConfigHeading is the heading every example workflow must carry in
// its header comments. It is the "a reviewer can point at each
// environment-specific value and say where it comes from" half of task t16:
// the graph's registry ids and granted environment values resolve OUTSIDE the
// document, so the document has to say where.
const deploymentConfigHeading = "Deployment configuration"

// The two environment values the pr-upkeep sweep needs granted. The script it
// runs is fetched at dispatch time, so WHOSE script it is must be a
// deployment's decision (the URL) and must be verifiable (the digest) --
// otherwise "configure the demo" quietly means "run the demo author's code".
const (
	sweepSourceURLRef    = "PR_UPKEEP_SWEEP_SOURCE_URL"
	sweepSourceDigestRef = "PR_UPKEEP_SWEEP_SOURCE_SHA256"
)

// hostMarker is one class of value that pins a graph to a deployment.
type hostMarker struct {
	name    string
	pattern *regexp.Regexp
	hint    string
}

// sourceURLPattern is broken out of hostMarkers because the argv guard below
// applies it on its own: a URL anywhere in a graph is a portability problem,
// but a URL in the argument list of something that then executes what it
// fetched is a supply-chain one, and the failure should say so.
var sourceURLPattern = regexp.MustCompile(`(?i)https?://`)

// hostMarkers are matched against the VALUE half of each line only.
//
// The hostname pattern deliberately does not use \b: `thor_repo` has a word
// character on both sides of "thor" and is exactly the kind of name this
// guard exists to catch, while `author`/`authority` -- which contain "thor"
// as a substring and appear all over these files' prose -- must not match.
// Requiring a non-alphanumeric neighbour (so `_` separates, letters do not)
// gets both cases right.
var hostMarkers = []hostMarker{
	{
		name:    "deployment hostname",
		pattern: regexp.MustCompile(`(?i)(^|[^a-z0-9])(thor|orin|spark)([^a-z0-9]|$)`),
		hint: "name the machine through the actor/runner registry id in `uses:` instead; " +
			"a hostname in a binding, a property name or a node id is one a loader cannot re-point",
	},
	{
		name:    "network address",
		pattern: regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
		hint: "an endpoint belongs in the actor registry row, which is where the engine already " +
			"resolves it from -- never in the graph",
	},
	{
		name:    "absolute host path",
		pattern: regexp.MustCompile(`(?i)(/home/|/root/|/Users/)[a-z0-9._-]`),
		hint: "a path is meaningful on exactly one host (issue #74); take it from run input, " +
			"or hand a portable handle instead",
	},
	{
		name:    "source URL",
		pattern: sourceURLPattern,
		hint: "a URL baked into a graph is a third party silently fetching the demo author's " +
			"bytes; name it as an `environmentRefs` value the deployment grants",
	},
}

// portableDoc is the slice of the workflow document these guards read. Only
// the asserted fields are declared, for the reason crosshosthandoff_test.go's
// wfDocument gives: a guard that decoded the whole schema would need editing
// by every unrelated authoring change.
type portableDoc struct {
	Spec struct {
		Nodes map[string]portableNode `json:"nodes"`
	} `json:"spec"`
}

type portableNode struct {
	Kind      string             `json:"kind"`
	Uses      string             `json:"uses"`
	Operation *portableOperation `json:"operation"`
	PreRun    *portableHook      `json:"pre_run"`
	PostRun   *portableHook      `json:"post_run"`
}

type portableHook struct {
	Operation portableOperation `json:"operation"`
}

type portableOperation struct {
	Argv            []string `json:"argv"`
	EnvironmentRefs []string `json:"environmentRefs"`
}

// loadPortable parses one example workflow document.
func loadPortable(t *testing.T, root, rel string) portableDoc {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	var doc portableDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cannot parse %s: %v", rel, err)
	}
	if len(doc.Spec.Nodes) == 0 {
		t.Fatalf("%s declares no nodes -- decoding is broken, and a guard over zero "+
			"nodes passes vacuously", rel)
	}
	return doc
}

// registryIDs returns every `actor://`/`runner://` identity the document
// places a node on, digest suffix stripped, sorted and de-duplicated. The
// stripped form is what internal/worker/registry.go looks up, so it is also
// what a deployment registers.
func (d portableDoc) registryIDs() []string {
	seen := map[string]bool{}
	for _, node := range d.Spec.Nodes {
		if node.Uses == "" {
			continue
		}
		id, _, _ := strings.Cut(node.Uses, "@")
		seen[id] = true
	}
	return sortedSet(seen)
}

// environmentRefs returns every granted environment value the document's code
// operations name, including hook operations.
func (d portableDoc) environmentRefs() []string {
	seen := map[string]bool{}
	for _, op := range d.operations() {
		for _, ref := range op.EnvironmentRefs {
			seen[ref] = true
		}
	}
	return sortedSet(seen)
}

// operations returns every code operation in the document, keyed by a label
// that names where it sits so a failure points at one place.
func (d portableDoc) operations() map[string]portableOperation {
	out := map[string]portableOperation{}
	for id, node := range d.Spec.Nodes {
		if node.Operation != nil {
			out[id] = *node.Operation
		}
		if node.PreRun != nil {
			out[id+" pre_run"] = node.PreRun.Operation
		}
		if node.PostRun != nil {
			out[id+" post_run"] = node.PostRun.Operation
		}
	}
	return out
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// splitYAMLComment splits a line into its value half and its comment half.
//
// It is deliberately simple: a line whose first non-space character is `#` is
// wholly a comment, and otherwise a comment begins at the first `#` preceded
// by whitespace. That mis-splits a `#` inside a quoted string or inside a
// block scalar's own code comments, which costs this guard a false NEGATIVE
// (it scans slightly less than the whole value) and never a false positive --
// the right direction for a lint that fails builds.
func splitYAMLComment(line string) (value, comment string) {
	if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "#") {
		return "", trimmed
	}
	if i := strings.Index(line, " #"); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

// isRegistryIdentityLine reports whether a line places a node on a registry
// identity. Those are the one value shape allowed to name a machine: the id is
// a key a deployment registers, not a host a loader must own. Every such id is
// separately required to be documented (see the deployment-configuration
// guard below), so the allowance is not a hole -- it is the second of the
// three named sources.
func isRegistryIdentityLine(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "uses:")
}

// TestNoExampleGraphValueNamesADeployment is task t16's first acceptance
// criterion made mechanical: loading an example into a different deployment
// must require changing only documented configuration, never editing the
// graph. A value naming this fleet's hostname, address, path or source URL is
// precisely an edit the loader would have to make.
func TestNoExampleGraphValueNamesADeployment(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range discoverExampleWorkflows(t, root) {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s: cannot read: %v", rel, err)
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			value, _ := splitYAMLComment(line)
			if strings.TrimSpace(value) == "" || isRegistryIdentityLine(value) {
				continue
			}
			for _, marker := range hostMarkers {
				match := marker.pattern.FindString(value)
				if match == "" {
					continue
				}
				t.Errorf("%s:%d: value names a %s (%q):\n    %s\nhint: %s",
					rel, i+1, marker.name, strings.TrimSpace(match), strings.TrimSpace(value), marker.hint)
			}
		}
	}
}

// TestEveryExampleDocumentsItsDeploymentConfiguration is the second
// acceptance criterion: a reviewer can point at each environment-specific
// value and say where it comes from.
//
// Run-input pointers already answer that question inside the document -- a
// `/run/input/x` names a property of spec.contract.input, and the loader
// supplies it. Registry identities and granted environment values do NOT:
// they resolve against a deployment's actor table and a worker process's
// environment, both outside the file. So the file has to name them, in a
// block a reader finds without knowing the codebase.
func TestEveryExampleDocumentsItsDeploymentConfiguration(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range discoverExampleWorkflows(t, root) {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s: cannot read: %v", rel, err)
			continue
		}

		var comments strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			_, comment := splitYAMLComment(line)
			if comment != "" {
				comments.WriteString(comment)
				comments.WriteByte('\n')
			}
		}
		prose := comments.String()

		if !strings.Contains(prose, deploymentConfigHeading) {
			t.Errorf("%s carries no %q block. Every value in this graph that resolves "+
				"OUTSIDE the document -- a registry id, a granted environment value -- has "+
				"to be nameable by a reader who has only the file (task t16).",
				rel, deploymentConfigHeading)
			continue
		}

		doc := loadPortable(t, root, rel)
		for _, id := range doc.registryIDs() {
			if !strings.Contains(prose, id) {
				t.Errorf("%s places a node on %s but never names it in its %q block. "+
					"That id is the actor/runner registry KEY a different deployment "+
					"registers against its own endpoint -- undocumented, it reads as a "+
					"host the loader must own.", rel, id, deploymentConfigHeading)
			}
		}
		for _, ref := range doc.environmentRefs() {
			if !strings.Contains(prose, ref) {
				t.Errorf("%s grants environment value %s to an operation but never names "+
					"it in its %q block. The runner boundary refuses the operation by name "+
					"when it is unset, and a loader cannot set a name nobody wrote down.",
					rel, ref, deploymentConfigHeading)
			}
		}
	}
}

// TestNoCodeOperationFetchesCodeFromAPinnedURL locks the sharpest finding of
// the t16 audit as a class, not as one fixed line. A URL in argv means the
// bytes that execute are chosen by whoever committed the graph, and a loader
// who "only configured the demo" is running them.
func TestNoCodeOperationFetchesCodeFromAPinnedURL(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range discoverExampleWorkflows(t, root) {
		doc := loadPortable(t, root, rel)
		for label, op := range doc.operations() {
			for i, arg := range op.Argv {
				loc := sourceURLPattern.FindStringIndex(arg)
				if loc == nil {
					continue
				}
				end := loc[0] + 60
				if end > len(arg) {
					end = len(arg)
				}
				t.Errorf("%s: operation %s argv[%d] contains an absolute URL (%s). "+
					"A graph that names WHERE its code comes from hands every loader the "+
					"same origin; name the source as an environmentRefs value the "+
					"deployment grants, and verify what comes back.",
					rel, label, i, strings.TrimSpace(arg[loc[0]:end]))
			}
		}
	}
}

// TestSweepNamesItsScriptSourceAsGrantedConfiguration is the positive half of
// the guard above, for the one example that genuinely has to fetch a script:
// the pr-upkeep image carries no checkout, and a code node has no file-mount
// mechanism yet (issue #50). Fetching is therefore fine; fetching a fixed
// origin without saying so is not.
//
// Both refs are required rather than one. The URL alone makes the origin a
// deployment's choice but leaves its CONTENT free to change under a mutable
// ref; the digest is what makes "I know which bytes ran" true for the loader
// the same way it is true for the image above it.
func TestSweepNamesItsScriptSourceAsGrantedConfiguration(t *testing.T) {
	doc := loadPortable(t, repoRoot(t), prUpkeepWorkflowPath)

	sweep, ok := doc.Spec.Nodes["sweep"]
	if !ok {
		t.Fatalf("%s declares no `sweep` node", prUpkeepWorkflowPath)
	}
	if sweep.Operation == nil {
		t.Fatalf("%s's sweep node declares no operation", prUpkeepWorkflowPath)
	}

	refs := sweep.Operation.EnvironmentRefs
	for _, want := range []string{sweepSourceURLRef, sweepSourceDigestRef} {
		if !containsString(refs, want) {
			t.Errorf("sweep's operation does not grant %s (environmentRefs: %v). "+
				"Without it the script's origin is whatever the committed graph says, "+
				"which is the t16 finding: a third party loading this example runs the "+
				"demo author's code.", want, refs)
		}
	}

	argv := strings.Join(sweep.Operation.Argv, "\n")
	for _, want := range []string{sweepSourceURLRef, sweepSourceDigestRef} {
		if !strings.Contains(argv, want) {
			t.Errorf("sweep's argv never reads %s, so granting it changes nothing. "+
				"The bootstrap has to fetch the URL it is given and refuse bytes that do "+
				"not match the digest it is given.", want)
		}
	}
}
