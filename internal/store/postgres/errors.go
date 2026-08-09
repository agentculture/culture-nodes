package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique constraint
// violation (23505). See https://www.postgresql.org/docs/current/errcodes-appendix.html.
const pgUniqueViolation = "23505"

// ErrDuplicateDigest is returned by CreateWorkflowVersion when a workflow
// version with the same namespace_id + content_digest already exists.
// workflow_versions.content_digest is unique per namespace by design
// (docs/initial-design/culture-nodes-prd-spec.md §11.3: an identical
// definition always resolves to the same immutable version) -- this is an
// expected outcome, not a database malfunction. Callers that need the
// existing row should follow up with GetWorkflowVersion.
var ErrDuplicateDigest = errors.New("postgres: workflow version with this content digest already exists")

// ErrNotFound is returned by lookup methods when no row matches.
var ErrNotFound = errors.New("postgres: not found")

// isUniqueViolation reports whether err is a PostgreSQL unique constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// uniqueViolationConstraint returns the violated constraint name for a
// unique violation error, or "" if err is not a unique violation.
func uniqueViolationConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName
	}
	return ""
}

// isNoRows reports whether err is pgx's "no rows in result set" sentinel,
// returned by QueryRow when a SELECT matches nothing.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// isDigestConstraint reports whether the given constraint name is the
// workflow_versions digest-uniqueness constraint, as opposed to the
// (namespace, workflow_key, version) constraint on the same table.
func isDigestConstraint(constraint string) bool {
	return strings.Contains(constraint, "digest")
}
