package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// parseLimit reads the "limit" query parameter, clamped to [1, max]. An
// absent or unparsable value falls back to def rather than erroring: a list
// endpoint's page size is a hint, not a contract a caller must get exactly
// right.
func parseLimit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// parseRFC3339 reads query parameter name as an RFC3339 timestamp, used by
// GET /v1alpha1/runs and GET /v1alpha1/node-runs' updated_since/
// updated_until (task t11). An absent parameter returns (nil, nil) — "no
// filter" — deliberately unlike parseLimit's silent fallback for a garbled
// value: a time filter that silently ignored a typo would return an
// unfiltered page while the caller believes it is filtered (a correctness
// bug the honesty condition h5 this task covers exists precisely to rule
// out), so a present-but-malformed value is refused with 400 instead.
func parseRFC3339(r *http.Request, name string) (*time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, badRequest(
			fmt.Sprintf("%s must be an RFC3339 timestamp, e.g. 2026-08-09T12:00:00Z", name),
			"parse %s=%q: %v", name, raw, err)
	}
	return &t, nil
}
