// Does a deploy's revision stamp actually reach the installed bridge?
// (task t32, issue #120 item 4)
//
// Every other file in this directory is a static text assertion over
// deploy/prod/deploy.sh, for the reason codexdeploylane_test.go states: the
// script targets production, so running it for real is a manual operator
// step. The static half is here too — the deploy must WRITE the stamp, and
// must write it before the install that copies it.
//
// But one assertion in this feature cannot be made statically, and it is the
// one the whole mechanism rests on: does the stamp actually reach the built
// wheel? `_revision.json` is generated per install rather than committed, and
// hatchling drops a VCS-ignored file from a wheel. If that wins, deploy.sh
// writes a stamp, the build silently discards it, and every `uv tool
// install`ed bridge reports a revision it cannot establish — a silent no-op
// inside the feature built to end a silent no-op.
//
// So TestTheRevisionStampSurvivesAWheelBuild really builds a wheel and looks
// inside it. Read its comment before changing it: the conditions under which
// hatchling drops the file are narrower than they first appear, and the test
// reproduces them deliberately rather than trusting the real tree to be in
// them. It needs `uv` and `git`, and is skipped without either.
package deploytest

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stampFile is codex_bridge.deployment.REVISION_STAMP_FILE. It is spelled out
// here rather than read from Python: this test is the guard against the two
// sides disagreeing, so deriving it from one of them would guard nothing.
const stampFile = "_revision.json"

// bridgeAdapters are the adapters whose wheels a deploy installs as copies.
var bridgeAdapters = []string{"codex", "claude-code", "colleague", "notify", "qwen"}

func repoRootDir(t *testing.T) string {
	t.Helper()
	// codexBridgeDir returns <root>/deploy/prod; the repo root is two up.
	return filepath.Dir(filepath.Dir(codexBridgeDir(t)))
}

