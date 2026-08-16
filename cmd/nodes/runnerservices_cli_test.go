package main

import (
	"path/filepath"
	"testing"
)

func TestRegisterRunnerServiceFileCreatesInspectableRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "runners.json")
	e := runnerServiceEntry{Name: "code", Endpoint: "https://runner.example", ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SecretFile: "/run/secrets/runner"}
	if err := registerRunnerServiceFile(path, e); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := loadRunnerServiceEntries(path)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0] != e {
		t.Fatalf("got %#v, want %#v", got, []runnerServiceEntry{e})
	}
	if err := registerRunnerServiceFile(path, e); err == nil {
		t.Fatal("duplicate registration was accepted")
	}
}
