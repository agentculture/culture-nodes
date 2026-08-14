package notifier

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCursorMissingFileStartsFromBeginning(t *testing.T) {
	c, err := LoadCursor(filepath.Join(t.TempDir(), "cursor.json"))
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got := c.Last(); got != "" {
		t.Fatalf("Last() = %q, want empty (no cursor file yet)", got)
	}
}

func TestLoadCursorEmptyPathIsRefused(t *testing.T) {
	if _, err := LoadCursor(""); err == nil {
		t.Fatal("LoadCursor(\"\") = nil error, want a refusal")
	}
}

func TestLoadCursorRefusesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt cursor file: %v", err)
	}
	if _, err := LoadCursor(path); err == nil {
		t.Fatal("LoadCursor accepted a corrupt cursor file; silently discarding it risks replaying the whole stream")
	}
}

func TestAdvancePersistsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if err := c.Advance("01AAAA"); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got := c.Last(); got != "01AAAA" {
		t.Fatalf("Last() = %q, want 01AAAA", got)
	}

	reloaded, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("reload LoadCursor: %v", err)
	}
	if got := reloaded.Last(); got != "01AAAA" {
		t.Fatalf("reloaded Last() = %q, want 01AAAA", got)
	}
}

func TestAdvanceIsANoOpGoingBackwards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if err := c.Advance("01BBBB"); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := c.Advance("01AAAA"); err != nil {
		t.Fatalf("Advance (backwards): %v", err)
	}
	if got := c.Last(); got != "01BBBB" {
		t.Fatalf("Last() = %q, want 01BBBB (a lexically-earlier id must not move the cursor backwards)", got)
	}
}

func TestSeenReportsAtOrBeforeLastID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if err := c.Advance("01BBBB"); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	cases := []struct {
		id   string
		want bool
	}{
		{"01AAAA", true},  // before LastID
		{"01BBBB", true},  // exactly LastID
		{"01CCCC", false}, // after LastID
	}
	for _, tc := range cases {
		if got := c.Seen(tc.id); got != tc.want {
			t.Errorf("Seen(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestMarkDeliveredAdvancesAndRemembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if err := c.MarkDelivered("01CCCC"); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if got := c.Last(); got != "01CCCC" {
		t.Fatalf("Last() = %q, want 01CCCC", got)
	}
	if !c.Seen("01CCCC") {
		t.Fatal("Seen(01CCCC) = false right after MarkDelivered, want true")
	}

	// Restart: a fresh Cursor loaded from the same file must remember the
	// delivery too, not just the position.
	reloaded, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("reload LoadCursor: %v", err)
	}
	if !reloaded.Seen("01CCCC") {
		t.Fatal("reloaded cursor forgot a delivered id")
	}
}

func TestDeliveredWindowIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	for i := 0; i < deliveredWindow+10; i++ {
		id := ulidLike(i)
		if err := c.MarkDelivered(id); err != nil {
			t.Fatalf("MarkDelivered(%d): %v", i, err)
		}
	}
	if got := len(c.st.Delivered); got > deliveredWindow {
		t.Fatalf("Delivered window grew to %d entries, want <= %d", got, deliveredWindow)
	}
}

// ulidLike returns a fixed-width, lexically-increasing id for i, standing
// in for a real ULID in tests that only care about ordering, not the real
// encoding.
func ulidLike(i int) string {
	const digits = "0123456789"
	s := make([]byte, 10)
	for pos := len(s) - 1; pos >= 0; pos-- {
		s[pos] = digits[i%10]
		i /= 10
	}
	return string(s)
}
