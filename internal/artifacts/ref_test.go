package artifacts_test

import (
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts"
)

func TestNewRefParseRefRoundTrip(t *testing.T) {
	ref := artifacts.NewRef("ns_01J000000000000000000000", "01J111111111111111111111")

	if got, want := string(ref), "artifact://ns_01J000000000000000000000/01J111111111111111111111"; got != want {
		t.Fatalf("NewRef = %q, want %q", got, want)
	}

	namespaceID, id, err := artifacts.ParseRef(ref)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if namespaceID != "ns_01J000000000000000000000" {
		t.Fatalf("namespaceID = %q, want %q", namespaceID, "ns_01J000000000000000000000")
	}
	if id != "01J111111111111111111111" {
		t.Fatalf("id = %q, want %q", id, "01J111111111111111111111")
	}
}

func TestParseRefRejectsMalformedRefs(t *testing.T) {
	cases := []struct {
		name string
		ref  artifacts.Ref
	}{
		{"missing scheme", "ns/id"},
		{"wrong scheme", "s3://ns/id"},
		{"missing separator", "artifact://ns-only"},
		{"empty namespace", "artifact:///id"},
		{"empty id", "artifact://ns/"},
		{"id with extra slash", "artifact://ns/id/extra"},
		{"empty ref", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := artifacts.ParseRef(tc.ref)
			if err == nil {
				t.Fatalf("ParseRef(%q) succeeded, want ErrInvalidRef", tc.ref)
			}
			if !errors.Is(err, artifacts.ErrInvalidRef) {
				t.Fatalf("ParseRef(%q) error = %v, want it to wrap ErrInvalidRef", tc.ref, err)
			}
		})
	}
}
