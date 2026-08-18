package headspace_test

// The stdout-capture tests (issue #189): a green code-node run must not
// discard what the process printed. When BridgeConfig carries an
// artifacts.Store, the bridge stores the run's captured-output excerpt as a
// durable artifact tied to the operation's attempt (ArtifactMeta.AttemptID)
// and records the returned ref on Result.Artifacts.StdoutRef -- so a reader
// can distinguish a process that printed {"emitted": 0} from one that
// printed {"emitted": 7}, and both from a process that printed nothing.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
)

// memStore is an in-memory artifacts.Store test double, following the shape
// of internal/artifacts/router_test.go's fakeStore: Put records bytes and
// metadata under a fresh Ref, Get streams them back, and Delete fails closed
// exactly like the real drivers.
type memStore struct {
	mu     sync.Mutex
	seq    int
	data   map[artifacts.Ref][]byte
	metas  map[artifacts.Ref]artifacts.ArtifactMeta
	putErr error
}

func newMemStore() *memStore {
	return &memStore{
		data:  map[artifacts.Ref][]byte{},
		metas: map[artifacts.Ref]artifacts.ArtifactMeta{},
	}
}

func (m *memStore) Put(_ context.Context, meta artifacts.ArtifactMeta, r io.Reader) (artifacts.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putErr != nil {
		return "", m.putErr
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	m.seq++
	ref := artifacts.NewRef(meta.NamespaceID, fmt.Sprintf("mem%06d", m.seq))
	sum := sha256.Sum256(content)
	meta.SizeBytes = int64(len(content))
	meta.Digest = artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	meta.Backend = artifacts.BackendPostgres
	meta.CreatedAt = time.Now().UTC()
	m.data[ref] = content
	m.metas[ref] = meta
	return ref, nil
}

func (m *memStore) Get(_ context.Context, ref artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	content, ok := m.data[ref]
	if !ok {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(content)), m.metas[ref], nil
}

func (m *memStore) Stat(_ context.Context, ref artifacts.Ref) (artifacts.ArtifactMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	meta, ok := m.metas[ref]
	if !ok {
		return artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	return meta, nil
}

func (m *memStore) Delete(context.Context, artifacts.Ref) error {
	return artifacts.ErrDeleteForbidden
}

func (m *memStore) Reap(context.Context, artifacts.Ref, string, time.Time) (artifacts.Tombstone, error) {
	return artifacts.Tombstone{}, errors.New("memStore: Reap not implemented")
}

var _ artifacts.Store = (*memStore)(nil)

// storeBridge builds a test bridge whose BridgeConfig carries store as its
// artifact store, under a fixed test namespace.
func storeBridge(t *testing.T, store artifacts.Store) *headspace.Bridge {
	t.Helper()
	return newTestBridge(t, func(cfg *headspace.BridgeConfig) {
		cfg.ArtifactStore = store
		cfg.ArtifactNamespace = "ns-test"
	})
}

// contextualizedOperation is baseOperation plus the run/node-run/attempt
// identity a real dispatch always carries.
func contextualizedOperation(t *testing.T, b *headspace.Bridge, argv []string) runners.Operation {
	t.Helper()
	op := baseOperation(t, b, argv)
	op.Context = &runners.Context{RunID: "run-1", NodeRunID: "nr-1", AttemptID: "att-1"}
	return op
}

// readStored fetches ref's content and metadata back out of store.
func readStored(t *testing.T, store artifacts.Store, ref string) ([]byte, artifacts.ArtifactMeta) {
	t.Helper()
	rc, meta, err := store.Get(context.Background(), artifacts.Ref(ref))
	if err != nil {
		t.Fatalf("Get(%s): %v", ref, err)
	}
	defer rc.Close() //nolint:errcheck
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stored artifact %s: %v", ref, err)
	}
	return content, meta
}

// TestExecute_CapturedStdout_StoredAsDurableAttemptArtifact is the issue #189
// case itself: a green run whose process printed {"emitted": 3} yields a
// Result whose Artifacts.StdoutRef resolves, through the store, to exactly
// that content, with the artifact's metadata tied to the dispatch attempt.
func TestExecute_CapturedStdout_StoredAsDurableAttemptArtifact(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	t.Setenv("NODES_FAKE_RUN_EXCERPT_JSON", `{\"sweep\": \"pr-upkeep\", \"emitted\": 3}\n`)

	store := newMemStore()
	b := storeBridge(t, store)
	op := contextualizedOperation(t, b, []string{"python3", "sweep.py"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.State != runners.StateCompleted {
		t.Fatalf("State = %s, want completed", result.State)
	}
	if result.Artifacts == nil || result.Artifacts.StdoutRef == "" {
		t.Fatalf("expected Artifacts.StdoutRef to carry the stored stdout ref, got: %+v", result.Artifacts)
	}

	content, meta := readStored(t, store, result.Artifacts.StdoutRef)
	want := "{\"sweep\": \"pr-upkeep\", \"emitted\": 3}\n"
	if string(content) != want {
		t.Fatalf("stored stdout = %q, want %q", content, want)
	}
	if meta.AttemptID != "att-1" {
		t.Fatalf("stored artifact AttemptID = %q, want %q -- the artifact must be tied to the attempt", meta.AttemptID, "att-1")
	}
	if meta.RunID != "run-1" {
		t.Fatalf("stored artifact RunID = %q, want %q", meta.RunID, "run-1")
	}
	if meta.Name != "stdout" {
		t.Fatalf("stored artifact Name = %q, want %q", meta.Name, "stdout")
	}

	obs, ok := result.Observations.Get("stdout_artifact")
	if !ok {
		t.Fatal("expected a stdout_artifact observation on the result")
	}
	if !obs.Measured || !obs.Complete {
		t.Fatalf("stdout_artifact should be measured+complete for an untruncated stored capture: %+v", obs)
	}
	if !strings.Contains(obs.Scope+obs.Note, result.Artifacts.StdoutRef) {
		t.Fatalf("stdout_artifact observation should name the stored ref %s: %+v", result.Artifacts.StdoutRef, obs)
	}
}

// TestExecute_EmptyStdout_StoredDistinguishablyEmpty: a process that printed
// nothing still yields a resolvable StdoutRef whose content is zero bytes --
// so "printed nothing" is a durable, queryable fact, distinct both from
// {"emitted": 0} (nonempty content) and from "no capture happened at all"
// (no ref).
func TestExecute_EmptyStdout_StoredDistinguishablyEmpty(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	t.Setenv("NODES_FAKE_RUN_EXCERPT_JSON", "")

	store := newMemStore()
	b := storeBridge(t, store)
	op := contextualizedOperation(t, b, []string{"python3", "-c", "pass"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Artifacts == nil || result.Artifacts.StdoutRef == "" {
		t.Fatalf("expected a StdoutRef even for an empty capture, got: %+v", result.Artifacts)
	}
	content, meta := readStored(t, store, result.Artifacts.StdoutRef)
	if len(content) != 0 {
		t.Fatalf("stored stdout should be empty, got %d bytes: %q", len(content), content)
	}
	if meta.SizeBytes != 0 {
		t.Fatalf("stored artifact SizeBytes = %d, want 0", meta.SizeBytes)
	}
}

// TestExecute_StdoutArtifact_CappedAt64KiB: an oversize capture is stored
// capped at 64 KiB, and the observation says the stored copy is incomplete
// rather than pretending otherwise.
func TestExecute_StdoutArtifact_CappedAt64KiB(t *testing.T) {
	const capBytes = 64 << 10
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	t.Setenv("NODES_FAKE_RUN_EXCERPT_JSON", strings.Repeat("a", capBytes+4096))

	store := newMemStore()
	b := storeBridge(t, store)
	op := contextualizedOperation(t, b, []string{"python3", "noisy.py"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Artifacts == nil || result.Artifacts.StdoutRef == "" {
		t.Fatalf("expected a StdoutRef for the capped capture, got: %+v", result.Artifacts)
	}
	content, _ := readStored(t, store, result.Artifacts.StdoutRef)
	if len(content) != capBytes {
		t.Fatalf("stored stdout = %d bytes, want exactly the %d byte cap", len(content), capBytes)
	}

	obs, ok := result.Observations.Get("stdout_artifact")
	if !ok {
		t.Fatal("expected a stdout_artifact observation on the result")
	}
	if !obs.Measured || obs.Complete {
		t.Fatalf("a capped capture must be measured but NOT complete: %+v", obs)
	}
}

// TestExecute_StdoutStoreFailure_DoesNotFailExecute: the run genuinely
// happened, so a failing store write must not turn the Result into a
// dispatch failure -- it is recorded honestly on the observation instead,
// exactly the posture exportDeclared already takes for export failures.
func TestExecute_StdoutStoreFailure_DoesNotFailExecute(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")

	store := newMemStore()
	store.putErr = errors.New("volume full")
	b := storeBridge(t, store)
	op := contextualizedOperation(t, b, []string{"python3", "-c", "print(1)"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute should not fail on a store write failure: %v", err)
	}
	if result.Artifacts != nil && result.Artifacts.StdoutRef != "" {
		t.Fatalf("StdoutRef must stay empty when the store write failed, got %q", result.Artifacts.StdoutRef)
	}
	obs, ok := result.Observations.Get("stdout_artifact")
	if !ok {
		t.Fatal("expected a stdout_artifact observation recording the store failure")
	}
	if obs.Measured || obs.Complete {
		t.Fatalf("a failed store write must not claim a measured, complete stored capture: %+v", obs)
	}
	if !strings.Contains(obs.Note, "volume full") {
		t.Fatalf("observation note should quote the store failure, got: %q", obs.Note)
	}
}

// TestExecute_NoArtifactStore_NoStdoutRef pins the nil-store default: a
// bridge with no store behaves exactly as before -- no ref, no
// stdout_artifact observation -- rather than half-claiming durability.
func TestExecute_NoArtifactStore_NoStdoutRef(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	b := newTestBridge(t, nil)
	op := contextualizedOperation(t, b, []string{"python3", "-c", "print(1)"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Artifacts != nil && result.Artifacts.StdoutRef != "" {
		t.Fatalf("a bridge with no store must not fabricate a StdoutRef, got %q", result.Artifacts.StdoutRef)
	}
	if _, ok := result.Observations.Get("stdout_artifact"); ok {
		t.Fatal("a bridge with no store must not emit a stdout_artifact observation")
	}
}

// TestNew_ArtifactStoreRequiresNamespace: ArtifactMeta.NamespaceID is
// required by the Store contract, so a store with no namespace is a
// misconfiguration worth failing at startup, mirroring the Profile check.
func TestNew_ArtifactStoreRequiresNamespace(t *testing.T) {
	_, err := headspace.New(headspace.BridgeConfig{
		Profile:       map[string]string{fakeDigest: headspace.DefaultProfilePython312},
		ArtifactStore: newMemStore(),
	})
	if err == nil {
		t.Fatal("expected New to refuse an ArtifactStore with no ArtifactNamespace")
	}
	if !strings.Contains(err.Error(), "ArtifactNamespace") {
		t.Fatalf("refusal should name ArtifactNamespace, got: %v", err)
	}
}
