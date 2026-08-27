package testslint

// The worktree reaper's two structural guarantees (task t17), enforced.
//
// `reap.py` is the other half of `workspace.provision()`: the provisioner
// mints one worktree per writer, the reaper gives them back. Two properties
// of that module are not things a Python unit test can protect, because both
// are about what the SOURCE says rather than what one bridge does:
//
//  1. It is byte-identical in every bridge that mints worktrees. Same
//     argument as preflightsurface_test.go's guard 1: three adapters each
//     carrying their own reaper is how one wrong reap becomes three wrong
//     reaps, and a reaper is the last place in this repo where a divergence
//     is merely cosmetic.
//
//  2. It never passes `--force` to `git worktree remove`. This is the whole
//     safety argument of the module, and it is a probed fact rather than a
//     preference:
//
//     $ git worktree remove <dirty>
//     fatal: '<dirty>' contains modified or untracked files, use --force to
//     delete it
//
//     That refusal is what stands between housekeeping and issue #78's data
//     loss. A future edit that "fixes" a stubborn worktree by forcing it
//     would pass every unit test in adapters/*/tests/test_reap.py — the
//     tests assert refusals, and a forcing reaper still refuses everything
//     it refuses today, it just also destroys what it used to leave alone.
//     So the guard has to live here, on the text.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reaperModules are the files that must be identical in every
// worktree-minting bridge: the half that decides, and the half that acts.
var reaperModules = []string{"reap.py", "reclaim.py"}

// provisionerModule is what makes a bridge a worktree-minting one. A bridge
// with a workspace owes a reaper; a bridge without one (notify,
// human-inbox: no checkout at all) owes nothing.
const provisionerModule = "workspace.py"

// TestTheReaperIsOneModuleNotThreeCopies is guard 1.
func TestTheReaperIsOneModuleNotThreeCopies(t *testing.T) {
	// A bridge with a workspace owes a reaper; one without a workspace has
	// nothing to reap and is not this guard's business.
	var provisioners []adapterPackage
	for _, pkg := range discoverAdapterPackages(t) {
		if pkg.has(t, provisionerModule) {
			provisioners = append(provisioners, pkg)
		}
	}
	for _, reaperModule := range reaperModules {
		assertReaperModuleIsUniform(t, provisioners, reaperModule)
	}
}

// assertReaperModuleIsUniform is guard 1 for one module: every worktree-minting
// bridge ships it, and every copy is byte-identical.
func assertReaperModuleIsUniform(t *testing.T, provisioners []adapterPackage, reaperModule string) {
	t.Helper()
	digests := map[string][]string{}
	var missing []string
	for _, pkg := range provisioners {
		if !pkg.has(t, reaperModule) {
			missing = append(missing, pkg.adapter)
			continue
		}
		sum := sha256.Sum256([]byte(pkg.read(t, reaperModule)))
		digest := hex.EncodeToString(sum[:])
		digests[digest] = append(digests[digest], pkg.adapter)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("adapters/%s mint worktrees (%s) but ship no %s: a bridge that can create a "+
			"writer's checkout and cannot give it back leaks one directory per node run",
			strings.Join(missing, ", "), provisionerModule, reaperModule)
	}
	if len(digests) == 0 {
		t.Fatalf("no adapter ships %s; the reaper task t17 built is gone", reaperModule)
	}
	if len(digests) > 1 {
		t.Fatalf("%s has diverged between bridges — it must be byte-identical everywhere. A "+
			"reaper is the one place where a divergence nobody noticed removes a directory "+
			"nobody agreed to remove.\n%s", reaperModule, digestGroupLines(digests))
	}
}

// digestGroupLines renders a digest -> adapters map as sorted `  <digest12>:
// <adapters>` lines, so a divergence failure names which bridges agree with
// which.
func digestGroupLines(digests map[string][]string) string {
	var lines []string
	for digest, adapters := range digests {
		sort.Strings(adapters)
		lines = append(lines, fmt.Sprintf("  %s: %s", digest[:12], strings.Join(adapters, ", ")))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// forceFlagPattern matches the flag in any form git accepts, plus the
// shorthand. The scan that applies it skips Python prose (pythonCodeLines),
// because the module docstrings quote git's own refusal and the comments
// explain why the flag is absent: prose may name it, code may not.
var forceFlagPattern = []scanPattern{{
	name:    "force-flag",
	pattern: regexp.MustCompile(`(?:"--force"|'--force'|"-f"|'-f'|\s--force\b)`),
}}

// TestTheReaperNeverForces is guard 2. It covers reclaim.py above all —
// that is the only module that can run a removal at all.
func TestTheReaperNeverForces(t *testing.T) {
	files := adapterModuleFiles(t, reaperModules...)
	for _, finding := range scanFiles(files, forceFlagPattern, pythonCodeLines) {
		t.Errorf("adapters/%s:%d passes a force flag: %q\n"+
			"`git worktree remove --force` deletes uncommitted work that no ref "+
			"holds. That is issue #78's data loss arriving as housekeeping. If a "+
			"worktree will not come off cleanly, the answer is the `refuse` "+
			"decision and a domain outcome of `retained`, never a flag.",
			finding.file, finding.line, finding.text)
	}
}

// TestTheReaperGuardActuallyReadsSomething is the anti-vacuity check the
// file-length scanner's own TestTheFileLengthScannerActuallyScans exists for:
// a guard that looked at no files reports no violations.
// It asserts on the output of adapterModuleFiles, which is the very collection
// TestTheReaperNeverForces scans -- so "the force guard saw nothing" cannot
// pass here and fail silently there.
func TestTheReaperGuardActuallyReadsSomething(t *testing.T) {
	var found []string
	for _, file := range adapterModuleFiles(t, reaperModules...) {
		found = append(found, file.rel)
	}
	sort.Strings(found)
	want := []string{
		"claude-code/reap.py", "claude-code/reclaim.py",
		"codex/reap.py", "codex/reclaim.py",
		"colleague/reap.py", "colleague/reclaim.py",
		"qwen/reap.py", "qwen/reclaim.py",
	}
	sort.Strings(want)
	if strings.Join(found, ",") != strings.Join(want, ",") {
		t.Fatalf("the reaper guards scanned %v, expected %v — either a bridge lost half of the "+
			"reaper or a new worktree-minting bridge was added without one", found, want)
	}
}

// TestTheReaperDocstringQuotesTheRefusalItReliesOn keeps the probed fact
// attached to the code that depends on it. The whole design rests on git
// refusing a dirty worktree; if that sentence ever leaves the module, the
// next reader has no way to know the refusal was measured rather than
// assumed.
func TestTheReaperDocstringQuotesTheRefusalItReliesOn(t *testing.T) {
	for _, pkg := range discoverAdapterPackages(t) {
		if !pkg.has(t, "reap.py") {
			continue
		}
		if !strings.Contains(pkg.read(t, "reap.py"), "use --force to delete it") {
			t.Errorf("adapters/%s/reap.py no longer quotes git's own refusal; the module's "+
				"safety argument is that the refusal is measured, not assumed", pkg.adapter)
		}
	}
}
