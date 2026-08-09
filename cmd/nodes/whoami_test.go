package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withCwd temporarily changes the process working directory for the
// duration of the test, restoring it afterwards. findCultureYAML/doctor's
// culture.yaml check both walk up from os.Getwd(), so tests that want a
// deterministic "no culture.yaml anywhere above here" need an isolated cwd.
func withCwd(t *testing.T, dir string) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Fatalf("restore Chdir(%s): %v", original, err)
		}
	})
}

func TestFindCultureYAMLFallsBackWhenAbsent(t *testing.T) {
	// t.TempDir() lives under the OS temp dir, well outside this repo, so
	// walking up from it should never find a culture.yaml.
	withCwd(t, t.TempDir())

	if _, ok := findCultureYAML(); ok {
		t.Fatal("findCultureYAML() found a culture.yaml above an isolated temp dir")
	}
}

func TestFindCultureYAMLWalksUpToParent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "culture.yaml"), []byte("agents:\n- suffix: test-agent\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	withCwd(t, nested)

	path, ok := findCultureYAML()
	if !ok {
		t.Fatal("findCultureYAML() did not find the ancestor culture.yaml")
	}
	want := filepath.Join(root, "culture.yaml")
	if path != want {
		t.Fatalf("findCultureYAML() = %q, want %q", path, want)
	}
}

func TestReadAgentFieldsFallsBackWithoutCultureYAML(t *testing.T) {
	withCwd(t, t.TempDir())

	nick, backend, model := readAgentFields()
	if nick != fallbackNick {
		t.Fatalf("nick = %q, want fallback %q", nick, fallbackNick)
	}
	if backend != "unknown" || model != "unknown" {
		t.Fatalf("backend/model = %q/%q, want unknown/unknown", backend, model)
	}
}

func TestReadAgentFieldsParsesFirstAgentBlock(t *testing.T) {
	dir := t.TempDir()
	content := "agents:\n" +
		"- suffix: my-agent\n" +
		"  backend: claude\n" +
		"  model: some-model\n" +
		"- suffix: second-agent\n" +
		"  backend: colleague\n"
	if err := os.WriteFile(filepath.Join(dir, "culture.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	withCwd(t, dir)

	nick, backend, model := readAgentFields()
	if nick != "my-agent" {
		t.Fatalf("nick = %q, want %q", nick, "my-agent")
	}
	if backend != "claude" {
		t.Fatalf("backend = %q, want %q (only the first agent block)", backend, "claude")
	}
	if model != "some-model" {
		t.Fatalf("model = %q, want %q", model, "some-model")
	}
}

func TestScalarAfterStripsQuotesAndHandlesEmpty(t *testing.T) {
	cases := []struct {
		line, key, want string
	}{
		{"suffix: my-agent", "suffix", "my-agent"},
		{"suffix: 'quoted'", "suffix", "quoted"},
		{`suffix: "double"`, "suffix", "double"},
		{"suffix:", "suffix", "unknown"},
		{"suffix:   ", "suffix", "unknown"},
	}
	for _, c := range cases {
		if got := scalarAfter(c.line, c.key); got != c.want {
			t.Errorf("scalarAfter(%q, %q) = %q, want %q", c.line, c.key, got, c.want)
		}
	}
}

func TestCmdWhoamiRejectsUnknownFlag(t *testing.T) {
	_, err := cmdWhoami([]string{"--bogus"}, false)
	if err == nil {
		t.Fatal("cmdWhoami with an unknown flag returned nil error")
	}
}
