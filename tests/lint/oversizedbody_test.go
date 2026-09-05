package testslint

// The oversized-body refusal is one helper, not seven copies (task t8).
//
// Task t1 put an identical `_refuse_oversized_body` method into every bridge
// that answers HTTP directly — it drains a declared-but-unread body to
// MAX_BODY_BYTES before closing the connection with a 413, so a client that
// lies about Content-Length cannot desynchronize the next request on a
// keep-alive socket, and so an unauthenticated caller cannot hold the
// single-threaded server open by declaring an enormous body it never sends.
// That is exactly the shape guarded elsewhere in this package for
// `preflight.py` (preflightsurface_test.go) and `dialin.py`
// (dialintransport_test.go): a security-relevant helper that must not be
// free to drift into seven slightly different bugs.
//
// adapters/jira is the one bridge that does NOT carry this helper. Its
// do_POST inlines its own Content-Length parse/413 path (task predates t1)
// and it has no do_DELETE at all, so it is exempted by name below rather
// than silently absent from the comparison.
//
// The two bridges nearest the 1000-line hard limit (filelength_test.go) are
// adapters/qwen (997 lines) and adapters/claude-code (991 lines): either one
// growing this helper by even a handful of lines risks tripping that guard,
// so an edit to `_refuse_oversized_body` almost certainly means trimming
// something else in the same file rather than reflowing the helper itself —
// and whatever the edit, it must land byte-identically in all seven bridges
// or this test fails.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// oversizedBodyExemptAdapter is the one bridge with its own 413 path.
const oversizedBodyExemptAdapter = "jira"

// oversizedBodyStartMarker and oversizedBodyEndMarker bound the region task
// t1 proved identical (sha256 9641a6b67c23...). The start is the method
// signature at its normal 4-space class-body indent; the end is the first
// following line that is exactly the 8-space `return True` inside the
// method's un-refused branch -- the last line of the helper.
const (
	oversizedBodyStartMarker = "    def _refuse_oversized_body"
	oversizedBodyEndMarker   = "        return True"
)

// oversizedBodyExpectedAdapters are the seven bridges the task names as
// carrying the helper (all adapters/*/src/<pkg> packages except jira).
// Listed explicitly, in addition to the exemption-by-name above, so a bridge
// silently losing the helper (rather than never having had it) is caught by
// the coverage check below instead of just quietly shrinking the digest map.
var oversizedBodyExpectedAdapters = []string{
	"claude-code", "codex", "colleague", "human-inbox", "notify", "pi", "qwen",
}

// extractOversizedBodyRegion returns the lines of source from
// oversizedBodyStartMarker up to and including the first following
// oversizedBodyEndMarker line, joined with "\n". ok is false when the start
// marker is absent, or the end marker never follows it.
func extractOversizedBodyRegion(source string) (region string, ok bool) {
	lines := strings.Split(source, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, oversizedBodyStartMarker) {
			start = i
			break
		}
	}
	if start == -1 {
		return "", false
	}
	for i := start; i < len(lines); i++ {
		if lines[i] == oversizedBodyEndMarker {
			return strings.Join(lines[start:i+1], "\n"), true
		}
	}
	return "", false
}

// extractMethodBody returns the lines of source from a `    def <name>`
// signature up to (not including) the next line that opens another method
// at the same 4-space indent, or end of file. ok is false when the method
// is not found.
func extractMethodBody(source, name string) (body string, ok bool) {
	lines := strings.Split(source, "\n")
	signature := "    def " + name
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, signature) {
			start = i
			break
		}
	}
	if start == -1 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "    def ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// firstDifferingLine returns a one-line hint pointing at the first line two
// regions disagree on, so a failure names where to look instead of just
// that two blobs of ~30 lines differ somewhere.
func firstDifferingLine(a, b string) string {
	linesA := strings.Split(a, "\n")
	linesB := strings.Split(b, "\n")
	n := len(linesA)
	if len(linesB) < n {
		n = len(linesB)
	}
	for i := 0; i < n; i++ {
		if linesA[i] != linesB[i] {
			return fmt.Sprintf("line %d differs:\n    - %s\n    + %s", i+1, linesA[i], linesB[i])
		}
	}
	if len(linesA) != len(linesB) {
		return fmt.Sprintf("regions differ in length: %d vs %d lines", len(linesA), len(linesB))
	}
	return "regions are byte-identical" // should not be reached by a caller
}

