package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	artifactpg "github.com/agentculture/culture-nodes/internal/artifacts/postgres"
)

func TestPutGetRoundTrip(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-roundtrip")

	d := artifactpg.New(s, artifactpg.DefaultCapBytes)
	content := []byte(`{"result":"ok","detail":"round trip test content"}`)

	ref, err := d.Put(ctx, artifacts.ArtifactMeta{
		NamespaceID: ns.ID,
		Name:        "result.json",
		MediaType:   "application/json",
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref == "" {
		t.Fatal("Put returned an empty ref")
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
		t.Fatalf("content = %q, want %q", got, content)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if meta.NamespaceID != ns.ID {
		t.Errorf("NamespaceID = %q, want %q", meta.NamespaceID, ns.ID)
	}
	if meta.Name != "result.json" {
		t.Errorf("Name = %q, want %q", meta.Name, "result.json")
	}
	if meta.MediaType != "application/json" {
		t.Errorf("MediaType = %q, want %q", meta.MediaType, "application/json")
	}
	if meta.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", meta.SizeBytes, len(content))
	}
	if meta.Backend != artifacts.BackendPostgres {
		t.Errorf("Backend = %q, want %q", meta.Backend, artifacts.BackendPostgres)
	}
	sum := sha256.Sum256(content)
	wantDigest := artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	if meta.Digest != wantDigest {
		t.Errorf("Digest = %q, want %q", meta.Digest, wantDigest)
	}
	if meta.CreatedAt.IsZero() {
		t.Error("CreatedAt was not set")
	}

	// Stat must agree with what Get reported, without reading content.
	statMeta, err := d.Stat(ctx, ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if statMeta.Digest != meta.Digest || statMeta.SizeBytes != meta.SizeBytes {
		t.Fatalf("Stat = %+v, want it to match Get's meta %+v", statMeta, meta)
	}
}

func TestPutWithRunAndAttempt(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-run")

	d := artifactpg.New(s, artifactpg.DefaultCapBytes)

	// RunID/AttemptID are foreign keys (runs.id / attempts.id); this repo's
	// runtime-execution tables are populated by later tasks (t7+), so this
	// test exercises the documented NULL path -- an artifact not (yet)
	// associated with a specific run or attempt -- which InsertArtifact
	// must accept.
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader([]byte("no run/attempt")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := d.Stat(ctx, ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if meta.RunID != "" {
		t.Errorf("RunID = %q, want empty (NULL)", meta.RunID)
	}
	if meta.AttemptID != "" {
		t.Errorf("AttemptID = %q, want empty (NULL)", meta.AttemptID)
	}
}

func TestPutExceedsCapReturnsErrTooLarge(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-cap")

	d := artifactpg.New(s, 16) // tiny cap so the test doesn't need a huge payload

	payload := bytes.Repeat([]byte("x"), 100)
	_, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader(payload))
	if !errors.Is(err, artifacts.ErrTooLarge) {
		t.Fatalf("Put error = %v, want it to wrap artifacts.ErrTooLarge", err)
	}
}

func TestPutAtExactCapSucceeds(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-cap-exact")

	d := artifactpg.New(s, 16)

	payload := bytes.Repeat([]byte("y"), 16) // exactly at the cap, must be accepted
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put at exact cap: %v", err)
	}
	if ref == "" {
		t.Fatal("Put returned an empty ref")
	}
}

func TestDefaultCapIsOneMiB(t *testing.T) {
	if artifactpg.DefaultCapBytes != 1<<20 {
		t.Fatalf("DefaultCapBytes = %d, want %d (1 MiB)", artifactpg.DefaultCapBytes, int64(1<<20))
	}
}

func TestGetUnknownRefReturnsNotFound(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-notfound")

	d := artifactpg.New(s, artifactpg.DefaultCapBytes)

	_, _, err := d.Get(ctx, artifacts.NewRef(ns.ID, "does-not-exist"))
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestReapRemovesBlobAndLeavesTombstone(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-delete")

	d := artifactpg.New(s, artifactpg.DefaultCapBytes)
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID, Name: "result.txt", MediaType: "text/plain"}, bytes.NewReader([]byte("to be deleted")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	tombstone, err := d.Reap(ctx, ref, "retention/30-days", time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if tombstone.Meta.Digest == "" || tombstone.Meta.Name != "result.txt" || tombstone.Meta.MediaType != "text/plain" || tombstone.Meta.SizeBytes != 13 {
		t.Fatalf("tombstone metadata = %#v", tombstone.Meta)
	}
	if _, _, err := d.Get(ctx, ref); !errors.Is(err, artifacts.ErrReaped) {
		t.Fatalf("Get after Reap error = %v, want ErrReaped", err)
	}
	if _, err := d.Stat(ctx, ref); err != nil {
		t.Fatalf("Stat after Reap = %v", err)
	}

	// The ON DELETE CASCADE on artifact_blobs.id (migrations/0005) must
	// have removed the blob row too, not just the metadata row.
	_, id, err := artifacts.ParseRef(ref)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	var count int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM artifact_blobs WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count artifact_blobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("artifact_blobs row count = %d, want 0 (cascade delete should have removed it)", count)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE artifact_tombstones SET reason='rewritten' WHERE id=$1`, tombstone.ID); err == nil {
		t.Fatal("artifact tombstone UPDATE succeeded, want immutable-row rejection")
	}

	if err := d.Delete(ctx, ref); !errors.Is(err, artifacts.ErrDeleteForbidden) {
		t.Fatalf("Delete error = %v, want ErrDeleteForbidden", err)
	}
}

func TestGetDetectsCorruptedBlob(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-artifacts-pg-corrupt")

	d := artifactpg.New(s, artifactpg.DefaultCapBytes)
	ref, err := d.Put(ctx, artifacts.ArtifactMeta{NamespaceID: ns.ID}, bytes.NewReader([]byte("pristine content")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Corrupt the stored bytes directly, bypassing the driver entirely --
	// simulates on-disk corruption or a rogue write, not anything the
	// driver itself would ever do. The replacement is deliberately the
	// same length as the original ("pristine content", 16 bytes) so this
	// exercises the digest check specifically, not the size check.
	_, id, err := artifacts.ParseRef(ref)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE artifact_blobs SET data = $1 WHERE id = $2`, []byte("tampered content"), id); err != nil {
		t.Fatalf("corrupt blob: %v", err)
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
