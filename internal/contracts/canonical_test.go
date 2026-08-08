package contracts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// canonicalGroups names the golden groups under testdata. Each group has a
// hand-written canonical form (<group>.canonical.json, byte-exact, no trailing
// newline), its digest (<group>.digest.txt), and one or more input variants
// (<group>.<variant>.json) that are logically identical but differ in key
// order and whitespace.
var canonicalGroups = []struct {
	name     string
	variants []string
}{
	{name: "record", variants: []string{"a", "b", "c"}},
	{name: "unicode-keys", variants: []string{"a", "b"}},
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestCanonicalJSONGolden proves that every key-order permutation of a
// document canonicalizes to the same bytes as the hand-written golden, and
// that the digest matches the golden digest.
func TestCanonicalJSONGolden(t *testing.T) {
	for _, group := range canonicalGroups {
		t.Run(group.name, func(t *testing.T) {
			golden := readFile(t, filepath.Join("testdata", "golden", group.name+".canonical.json"))
			wantDigest := strings.TrimSpace(string(readFile(t,
				filepath.Join("testdata", "golden", group.name+".digest.txt"))))

			if got := contracts.Digest(golden); got != wantDigest {
				t.Fatalf("golden digest file disagrees with Digest(golden):\n got %s\nwant %s", got, wantDigest)
			}

			for _, variant := range group.variants {
				variant := variant
				t.Run(variant, func(t *testing.T) {
					raw := readFile(t, filepath.Join("testdata", "canonical", group.name+"."+variant+".json"))

					got, err := contracts.CanonicalJSON(json.RawMessage(raw))
					if err != nil {
						t.Fatalf("CanonicalJSON: %v", err)
					}
					if !bytes.Equal(got, golden) {
						t.Errorf("canonical form mismatch\n got: %s\nwant: %s", got, golden)
					}
					digest, err := contracts.DigestValue(json.RawMessage(raw))
					if err != nil {
						t.Fatalf("DigestValue: %v", err)
					}
					if digest != wantDigest {
						t.Errorf("digest mismatch\n got %s\nwant %s", digest, wantDigest)
					}
				})
			}
		})
	}
}

// TestCanonicalJSONStableAcrossRepeatedRuns guards against Go's randomized map
// iteration order leaking into the canonical form or the digest.
func TestCanonicalJSONStableAcrossRepeatedRuns(t *testing.T) {
	doc := map[string]any{
		"zeta": 1, "alpha": 2, "mu": 3, "beta": 4, "omega": 5,
		"nested": map[string]any{
			"z": []any{1, 2, map[string]any{"b": true, "a": false}},
			"y": "value",
			"x": nil,
		},
	}

	first, err := contracts.CanonicalJSON(doc)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	firstDigest := contracts.Digest(first)

	for i := 0; i < 500; i++ {
		got, err := contracts.CanonicalJSON(doc)
		if err != nil {
			t.Fatalf("CanonicalJSON (iteration %d): %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("canonical form changed on iteration %d:\n got: %s\nwant: %s", i, got, first)
		}
		if d := contracts.Digest(got); d != firstDigest {
			t.Fatalf("digest changed on iteration %d: %s != %s", i, d, firstDigest)
		}
	}
}

func TestCanonicalJSONSortsKeysLexicographically(t *testing.T) {
	got, err := contracts.CanonicalJSON(json.RawMessage(`{"b":1,"A":2,"a":3,"aa":4,"":5}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"":5,"A":2,"a":3,"aa":4,"b":1}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalJSONHasNoInsignificantWhitespace(t *testing.T) {
	got, err := contracts.CanonicalJSON(json.RawMessage("{\n  \"a\" : [ 1 , 2 ],\n  \"b\" : { }\n}\n"))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"a":[1,2],"b":{}}`
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Error("canonical output must not end with a newline")
	}
}

// TestCanonicalJSONPreservesNumberLiterals locks the documented number rule:
// canonicalization never re-spells a number. A literal that arrived as JSON
// text survives verbatim (so a 30-digit integer is not silently pushed through
// float64), and a Go value is spelled by encoding/json's own defaults.
func TestCanonicalJSONPreservesNumberLiterals(t *testing.T) {
	const big = "123456789012345678901234567890"
	got, err := contracts.CanonicalJSON(json.RawMessage(
		`{"big":` + big + `,"exp":1e3,"one":1,"onepointzero":1.0,"neg":-0.5}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"big":` + big + `,"exp":1e3,"neg":-0.5,"one":1,"onepointzero":1.0}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}

	// The same logical values as Go floats take encoding/json's spelling.
	got, err = contracts.CanonicalJSON(map[string]any{"one": 1.0, "exp": float64(1000)})
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if want := `{"exp":1000,"one":1}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalJSONStringEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"utf8 preserved", `{"k":"héllo ✓"}`, `{"k":"héllo ✓"}`},
		{"html not escaped", `{"k":"<tag> & \"quoted\""}`, `{"k":"<tag> & \"quoted\""}`},
		{"control chars escaped", "{\"k\":\"a\\tb\\nc\"}", `{"k":"a\tb\nc"}`},
		{"escaped unicode folded to utf8", `{"k":"\u00e9"}`, `{"k":"é"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contracts.CanonicalJSON(json.RawMessage(tc.in))
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestCanonicalJSONDuplicateKeys documents that the last occurrence wins, the
// behaviour of encoding/json. Duplicate keys are not rejected here; a document
// that reaches this function has already been accepted as JSON.
func TestCanonicalJSONDuplicateKeys(t *testing.T) {
	got, err := contracts.CanonicalJSON(json.RawMessage(`{"a":1,"a":2}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if want := `{"a":2}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalJSONStructFieldsAreSorted(t *testing.T) {
	type record struct {
		Zulu  string `json:"zulu"`
		Alpha string `json:"alpha"`
		Mike  int    `json:"mike"`
	}
	got, err := contracts.CanonicalJSON(record{Zulu: "z", Alpha: "a", Mike: 3})
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if want := `{"alpha":"a","mike":3,"zulu":"z"}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalJSONTopLevelScalars(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`null`, `null`},
		{`true`, `true`},
		{`"s"`, `"s"`},
		{`3`, `3`},
		{`[]`, `[]`},
	} {
		got, err := contracts.CanonicalJSON(json.RawMessage(tc.in))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s): %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("CanonicalJSON(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalJSONErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"not json", json.RawMessage(`{`)},
		{"trailing content", json.RawMessage(`{} {}`)},
		{"unsupported value", math.Inf(1)},
		{"unsupported type", make(chan int)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := contracts.CanonicalJSON(tc.in); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("{}"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := contracts.Digest([]byte("{}")); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := contracts.Digest([]byte("{}")); !strings.HasPrefix(got, "sha256:") {
		t.Errorf("digest %q lacks the sha256: prefix", got)
	}
}

func TestDigestValueMatchesDigestOfCanonicalJSON(t *testing.T) {
	doc := json.RawMessage(`{"b":[1,{"d":4,"c":3}],"a":"x"}`)
	canonical, err := contracts.CanonicalJSON(doc)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	got, err := contracts.DigestValue(doc)
	if err != nil {
		t.Fatalf("DigestValue: %v", err)
	}
	if want := contracts.Digest(canonical); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestDigestValueIgnoresKeyOrder is the property the ledger and the compiler
// both depend on: the digest identifies content, not the spelling of the
// document that carried it.
func TestDigestValueIgnoresKeyOrder(t *testing.T) {
	a, err := contracts.DigestValue(json.RawMessage(`{"a":1,"b":{"c":2,"d":[3,4]}}`))
	if err != nil {
		t.Fatalf("DigestValue: %v", err)
	}
	b, err := contracts.DigestValue(json.RawMessage("{\n\t\"b\": {\"d\": [3, 4], \"c\": 2},\n\t\"a\": 1\n}"))
	if err != nil {
		t.Fatalf("DigestValue: %v", err)
	}
	if a != b {
		t.Errorf("digests differ for logically identical documents: %s != %s", a, b)
	}
}
