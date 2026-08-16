package actors_test

import (
	"crypto/sha256"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

func TestInboundCredentialVerifierHash(t *testing.T) {
	want := sha256.Sum256([]byte("right-value"))
	verifier, err := actors.NewInboundCredentialVerifier(want[:], "", nil)
	if err != nil {
		t.Fatalf("NewInboundCredentialVerifier: %v", err)
	}
	if !verifier.Verify("right-value") {
		t.Error("correct presented value was refused")
	}
	if verifier.Verify("wrong-value") {
		t.Error("wrong presented value was accepted")
	}
}

func TestInboundCredentialVerifierEnvironmentReference(t *testing.T) {
	lookups := 0
	verifier, err := actors.NewInboundCredentialVerifier(nil, "BRIDGE_DIAL_IN_KEY", func(name string) (string, bool) {
		lookups++
		if name != "BRIDGE_DIAL_IN_KEY" {
			t.Fatalf("lookup name = %q", name)
		}
		return "right-value", true
	})
	if err != nil {
		t.Fatalf("NewInboundCredentialVerifier: %v", err)
	}
	if !verifier.Verify("right-value") || verifier.Verify("wrong-value") {
		t.Error("environment-backed verifier did not distinguish right from wrong")
	}
	if lookups != 2 {
		t.Errorf("environment lookups = %d, want 2 (read at each use)", lookups)
	}
}

func TestInboundCredentialVerifierRejectsAmbiguousRecords(t *testing.T) {
	hash := sha256.Sum256([]byte("value"))
	cases := []struct {
		name string
		hash []byte
		env  string
	}{
		{"neither", nil, ""},
		{"both", hash[:], "KEY"},
		{"wrong hash width", []byte("short"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := actors.NewInboundCredentialVerifier(tc.hash, tc.env, nil); err == nil {
				t.Fatal("ambiguous or malformed record was accepted")
			}
		})
	}
}

func TestMissingEnvironmentCredentialNeverMatches(t *testing.T) {
	verifier, err := actors.NewInboundCredentialVerifier(nil, "ABSENT_KEY", func(string) (string, bool) {
		return "", false
	})
	if err != nil {
		t.Fatalf("NewInboundCredentialVerifier: %v", err)
	}
	if verifier.Verify("") {
		t.Error("an absent environment value matched an empty presentation")
	}
}
