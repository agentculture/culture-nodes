package invariants

// Invariant gates for the attempts-evidence-humans-loops batch (t20), kept
// running for every batch after it. Two batch-wide promises are gated here,
// both as pure source scans — no database, no network, fast enough for every
// `go test ./...`:
//
//  1. Provider neutrality (spec c16, honesty h14). The mechanical half
//     already lives in internal/actors/neutrality_test.go
//     (TestNoProviderNamesInRuntimeCode, TestNoAgentSDKDependency) and runs
//     with `go test ./internal/actors/`. This file adds two things on top:
//     a pinned-digest check that the neutrality test itself has not been
//     modified (h14 says "unmodified and green through the whole batch"),
//     and an actor-kind sweep the neutrality test does not do — no dispatch
//     path reads an actor's kind for branching; internal/api/grades.go
//     stays the one sanctioned kind-aware spot.
//
//  2. The ledger authority ladder (spec c17, honesty h15). Agents propose;
//     runner-boundary code observes; validators derive; humans confirm only
//     through the ledger's review transaction (CommitReview) — plus the one
//     recorded precedent, the human-grader direct-grade path in
//     internal/api/grades.go (checkHumanAuthority in
//     internal/ledger/authority.go admits it at runtime). The sweep below
//     encodes, per authority constant, the exact set of non-test files
//     allowed to mention it, verified against the tree at the batch base
//     (commit b975a60) plus the batch's own sanctioned writers.
//
// If one of these tests fails on a file you just added: do NOT extend the
// allowlist to make it pass. Either move the write behind the proper
// boundary (observed evidence through internal/runners; confirmation
// through ledger.CommitReview; derivation behind a validator/engine-origin
// deterministic producer), or — deliberately, in a reviewed change that
// says so — extend the allowlist here AND the table in docs/invariants.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// neutralityTestDigest pins internal/actors/neutrality_test.go byte-for-byte.
// It is the SHA-256 of the file at the batch base (commit b975a60, the
// attempts-evidence-humans-loops plan landing) — verified identical at t20
// build time with `git diff b975a60 HEAD -- internal/actors/neutrality_test.go`
// (empty). Honesty condition h14 requires the file unmodified and green
// through the whole batch; any later deliberate change to the neutrality
// test must update this digest in the same reviewed diff.
const neutralityTestDigest = "3b335704e0c947b0181d5cc21536e3b3b6bdc313842d8cdcc493b943af6da07a"

// repoRoot locates the repository root from this file's own path, the same
// technique neutrality_test.go uses — the test must work from any working
// directory `go test` happens to use.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root")
	}
	// internal/invariants/invariants_test.go -> internal/invariants -> internal -> root
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// TestNeutralityGuardUnmodifiedAndPresent is the h14 gate. The real
// neutrality assertions run in internal/actors (go test ./internal/actors/
// runs TestNoProviderNamesInRuntimeCode and TestNoAgentSDKDependency); this
// test proves that file is still there, still contains those two test
// functions (so they still run), and is byte-identical to the batch base.
func TestNeutralityGuardUnmodifiedAndPresent(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "actors", "neutrality_test.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("internal/actors/neutrality_test.go is unreadable (%v): "+
			"h14 requires the neutrality test present, unmodified, and green through the batch", err)
	}

	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != neutralityTestDigest {
		t.Errorf("internal/actors/neutrality_test.go digest %s != pinned %s\n"+
			"h14: the neutrality test stays unmodified through the batch. If a LATER, reviewed change "+
			"to the neutrality test is deliberate, update neutralityTestDigest here and docs/invariants.md "+
			"in the same diff — never weaken the test to admit a provider branch.",
			got, neutralityTestDigest)
	}

	for _, fn := range []string{
		"func TestNoProviderNamesInRuntimeCode(",
		"func TestNoAgentSDKDependency(",
	} {
		if !strings.Contains(string(data), fn) {
			t.Errorf("neutrality_test.go no longer declares %q — the guard would silently stop running", fn)
		}
	}
}