// oversizedBodyRegions groups every non-exempt adapter's extracted region by
// its exact text, and reports any adapter that defines do_POST but is
// missing the region entirely. It is the body shared by the positive test
// and (via a synthetic package list) the negative-case test below.
type oversizedBodyRegions struct {
	byText  map[string][]string // region text -> adapters carrying it
	missing []string            // adapters that define do_POST but lack the region
}

// oversizedBodyGuardSource returns the server.py source of a package the
// guard applies to, and ok=false for one it does not: the exempt bridge, a
// package with no server.py, or one that does not answer HTTP at all. Both
// tests below read "a bridge that must carry the helper" through this, so
// the two cannot drift apart on which bridges those are.
func oversizedBodyGuardSource(t *testing.T, pkg adapterPackage) (source string, ok bool) {
	t.Helper()
	if pkg.adapter == oversizedBodyExemptAdapter {
		return "", false
	}
	if !pkg.has(t, "server.py") {
		return "", false
	}
	source = pkg.read(t, "server.py")
	if !strings.Contains(source, "def do_POST") {
		return "", false // not an HTTP-answering bridge at all
	}
	return source, true
}

func groupOversizedBodyRegions(packages []adapterPackage, t *testing.T) oversizedBodyRegions {
	t.Helper()
	result := oversizedBodyRegions{byText: map[string][]string{}}
	for _, pkg := range packages {
		source, ok := oversizedBodyGuardSource(t, pkg)
		if !ok {
			continue
		}
		region, ok := extractOversizedBodyRegion(source)
		if !ok {
			result.missing = append(result.missing, pkg.adapter)
			continue
		}
		result.byText[region] = append(result.byText[region], pkg.adapter)
	}
	return result
}

// TestOversizedBodyHelperIsByteIdenticalAcrossBridges is the positive case.
func TestOversizedBodyHelperIsByteIdenticalAcrossBridges(t *testing.T) {
	packages := discoverAdapterPackages(t)
	regions := groupOversizedBodyRegions(packages, t)

	if len(regions.missing) > 0 {
		sort.Strings(regions.missing)
		t.Fatalf("adapters/%s define do_POST but have no _refuse_oversized_body region "+
			"(from %q to the first following %q): every HTTP-answering bridge except %s "+
			"must carry the helper", strings.Join(regions.missing, ", adapters/"),
			oversizedBodyStartMarker, oversizedBodyEndMarker, oversizedBodyExemptAdapter)
	}

	if len(regions.byText) == 0 {
		t.Fatal("no adapter carries _refuse_oversized_body; task t1's helper is gone")
	}

	if len(regions.byText) > 1 {
		// Pick two groups deterministically and show where they first
		// disagree, so the failure names a line instead of just a count.
		var texts []string
		for text := range regions.byText {
			texts = append(texts, text)
		}
		sort.Slice(texts, func(i, j int) bool {
			return strings.Join(regions.byText[texts[i]], ",") < strings.Join(regions.byText[texts[j]], ",")
		})
		var summary []string
		for _, text := range texts {
			adapters := append([]string(nil), regions.byText[text]...)
			sort.Strings(adapters)
			summary = append(summary, fmt.Sprintf("  %s", strings.Join(adapters, ", ")))
		}
		t.Fatalf("_refuse_oversized_body has diverged between bridges -- it must be "+
			"byte-identical everywhere (task t1), because a security-relevant body-size "+
			"refusal drifting into seven slightly different versions is exactly the bug "+
			"class this guard exists to catch. Groups:\n%s\n%s",
			strings.Join(summary, "\n"), firstDifferingLine(texts[0], texts[1]))
	}

	// Every expected adapter must actually be among the carriers.
	var carriers []string
	for _, adapters := range regions.byText {
		carriers = append(carriers, adapters...)
	}
	carrying := map[string]bool{}
	for _, a := range carriers {
		carrying[a] = true
	}
	var missingExpected []string
	for _, want := range oversizedBodyExpectedAdapters {
		if !carrying[want] {
			missingExpected = append(missingExpected, want)
		}
	}
	if len(missingExpected) > 0 {
		sort.Strings(missingExpected)
		t.Errorf("expected adapters/%s to carry _refuse_oversized_body but none of the "+
			"discovered packages did -- either the adapter directory moved or the helper "+
			"was removed", strings.Join(missingExpected, ", adapters/"))
	}
}

