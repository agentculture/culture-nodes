package runnerservice

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// Record is one operation's status as this service holds it.
//
// It is deliberately not the operation document. What is kept is what the
// status endpoint must be able to answer with, plus the few digests a
// recovery result needs to be schema-valid — and nothing else. In particular
// the callback token is NOT here: it is caller-issued bearer material, it
// lives in memory for the lifetime of the operation, and it is never written
// to disk. A callback is best-effort by contract, so losing one across a
// restart costs latency and nothing more.
type Record struct {
	OperationID string          `json:"operation_id"`
	State       runners.State   `json:"state"`
	Result      *runners.Result `json:"result,omitempty"`
	// DocumentDigest is the canonical digest of the operation document this
	// id was accepted with. It is what makes "same key, different document" a
	// 409 rather than a silently different execution.
	DocumentDigest string             `json:"document_digest"`
	Acceptance     runners.Acceptance `json:"acceptance"`
	// Replay carries the pins a result must report. They are facts about the
	// operation document, not measurements of an execution, which is why a
	// recovery result may state them without claiming to have observed
	// anything.
	Replay     Replay     `json:"replay"`
	AcceptedAt time.Time  `json:"accepted_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// ExpiresAt is when the declared retention elapses. It is set only once
	// the operation is terminal: retention is a promise about how long a
	// finished operation's status stays readable, and an operation still
	// running has not started counting.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Replay holds the digests a runner result must report for the operation to
// stay replayable.
type Replay struct {
	RunnerRevision string  `json:"runner_revision"`
	ImageDigest    string  `json:"image_digest"`
	PolicyDigest   string  `json:"policy_digest"`
	InputDigest    *string `json:"input_digest,omitempty"`
}

// Store holds per-operation status for at least the declared retention.
//
// Durable reports whether the implementation keeps a status across a process
// restart. It is on the interface rather than in a comment because the
// protocol's retention promise is exactly the thing a store either keeps or
// does not, and a deployment should be able to ask.
type Store interface {
	Put(record Record) error
	Get(operationID string) (Record, bool, error)
	List() ([]Record, error)
	Delete(operationID string) error
	Durable() bool
}

// MemoryStore keeps status in memory only.
//
// It is honest about what that means: Durable reports false, because a
// restart forgets every operation this store holds, and the protocol's
// retention promise ("never let an operation's status disappear before that
// retention elapses") cannot be kept across one. It exists for tests and for
// a deployment that has explicitly accepted that limit.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

// NewMemoryStore returns an in-memory, non-durable status store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]Record{}}
}

// Put writes a record, replacing any earlier version of it.
func (m *MemoryStore) Put(record Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[record.OperationID] = record
	return nil
}

// Get returns a record and whether it exists.
func (m *MemoryStore) Get(operationID string) (Record, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.records[operationID]
	return record, ok, nil
}

// List returns every held record, ordered by operation id for determinism.
func (m *MemoryStore) List() ([]Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedRecords(m.records), nil
}

// Delete removes a record. Deleting an absent record is not an error: a sweep
// racing a restart should not fail for tidying something already tidy.
func (m *MemoryStore) Delete(operationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, operationID)
	return nil
}

// Durable reports false. See the type's doc comment.
func (m *MemoryStore) Durable() bool { return false }

// FileStore keeps each operation's status in its own JSON file under a state
// directory, written atomically and fsynced, with an in-memory index in front
// of it so a status read never touches the disk.
//
// This is the store that keeps the protocol's retention promise across a
// process restart. What it does NOT survive is the loss of the directory
// itself: an operator who points it at a tmpfs or an ephemeral container
// filesystem has a store that is durable for exactly as long as that
// filesystem is, and the promise inherits that limit.
type FileStore struct {
	dir string

	mu      sync.RWMutex
	records map[string]Record
}

// stateFileSuffix is the extension every record file carries. Files without
// it are left alone, so a state directory can hold an operator's own notes.
const stateFileSuffix = ".json"

// NewFileStore opens (creating if needed) a durable status store under dir.
//
// A file it cannot read or parse is a hard error, not a skipped entry. A
// runner that silently forgets one operation has made that operation's
// outcome unlearnable while still claiming, in every acceptance it issues,
// that it keeps status for the declared retention — so this fails loudly at
// startup instead.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("runnerservice: a file-backed status store needs a state directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("runnerservice: create the state directory %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("runnerservice: read the state directory %s: %w", dir, err)
	}

	store := &FileStore{dir: dir, records: map[string]Record{}}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != stateFileSuffix {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, readErr := os.ReadFile(path) //nolint:gosec // path is built from the configured state directory
		if readErr != nil {
			return nil, fmt.Errorf("runnerservice: read the status record %s: %w", path, readErr)
		}
		var record Record
		if unmarshalErr := json.Unmarshal(raw, &record); unmarshalErr != nil {
			return nil, fmt.Errorf("runnerservice: parse the status record %s: %w", path, unmarshalErr)
		}
		if record.OperationID == "" {
			return nil, fmt.Errorf("runnerservice: the status record %s names no operation", path)
		}
		store.records[record.OperationID] = record
	}
	return store, nil
}

// Dir returns the state directory backing this store, so a process can report
// where its durability actually lives.
func (f *FileStore) Dir() string { return f.dir }

// Put writes a record durably, then updates the index. The order matters: an
// index entry the disk does not have would survive exactly until the restart
// it exists to survive.
func (f *FileStore) Put(record Record) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("runnerservice: encode the status record for %s: %w", record.OperationID, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writeAtomic(f.path(record.OperationID), raw); err != nil {
		return err
	}
	f.records[record.OperationID] = record
	return nil
}

// writeAtomic writes through a temporary file, fsyncs it, and renames it into
// place, so a crash mid-write leaves either the previous record or the new
// one — never half of either.
func (f *FileStore) writeAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(f.dir, ".record-*")
	if err != nil {
		return fmt.Errorf("runnerservice: create a temporary status record: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runnerservice: write the status record %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runnerservice: sync the status record %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runnerservice: close the status record %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("runnerservice: set permissions on the status record %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("runnerservice: install the status record %s: %w", path, err)
	}
	// Fsync the directory too. A rename is only durable once the directory
	// entry itself has reached the disk, and this store's whole reason to
	// exist is that a status survives the process — which for a power loss
	// means surviving the page cache as well.
	return syncDir(f.dir)
}

// syncDir fsyncs a directory so a rename into it is durable. A filesystem
// that refuses the operation (some do) is not an error: the record is already
// written and renamed, and failing the Put would turn a weaker durability
// guarantee into a lost status, which is strictly worse.
func syncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // dir is this store's own configured directory
	if err != nil {
		return nil
	}
	defer func() { _ = handle.Close() }()
	_ = handle.Sync()
	return nil
}

// Get returns a record from the index.
func (f *FileStore) Get(operationID string) (Record, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	record, ok := f.records[operationID]
	return record, ok, nil
}

// List returns every held record, ordered by operation id.
func (f *FileStore) List() ([]Record, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return sortedRecords(f.records), nil
}

// Delete removes a record from disk and from the index.
func (f *FileStore) Delete(operationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.Remove(f.path(operationID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("runnerservice: remove the status record for %s: %w", operationID, err)
	}
	delete(f.records, operationID)
	return nil
}

// Durable reports true.
func (f *FileStore) Durable() bool { return true }

// path names a record's file. The operation id is base64url-encoded rather
// than used directly: the schema's identifier pattern admits `/`, `:` and `@`,
// so an id is not a filename, and sanitising by substitution would let two
// distinct ids collide onto one file.
func (f *FileStore) path(operationID string) string {
	return filepath.Join(f.dir, base64.RawURLEncoding.EncodeToString([]byte(operationID))+stateFileSuffix)
}

func sortedRecords(records map[string]Record) []Record {
	out := make([]Record, 0, len(records))
	for _, record := range records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })
	return out
}
