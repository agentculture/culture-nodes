package testslint

// The all-backends rule, enforced (issue #67, task t15).
//
// The preflight capability surface is ONE contract: the engine composes a
// briefing from whatever a bridge advertised (internal/preflight), and each
// bridge contributes only its own measured host facts. The failure mode this
// file exists to catch is the one the repo has already paid for once —
// `resolve_actor_row_id` shipped as the same bug in three separate deploy
// lanes because each lane inlined its own copy instead of calling a shared
// helper (see deploy/prod/deploy.sh's comment at that function, and
// tests/deploy/humaninboxdeploylane_test.go, which is this file's sibling in
// spirit).
//
// So the guards here are about SHAPE, not behaviour (each adapter's own
// pytest suite covers behaviour):
//
//  1. `preflight.py` is byte-identical in every bridge that has it.
//  2. Its protocol version agrees with the Go control plane's, across the
//     language boundary.
//  3. A bridge either advertises the surface completely or not at all —
//     no bridge half-implements it.
//  4. No adapter re-declares the protocol in its own backend-specific file.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/preflight"
)

// advertisingAdapters are the four bridges task t15 puts the surface on.
// adapters/human-inbox is deliberately absent: it advertises nothing, and it
// is the living subject of the task's second acceptance criterion — a bridge
// that does not advertise the surface leaves its actor dispatching exactly as
// before. Guard 3 below is what keeps that a choice rather than a half-done
// job, and what lets human-inbox opt in later without editing this list.
var advertisingAdapters = []string{"claude-code", "codex", "colleague", "notify"}

// sharedModule is the protocol file — the one guards 2, 3 and 4 read.
const sharedModule = "preflight.py"

// sharedModules are ALL the files that must be identical everywhere.
//
// `deployment.py` (task t32) joined `preflight.py` for a blunt reason:
// preflight.py was 79 lines from the repo's 1000-line hard limit and the
// deployed-revision measurement is a self-contained concern. Splitting it
// without extending guard 1 would have created exactly the thing that guard
// exists to prevent — a shared module free to diverge between four bridges —
// so the split and this list are one change.
var sharedModules = []string{sharedModule, "deployment.py"}

// backendModule is the only per-bridge file in this feature.
const backendModule = "capabilities.py"

// adapterPackage is one adapter's Python package directory.
type adapterPackage struct {
	adapter string // adapters/<adapter>
	dir     string // absolute path to src/<pkg>
}

