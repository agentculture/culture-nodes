package api

import (
	"net/http"
	"strconv"
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
