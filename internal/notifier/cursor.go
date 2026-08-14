package notifier

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// deliveredWindow bounds how many recently-delivered event ids the cursor
// file remembers alongside the resume position itself (LastID). LastID
// alone already keeps a resumed connection's "?from=" query (strictly
// greater-than, see internal/api/events.go's pollCrossRunEvents) from ever
// re-handing the daemon an event it has durably advanced past; Delivered
// exists only as a second, independent guard -- e.g. against an operator
// restarting the daemon from a stale backup of the cursor file, or any
// future resume path that turns out not to be strictly exclusive the way
// today's is. A handful of entries is enough: it is not a growing log.
const deliveredWindow = 32

// cursorState is the cursor file's on-disk shape.
type cursorState struct {
	// LastID is the id of the last event this daemon has durably consumed
	// from the cross-run stream -- delivered to the webhook or not. It is
	// the value the next connection resumes from via "?from=".
	LastID string `json:"last_id"`
	// Delivered holds the ids of the most recent events actually handed to
	// notify.Notify (i.e. lifecycle events, whether the POST itself
	// succeeded or failed -- see Cursor's doc comment), newest last,
	// trimmed to deliveredWindow.
	Delivered []string `json:"delivered,omitempty"`
}

// Cursor is the daemon's durable position in the cross-run SSE feed: a
// JSON file written with write-temp-rename so a reader (this process, on
// its own restart) never observes a half-written file.
//
// # Why the file is written before the side effect it records, not after
//
// Advance and MarkDelivered both persist to disk BEFORE the caller
// performs the corresponding action (nothing observable for Advance;
// the notify.Notify webhook attempt for MarkDelivered). That ordering is
// deliberate: it means a crash can only ever cause a *missed* webhook post
// (the file already says the event was consumed/delivered, but the POST
// underneath never actually fired), never a *duplicate* one (the file
// already excludes that event from the next resume's "?from=" window and
// from Delivered's membership check before the daemon would ever consider
// posting it again). A duplicate Discord message is a visible, confusing
// bug a human has to notice and explain; a rare missed message during a
// hard kill is the same fail-open posture internal/notify's own Post
// already accepts (one bounded attempt, no retries -- see webhook.go).
// See docs/plans/2026-08-13-economy-discord-graphs.md task t14 and this
// package's TestRestart* cases for the guarantee this buys.
type Cursor struct {
	path string
	st   cursorState
}

// LoadCursor reads path's cursor file, or returns a fresh zero-value
// Cursor (LastID == "", meaning "resume from the start of the stream") if
// path does not exist yet -- the daemon's very first run. A path that
// exists but cannot be parsed is a hard error: silently discarding a
// corrupt cursor would risk replaying (and re-notifying) a run's entire
// history, exactly the duplicate-delivery outcome this package exists to
// prevent.
func LoadCursor(path string) (*Cursor, error) {
	if path == "" {
		return nil, fmt.Errorf("notifier: cursor path must not be empty")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own configuration
	if err != nil {
		if os.IsNotExist(err) {
			return &Cursor{path: path}, nil
		}
		return nil, fmt.Errorf("notifier: read cursor file %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &Cursor{path: path}, nil
	}
	var st cursorState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("notifier: parse cursor file %s: %w", path, err)
	}
	return &Cursor{path: path, st: st}, nil
}

// Last returns the resume position: the value the next SSE connection
// should send as "?from=" (and Last-Event-ID). Empty means "from the
// start of the stream".
func (c *Cursor) Last() string {
	return c.st.LastID
}

// Seen reports whether id has already been durably consumed: either it is
// lexically at-or-before LastID (ULIDs sort lexically, see
// internal/store.NewULID's doc comment -- this is a cheap, exact test, not
// a heuristic), or it appears in the recent Delivered window. A caller
// uses this to skip a frame the server hands back that the daemon has
// already fully accounted for -- defensively, since a correctly-honored
// "?from=" resume should not produce one, but a client that trusts its own
// server-side contract absolutely has no way to notice a future regression
// in it.
func (c *Cursor) Seen(id string) bool {
	if id == "" {
		return false
	}
	if c.st.LastID != "" && id <= c.st.LastID {
		return true
	}
	for _, d := range c.st.Delivered {
		if d == id {
			return true
		}
	}
	return false
}

// Advance persists id as the new resume position, with no change to
// Delivered -- for a consumed event the daemon is not about to attempt
// delivery for (a non-lifecycle event, or one already Seen). A no-op, and
// no write, when id is not lexically after the current LastID.
func (c *Cursor) Advance(id string) error {
	if id == "" || (c.st.LastID != "" && id <= c.st.LastID) {
		return nil
	}
	c.st.LastID = id
	return c.persist()
}

// MarkDelivered persists id as both the new resume position and a member
// of the Delivered window, and must be called BEFORE the caller attempts
// the corresponding webhook delivery -- see Cursor's doc comment for why
// that ordering is what makes a crash lose a notification rather than
// duplicate one.
func (c *Cursor) MarkDelivered(id string) error {
	if id == "" {
		return fmt.Errorf("notifier: MarkDelivered: empty id")
	}
	if c.st.LastID == "" || id > c.st.LastID {
		c.st.LastID = id
	}
	c.st.Delivered = append(c.st.Delivered, id)
	if len(c.st.Delivered) > deliveredWindow {
		c.st.Delivered = c.st.Delivered[len(c.st.Delivered)-deliveredWindow:]
	}
	return c.persist()
}

// persist writes c.st to c.path with write-temp-rename: write the full
// content to a sibling temp file, fsync it, then atomically rename it over
// the real path. A reader (this process on its own restart) can therefore
// never observe a partially-written cursor file, even across a kill at an
// arbitrary point in this function -- the rename is the only step that can
// make the new content visible, and renames within one directory are
// atomic on every platform this daemon targets.
func (c *Cursor) persist() error {
	data, err := json.Marshal(c.st)
	if err != nil {
		return fmt.Errorf("notifier: marshal cursor state: %w", err)
	}

	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".cursor-*.tmp")
	if err != nil {
		return fmt.Errorf("notifier: create temp cursor file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("notifier: write temp cursor file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("notifier: sync temp cursor file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("notifier: close temp cursor file: %w", err)
	}
	if err := os.Rename(tmpPath, c.path); err != nil {
		return fmt.Errorf("notifier: rename cursor file into place: %w", err)
	}
	return nil
}