// TestBridgesCallTheOversizedBodyGuardInPostAndDelete asserts do_POST and
// do_DELETE each actually call the helper -- carrying an identical,
// unused method would pass the byte-identity check above while refusing
// nothing.
func TestBridgesCallTheOversizedBodyGuardInPostAndDelete(t *testing.T) {
	for _, pkg := range discoverAdapterPackages(t) {
		source, ok := oversizedBodyGuardSource(t, pkg)
		if !ok {
			continue
		}
		if _, ok := extractOversizedBodyRegion(source); !ok {
			continue // reported by the positive test above
		}
		t.Run(pkg.adapter, func(t *testing.T) {
			assertOversizedBodyGuardIsWired(t, pkg.adapter, source)
		})
	}
}

// assertOversizedBodyGuardIsWired checks one bridge: every HTTP method that
// takes a body calls the helper. Split out of the test above so the
// per-method assertions are not read three loops deep (SonarCloud go:S3776).
func assertOversizedBodyGuardIsWired(t *testing.T, adapter, source string) {
	t.Helper()
	for _, method := range []string{"do_POST", "do_DELETE"} {
		body, ok := extractMethodBody(source, method)
		if !ok {
			t.Fatalf("adapters/%s server.py has no %s method", adapter, method)
		}
		if !strings.Contains(body, "self._refuse_oversized_body()") {
			t.Errorf("adapters/%s's %s never calls self._refuse_oversized_body(): "+
				"the guard is defined but not wired in", adapter, method)
		}
	}
}

// TestOversizedBodyComparisonDetectsDrift is the negative case: it proves
// groupOversizedBodyRegions actually flags a divergence rather than passing
// on any input, the way TestTheFileLengthScannerActuallyScans proves the
// line-length scanner scans. It builds a synthetic two-package fixture from
// one real region (altered by a single byte in the second copy) so the test
// does not depend on any adapter currently disagreeing.
func TestOversizedBodyComparisonDetectsDrift(t *testing.T) {
	packages := discoverAdapterPackages(t)
	var reference string
	for _, pkg := range packages {
		if pkg.adapter == oversizedBodyExemptAdapter || !pkg.has(t, "server.py") {
			continue
		}
		source := pkg.read(t, "server.py")
		if region, ok := extractOversizedBodyRegion(source); ok {
			reference = region
			break
		}
	}
	if reference == "" {
		t.Fatal("no adapter yielded a reference region to corrupt; can't exercise the negative case")
	}

	corrupted := strings.Replace(reference, "MAX_BODY_BYTES", "MAX_BODY_BYTEZ", 1)
	if corrupted == reference {
		t.Fatal("fixture setup did not actually change the reference region")
	}

	dir := t.TempDir()
	fixture := []adapterPackage{
		{adapter: "fixture-a", dir: writeFixtureServer(t, dir, "fixture_a", reference)},
		{adapter: "fixture-b", dir: writeFixtureServer(t, dir, "fixture_b", corrupted)},
	}

	regions := groupOversizedBodyRegions(fixture, t)
	if len(regions.missing) != 0 {
		t.Fatalf("fixture packages reported missing: %v", regions.missing)
	}
	if len(regions.byText) != 2 {
		t.Fatalf("comparison helper did not detect the one-byte drift: got %d distinct "+
			"region(s), want 2", len(regions.byText))
	}
}

// writeFixtureServer writes a minimal server.py under dir/pkgName/server.py
// that defines do_POST and embeds the given oversized-body region verbatim,
// and returns the package directory (what adapterPackage.dir expects).
func writeFixtureServer(t *testing.T, root, pkgName, region string) string {
	t.Helper()
	pkgDir := filepath.Join(root, pkgName)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", pkgDir, err)
	}
	body := region + "\n\n    def do_POST(self) -> None:  # noqa: N802 - stdlib naming\n" +
		"        if self._refuse_oversized_body():\n            return\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "server.py"), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s/server.py: %v", pkgName, err)
	}
	return pkgDir
}
