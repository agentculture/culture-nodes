package testslint

// The design-token guard for the web app (task t4).
//
// culture-design/ is the one place the UI's colour, type and radius decisions
// are allowed to live: tokens.css is a byte-verbatim copy of the pinned org
// stylesheet and palette.ts's every hex traces back to an org source file
// (scripts/check-culture-design.mjs asserts both). That pin is worth nothing
// if a component next door writes `#7fdcc9` inline -- the value stops tracing
// to anything, and re-pinning the design layer silently leaves it behind.
//
// So: outside web/src/culture-design/, a colour, a font face or a radius is
// referenced, never written. Four literal shapes carry those decisions and
// this guard refuses all four.
//
// The colour-function shape is spelled wider than the criterion's `rgba(`
// deliberately. The criterion is a floor, and a guard that refuses `rgba(` but
// waves `hsl(200 40% 60%)` through does not enforce the rule -- it enforces a
// spelling of it. `color-mix()` is NOT on the list: every one in this tree
// derives from a token (`color-mix(in srgb, var(--accent) 12%, transparent)`),
// which is the composition the rule is asking for, and a color-mix over a raw
// hex is already caught by the hex pattern inside it.
//
// The guard scans prose-free lines only. Comments in this tree quote GitHub
// issue numbers constantly, and `#270` is three hex digits -- a scanner that
// could not tell a comment from a declaration would fire on the sentences
// explaining the rule. Prose may name a number; code may not name a colour.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	// webSourceTree is the scanned prefix: the app's own source, not its
	// build output or its dependencies.
	webSourceTree = "web/src/"
	// webDesignLayer is the one allowlisted directory -- the design layer
	// itself, whose whole job is to hold these literals.
	webDesignLayer = "web/src/culture-design/"
)

// webSourceExtensions are the three the app's styling can be written in.
var webSourceExtensions = map[string]bool{".css": true, ".ts": true, ".tsx": true}

// webTokenExemption is one file the guard tolerates *for now*, and the count
// it tolerates. The count is what stops an exemption becoming an amnesty: a
// file listed here is pinned at exactly the findings it has today, so a new
// literal in an exempt file still fails, and a file that gets cleaned up fails
// too -- which is how the list is forced to empty itself rather than being
// trusted to.
//
// Every entry names the task that removes it. The plan requires this slice to
// be empty by the end of wave A.
type webTokenExemption struct {
	file     string
	findings int
	reason   string
}

// webTokenExemptions is the whole exempt list. See webTokenExemption for why
// each entry carries a count.
//
// Both entries were measured against 9d2d730, the tip of feat/web-ui-lift when
// t4 landed, and both are scheduled to disappear inside wave A. Neither file
// belongs to t4 -- the guard's own task is file-disjoint from every other one
// -- so t4 records what it found rather than reaching into somebody else's
// file to fix it.
var webTokenExemptions = []webTokenExemption{
	{
		file:     "web/src/components/MeshCanvas.tsx",
		findings: 6,
		reason: "the canvas paints the terminal palette imperatively: two `#7fdcc9` sprite " +
			"colours and four `rgba(${rgb}, a)` gradient stops. t7 (wave 3) deletes this file " +
			"outright and rebuilds Mesh on React Flow + CultureNode, so the entry goes with it.",
	},
	{
		file:     "web/src/styles/app.css",
		findings: 22,
		reason: "three separate debts, and only two of them have an owner. " +
			"(a) The `.mesh-*` block (2325-2412) hard-codes the dark terminal ground, its " +
			"borders and its legend swatches -- t7 deletes MeshCanvas and takes these rules " +
			"with it. " +
			"(b) The `:root` block (15-20) re-declares four palette hexes as run-state tokens " +
			"and invents `--nodes-card-radius: 0.85rem`; the file's own header admits these are " +
			"copies of culture-design/palette.ts, so they belong in the design layer that t3 " +
			"is building. " +
			"(c) UNOWNED as of wave A's dispatch: seven control-chrome rem radii on inputs, " +
			"selects and tabs (1201, 1246, 1264, 1430, 2077, 2164, 2953) and the " +
			"`font-family: var(--mono, monospace)` at 2560, whose `--mono` is defined nowhere " +
			"-- tokens.css spells it `--font-mono`, so that textarea renders the browser " +
			"default rather than the design stack. No wave-A task names them; they need " +
			"routing before this entry can reach zero.",
	},
}