// actorKindRead matches a read of a Kind field off an actor-shaped
// identifier: grader.Kind, actor.Kind, resolvedActor.Kind, actorRow.Kind…
// Node kinds (node.Kind), event kinds (ev.Kind), and ledger origin kinds
// (rec.Origin.Kind) do not match, and are legitimate. Like the neutrality
// test's provider grep, this is a deliberate textual tripwire, not an AST
// walk: a rename to a non-actor-ish variable can slip past it, but the
// change that looks harmless in a diff — `switch actor.Kind` in a dispatch
// path — cannot.
var actorKindRead = regexp.MustCompile(`(?i)\b\w*(actor|grader)\w*\.Kind\b`)

// dispatchTrees are the packages provider neutrality binds (the same set
// neutrality_test.go scans): everywhere an actor's kind could influence
// what the control plane does, as opposed to what it stores or reports.
var dispatchTrees = []string{"actors", "worker", "engine", "compiler", "api"}

// sanctionedKindAware are the files allowed to branch on actor kind, each
// with the standing that earns it. Every one of them decides a LEDGER
// question — what origin or authority a record carries — from the producing
// actor's registered kind, and none of them is on a dispatch path. Spec c16
// names the grades API as the precedent all other kind-aware code follows.
var sanctionedKindAware = map[string]string{
	"internal/api/grades.go": "grade authority follows the grader's registered kind: a human grading directly " +
		"is their own confirmation, an agent grading is a proposal (the c16 precedent)",
	"internal/api/preflights.go": "a clarify-then-commit acknowledgement's ledger ORIGIN follows the " +
		"acknowledging actor's registered kind — agent when a bridge answers for itself, human when an " +
		"operator answers on its behalf. The authority is proposed either way (issue #67, task t14), so " +
		"the kind decides who the record says produced it, never what the control plane does with it: " +
		"the gate's dispatch-side half (internal/worker/clarifygate.go) reads no kind at all",
}

// TestActorKindReadsStayOutOfDispatch is the c16 sweep the neutrality test
// does not perform: no non-test file in the dispatch trees reads an actor's
// kind, except the sanctioned grades path.
func TestActorKindReadsStayOutOfDispatch(t *testing.T) {
	root := repoRoot(t)
	internalDir := filepath.Join(root, "internal")

	scanned := 0
	sanctionedHits := map[string]bool{}
	for _, tree := range dispatchTrees {
		treeRoot := filepath.Join(internalDir, tree)
		if _, err := os.Stat(treeRoot); err != nil {
			t.Fatalf("dispatch tree %s is not scannable (%v); the actor-kind guard is watching nothing", treeRoot, err)
		}
		err := filepath.WalkDir(treeRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			rel := relSlash(t, root, path)
			loc := actorKindRead.FindIndex(data)
			if loc == nil {
				return nil
			}
			if _, sanctioned := sanctionedKindAware[rel]; sanctioned {
				sanctionedHits[rel] = true
				return nil
			}
			t.Errorf("%s reads an actor kind (%s)\n"+
				"c16: dispatch never branches on actor kind — human actors ride the same 202+callback path as "+
				"agents. Either drop the kind read (dispatch resolves endpoint_ref and metadata only), or, if "+
				"this is genuinely new sanctioned kind-aware code OUTSIDE dispatch following the grades-API "+
				"precedent, extend sanctionedKindAware deliberately in a reviewed change and record it in "+
				"docs/invariants.md.",
				rel, lineAt(data, loc[0]))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", treeRoot, err)
		}
	}

	if scanned == 0 {
		t.Fatal("the actor-kind guard scanned no files; it is not proving anything")
	}
	for rel := range sanctionedKindAware {
		if !sanctionedHits[rel] {
			t.Errorf("%s no longer matches the actor-kind pattern — either the sanctioned branch moved "+
				"(update sanctionedKindAware and docs/invariants.md) or the pattern rotted and this guard "+
				"is no longer detecting anything", rel)
		}
	}
	t.Logf("scanned %d non-test .go files across internal/%v for actor-kind reads", scanned, dispatchTrees)
}

