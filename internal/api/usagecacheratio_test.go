package api

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t8 (frame claim c8): cache reads are reported OUTSIDE input_tokens by
// every backend that reports them at all — a codex attempt's
// `cached_input_tokens` is a *sibling* of `input_tokens`, not a subset of it.
// Dividing cached/input therefore has no bound at 1.0, and the /stats tile
// rendered "588% cached" on real production data. The honest denominator is
// the total prompt the attempt actually consumed: input + cached.
//
// These are pure unit tests over usageOut — no fixture, no database — so the
// ratio's arithmetic is pinned even where the DB-backed suite skips.
func TestUsageOutCacheRatioNeverExceedsOneWhenCacheReadsAreOutsideInput(t *testing.T) {
	// The shape that produced the 588% reading: cache reads dwarf the
	// uncached input they sit beside.
	out := usageOut(postgres.UsageRollup{
		InputTokens:       1000,
		CachedInputTokens: 5880,
		AttemptsReported:  1,
	})
	if out.CacheRatio == nil {
		t.Fatal("CacheRatio is nil, want computed (input+cached > 0)")
	}
	if *out.CacheRatio > 1.0 {
		t.Fatalf("CacheRatio = %v, want <= 1.0 — a cache hit rate above 100%% is not a fact", *out.CacheRatio)
	}
	want := 5880.0 / 6880.0
	if *out.CacheRatio < want-0.0001 || *out.CacheRatio > want+0.0001 {
		t.Fatalf("CacheRatio = %v, want ~%v (cached/(input+cached))", *out.CacheRatio, want)
	}
}

func TestUsageOutCacheRatioCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rollup   postgres.UsageRollup
		wantNil  bool
		wantJust float64
	}{
		{
			name:     "half the prompt was served from cache",
			rollup:   postgres.UsageRollup{InputTokens: 100, CachedInputTokens: 100, AttemptsReported: 1},
			wantJust: 0.5,
		},
		{
			name:     "a backend that reports no cache telemetry reads as 0% cached, not as unmeasurable",
			rollup:   postgres.UsageRollup{InputTokens: 200, AttemptsReported: 1},
			wantJust: 0.0,
		},
		{
			// A fully-resumed turn can report every prompt token as a cache
			// read and no uncached input at all. Gating on input_tokens > 0
			// (the pre-t8 rule) called that unmeasurable; it is measurably
			// 100% cached.
			name:     "an entirely cached prompt is 100%, not unmeasurable",
			rollup:   postgres.UsageRollup{InputTokens: 0, CachedInputTokens: 4096, AttemptsReported: 1},
			wantJust: 1.0,
		},
		{
			name:    "nothing in scope reported any prompt tokens at all — never a fabricated 0/0",
			rollup:  postgres.UsageRollup{AttemptsNotReported: 3},
			wantNil: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := usageOut(tc.rollup)
			if tc.wantNil {
				if out.CacheRatio != nil {
					t.Fatalf("CacheRatio = %v, want nil", *out.CacheRatio)
				}
				return
			}
			if out.CacheRatio == nil {
				t.Fatalf("CacheRatio is nil, want ~%v", tc.wantJust)
			}
			if *out.CacheRatio < tc.wantJust-0.0001 || *out.CacheRatio > tc.wantJust+0.0001 {
				t.Fatalf("CacheRatio = %v, want ~%v", *out.CacheRatio, tc.wantJust)
			}
			if *out.CacheRatio > 1.0 {
				t.Fatalf("CacheRatio = %v, want <= 1.0", *out.CacheRatio)
			}
		})
	}
}