// webCSSVarRef is a bare custom-property reference -- `var(--name)` with no
// fallback. Stripping these from a declaration's value leaves exactly the part
// that was written rather than referenced. A `var(--name, literal)` fallback
// deliberately does NOT match: the fallback is a literal, and it is the one
// that renders whenever the token is missing or misspelled.
var webCSSVarRef = regexp.MustCompile(`var\(\s*--[a-zA-Z0-9_-]+\s*\)`)

// webRemLength is a rem length anywhere in a declaration value.
var webRemLength = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_.-])\d*\.?\d+rem\b`)

// webKeywordValues are the CSS-wide keywords that name no font and no length,
// so a declaration set to one of them has decided nothing.
var webKeywordValues = map[string]bool{
	"inherit": true, "initial": true, "unset": true, "revert": true,
	"revert-layer": true, "none": true, "": true,
}

// webTokenPatterns are the four literal shapes the design layer owns.
var webTokenPatterns = []scanPattern{
	{
		name:    "hex colour",
		pattern: regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`),
		allow:   hexIsNotAColour,
	},
	{
		name:    "colour function",
		pattern: regexp.MustCompile(`(?i)\b(?:rgba?|hsla?|hwb|(?:ok)?(?:lab|lch))\(`),
	},
	{
		name:    "font-family literal",
		pattern: regexp.MustCompile(`(?i)font-?family\s*:[^;}]*`),
		allow:   declarationIsTokenOnly,
	},
	{
		name:    "rem radius literal",
		pattern: regexp.MustCompile(`(?i)(?:border[a-z-]*radius|--[a-z-]*radius)\s*:[^;}]*`),
		allow:   radiusHasNoRemLiteral,
	},
}

// hexIsNotAColour tolerates a `#` run whose length is not one CSS accepts as a
// colour (3, 4, 6 or 8 digits). `#12345` is not a colour and neither is the
// leading run of a longer identifier.
func hexIsNotAColour(match string) bool {
	digits := len(match) - 1
	return digits != 3 && digits != 4 && digits != 6 && digits != 8
}

// declarationValue extracts what a matched declaration was set to. A quoted
// value (the JS/TSX form, `fontFamily: "var(--font-mono)"`) is unwrapped so
// the rest of an object literal on the same line is not mistaken for part of
// it; a CSS value runs to the `;` or `}` the pattern already stopped at.
func declarationValue(match string) string {
	_, value, found := strings.Cut(match, ":")
	if !found {
		return ""
	}
	value = strings.TrimSpace(value)
	if quote := firstByte(value); quote == '"' || quote == '\'' || quote == '`' {
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			value = value[1 : 1+end]
		}
	}
	return value
}

func firstByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

// declarationIsTokenOnly reports whether a declaration's value is built out of
// token references and nothing else -- the shape the whole guard is asking
// for. It is the `allow` for font-family; a stack like `ui-monospace, Menlo`
// leaves text behind and fails.
func declarationIsTokenOnly(match string) bool {
	value := webCSSVarRef.ReplaceAllString(declarationValue(match), "")
	value = strings.Trim(value, " \t,;'\"`")
	return webKeywordValues[strings.ToLower(value)]
}

// radiusHasNoRemLiteral is the `allow` for the radius pattern: `var(--radius)`
// and `calc(var(--radius) / 2)` pass, `0.6rem` does not. Pixel radii
// (`999px` for a pill, `2px` for a hairline square) are deliberately outside
// this rule -- the acceptance criterion names rem literals, which are the ones
// that duplicate the design layer's spacing scale.
func radiusHasNoRemLiteral(match string) bool {
	return !webRemLength.MatchString(declarationValue(match))
}

