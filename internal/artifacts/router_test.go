package artifacts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
)

// fakeStore is an in-memory artifacts.Store used to test Router's routing
// and dispatch logic in isolation, without a real Postgres or MinIO
// backend. It mimics the one property Router's doc comment requires of the
// two Stores it composes: Put records a metadata row that later Stat/Get/
// Delete calls (on either fake, since the test wires both fakes to share
// one *sharedMetadata) can find regardless of which fake actually holds the
// bytes.
type fakeStore struct {
	name artifacts.Backend
	meta *sharedMetadata
	data map[artifacts.Ref][]byte
}

// sharedMetadata stands in for the real Postgres artifacts table: both
// fakeStore instances in a test write/read the same map, exactly as the
// real postgres and s3 drivers both write/read the same artifacts table.
type sharedMetadata struct {
	mu         sync.Mutex
	records    map[artifacts.Ref]artifacts.ArtifactMeta
	tombstones map[artifacts.Ref]artifacts.Tombstone
	nextID     int
}

func newSharedMetadata() *sharedMetadata {
	return &sharedMetadata{records: map[artifacts.Ref]artifacts.ArtifactMeta{}, tombstones: map[artifacts.Ref]artifacts.Tombstone{}}
}

func newFakeStore(name artifacts.Backend, meta *sharedMetadata) *fakeStore {
	return &fakeStore{name: name, meta: meta, data: map[artifacts.Ref][]byte{}}
}

func (f *fakeStore) Put(_ context.Context, meta artifacts.ArtifactMeta, r io.Reader) (artifacts.Ref, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	f.meta.mu.Lock()
	f.meta.nextID++
	id := fmt.Sprintf("id-%03d", f.meta.nextID)
	f.meta.mu.Unlock()

	ref := artifacts.NewRef(meta.NamespaceID, id)
	sum := sha256.Sum256(data)

	meta.SizeBytes = int64(len(data))
	meta.Digest = artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	meta.Backend = f.name

	f.data[ref] = data

	f.meta.mu.Lock()
	f.meta.records[ref] = meta
	f.meta.mu.Unlock()

	return ref, nil
}

func (f *fakeStore) Get(_ context.Context, ref artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	f.meta.mu.Lock()
	meta, ok := f.meta.records[ref]
	tombstone, reaped := f.meta.tombstones[ref]
	f.meta.mu.Unlock()
	if reaped {
		return nil, meta, &artifacts.ReapedError{Tombstone: tombstone}
	}
	if !ok || meta.Backend != f.name {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	data, ok := f.data[ref]
	if !ok {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), meta, nil
}

func (f *fakeStore) Stat(_ context.Context, ref artifacts.Ref) (artifacts.ArtifactMeta, error) {
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	meta, ok := f.meta.records[ref]
	if !ok {
		return artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	return meta, nil
}

func (f *fakeStore) Delete(_ context.Context, ref artifacts.Ref) error {
	return artifacts.ErrDeleteForbidden
}

func (f *fakeStore) Reap(_ context.Context, ref artifacts.Ref, reason string, reapedAt time.Time) (artifacts.Tombstone, error) {
	f.meta.mu.Lock()
	defer f.meta.mu.Unlock()
	meta, ok := f.meta.records[ref]
	if !ok || meta.Backend != f.name {
		return artifacts.Tombstone{}, artifacts.ErrNotFound
	}
	tombstone := artifacts.Tombstone{Ref: ref, ReapedAt: reapedAt, Reason: reason, Meta: meta}
	f.meta.tombstones[ref] = tombstone
	delete(f.data, ref)
	return tombstone, nil
}

func newRouterFixture() (router *artifacts.Router, small, object *fakeStore) {
	meta := newSharedMetadata()
	small = newFakeStore(artifacts.BackendPostgres, meta)
	object = newFakeStore(artifacts.BackendS3, meta)
	return artifacts.NewRouter(small, object, 8), small, object
}

func TestRouterPutRoutesUnderThresholdToSmall(t *testing.T) {
	router, small, object := newRouterFixture()
	ctx := context.Background()

	ref, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader([]byte("1234567"))) // 7 bytes < 8
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := small.data[ref]; !ok {
		t.Fatalf("ref %s not found in small store", ref)
	}
	if _, ok := object.data[ref]; ok {
		t.Fatalf("ref %s unexpectedly landed in object store", ref)
	}
}

func TestRouterPutRoutesAtThresholdToSmall(t *testing.T) {
	router, small, object := newRouterFixture()
	ctx := context.Background()

	ref, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader([]byte("12345678"))) // exactly 8 bytes == threshold
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := small.data[ref]; !ok {
		t.Fatalf("ref %s not found in small store", ref)
	}
	if _, ok := object.data[ref]; ok {
		t.Fatalf("ref %s unexpectedly landed in object store", ref)
	}
}

func TestRouterPutRoutesOverThresholdToObject(t *testing.T) {
	router, small, object := newRouterFixture()
	ctx := context.Background()

	content := []byte("123456789") // 9 bytes > 8 threshold
	ref, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := object.data[ref]
	if !ok {
		t.Fatalf("ref %s not found in object store", ref)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("object store content = %q, want %q (replay of the peeked prefix must be intact)", got, content)
	}
	if _, ok := small.data[ref]; ok {
		t.Fatalf("ref %s unexpectedly landed in small store", ref)
	}
}

func TestRouterPutLargePayloadPreservesAllBytes(t *testing.T) {
	router, _, object := newRouterFixture()
	ctx := context.Background()

	content := bytes.Repeat([]byte("artifact-router-test-"), 1000) // well over threshold
	ref, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !bytes.Equal(object.data[ref], content) {
		t.Fatalf("object store content length = %d, want %d, and/or content differs", len(object.data[ref]), len(content))
	}
}

