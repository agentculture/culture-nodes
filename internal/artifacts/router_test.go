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
	mu      sync.Mutex
	records map[artifacts.Ref]artifacts.ArtifactMeta
	nextID  int
}

func newSharedMetadata() *sharedMetadata {
	return &sharedMetadata{records: map[artifacts.Ref]artifacts.ArtifactMeta{}}
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
	f.meta.mu.Unlock()
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
	f.meta.mu.Lock()
	meta, ok := f.meta.records[ref]
	if !ok || meta.Backend != f.name {
		f.meta.mu.Unlock()
		return artifacts.ErrNotFound
	}
	delete(f.meta.records, ref)
	f.meta.mu.Unlock()
	delete(f.data, ref)
	return nil
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

func TestRouterDeleteDispatchesToRecordedBackend(t *testing.T) {
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

	if err := router.Delete(ctx, smallRef); err != nil {
		t.Fatalf("Delete small: %v", err)
	}
	if _, ok := small.data[smallRef]; ok {
		t.Fatal("small store still has data after Delete")
	}

	if err := router.Delete(ctx, largeRef); err != nil {
		t.Fatalf("Delete large: %v", err)
	}
	if _, ok := object.data[largeRef]; ok {
		t.Fatal("object store still has data after Delete")
	}

	if _, err := router.Stat(ctx, smallRef); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("Stat after delete = %v, want ErrNotFound", err)
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
