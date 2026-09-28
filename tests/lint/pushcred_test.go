package testslint

import (
	"os"
	"path/filepath"
	"testing"
)

func samePushcredCopies(copies [][]byte) bool {
	if len(copies) != 5 {
		return false
	}
	for _, copy := range copies[1:] {
		if string(copy) != string(copies[0]) {
			return false
		}
	}
	return true
}

func TestPushcredIdenticalInAllFiveBridges(t *testing.T) {
	paths := []struct{ adapter, pkg string }{
		{"claude-code", "claude_code_bridge"}, {"codex", "codex_bridge"},
		{"qwen", "qwen_bridge"}, {"pi", "pi_bridge"},
		{"colleague", "colleague_bridge"},
	}
	var copies [][]byte
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(repoRoot(t), "adapters", path.adapter, "src", path.pkg, "pushcred.py"))
		if err != nil {
			t.Fatal(err)
		}
		copies = append(copies, data)
	}
	if !samePushcredCopies(copies) {
		t.Fatal("pushcred.py must be byte-identical in all five bridges")
	}
}

func TestPushcredGuardRejectsDrift(t *testing.T) {
	copies := [][]byte{[]byte("same"), []byte("same"), []byte("same"), []byte("same"), []byte("same")}
	if !samePushcredCopies(copies) {
		t.Fatal("identical copies rejected")
	}
	copies[3] = []byte("drift")
	if samePushcredCopies(copies) {
		t.Fatal("drifted copy accepted")
	}
}