func TestRouterPutEmptyPayloadRoutesToSmall(t *testing.T) {
	router, small, _ := newRouterFixture()
	ctx := context.Background()

	ref, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if data, ok := small.data[ref]; !ok || len(data) != 0 {
		t.Fatalf("small store data = %v, ok=%v, want empty and present", small.data[ref], ok)
	}
}

func TestRouterGetDispatchesToRecordedBackend(t *testing.T) {
	router, _, _ := newRouterFixture()
	ctx := context.Background()

	smallRef, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader([]byte("small")))
	if err != nil {
		t.Fatalf("Put small: %v", err)
	}
	largeRef, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(bytes.Repeat([]byte("x"), 100)))
	if err != nil {
		t.Fatalf("Put large: %v", err)
	}

	rc, meta, err := router.Get(ctx, smallRef)
	if err != nil {
		t.Fatalf("Get small: %v", err)
	}
	defer rc.Close()
	if got, _ := io.ReadAll(rc); string(got) != "small" {
		t.Fatalf("Get small content = %q, want %q", got, "small")
	}
	if meta.Backend != artifacts.BackendPostgres {
		t.Fatalf("Get small Backend = %q, want %q", meta.Backend, artifacts.BackendPostgres)
	}

	rc2, meta2, err := router.Get(ctx, largeRef)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	defer rc2.Close()
	got2, _ := io.ReadAll(rc2)
	if len(got2) != 100 {
		t.Fatalf("Get large content length = %d, want 100", len(got2))
	}
	if meta2.Backend != artifacts.BackendS3 {
		t.Fatalf("Get large Backend = %q, want %q", meta2.Backend, artifacts.BackendS3)
	}
}

func TestRouterStatIsBackendAgnostic(t *testing.T) {
	router, _, _ := newRouterFixture()
	ctx := context.Background()

	largeRef, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(bytes.Repeat([]byte("x"), 100)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := router.Stat(ctx, largeRef)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if meta.Backend != artifacts.BackendS3 {
		t.Fatalf("Stat Backend = %q, want %q", meta.Backend, artifacts.BackendS3)
	}
	if meta.SizeBytes != 100 {
		t.Fatalf("Stat SizeBytes = %d, want 100", meta.SizeBytes)
	}
}

func TestRouterReapLeavesResolvableTombstone(t *testing.T) {
	router, small, object := newRouterFixture()
	ctx := context.Background()

	smallRef, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader([]byte("s")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	largeRef, err := router.Put(ctx, artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader(bytes.Repeat([]byte("x"), 100)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	reapedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if _, err := router.Reap(ctx, smallRef, "retention/30-days", reapedAt); err != nil {
		t.Fatalf("Reap small: %v", err)
	}
	if _, ok := small.data[smallRef]; ok {
		t.Fatal("small store still has data after Reap")
	}

	if _, err := router.Reap(ctx, largeRef, "retention/30-days", reapedAt); err != nil {
		t.Fatalf("Reap large: %v", err)
	}
	if _, ok := object.data[largeRef]; ok {
		t.Fatal("object store still has data after Reap")
	}

	rc, meta, err := router.Get(ctx, smallRef)
	var reaped *artifacts.ReapedError
	if !errors.As(err, &reaped) {
		t.Fatalf("Get after reap = %v, want ReapedError", err)
	}
	if reaped.Tombstone.Ref != smallRef || reaped.Tombstone.Reason != "retention/30-days" || !reaped.Tombstone.ReapedAt.Equal(reapedAt) {
		t.Fatalf("tombstone = %#v", reaped.Tombstone)
	}

	// The digest is the load-bearing field: it is what lets someone who still
	// holds a copy prove it is the same bytes that were reaped. It lives on the
	// tombstone, and ONLY there.
	if reaped.Tombstone.Meta.Digest == "" {
		t.Fatalf("tombstone carries no digest: %#v", reaped.Tombstone.Meta)
	}

	// Get returns ZERO values beside the error, deliberately, and this assertion
	// is what keeps it that way. Handing back the reaped artifact's metadata as
	// a normal-looking second return would give a caller that ignores the error
	// a populated ArtifactMeta describing content that no longer exists -- the
	// exact "record points at something that is not there" failure this task
	// exists to prevent, reintroduced one layer up. A reader who wants to know
	// what is gone unwraps the ReapedError and says so.
	if rc != nil {
		t.Errorf("Get after reap returned a reader; there are no bytes to read")
	}
	if meta != (artifacts.ArtifactMeta{}) {
		t.Errorf("Get after reap returned metadata %#v beside its error; want the zero value, "+
			"so a caller cannot mistake a reaped artifact for a live one", meta)
	}
}

func TestRouterDeleteIsAlwaysRefused(t *testing.T) {
	router, _, _ := newRouterFixture()
	ref, err := router.Put(context.Background(), artifacts.ArtifactMeta{NamespaceID: "ns1"}, bytes.NewReader([]byte("kept")))
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Delete(context.Background(), ref); !errors.Is(err, artifacts.ErrDeleteForbidden) {
		t.Fatalf("Delete = %v, want ErrDeleteForbidden", err)
	}
}

func TestRouterGetUnknownRefReturnsNotFound(t *testing.T) {
	router, _, _ := newRouterFixture()
	ctx := context.Background()

	_, _, err := router.Get(ctx, artifacts.NewRef("ns1", "does-not-exist"))
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("Get unknown ref error = %v, want ErrNotFound", err)
	}
}