// TestEveryAdapterDeclaresTheStampAsABuildArtifact is the static half: the
// TOML line exists in all four, so a fifth bridge added by copying one of
// them inherits it.
func TestEveryAdapterDeclaresTheStampAsABuildArtifact(t *testing.T) {
	root := repoRootDir(t)
	for _, adapter := range bridgeAdapters {
		path := filepath.Join(root, "adapters", adapter, "pyproject.toml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(raw), stampFile+`"]`) {
			t.Errorf("adapters/%s/pyproject.toml declares no artifacts entry for %s: the stamp is "+
				"VCS-ignored, so hatchling would leave it out of the wheel and the installed bridge "+
				"would report a revision it cannot establish", adapter, stampFile)
		}
	}
}

// TestTheRevisionStampSurvivesAWheelBuild is the empirical half, and it
// builds the adapter in a COPY with its own git repository rather than in
// place. The copy is not tidiness — it is what makes the test prove anything.
//
// Measured while writing this: hatchling resolves `.gitignore` relative to
// ITS OWN project root, which for these adapters is `adapters/<name>/`, not
// the repository root. The repo-root rule this feature adds
// (`adapters/*/src/*/_revision.json`) is therefore invisible to the build,
// and a wheel built today carries the stamp with or without the `artifacts`
// entry. A test run against the real tree would pass either way and prove
// nothing.
//
// The hazard is one line away, though, and it is the kind of line somebody
// adds while tidying: an ADAPTER-LOCAL `.gitignore` naming the stamp makes
// hatchling drop it from the wheel, measured directly —
//
//	local .gitignore + artifacts entry  -> stamp in wheel: true
//	local .gitignore, artifacts removed -> stamp in wheel: false
//
// So the copy gets exactly that local `.gitignore`, and the test asserts the
// stamp survives it. That makes the `artifacts` entry load-bearing here even
// though it is currently redundant in the real tree, and it means the
// ablation ("delete the artifacts line") fails the way an enforcement should.
func TestTheRevisionStampSurvivesAWheelBuild(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv is not on PATH; skipping the real wheel build")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH; hatchling needs a VCS root to apply an ignore rule at all")
	}
	root := repoRootDir(t)

	// codex alone: the four adapters' build configuration is identical (the
	// static test above pins that), and building four wheels would spend
	// four times the wall clock to re-answer one question.
	build := filepath.Join(t.TempDir(), "codex")
	if out, err := exec.Command("cp", "-r", filepath.Join(root, "adapters", "codex"), build).CombinedOutput(); err != nil {
		t.Fatalf("copy the adapter: %v\n%s", err, out)
	}
	// A build directory the copy may have inherited would be reused verbatim.
	for _, junk := range []string{".venv", "dist", "build"} {
		_ = os.RemoveAll(filepath.Join(build, junk))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A", "-f"}} {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Dir = build
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	stampRel := filepath.Join("src", "codex_bridge", stampFile)
	// The hazard, reproduced: an adapter-local ignore rule for the stamp.
	if err := os.WriteFile(filepath.Join(build, ".gitignore"),
		[]byte(filepath.ToSlash(stampRel)+"\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	const revision = "774d5153c32a2e2fdb86f699d814977d111f1408"
	stamp, err := json.Marshal(map[string]string{"revision": revision, "source": "revisionstamp_test"})
	if err != nil {
		t.Fatalf("marshal stamp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(build, stampRel), stamp, 0o600); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	out := t.TempDir()
	cmd := exec.CommandContext(ctx, uv, "build", "--wheel", "--out-dir", out, ".")
	cmd.Dir = build
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("uv build failed (offline or no build backend available): %v\n%s", err, combined)
	}

	wheels, err := filepath.Glob(filepath.Join(out, "*.whl"))
	if err != nil || len(wheels) != 1 {
		t.Fatalf("expected exactly one wheel in %s, got %v (%v)", out, wheels, err)
	}

	zr, err := zip.OpenReader(wheels[0])
	if err != nil {
		t.Fatalf("open wheel: %v", err)
	}
	defer zr.Close()

	want := "codex_bridge/" + stampFile
	for _, f := range zr.File {
		if f.Name != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s in wheel: %v", want, err)
		}
		defer rc.Close()
		var got map[string]string
		if err := json.NewDecoder(rc).Decode(&got); err != nil {
			t.Fatalf("decode %s from wheel: %v", want, err)
		}
		if got["revision"] != revision {
			t.Fatalf("wheel's stamp names revision %q, want %q", got["revision"], revision)
		}
		return
	}

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	t.Fatalf("%s is NOT in the built wheel, so a `uv tool install`ed bridge would carry no revision "+
		"stamp and could not say what it is running. hatchling drops a VCS-ignored file; the "+
		"[tool.hatch.build] artifacts entry is what overrides it. Wheel contains: %v",
		want, names)
}

// --- deploy.sh writes it, and writes it in time -----------------------------

// TestTheDeployStampsTheRevisionItShips is the static counterpart: a build
// that CAN carry a stamp is useless if the deploy never writes one.
func TestTheDeployStampsTheRevisionItShips(t *testing.T) {
	script := deployScriptText(t)

	if !strings.Contains(script, stampFile) {
		t.Fatalf("deploy/prod/deploy.sh never writes a %s: it resolves $REVISION and ships that "+
			"exact tree, so it is the only party that knows what it deployed — and issue #120 is "+
			"what happens when nothing records it", stampFile)
	}
	if !strings.Contains(script, "stamp_revision") {
		t.Error("deploy.sh has no stamp_revision helper: every bridge lane must stamp the same way, " +
			"and three inlined copies of one write is how resolve_actor_row_id shipped as the same " +
			"bug in three lanes")
	}

	// Order is the load-bearing part, and it is checked PER LANE against the
	// exact call rather than against the helper's name. An earlier version of
	// this test searched for "stamp_revision " and matched the usage line in
	// the helper's own doc comment, which sits above every install in the
	// file — so it passed with the call moved after the install, i.e. it
	// guarded nothing. Matching `stamp_revision "$<var>" <adapter>` cannot hit
	// a comment. The variable is `$host` for the login-user lanes and
	// `$target` for the lanes that install into an engine account (#243) —
	// the ssh target that IS the account — and both are the first argument.
	for _, lane := range []struct{ adapter, installNeedle string }{
		{"codex", `uv tool install --force ./$REMOTE_DIR/adapters/codex`},
		{"notify", `uv tool install --force ./$REMOTE_DIR/adapters/notify`},
		{"claude-code", `uv tool install --force ./$REMOTE_DIR/adapters/claude-code`},
		{"qwen", `uv tool install --force ./$REMOTE_DIR/adapters/qwen`},
	} {
		call := regexp.MustCompile(`stamp_revision "\$(host|target)" ` + regexp.QuoteMeta(lane.adapter) + ` `)
		stampAt := -1
		if loc := call.FindStringIndex(script); loc != nil {
			stampAt = loc[0]
		}
		installAt := strings.Index(script, lane.installNeedle)
		if stampAt < 0 {
			t.Errorf("the %s lane never calls %s, so that bridge is installed with no record of what "+
				"revision it is", lane.adapter, call)
			continue
		}
		if installAt < 0 {
			t.Errorf("could not find the %s install in deploy.sh; this test's needle is stale", lane.adapter)
			continue
		}
		// `uv tool install` COPIES, so a stamp written afterwards lands only
		// in the shipped archive the next deploy's `rm -rf` deletes, and
		// never in the installed bridge at all.
		if stampAt > installAt {
			t.Errorf("the %s lane stamps the revision AFTER `uv tool install` copies the package: the "+
				"stamp would land in the shipped archive the next deploy deletes, and the installed "+
				"bridge would still report a revision it cannot establish", lane.adapter)
		}
	}
}

// TestTheStampIsAFullCommitSHA pins that the deploy stamps the resolved
// 40-hex revision rather than the branch name it was given. The bridge-side
// reader refuses anything else (deployment._full_commit_sha), so a deploy
// stamping "$BRANCH" would produce a stamp that is silently ignored.
func TestTheStampIsAFullCommitSHA(t *testing.T) {
	script := deployScriptText(t)
	stampAt := strings.Index(script, "stamp_revision()")
	if stampAt < 0 {
		t.Fatal("no stamp_revision() definition in deploy.sh")
	}
	body := script[stampAt:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "rev-parse --short") {
		t.Error("stamp_revision writes an ABBREVIATED sha: the bridge refuses anything that is not a " +
			"full 40-character lowercase hex commit id, so the stamp would be ignored and the " +
			"install would report no revision at all")
	}
	if !strings.Contains(body, "$REVISION") {
		t.Error("stamp_revision does not write $REVISION, the resolved commit this deploy shipped")
	}
}

// --- the control plane's own revision (issue #104) --------------------------

// TestTheControlPlaneImageIsBuiltWithItsRevision pins the other half of task
// t32's second criterion. The bridges answer for themselves; the control
// plane has to be TOLD, because its image is built from a `git archive` with
// no .git in it and the Go toolchain therefore stamps nothing.
//
// #104 is the cost of the gap: `culture-nodes:prod` on thor was fifteen hours
// old and running none of a merged batch, and the way that was established
// was a POST at a route that should have existed, answering 405.
func TestTheControlPlaneImageIsBuiltWithItsRevision(t *testing.T) {
	root := repoRootDir(t)

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), "ARG REVISION") {
		t.Error("the Dockerfile declares no REVISION build arg, so the image cannot be told what it is")
	}
	if !strings.Contains(string(dockerfile), "-X main.revision=${REVISION}") {
		t.Error("the Dockerfile does not inject main.revision: the binary would serve no revision on " +
			"GET /v1alpha1/version and a live test could not say which code it tested")
	}

	script := deployScriptText(t)
	if !strings.Contains(script, "--build-arg REVISION=$REVISION") {
		t.Error("deploy.sh's docker build passes no REVISION: it is the only party that resolved the " +
			"commit it shipped")
	}
	// Both build lanes, because compose rebuilds the same Dockerfile by a
	// different route and a revision stamped into only one of them makes the
	// answer depend on which lane last ran.
	if !strings.Contains(script, "NODES_BUILD_REVISION=$REVISION docker compose") {
		t.Error("deploy.sh's compose lane passes no NODES_BUILD_REVISION, so a `compose up --build` " +
			"would rebuild the image without one and quietly erase the answer")
	}

	for _, name := range []string{"compose.thor.yml", "compose.orin.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, "deploy", "prod", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(raw), "REVISION: ${NODES_BUILD_REVISION:-}") {
			t.Errorf("%s's build block forwards no REVISION arg", name)
		}
	}
}