// authorityAllowlists is the c17/h15 ladder, encoded as: for each gated
// token, the exact set of non-test .go files (repo-relative, slash paths)
// allowed to mention it anywhere in the repository. The sets were verified
// against the tree at the t20 build (batch base b975a60 plus the merged
// wave-0/1 tasks) — every entry names why it is allowed.
//
// Mention-level granularity is deliberate: a new *reader* of an authority
// constant is cheap to allowlist in review, and a new *writer* hiding as a
// reader is exactly what a looser sweep would miss.
var authorityAllowlists = []struct {
	token string
	rule  string
	files map[string]string // rel path -> why it is allowed
}{
	{
		token: "AuthorityObserved",
		rule: "observed authority belongs to the runner boundary: only internal/runners constructs " +
			"observed evidence, from facts the boundary directly measured (PRD §10.4). No agent path — " +
			"worker completions, engine deltas, bridges — may claim it.",
		files: map[string]string{
			"internal/ledger/record.go":      "vocabulary: defines the Authority constants",
			"internal/ledger/authority.go":   "append-time enforcement: checkRunnerAuthority admits observed evidence only with a runner manifest",
			"internal/engine/ledgerdelta.go": "refusal gate: a node's declared delta may propose or observe per its contract; everything else is rejected",
			"internal/runners/dispatch.go":   "THE writer: EvidenceRecord stamps OriginRunner + AuthorityObserved from boundary-measured observations",
		},
	},
	{
		token: "OriginRunner",
		rule: "runner origin is stamped only where the runner boundary itself reports — a worker or " +
			"engine file constructing an OriginRunner record would be an agent path impersonating the boundary.",
		files: map[string]string{
			"internal/ledger/record.go":    "vocabulary: defines the Origin kinds",
			"internal/ledger/authority.go": "append-time enforcement: runners write observed evidence only, manifest-checked",
			"internal/runners/dispatch.go": "THE writer: the runner boundary's own evidence records",
		},
	},
	{
		token: "AuthorityConfirmed",
		rule: "confirmed authority is human acceptance: written only by the ledger's review transaction " +
			"(CommitReview, via Verdict.authority) and the sanctioned human-grader direct-grade path " +
			"(internal/api/grades.go — a human grading directly is their own confirmation, admitted by " +
			"checkHumanAuthority). No actor promotes its own proposal.",
		files: map[string]string{
			"internal/ledger/record.go":      "vocabulary: defines the Authority constants",
			"internal/ledger/authority.go":   "append-time enforcement: confirmed/rejected only inside review transactions (plus the human-grade carve-out)",
			"internal/ledger/ledger.go":      "THE writer: Verdict.authority maps CommitReview verdicts to confirmed/rejected review records",
			"internal/ledger/projection.go":  "reader: projections fold confirmed decisions into live state",
			"internal/api/grades.go":         "sanctioned writer: human-grader direct grade lands confirmed (the c16 precedent)",
			"internal/devague/claims.go":     "pre-batch import mapper: mirrors decisions humans already recorded in devague frames into review records",
			"internal/devague/deviations.go": "pre-batch import mapper: mirrors devague deviation approvals into review records (task t22) — the same base-record/review-record split claims.go uses for claims, evaluated against this exact invariant in the function's own doc comment",
		},
	},
	{
		token: "AuthorityDerived",
		rule: "derived authority belongs to deterministic producers (validator/engine origin) computing " +
			"from referenced records — acceptance and success-signal evaluators, assurance hooks. An agent " +
			"or human asserting derived would be claiming computation nobody ran.",
		files: map[string]string{
			"internal/ledger/record.go":        "vocabulary: defines the Authority constants",
			"internal/ledger/authority.go":     "append-time enforcement: engine/validator origins write derived and only derived",
			"internal/worker/acceptance.go":    "validator-origin writer: pre-announced acceptance evaluation (issue 37)",
			"internal/worker/successsignal.go": "validator-origin writer: mechanical success_signal evaluation (t18, issue 37)",
			"internal/worker/hooks.go":         "validator-origin writer: assurance-hook rejection reviews at the runner boundary",
			"internal/devague/deliverables.go": "engine-origin writer: pre-batch devague import derives delivery summaries",
			"internal/preflight/records.go":    "engine-origin writer: the clarify-then-commit gate's briefing is a deterministic composition of advertised host state and the pinned task declaration (issue #67, task t14)",
		},
	},
}

