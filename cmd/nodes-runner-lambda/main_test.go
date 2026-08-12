package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestTailBufferKeepsOnlyTheTailWithoutGrowing pins the review finding that
// motivated the bounded implementation: the buffer must hold the last limit
// bytes AND its backing array must never exceed cap=limit, whatever the
// write pattern — many small writes, one huge write, or a mix.
func TestTailBufferKeepsOnlyTheTailWithoutGrowing(t *testing.T) {
	const limit = 16

	cases := []struct {
		name   string
		writes []string
		want   string
	}{
		{"under limit", []string{"abc", "def"}, "abcdef"},
		{"exact limit", []string{"0123456789abcdef"}, "0123456789abcdef"},
		{"rolls over across writes", []string{"0123456789", "abcdefghij"}, "456789abcdefghij"},
		{"single write far over limit", []string{strings.Repeat("x", 4096) + "TAIL-0123456789"}, "xTAIL-0123456789"},
		{"huge then small", []string{strings.Repeat("y", 1024), "end"}, strings.Repeat("y", 13) + "end"},
		{"empty writes are no-ops", []string{"", "abc", ""}, "abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tb := &tailBuffer{limit: limit}
			for _, w := range tc.writes {
				n, err := tb.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", w, n, err, len(w))
				}
				if cap(tb.buf) > limit {
					t.Fatalf("backing array grew to cap %d after %q; the bound must hold for capacity, not just length", cap(tb.buf), w)
				}
			}
			if got := tb.String(); got != tc.want {
				t.Errorf("tail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunRefusalTyping pins the colleague-review fold: policy refusals and
// runner-side failures must carry distinct errorType values, and an
// executable operation must produce a report, not a refusal.
func TestRunRefusalTyping(t *testing.T) {
	deadline := time.Now().Add(time.Minute)

	op := func(mutate func(m map[string]any)) []byte {
		m := map[string]any{
			"operation_id":    "op_test",
			"runner":          "lambda",
			"runner_revision": "r1",
			"execution":       map[string]any{"kind": "function", "image_ref": "k", "image_digest": "sha256:0"},
			"command":         map[string]any{"argv": []string{"true"}, "environment_refs": []string{}},
			"policy":          map[string]any{"timeout_seconds": 30, "network": "full", "allowed_output_paths": []string{}},
			"evidence":        map[string]any{"capture_exit": true},
		}
		if mutate != nil {
			mutate(m)
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return encoded
	}

	t.Run("workspace refused as OperationRefused", func(t *testing.T) {
		_, ref := run(op(func(m map[string]any) {
			m["workspace"] = map[string]any{"source_ref": "s3://x", "source_digest": "sha256:0", "write_mode": "read-only"}
		}), deadline)
		if ref == nil || ref.kind != "OperationRefused" {
			t.Fatalf("refusal = %+v, want kind OperationRefused", ref)
		}
	})

	t.Run("unstartable argv fails as RunnerError", func(t *testing.T) {
		_, ref := run(op(func(m map[string]any) {
			m["command"] = map[string]any{"argv": []string{"/nonexistent-binary-for-test"}, "environment_refs": []string{}}
		}), deadline)
		if ref == nil || ref.kind != "RunnerError" {
			t.Fatalf("refusal = %+v, want kind RunnerError", ref)
		}
	})

	t.Run("runnable argv reports exit 0", func(t *testing.T) {
		rep, ref := run(op(nil), deadline)
		if ref != nil {
			t.Fatalf("unexpected refusal: %v", ref)
		}
		if rep.ExitCode == nil || *rep.ExitCode != 0 {
			t.Fatalf("report = %+v, want exit_code 0", rep)
		}
	})
}