// webCodeLines hides prose from the scan: `//` line comments, and everything
// inside a `/* ... */` block. A trailing comment on a code line is left in --
// the declaration ahead of it is scanned either way, and a comment that quotes
// the very hex its declaration writes is not a second offence worth hiding.
//
// Tracking the block explicitly (rather than assuming a leading `*`) is what
// keeps a CSS universal selector -- `*, *::before, *::after {` -- scannable.
func webCodeLines() lineFilter {
	inBlock := false
	return func(line string) bool {
		trimmed := strings.TrimSpace(line)
		prose := inBlock || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*")
		switch opens, closes := strings.Count(line, "/*"), strings.Count(line, "*/"); {
		case opens > closes:
			inBlock = true
		case closes > opens:
			inBlock = false
		}
		return !prose
	}
}

// isScannedWebSource selects the committed web source this guard governs:
// styling-capable files under web/src, minus the design layer that owns the
// literals.
func isScannedWebSource(rel string) bool {
	if !strings.HasPrefix(rel, webSourceTree) || strings.HasPrefix(rel, webDesignLayer) {
		return false
	}
	return webSourceExtensions[strings.ToLower(filepath.Ext(rel))]
}

// scanWebTokens runs the guard over the committed tree and groups what it
// found by file, which is the granularity the exempt list works at.
func scanWebTokens(t *testing.T) map[string][]scanFinding {
	t.Helper()
	root := repoRoot(t)
	files := mustReadSourceFiles(t, root, committedFiles(t, root), isScannedWebSource)
	if len(files) == 0 {
		t.Fatalf("token guard scanned no files under %s%s; it would report green over any tree", root, webSourceTree)
	}
	byFile := map[string][]scanFinding{}
	for _, finding := range scanFiles(files, webTokenPatterns, webCodeLines) {
		byFile[finding.file] = append(byFile[finding.file], finding)
	}
	return byFile
}

func exemptionsByFile() map[string]webTokenExemption {
	byFile := map[string]webTokenExemption{}
	for _, exemption := range webTokenExemptions {
		byFile[exemption.file] = exemption
	}
	return byFile
}

