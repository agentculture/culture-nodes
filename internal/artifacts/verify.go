package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// verifyingReadCloser wraps an io.ReadCloser, hashing bytes as they pass
// through Read and checking the accumulated size and digest against the
// values recorded at Put time -- so a Get "verifies size and (streamingly)
// digest" without ever buffering the whole artifact in memory to do it. The
// check runs the moment the underlying reader reports io.EOF: on mismatch,
// that final Read call returns the mismatch error instead of io.EOF, so a
// caller cannot observe a "successful" end of stream for corrupted content.
// A caller that Closes before reading to EOF never triggered the check at
// all -- Close reports whatever the underlying Close reports, plus the
// cached verification error if (and only if) one was already found.
type verifyingReadCloser struct {
	rc         io.ReadCloser
	hash       hash.Hash
	wantSize   int64
	gotSize    int64
	wantDigest string
	verified   bool
	verifyErr  error
}

// NewVerifyingReadCloser returns rc wrapped so that once its content has
// been read to completion, its total byte count and sha256 digest are
// checked against wantSize and wantDigest (a "sha256:<hex>" string as
// produced by DigestPrefix). Both driver subpackages use this so Get
// enforces the same guarantee regardless of backend.
func NewVerifyingReadCloser(rc io.ReadCloser, wantSize int64, wantDigest string) io.ReadCloser {
	return &verifyingReadCloser{rc: rc, hash: sha256.New(), wantSize: wantSize, wantDigest: wantDigest}
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
		v.gotSize += int64(n)
	}
	if err == io.EOF { //nolint:errorlint // io.Reader contract: exact sentinel, never wrapped
		if verr := v.verify(); verr != nil {
			return n, verr
		}
	}
	return n, err
}

func (v *verifyingReadCloser) Close() error {
	closeErr := v.rc.Close()
	if v.verified && v.verifyErr != nil {
		return v.verifyErr
	}
	return closeErr
}

// verify runs the size and digest check at most once; a caller that reads
// to EOF and then Closes sees the same cached result from both, not a
// second (and potentially differently-timed) check.
func (v *verifyingReadCloser) verify() error {
	if v.verified {
		return v.verifyErr
	}
	v.verified = true

	if v.gotSize != v.wantSize {
		v.verifyErr = fmt.Errorf("%w: read %d bytes, want %d", ErrSizeMismatch, v.gotSize, v.wantSize)
		return v.verifyErr
	}

	got := DigestPrefix + hex.EncodeToString(v.hash.Sum(nil))
	if got != v.wantDigest {
		v.verifyErr = fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, v.wantDigest)
		return v.verifyErr
	}

	return nil
}
