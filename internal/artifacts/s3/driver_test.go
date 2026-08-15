package s3_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	artifacts3 "github.com/agentculture/culture-nodes/internal/artifacts/s3"
	"github.com/agentculture/culture-nodes/internal/store"
)

func testBucket(t *testing.T) string {
	t.Helper()
	// S3 bucket names must be lowercase; store.NewULID() is uppercase
	// Crockford base32 (alnum only), so lowercasing it is always a valid
	// bucket-name suffix.
	return "nodes-artifacts-test-" + strings.ToLower(store.NewULID())
}

func newDriver(t *testing.T) *artifacts3.Driver {
	t.Helper()
	s := requireBackends(t)
	ctx := context.Background()

	d, err := artifacts3.New(ctx, artifacts3.Config{
		Endpoint:  testMinIOEndpoint,
		AccessKey: testMinIOAccessKey,
		SecretKey: testMinIOSecretKey,
		Bucket:    testBucket(t),
		UseTLS:    false,
	}, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestPutGetRoundTrip(t *testing.T) {
	s := requireBackends(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-s3-roundtrip")

	d := newDriver(t)
	content := bytes.Repeat([]byte("artifact-s3-roundtrip-"), 500) // a few KB, to exercise the streaming path

	ref, err := d.Put(ctx, artifacts.ArtifactMeta{
		NamespaceID: ns.ID,
		Name:        "big-report.txt",
		MediaType:   "text/plain",
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, meta, err := d.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content length = %d, want %d (and/or content differs)", len(got), len(content))
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if meta.Backend != artifacts.BackendS3 {
		t.Errorf("Backend = %q, want %q", meta.Backend, artifacts.BackendS3)
	}
	if meta.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", meta.SizeBytes, len(content))
	}
	if meta.Name != "big-report.txt" {
		t.Errorf("Name = %q, want %q", meta.Name, "big-report.txt")
	}
	sum := sha256.Sum256(content)
	wantDigest := artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	if meta.Digest != wantDigest {
		t.Errorf("Digest = %q, want %q", meta.Digest, wantDigest)
	}

	statMeta, err := d.Stat(ctx, ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if statMeta.Digest != meta.Digest || statMeta.SizeBytes != meta.SizeBytes {
		t.Fatalf("Stat = %+v, want it to match Get's meta %+v", statMeta, meta)
	}
}

func TestPutSmallPayload(t *testing.T) {
	s := requireBackends(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-s3-small")

	d := newDriver(t)
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader([]byte("tiny")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, _, err := d.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "tiny" {
		t.Fatalf("content = %q, want %q", got, "tiny")
	}
}

func TestGetUnknownRefReturnsNotFound(t *testing.T) {
	s := requireBackends(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-s3-notfound")

	d := newDriver(t)
	_, _, err := d.Get(ctx, artifacts.NewRef(ns.ID, "does-not-exist"))
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestReapRemovesObjectAndLeavesTombstone(t *testing.T) {
	s := requireBackends(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-s3-delete")

	d := newDriver(t)
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader([]byte("delete me")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := d.Reap(ctx, ref, "retention/30-days", time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if _, _, err := d.Get(ctx, ref); !errors.Is(err, artifacts.ErrReaped) {
		t.Fatalf("Get after Reap error = %v, want ErrReaped", err)
	}
	if err := d.Delete(ctx, ref); !errors.Is(err, artifacts.ErrDeleteForbidden) {
		t.Fatalf("Delete error = %v, want ErrDeleteForbidden", err)
	}
}

// TestDigestMismatchDetected proves Get fails loudly rather than handing
// back bytes when bucket content has been altered out from under the
// metadata row that describes it: a second, independent minio.Client --
// standing in for any other actor with direct bucket access, not going
// through this package's Driver at all -- overwrites the object after Put,
// and Get must surface that as ErrDigestMismatch, not silently return the
// tampered bytes.
func TestDigestMismatchDetected(t *testing.T) {
	s := requireBackends(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-s3-corrupt")

	bucket := testBucket(t)
	d, err := artifacts3.New(ctx, artifacts3.Config{
		Endpoint:  testMinIOEndpoint,
		AccessKey: testMinIOAccessKey,
		SecretKey: testMinIOSecretKey,
		Bucket:    bucket,
		UseTLS:    false,
	}, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	original := []byte("pristine object content")
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader(original))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	namespaceID, id, err := artifacts.ParseRef(ref)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}

	// A second, independent client -- not this test's Driver -- reaches
	// into the bucket directly and overwrites the object with
	// same-length-but-different content, so this exercises the digest
	// check specifically rather than the size check.
	rawClient, err := minio.New(testMinIOEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(testMinIOAccessKey, testMinIOSecretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("second minio client: %v", err)
	}
	// Flip bytes in a copy rather than hand-writing a same-length literal,
	// so "same length, different content" is true by construction instead
	// of by manual character counting.
	corrupted := append([]byte(nil), original...)
	for i := range corrupted {
		corrupted[i] ^= 0xFF
	}
	key := namespaceID + "/" + id
	if _, err := rawClient.PutObject(ctx, bucket, key, bytes.NewReader(corrupted), int64(len(corrupted)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("corrupt object directly: %v", err)
	}

	rc, _, err := d.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	_, readErr := io.ReadAll(rc)
	if !errors.Is(readErr, artifacts.ErrDigestMismatch) {
		t.Fatalf("read error = %v, want ErrDigestMismatch (Get must fail loudly on corrupted content)", readErr)
	}
}
