// switchlock.go makes a switch flip and an in-flight declaration evaluation
// mutually exclusive per namespace (task t38b, #328; review findings A1 and
// A2). Without it a handler reads the mode once and then acts on it for the
// rest of its evaluation while a flip commits underneath it:
//
//   - after -> before: the handler read 'after', the flip froze every node
//     open at that moment, and the handler then opened a NEW node the flip
//     never saw -- 'open' in 'before', never frozen, never expired, holding
//     a per-subject slot -- with its firing, marker and budget spend written
//     in 'before'.
//   - before -> after: the handler read 'before' and went to store the event
//     for a frozen node, the flip thawed and replayed first, the node was no
//     longer frozen so nothing was stored, and the event was never evaluated.
//
// The fix is one PostgreSQL advisory lock per namespace, keyed by
// switchLockKey. Every evaluation that acts on the mode (Router's delivered
// events, the scheduler Driver's per-namespace pass) holds it SHARED from
// its mode read to its last write; a flip (PostgresSwitchStore.Flip, and
// FlipSwitch around the flip plus its post-commit thaw and replay) holds it
// EXCLUSIVE. A flip therefore waits for every evaluation already in flight,
// and an evaluation's mode stays true until it finishes. Evaluations never
// wait on each other.
//
// The lock is session-level on a DEDICATED connection, not one borrowed from
// the store's pool: the evaluation itself needs pool connections, and a
// holder parked on a pool connection while waiting for another would
// deadlock the pool under load. switchLockSlots bounds how many dedicated
// connections one process opens at once. A holder never takes a second
// switch lock -- the context records what it already holds, so the replay
// FlipSwitch runs under its exclusive lock, and any nested path, reuses it
// instead of queueing behind itself.
//
// None of this runs inside a delivery's transaction: Router is offered the
// delivery after it commits (postgres.DeliveredEventHandler), and that stays
// so -- a slow flip delays the declaration evaluation, never the delivery.
package declengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SwitchLocker is the optional SwitchStore capability Router and Driver use
// to hold the namespace's switch lock around an evaluation. A SwitchStore
// without it (the in-memory test fakes) is simply not serialized.
type SwitchLocker interface {
	// HoldShared runs fn while holding the namespace's switch lock shared:
	// concurrent with other evaluations, exclusive of any flip.
	HoldShared(ctx context.Context, namespaceID string, fn func(context.Context) error) error
	// HoldExclusive runs fn while holding it exclusively: after every
	// in-flight evaluation has finished, and before any new one starts.
	HoldExclusive(ctx context.Context, namespaceID string, fn func(context.Context) error) error
}

// switchLockSlots bounds the dedicated connections this process holds for
// switch locks at once. Holders never nest (see heldSwitchLock), so a full
// set of slots always drains.
var switchLockSlots = make(chan struct{}, 16)

func switchLockKey(namespaceID string) string { return "decl-switch:" + namespaceID }

type switchLockCtxKey struct{ namespaceID string }

const (
	switchHeldShared    = "shared"
	switchHeldExclusive = "exclusive"
)

// HoldShared implements SwitchLocker.
func (s PostgresSwitchStore) HoldShared(ctx context.Context, namespaceID string, fn func(context.Context) error) error {
	return s.hold(ctx, namespaceID, switchHeldShared, fn)
}

// HoldExclusive implements SwitchLocker.
func (s PostgresSwitchStore) HoldExclusive(ctx context.Context, namespaceID string, fn func(context.Context) error) error {
	return s.hold(ctx, namespaceID, switchHeldExclusive, fn)
}

func (s PostgresSwitchStore) hold(ctx context.Context, namespaceID, want string, fn func(context.Context) error) error {
	if namespaceID == "" {
		return errors.New("declengine: namespace id required")
	}
	switch held, _ := ctx.Value(switchLockCtxKey{namespaceID}).(string); {
	case held == switchHeldExclusive, held == switchHeldShared && want == switchHeldShared:
		return fn(ctx)
	case held == switchHeldShared:
		// Upgrading would wait on this very holder's shared lock forever.
		return errors.New("declengine: cannot flip the engine switch from inside an evaluation holding it shared")
	}
	if s.Store == nil {
		return errors.New("declengine: switch lock needs a store")
	}
	select {
	case switchLockSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-switchLockSlots }()
	conn, err := pgx.ConnectConfig(ctx, s.Store.Pool().Config().ConnConfig)
	if err != nil {
		return fmt.Errorf("declengine: switch lock connection: %w", err)
	}
	// Closing the session releases its advisory lock, on every path --
	// including a caller whose context was cancelled mid-evaluation.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	lockSQL := `SELECT pg_advisory_lock_shared(hashtextextended($1,0))`
	if want == switchHeldExclusive {
		lockSQL = `SELECT pg_advisory_lock(hashtextextended($1,0))`
	}
	if _, err := conn.Exec(ctx, lockSQL, switchLockKey(namespaceID)); err != nil {
		return fmt.Errorf("declengine: take switch lock: %w", err)
	}
	return fn(context.WithValue(ctx, switchLockCtxKey{namespaceID}, want))
}

// holdShared runs fn under sw's shared switch lock when sw can take one.
func holdShared(ctx context.Context, sw SwitchStore, namespaceID string, fn func(context.Context) error) error {
	if l, ok := sw.(SwitchLocker); ok {
		return l.HoldShared(ctx, namespaceID, fn)
	}
	return fn(ctx)
}

// FlipSwitch is the whole of a flip as the control plane performs it, under
// the namespace's exclusive switch lock: the recorded flip (freezing open
// nodes in the flip's own transaction when it lands on 'before'), then, on
// 'after', the post-commit thaw and replay (freeze.go says why that cannot
// be a hook). The replay runs before the lock is released, so no evaluation
// can observe 'after' while nodes are still frozen from the flip it just
// made, and no flip back can freeze underneath a replay in progress.
//
// err is a failed flip (nothing changed). replayErr is a flip that committed
// but whose replay failed: the flip stands, and the scheduler's Driver
// re-runs the replay on its next tick. e may be nil only for a flip to
// 'before' or 'shadow'.
func FlipSwitch(ctx context.Context, sw PostgresSwitchStore, e *Engine, namespaceID, mode, actor, reason string, now func() time.Time) (previous string, replayErr, err error) {
	if now == nil {
		now = time.Now
	}
	if mode == ModeAfter && e == nil {
		return "", nil, errors.New("declengine: a flip to 'after' needs the engine that replays frozen nodes")
	}
	fb := PostgresBackend{Store: sw.Store}
	err = sw.HoldExclusive(ctx, namespaceID, func(ctx context.Context) error {
		var hooks []FlipHook
		if mode == ModeBefore {
			hooks = append(hooks, FreezeHookAt(fb, now))
		}
		var flipErr error
		previous, flipErr = sw.Flip(ctx, namespaceID, mode, actor, reason, hooks...)
		if flipErr != nil || mode != ModeAfter {
			return flipErr
		}
		replayErr = ThawAndReplay(ctx, e, fb, namespaceID, now().UTC())
		return nil
	})
	return previous, replayErr, err
}

var _ SwitchLocker = PostgresSwitchStore{}
