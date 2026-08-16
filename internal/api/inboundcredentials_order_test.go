// Package-internal tests for the inbound credential request decoder, which
// reaches an unexported type — the sibling inboundcredentials_test.go is an
// external `api_test` package and cannot see it. Same split as
// runs_filter_test.go and logging_test.go.
package api

import (
	"encoding/json"
	"testing"
)

// TestSuppliedMaterialIsDeterministicAcrossManyForbiddenFields pins the fix for
// a review finding on PR #154: suppliedMaterial iterated a Go map, and Go
// randomises map iteration, so a request carrying several forbidden fields was
// refused by a DIFFERENT field name on each run.
//
// The refusal names a field so an operator is told which key to remove. A hint
// that varies run to run cannot be automated against and cannot be asserted on.
//
// Iterating many times is the point: a single call would pass against the buggy
// version roughly one time in seven, which is exactly the kind of test that
// lets a nondeterminism bug survive.
func TestSuppliedMaterialIsDeterministicAcrossManyForbiddenFields(t *testing.T) {
	req := inboundCredentialRequest{
		Credential:      json.RawMessage(`"a"`),
		Token:           json.RawMessage(`"b"`),
		Secret:          json.RawMessage(`"c"`),
		Verifier:        json.RawMessage(`"d"`),
		VerifierSHA256:  json.RawMessage(`"e"`),
		VerifierEnvName: json.RawMessage(`"f"`),
		Digest:          json.RawMessage(`"g"`),
	}
	const want = "credential"
	for i := 0; i < 200; i++ {
		if got := req.suppliedMaterial(); got != want {
			t.Fatalf("suppliedMaterial() = %q on iteration %d, want %q every time — "+
				"the refusal hint must not vary run to run", got, i, want)
		}
	}
}

// TestSuppliedMaterialNamesTheFirstDeclaredFieldPresent pins the ORDER itself,
// not merely that it is stable: a later field must not shadow an earlier one,
// and an explicit JSON null is not supplied material.
func TestSuppliedMaterialNamesTheFirstDeclaredFieldPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  inboundCredentialRequest
		want string
	}{
		{"token before secret", inboundCredentialRequest{
			Token: json.RawMessage(`"b"`), Secret: json.RawMessage(`"c"`)}, "token"},
		{"verifier before its two spellings", inboundCredentialRequest{
			Verifier: json.RawMessage(`"d"`), VerifierSHA256: json.RawMessage(`"e"`),
			VerifierEnvName: json.RawMessage(`"f"`)}, "verifier"},
		{"digest alone", inboundCredentialRequest{
			Digest: json.RawMessage(`"g"`)}, "digest_sha256"},
		{"an explicit null is not supplied material", inboundCredentialRequest{
			Credential: json.RawMessage(`null`)}, ""},
		{"nothing supplied", inboundCredentialRequest{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.suppliedMaterial(); got != tc.want {
				t.Fatalf("suppliedMaterial() = %q, want %q", got, tc.want)
			}
		})
	}
}