// discoverAdapterPackages finds every adapters/*/src/<pkg> directory, so a
// fifth bridge is covered by these guards the day it is added rather than the
// day someone remembers to add it to a list.
func discoverAdapterPackages(t *testing.T) []adapterPackage {
	t.Helper()
	root := filepath.Join(repoRoot(t), "adapters")
	adapters, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read adapters/: %v", err)
	}

	var found []adapterPackage
	for _, adapter := range adapters {
		if !adapter.IsDir() {
			continue
		}
		srcDir := filepath.Join(root, adapter.Name(), "src")
		packages, err := os.ReadDir(srcDir)
		if err != nil {
			continue // an adapter without a src/ layout is not a Python bridge
		}
		for _, pkg := range packages {
			if pkg.IsDir() {
				found = append(found, adapterPackage{
					adapter: adapter.Name(),
					dir:     filepath.Join(srcDir, pkg.Name()),
				})
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no adapters/*/src/<package> directories found; the scan is looking in the wrong place")
	}
	sort.Slice(found, func(i, j int) bool { return found[i].adapter < found[j].adapter })
	return found
}

func (p adapterPackage) has(t *testing.T, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(p.dir, name))
	return err == nil
}

func (p adapterPackage) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		t.Fatalf("read %s/%s: %v", p.adapter, name, err)
	}
	return string(raw)
}

// TestTheCapabilitySurfaceIsOneModuleNotFourInlineCopies is guard 1.
func TestTheCapabilitySurfaceIsOneModuleNotFourInlineCopies(t *testing.T) {
	for _, module := range sharedModules {
		t.Run(module, func(t *testing.T) { assertModuleIsIdenticalEverywhere(t, module) })
	}
}

// assertModuleIsIdenticalEverywhere is guard 1's body for ONE shared module.
func assertModuleIsIdenticalEverywhere(t *testing.T, sharedModule string) {
	t.Helper()
	digests := map[string][]string{}
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, sharedModule) {
			continue
		}
		sum := sha256.Sum256([]byte(pkg.read(t, sharedModule)))
		digest := hex.EncodeToString(sum[:])
		digests[digest] = append(digests[digest], pkg.adapter)
	}

	if len(digests) == 0 {
		t.Fatalf("no adapter ships %s; the capability surface task t15 built is gone", sharedModule)
	}
	if len(digests) > 1 {
		var lines []string
		for digest, adapters := range digests {
			sort.Strings(adapters)
			lines = append(lines, fmt.Sprintf("  %s: %s", digest[:12], strings.Join(adapters, ", ")))
		}
		sort.Strings(lines)
		t.Fatalf("%s has diverged between bridges — it must be byte-identical everywhere, because "+
			"four adapters each carrying their own version of one protocol is exactly how "+
			"resolve_actor_row_id shipped as the same bug three times. Anything backend-specific "+
			"belongs in %s.\n%s", sharedModule, backendModule, strings.Join(lines, "\n"))
	}

	// And the four the task names must actually be among them.
	advertising := map[string]bool{}
	for _, adapters := range digests {
		for _, adapter := range adapters {
			advertising[adapter] = true
		}
	}
	for _, want := range advertisingAdapters {
		if !advertising[want] {
			t.Errorf("adapters/%s does not ship %s: the all-backends rule says a capability "+
				"surface on some bridges and not others is a bug, not a rollout", want, sharedModule)
		}
	}
}

// pythonConstant reads a module-level `NAME = "value"` string constant.
func pythonConstant(t *testing.T, source, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` = "([^"]*)"`)
	m := re.FindStringSubmatch(source)
	if m == nil {
		t.Fatalf("no module-level constant %s found in %s", name, sharedModule)
	}
	return m[1]
}

// TestThePythonAndGoHalvesAgreeOnTheProtocol is guard 2.
//
// The version is the one field whose disagreement is silent in the worst way:
// internal/preflight.ParseSurface refuses a surface declaring a version it
// does not speak, so a bridge bumped on its own would not advertise a wrong
// fact — it would stop advertising at all, and the operator would find out
// when a gated dispatch could no longer be configured.
func TestThePythonAndGoHalvesAgreeOnTheProtocol(t *testing.T) {
	packages := discoverAdapterPackages(t)
	var source string
	for _, pkg := range packages {
		if pkg.has(t, sharedModule) {
			source = pkg.read(t, sharedModule)
			break
		}
	}
	if source == "" {
		t.Fatalf("no adapter ships %s", sharedModule)
	}

	if got := pythonConstant(t, source, "PROTOCOL_VERSION"); got != preflight.ProtocolVersion {
		t.Errorf("the bridges advertise protocol version %q while internal/preflight speaks %q; "+
			"a bridge that declares a version this control plane does not know is refused at "+
			"configuration time", got, preflight.ProtocolVersion)
	}
	if got := pythonConstant(t, source, "CAPABILITY_KEY"); got != preflight.CapabilityKey {
		t.Errorf("the bridges advertise under capabilities.%q while internal/preflight reads "+
			"capabilities.%q", got, preflight.CapabilityKey)
	}
	if got := pythonConstant(t, source, "CAPABILITIES_PATH"); got != actors.CapabilitiesPath {
		t.Errorf("the bridges serve their surface at %q while internal/actors declares %q; the "+
			"conformance kit probes the declared path, so an operator following the protocol "+
			"would find nothing there", got, actors.CapabilitiesPath)
	}
}

// TestABridgeAdvertisesTheSurfaceCompletelyOrNotAtAll is guard 3 — the one
// that makes acceptance criterion 2 a property rather than a hope. A bridge
// with none of these files dispatches exactly as it did before t15 (today:
// adapters/human-inbox). A bridge with some of them is the state nobody
// wants: an operator reads a capability route in one place, a registration
// helper in another, and the two disagree about what this host does.
func TestABridgeAdvertisesTheSurfaceCompletelyOrNotAtAll(t *testing.T) {
	for _, pkg := range discoverAdapterPackages(t) {
		t.Run(pkg.adapter, func(t *testing.T) { assertSurfaceIsWholeOrAbsent(t, pkg) })
	}
}

// assertSurfaceIsWholeOrAbsent is guard 3's body for ONE adapter. It lives
// outside the subtest closure so the checks read as a flat list rather than
// nested two deep inside a loop.
func assertSurfaceIsWholeOrAbsent(t *testing.T, pkg adapterPackage) {
	t.Helper()

	hasShared := pkg.has(t, sharedModule)
	hasBackend := pkg.has(t, backendModule)
	if !hasShared && !hasBackend {
		t.Skipf("adapters/%s advertises no capability surface, and dispatches exactly as "+
			"it did before task t15", pkg.adapter)
	}
	if hasShared != hasBackend {
		t.Fatalf("adapters/%s ships one half of the surface (%s=%v, %s=%v): the protocol "+
			"without the facts advertises nothing, and the facts without the protocol are "+
			"a second dialect", pkg.adapter, sharedModule, hasShared, backendModule, hasBackend)
	}
	// Every shared module, not only the protocol one: a bridge carrying
	// preflight.py without deployment.py advertises a `deployment` key its
	// capabilities.py cannot fill, which is the half-done state this guard
	// exists to refuse.
	for _, module := range sharedModules {
		if !pkg.has(t, module) {
			t.Fatalf("adapters/%s advertises the capability surface but does not ship the shared "+
				"%s: a bridge takes every shared module or none of them", pkg.adapter, module)
		}
	}

	server := pkg.read(t, "server.py")
	requireContains(t, server, "preflight.CAPABILITIES_PATH",
		fmt.Sprintf("adapters/%s serves no route at preflight.CAPABILITIES_PATH: an operator "+
			"registering this actor cannot read the facts off the host that has them", pkg.adapter))
	requireContains(t, server, "capabilities.host_facts",
		fmt.Sprintf("adapters/%s does not build its surface from %s.host_facts",
			pkg.adapter, backendModule))

	requireContains(t, pkg.read(t, "__main__.py"), "--print-capabilities",
		fmt.Sprintf("adapters/%s has no --print-capabilities flag: registering an actor "+
			"before its bridge has ever started would mean hand-writing host facts", pkg.adapter))
}

// sharedImportRE matches a `from <package> import ..., preflight, ...` line.
var sharedImportRE = regexp.MustCompile(`(?m)^from \w+ import (?:[\w, ]*, )?preflight\b`)

// requireContains reports msg when haystack lacks needle. It exists so a run
// of "this file must mention that symbol" checks stays a flat list of calls
// instead of a stack of near-identical if blocks.
func requireContains(t *testing.T, haystack, needle, msg string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Error(msg)
	}
}

// TestNoBridgeRedeclaresTheProtocolInItsOwnFile is guard 4.
//
// The backend-specific module supplies FACTS. The moment it also carries a
// version string or composes the `preflight` block itself, there are two
// definitions of one document and only one of them gets updated next time.
func TestNoBridgeRedeclaresTheProtocolInItsOwnFile(t *testing.T) {
	banned := []string{
		`"protocol_version"`,
		`'protocol_version'`,
		`"` + preflight.CapabilityKey + `":`,
	}
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, backendModule) {
			continue
		}
		source := pkg.read(t, backendModule)
		// Matched as an import-list member rather than as the literal
		// "import preflight": task t32 added a second shared module, so the
		// real line reads `from <pkg> import deployment, preflight` and a
		// substring check silently stopped guarding anything.
		if !sharedImportRE.MatchString(source) {
			t.Errorf("adapters/%s's %s does not import the shared %s",
				pkg.adapter, backendModule, sharedModule)
		}
		for _, needle := range banned {
			if strings.Contains(source, needle) {
				t.Errorf("adapters/%s's %s contains %s: the document's shape is %s's alone — "+
					"this file supplies measured facts and calls preflight.host_block",
					pkg.adapter, backendModule, needle, sharedModule)
			}
		}
	}
}