// deterministicOriginByFile pairs each derived-authority writer with the
// deterministic origin it must stamp — the runtime rule (authority.go:
// engine/validator origins write derived and only derived) needs the writer
// to actually claim that origin, so the sweep checks the pairing at source
// level too.
var deterministicOriginByFile = map[string]string{
	"internal/worker/acceptance.go":    "OriginValidator",
	"internal/worker/successsignal.go": "OriginValidator",
	"internal/worker/hooks.go":         "OriginValidator",
	"internal/devague/deliverables.go": "OriginEngine",
	"internal/preflight/records.go":    "OriginEngine",
}

// TestAuthorityLadderWritersAreAllowlisted is the c17/h15 gate: for each
// gated authority token, every non-test .go file in the repository that
// mentions it must be on that token's allowlist, and every allowlisted file
// must still mention it (a stale allowlist entry is rot — the boundary it
// described has moved, and the list must be re-verified, not accreted).
func TestAuthorityLadderWritersAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	files := goSourceFiles(t, root)
	if len(files) == 0 {
		t.Fatal("the authority sweep found no non-test .go files; it is not proving anything")
	}

	contents := make(map[string][]byte, len(files))
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		contents[rel] = data
	}

	for _, gate := range authorityAllowlists {
		pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(gate.token) + `\b`)
		seen := make(map[string]bool, len(gate.files))
		for _, rel := range files {
			loc := pattern.FindIndex(contents[rel])
			if loc == nil {
				continue
			}
			if _, ok := gate.files[rel]; ok {
				seen[rel] = true
				continue
			}
			t.Errorf("%s mentions %s (%s) but is not on that token's allowlist.\n"+
				"Invariant (c17/h15): %s\n"+
				"Either move the write behind the proper boundary, or — deliberately, in a reviewed "+
				"change — extend authorityAllowlists here AND the table in docs/invariants.md, stating "+
				"why the new file has standing.",
				rel, gate.token, lineAt(contents[rel], loc[0]), gate.rule)
		}
		for rel, why := range gate.files {
			if !seen[rel] {
				t.Errorf("allowlisted file %s no longer mentions %s (was allowed as: %s) — "+
					"the boundary this allowlist described has moved; re-verify the true writer set "+
					"and update authorityAllowlists and docs/invariants.md together", rel, gate.token, why)
			}
		}
	}

	for rel, origin := range deterministicOriginByFile {
		data, ok := contents[rel]
		if !ok {
			t.Errorf("derived-authority writer %s is missing from the tree; update the allowlists to the true writer set", rel)
			continue
		}
		if !regexp.MustCompile(`\b` + origin + `\b`).Match(data) {
			t.Errorf("%s writes AuthorityDerived but no longer stamps %s — derived records must carry a "+
				"deterministic origin (PRD §10.4); the runtime check in internal/ledger/authority.go will "+
				"refuse anything else at append time", rel, origin)
		}
	}

	t.Logf("swept %d non-test .go files for %d gated authority tokens", len(files), len(authorityAllowlists))
}

// goSourceFiles walks the whole repository for non-test .go files, returning
// repo-relative slash paths. Test files are excluded on purpose: tests
// construct records of every authority to exercise the runtime checks.
func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".claude", ".teken", ".local":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, relSlash(t, root, path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

func relSlash(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("relativize %s under %s: %v", path, root, err)
	}
	return filepath.ToSlash(rel)
}

// lineAt returns the trimmed source line an offset falls on, quoted, so a
// failure points at real text rather than a byte number.
func lineAt(data []byte, offset int) string {
	start := strings.LastIndexByte(string(data[:offset]), '\n') + 1
	end := strings.IndexByte(string(data[offset:]), '\n')
	if end < 0 {
		end = len(data)
	} else {
		end += offset
	}
	line := strings.TrimSpace(string(data[start:end]))
	const maxLine = 140
	if len(line) > maxLine {
		line = line[:maxLine] + "…"
	}
	return fmt.Sprintf("%q", line)
}
