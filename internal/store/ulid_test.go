package store

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewULIDShapeAndAlphabet(t *testing.T) {
	id := NewULID()

	if len(id) != 26 {
		t.Fatalf("NewULID() length = %d, want 26 (id=%q)", len(id), id)
	}

	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for i, r := range id {
		if !strings.ContainsRune(alphabet, r) {
			t.Fatalf("NewULID() char %d = %q is outside the Crockford base32 alphabet (id=%q)", i, r, id)
		}
	}
}

func TestNewULIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	const n = 10_000
	for i := 0; i < n; i++ {
		id := NewULID()
		if seen[id] {
			t.Fatalf("NewULID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

// TestNewULIDMonotonicWithinSameMillisecond generates IDs back-to-back (so
// many land in the same millisecond) and asserts generation order equals
// lexical sort order -- the property that makes ULIDs useful as primary
// keys ordered by creation time.
func TestNewULIDMonotonicWithinSameMillisecond(t *testing.T) {
	const n = 5_000
	generated := make([]string, n)
	for i := 0; i < n; i++ {
		generated[i] = NewULID()
	}

	sorted := make([]string, n)
	copy(sorted, generated)
	sort.Strings(sorted)

	for i := range generated {
		if generated[i] != sorted[i] {
			t.Fatalf("ULID generation order is not lexically sorted at index %d: generated=%q sorted=%q",
				i, generated[i], sorted[i])
		}
	}
}

// TestNewULIDMonotonicAcrossMilliseconds sleeps across a millisecond
// boundary and checks the later ID still sorts after the earlier one.
func TestNewULIDMonotonicAcrossMilliseconds(t *testing.T) {
	first := NewULID()
	time.Sleep(2 * time.Millisecond)
	second := NewULID()

	if second <= first {
		t.Fatalf("expected second ULID %q to sort after first %q", second, first)
	}
}
