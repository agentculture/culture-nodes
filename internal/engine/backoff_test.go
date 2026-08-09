package engine

import (
	"testing"
	"time"
)

func TestBackoffFollowsThePolicy(t *testing.T) {
	e := &Engine{retryBase: time.Second, retryMax: 30 * time.Second}

	cases := []struct {
		backoff string
		attempt int
		want    time.Duration
	}{
		// `none` is the compiler's default for a single-attempt policy: a
		// retry that was never going to happen needs no delay.
		{"none", 2, 0},
		{"", 2, 0},
		{"fixed", 2, time.Second},
		{"fixed", 5, time.Second},
		{"linear", 2, time.Second},
		{"linear", 4, 3 * time.Second},
		{"exponential", 2, time.Second},
		{"exponential", 3, 2 * time.Second},
		{"exponential", 4, 4 * time.Second},
		// The cap keeps a long retry budget from scheduling its last attempt
		// beyond any useful horizon.
		{"exponential", 12, 30 * time.Second},
	}

	for _, tc := range cases {
		got := e.backoff(RetryPolicy{Backoff: tc.backoff}, tc.attempt)
		if got != tc.want {
			t.Errorf("backoff(%q, attempt %d) = %s, want %s", tc.backoff, tc.attempt, got, tc.want)
		}
	}
}

// A first attempt has no backoff to compute; asking for one is a caller
// mistake that must not produce a negative shift.
func TestBackoffTreatsAFirstAttemptAsTheSecond(t *testing.T) {
	e := &Engine{retryBase: time.Second, retryMax: time.Minute}
	if got := e.backoff(RetryPolicy{Backoff: "exponential"}, 0); got != time.Second {
		t.Errorf("backoff for attempt 0 = %s, want %s", got, time.Second)
	}
}