func describeFindings(findings []scanFinding) string {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		lines = append(lines, fmt.Sprintf("  %s:%d %s: %s", finding.file, finding.line, finding.name, finding.text))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestWebSourceCarriesNoDesignLiterals is the rule: outside
// web/src/culture-design/, colours, faces and rem radii are referenced from
// tokens, never written.
func TestWebSourceCarriesNoDesignLiterals(t *testing.T) {
	exempt := exemptionsByFile()
	for file, findings := range scanWebTokens(t) {
		if _, isExempt := exempt[file]; isExempt {
			continue // TestWebTokenExemptionsAreStillTrue owns these
		}
		t.Errorf("%s writes design literals that belong in %s:\n%s",
			file, webDesignLayer, describeFindings(findings))
	}
}

// TestWebTokenExemptionsAreStillTrue is the pressure that empties the exempt
// list. Each entry is pinned at the count it was written against, so an exempt
// file may not gain a literal, and may not quietly lose one either: the moment
// the task named in a reason lands, this test fails and the entry has to go.
//
// An exemption is a dated statement about the tree, not a permanent licence.
func TestWebTokenExemptionsAreStillTrue(t *testing.T) {
	found := scanWebTokens(t)
	for _, exemption := range webTokenExemptions {
		actual := len(found[exemption.file])
		switch {
		case actual == exemption.findings:
			continue
		case actual == 0:
			t.Errorf("%s is exempt for %d findings but is now clean: delete its entry from webTokenExemptions (%s)",
				exemption.file, exemption.findings, exemption.reason)
		case actual < exemption.findings:
			t.Errorf("%s is exempt for %d findings but now has %d: shrink the entry to %d so the remaining ones stay visible (%s)\n%s",
				exemption.file, exemption.findings, actual, actual, exemption.reason, describeFindings(found[exemption.file]))
		default:
			t.Errorf("%s is exempt for %d findings and now has %d: the new literals are not covered by the exemption (%s)\n%s",
				exemption.file, exemption.findings, actual, exemption.reason, describeFindings(found[exemption.file]))
		}
	}
}

// TestWebTokenExemptionsAreDeclaredHonestly checks the shape of the list
// itself: an entry with no reason, no count, or a path this guard does not
// even scan is an exemption that explains nothing.
func TestWebTokenExemptionsAreDeclaredHonestly(t *testing.T) {
	seen := map[string]bool{}
	for _, exemption := range webTokenExemptions {
		if !isScannedWebSource(exemption.file) {
			t.Errorf("exemption %q is not a file this guard scans; it exempts nothing", exemption.file)
		}
		if seen[exemption.file] {
			t.Errorf("exemption %q is listed twice; the counts cannot both be the pinned one", exemption.file)
		}
		seen[exemption.file] = true
		if exemption.findings <= 0 {
			t.Errorf("exemption %q pins %d findings; an exemption for nothing should be deleted", exemption.file, exemption.findings)
		}
		if strings.TrimSpace(exemption.reason) == "" {
			t.Errorf("exemption %q carries no reason; the criterion is that the guard lists what it exempts *with a reason*", exemption.file)
		}
	}
}

// webScannerFixtures are the negative fixture: files that must be caught, and
// files that must not be. The repo-wide tests above pass over a clean tree and
// over a scanner broken in any of a dozen ways -- a wrong prefix, an empty
// extension map, an `allow` that returns true for everything all read as the
// same silent green.
var webScannerFixtures = []struct {
	name     string
	rel      string
	body     string
	patterns []string // pattern names expected to fire, in line order
}{
	{
		name: "six-digit hex in CSS",
		rel:  "web/src/styles/x.css", body: ".a { color: #7fdcc9; }",
		patterns: []string{"hex colour"},
	},
	{
		name: "three-digit hex in TSX",
		rel:  "web/src/components/X.tsx", body: `const c = "#abc";`,
		patterns: []string{"hex colour"},
	},
	{
		name: "eight-digit hex with alpha",
		rel:  "web/src/styles/x.css", body: ".a { color: #7fdcc980; }",
		patterns: []string{"hex colour"},
	},
	{
		name: "rgba call",
		rel:  "web/src/styles/x.css", body: ".a { border: 1px solid rgba(233, 236, 248, 0.12); }",
		patterns: []string{"colour function"},
	},
	{
		name: "rgb call in a template literal",
		rel:  "web/src/components/X.tsx", body: "const g = `rgb(${r})`;",
		patterns: []string{"colour function"},
	},
	{
		name: "hsl call, the spelling the criterion does not name",
		rel:  "web/src/styles/x.css", body: ".a { color: hsl(174 55% 68%); }",
		patterns: []string{"colour function"},
	},
	{
		name: "oklch call",
		rel:  "web/src/styles/x.css", body: ".a { color: oklch(0.7 0.1 180); }",
		patterns: []string{"colour function"},
	},
	{
		name: "font stack written out",
		rel:  "web/src/styles/x.css", body: ".a { font-family: ui-monospace, Menlo, monospace; }",
		patterns: []string{"font-family literal"},
	},
	{
		name: "font token with a literal fallback",
		rel:  "web/src/styles/x.css", body: ".a { font-family: var(--mono, monospace); }",
		patterns: []string{"font-family literal"},
	},
	{
		name: "rem border-radius",
		rel:  "web/src/styles/x.css", body: ".a { border-radius: 0.6rem; }",
		patterns: []string{"rem radius literal"},
	},
	{
		name: "rem radius on a corner longhand",
		rel:  "web/src/styles/x.css", body: ".a { border-top-left-radius: 0.85rem; }",
		patterns: []string{"rem radius literal"},
	},
	{
		name: "rem radius in a custom property",
		rel:  "web/src/styles/x.css", body: ":root { --nodes-card-radius: 0.85rem; }",
		patterns: []string{"rem radius literal"},
	},
	{
		name:     "several shapes on consecutive lines",
		rel:      "web/src/styles/x.css",
		body:     ".a {\n  color: #10142b;\n  background: rgba(16, 20, 43, 0.92);\n  border-radius: 0.5rem;\n}",
		patterns: []string{"hex colour", "colour function", "rem radius literal"},
	},

	// --- and the shapes that must NOT fire ---
	{
		name: "tokens only",
		rel:  "web/src/styles/x.css",
		body: ".a {\n  color: var(--ink);\n  font-family: var(--font-mono);\n  border-radius: var(--radius);\n}",
	},
	{
		name: "calc over a radius token",
		rel:  "web/src/styles/x.css", body: ".a { border-radius: calc(var(--radius) / 2); }",
	},
	{
		name: "color-mix composing tokens",
		rel:  "web/src/styles/x.css",
		body: ".a { background: color-mix(in srgb, var(--accent) 12%, transparent); }",
	},
	{
		name: "a word merely ending in lab or lch",
		rel:  "web/src/routes/X.tsx", body: "const t = collab(state), u = welch(state);",
	},
	{
		name: "pill and hairline pixel radii",
		rel:  "web/src/styles/x.css", body: ".a { border-radius: 999px; }\n.b { border-radius: 2px; }",
	},
	{
		name: "font-family inherited",
		rel:  "web/src/styles/x.css", body: ".a { font-family: inherit; }",
	},
	{
		name: "issue number in a line comment",
		rel:  "web/src/components/X.tsx", body: "// expired is never a button (issue #265).",
	},
	{
		name: "issue number in a block comment",
		rel:  "web/src/routes/X.tsx",
		body: "/**\n * The first screen (task t17, spec c25, issue #270).\n * Colours like #7fdcc9 are named here, not written.\n */",
	},
	{
		name: "universal selector after a closed comment block",
		rel:  "web/src/styles/x.css",
		body: "/* reset */\n*,\n*::before {\n  box-sizing: border-box;\n}",
	},
	{
		name: "css id selector that is not a colour",
		rel:  "web/src/styles/x.css", body: "#agent-state { display: none; }",
	},
	{
		name: "numeric borderRadius in a react-flow path option",
		rel:  "web/src/routes/X.tsx", body: "pathOptions: { borderRadius: 14, offset: 12 },",
	},
	{
		name: "tsx font token as a quoted style value",
		rel:  "web/src/components/X.tsx", body: `style={{ fontFamily: "var(--font-mono)", color: "var(--ink)" }}`,
	},
}

// TestTheWebTokenScannerActuallyFires is the gate on the gate.
func TestTheWebTokenScannerActuallyFires(t *testing.T) {
	for _, fixture := range webScannerFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(fixture.rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(fixture.body), 0o600); err != nil {
				t.Fatal(err)
			}
			files, err := readSourceFiles(root, []string{fixture.rel}, isScannedWebSource)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 {
				t.Fatalf("fixture %s was not selected for scanning; the guard would never see it", fixture.rel)
			}
			var fired []string
			for _, finding := range scanFiles(files, webTokenPatterns, webCodeLines) {
				fired = append(fired, finding.name)
			}
			if !equalStringSlices(fired, fixture.patterns) {
				t.Errorf("scanning %q fired %v, want %v\n%s", fixture.body, fired, fixture.patterns, fixture.body)
			}
		})
	}
}

// TestTheWebTokenScannerIgnoresTheDesignLayer pins the allowlist: the design
// layer is full of exactly these literals and must stay invisible to the scan,
// while its neighbours must not.
func TestTheWebTokenScannerIgnoresTheDesignLayer(t *testing.T) {
	cases := map[string]bool{
		"web/src/culture-design/tokens.css": false,
		"web/src/culture-design/palette.ts": false,
		"web/src/culture-design/mark.tsx":   false,
		"web/src/culture-design/README.md":  false,
		"web/src/styles/app.css":            true,
		"web/src/components/Header.tsx":     true,
		"web/src/domain/graph.ts":           true,
		"web/src/README.md":                 false,
		"web/e2e/mesh.spec.ts":              false,
		"internal/api/mesh.go":              false,
	}
	for rel, want := range cases {
		if got := isScannedWebSource(rel); got != want {
			t.Errorf("isScannedWebSource(%q) = %v, want %v", rel, got, want)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
