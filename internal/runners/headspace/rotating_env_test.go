package headspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEnvRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-app-runner.env")
	t.Setenv("NODES_RUNNER_ROTATING_ENV_FILE", path)
	t.Setenv("GITHUB_TOKEN", "old-pat")
	t.Setenv("OTHER_REF", "process-other")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("# rotated token\n\nGITHUB_TOKEN='first-app-token'\nUNREQUESTED=do-not-forward\nOTHER_REF=\"file-other\"\n")
	values, missing := resolveEnv([]string{"GITHUB_TOKEN", "OTHER_REF"})
	if len(missing) != 0 || len(values) != 2 || values["GITHUB_TOKEN"] != "first-app-token" || values["OTHER_REF"] != "file-other" {
		t.Fatalf("unexpected first resolution: values=%v missing=%v", values, missing)
	}
	if _, ok := values["UNREQUESTED"]; ok {
		t.Fatal("unrequested rotating key was forwarded")
	}

	write("GITHUB_TOKEN=second-app-token\n")
	values, missing = resolveEnv([]string{"GITHUB_TOKEN", "OTHER_REF"})
	if len(missing) != 0 || values["GITHUB_TOKEN"] != "second-app-token" || values["OTHER_REF"] != "process-other" {
		t.Fatalf("unexpected second resolution: values=%v missing=%v", values, missing)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	values, missing = resolveEnv([]string{"GITHUB_TOKEN"})
	if len(missing) != 0 || values["GITHUB_TOKEN"] != "old-pat" {
		t.Fatalf("missing file did not fall back to process env: values=%v missing=%v", values, missing)
	}

	t.Setenv("NODES_RUNNER_ROTATING_ENV_FILE", t.TempDir())
	values, missing = resolveEnv([]string{"GITHUB_TOKEN"})
	if len(missing) != 0 || values["GITHUB_TOKEN"] != "old-pat" {
		t.Fatal("unreadable file did not fall back to process env")
	}

	t.Setenv("NODES_RUNNER_ROTATING_ENV_FILE", path)
	write("FILE_ONLY=file-only\n")
	values, missing = resolveEnv([]string{"FILE_ONLY", "FILE_MISSING"})
	if values["FILE_ONLY"] != "file-only" || len(missing) != 1 || missing[0] != "FILE_MISSING" {
		t.Fatal("file-only value or missing ref was not resolved correctly")
	}
}
