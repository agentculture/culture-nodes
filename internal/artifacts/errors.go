package artifacts

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound is returned by Get, Stat, and Delete when ref does not
	// resolve to an artifact -- either no metadata row exists for it at
	// all, or (when a driver is used directly rather than through Router)
	// the ref's recorded Backend is not the one this particular Store
	// implements.
	ErrNotFound = errors.New("artifacts: not found")

	// ErrTooLarge is returned by a Store.Put whose payload exceeds that
	// store's configured size cap -- currently only
	// internal/artifacts/postgres, whose small-artifact cap defaults to
	// 1 MiB (postgres.DefaultCapBytes). The caller should use the object
	// (S3/MinIO) driver instead, or let Router make that choice
	// automatically based on payload size.
	ErrTooLarge = errors.New("artifacts: payload exceeds the configured size cap; use the object (S3/MinIO) driver")

	// ErrSizeMismatch is returned while reading a Get result when the
	// bytes actually streamed back do not total the size recorded for ref
	// at Put time.
	ErrSizeMismatch = errors.New("artifacts: size mismatch between stored content and recorded metadata")

	// ErrDigestMismatch is returned while reading a Get result when the
	// sha256 digest of the bytes actually streamed back does not match the
	// digest recorded for ref at Put time -- the strongest signal this
	// package has that stored content was corrupted or tampered with after
	// Put.
	ErrDigestMismatch = errors.New("artifacts: digest mismatch between stored content and recorded metadata")

	// ErrInvalidRef is returned by ParseRef (and therefore by any Store
	// method that calls it) when a ref is not a well-formed
	// "artifact://<namespace>/<id>" URI.
	ErrInvalidRef      = errors.New("artifacts: invalid ref")
	ErrDeleteForbidden = errors.New("artifacts: raw delete forbidden; use retention Reap with a reason")
	ErrReaped          = errors.New("artifacts: content reaped")
)

// ReapedError carries the explicit, durable resolution for a reaped Ref.
type ReapedError struct{ Tombstone Tombstone }

func (e *ReapedError) Error() string {
	return fmt.Sprintf("%s at %s by %s", ErrReaped, e.Tombstone.ReapedAt.UTC().Format("2006-01-02T15:04:05Z"), e.Tombstone.Reason)
}

func (e *ReapedError) Unwrap() error { return ErrReaped }
