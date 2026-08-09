package clifmt

import (
	"reflect"
	"testing"
)

func TestHasJSONFlag(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"absent", []string{"whoami"}, false},
		{"bare flag", []string{"whoami", "--json"}, true},
		{"assignment form", []string{"whoami", "--json=true"}, true},
		{"before verb", []string{"--json", "whoami"}, true},
		{"empty argv", nil, false},
		{"substring should not match", []string{"--jsonish"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasJSONFlag(c.argv); got != c.want {
				t.Fatalf("HasJSONFlag(%v) = %v, want %v", c.argv, got, c.want)
			}
		})
	}
}

func TestStripJSONFlagRemovesEveryOccurrence(t *testing.T) {
	rest, found := StripJSONFlag([]string{"explain", "--json", "cli", "overview"})
	if !found {
		t.Fatalf("found = false, want true")
	}
	want := []string{"explain", "cli", "overview"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("rest = %v, want %v", rest, want)
	}
}

func TestStripJSONFlagNoneFound(t *testing.T) {
	rest, found := StripJSONFlag([]string{"whoami"})
	if found {
		t.Fatalf("found = true, want false")
	}
	if !reflect.DeepEqual(rest, []string{"whoami"}) {
		t.Fatalf("rest = %v, want unchanged input", rest)
	}
}
