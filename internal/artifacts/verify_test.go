package artifacts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts"
)

func digestOf(t *testing.T, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	return artifacts.DigestPrefix + hex.EncodeToString(sum[:])
}

func TestVerifyingReadCloserPassesMatchingContent(t *testing.T) {
	content := []byte("the quick brown fox jumps over the lazy dog")
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), int64(len(content)), digestOf(t, content))

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("read %q, want %q", got, content)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestVerifyingReadCloserDetectsSizeMismatch(t *testing.T) {
	content := []byte("short")
	// Claim a larger size than the content actually is.
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), int64(len(content))+1, digestOf(t, content))

	_, err := io.ReadAll(rc)
	if !errors.Is(err, artifacts.ErrSizeMismatch) {
		t.Fatalf("ReadAll error = %v, want ErrSizeMismatch", err)
	}
}

func TestVerifyingReadCloserDetectsDigestMismatch(t *testing.T) {
	content := []byte("original content")
	wrongDigest := digestOf(t, []byte("different content, same length!"))
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), int64(len(content)), wrongDigest)

	_, err := io.ReadAll(rc)
	if !errors.Is(err, artifacts.ErrDigestMismatch) {
		t.Fatalf("ReadAll error = %v, want ErrDigestMismatch", err)
	}
}

func TestVerifyingReadCloserCachesVerificationOnClose(t *testing.T) {
	content := []byte("short")
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), int64(len(content))+1, digestOf(t, content))

	_, readErr := io.ReadAll(rc)
	if !errors.Is(readErr, artifacts.ErrSizeMismatch) {
		t.Fatalf("ReadAll error = %v, want ErrSizeMismatch", readErr)
	}

	closeErr := rc.Close()
	if !errors.Is(closeErr, artifacts.ErrSizeMismatch) {
		t.Fatalf("Close error = %v, want the cached ErrSizeMismatch", closeErr)
	}
}

func TestVerifyingReadCloserEarlyCloseDoesNotFalselyFail(t *testing.T) {
	content := []byte("a much longer body than the caller ends up reading")
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(content)), int64(len(content)), digestOf(t, content))

	buf := make([]byte, 4)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Close before reaching EOF: verification never ran, so Close must not
	// report a spurious mismatch just because fewer bytes were read than
	// wantSize.
	if err := rc.Close(); err != nil {
		t.Fatalf("Close on an early-closed reader = %v, want nil (no verification claim was made)", err)
	}
}
