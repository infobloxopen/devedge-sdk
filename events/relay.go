package events

import (
	"context"
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// Relay is the outbox→bus pump of the event-bus stack (Phase 1). It reads the WRITE-ONLY
// outbox FORWARD (via [persistence.OutboxStore.ReadAfter] + a sidecar
// [persistence.OutboxCursorStore]), publishes each committed event to the [Bus], and
// ADVANCES its sidecar cursor past the event — it NEVER mutates an outbox row. It is the
// bus PRODUCER; it does NOT run domain handlers (that is the [Consumer]'s job, on the
// other side of the bus).
//
// This is the relay half of the old in-process dispatcher's outbox-reading loop, lifted
// out so the read path (outbox→bus) and the dispatch path (bus→handlers) are decoupled by
// the bus seam. The forward cursor, head-of-line poison handling, and write-only
// invariant are unchanged — only the per-event action changed from "deliver to handlers"
// to "publish to the bus".
//
// LEADER-ELECTED: only ONE relay per service may pump the outbox, or every event would be
// published to the bus more than once. The relay acquires a [Leader] claim before pumping
// and only runs while it holds leadership; a second relay (another replica) that cannot
// acquire the claim idles. At-least-once downstream (the consumer's idempotency marker)
// dedups any brief two-leader overlap during a failover, so the seam need not be perfectly
// fenced.
//
// At-least-once: a crash between Publish and the cursor-advance re-publishes the event on
// the next pump (the cursor is still behind it); the consumer's per-(event, handler)
// idempotency marker makes the redelivery a no-op effect.
//
// Poison / head-of-line: the head event is the oldest un-published event. If Publish to
// the bus fails, the cursor does not advance (advancing would skip the gap and lose the
// event — the outbox is write-only, the cursor is the only progress), so the batch stops
// at the head: bounded head-of-line blocking. After maxAttempts consecutive Publish
// failures on the SAME head event, the relay dead-letters it (in the sidecar) and advances
// past it so a permanently un-publishable event does not wedge the relay forever. The
// failure count lives in the sidecar, not in an outbox attempts column.
type Relay struct {
	store       persistence.OutboxStore
	cursors     persistence.OutboxCursorStore
	cursorName  string
	bus         BusPublisher
	leader      Leader
	topicOf     func(Event) string
	keyOf       func(Event) string
	maxAttempts int
}

// RelayOption configures a [Relay].
type RelayOption func(*Relay)

// WithRelayCursorName sets the sidecar cursor name this relay advances. Defaults to
// [DefaultCursorName]. A service runs one relay, so one cursor is sufficient; override it
// only if a service runs more than one logical relay over the same outbox.
func WithRelayCursorName(name string) RelayOption {
	return func(r *Relay) {
		if name != "" {
			r.cursorName = name
		}
	}
}

// WithRelayMaxAttempts sets the poison cutoff: after this many consecutive failed
// publishes of the SAME head event, the relay dead-letters it and advances past it. A
// non-positive value keeps the default ([persistence.DefaultMaxOutboxAttempts]).
func WithRelayMaxAttempts(n int) RelayOption {
	return func(r *Relay) {
		if n > 0 {
			r.maxAttempts = n
		}
	}
}

// WithRelayLeader sets the [Leader] that gates the relay so only one relay per service
// pumps the outbox. Defaults to a fresh [SingleProcessLeader] (dev/single-replica). Pass
// a cross-process Leader (PG advisory lock / lease) for a multi-replica deployment.
func WithRelayLeader(l Leader) RelayOption {
	return func(r *Relay) {
		if l != nil {
			r.leader = l
		}
	}
}

// WithRelayTopicMapper overrides how an event maps to a bus topic. The default is one
// topic per event Type ([busTopicForEvent]). Use it to fan several event types onto one
// topic, or to namespace topics per service.
func WithRelayTopicMapper(fn func(Event) string) RelayOption {
	return func(r *Relay) {
		if fn != nil {
			r.topicOf = fn
		}
	}
}

// WithRelayKeyMapper overrides the partition/ordering key for an event. The default keys
// by aggregate identity ([busKeyForEvent]) so per-aggregate order is preserved on a
// partitioned broker.
func WithRelayKeyMapper(fn func(Event) string) RelayOption {
	return func(r *Relay) {
		if fn != nil {
			r.keyOf = fn
		}
	}
}

// NewRelay returns a Relay that reads store forward via the cursors sidecar and publishes
// each event to bus. cursors is the [persistence.OutboxCursorStore] the relay advances
// (its own progress — the outbox is write-only); pass nil for a fresh in-memory
// [persistence.MemoryOutboxCursorStore] (dev default). The poison cutoff defaults to
// [persistence.DefaultMaxOutboxAttempts]; the leader defaults to a single-process leader.
func NewRelay(store persistence.OutboxStore, cursors persistence.OutboxCursorStore, bus BusPublisher, opts ...RelayOption) *Relay {
	if cursors == nil {
		cursors = persistence.NewMemoryOutboxCursorStore()
	}
	r := &Relay{
		store:       store,
		cursors:     cursors,
		cursorName:  DefaultCursorName,
		bus:         bus,
		leader:      NewSingleProcessLeader(),
		topicOf:     busTopicForEvent,
		keyOf:       busKeyForEvent,
		maxAttempts: persistence.DefaultMaxOutboxAttempts,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// PumpOnce reads up to limit events forward from the sidecar cursor and publishes each to
// the bus in (created_time, id) order, advancing the cursor PAST every event it resolves.
// It returns the number of events resolved this pass (published or dead-lettered) — how
// far the cursor advanced. A poller calls PumpOnce on a tick; tests call it directly to
// drive the relay deterministically.
//
// PumpOnce does NOT take the leader claim itself — [Relay.Run] holds leadership for the
// loop. Call PumpOnce directly only in a single-relay test or after acquiring leadership.
//
// Write-only: the relay never mutates an outbox row. It loads its position from the
// sidecar, ReadAfter pulls the next batch without touching the rows, and on a successful
// Publish it advances the sidecar cursor. The first event of the batch is the head; if it
// fails to publish, the cursor cannot advance past it (that would skip a gap), so the
// batch stops there — bounded head-of-line blocking, then dead-letter after maxAttempts.
func (r *Relay) PumpOnce(ctx context.Context, limit int) (published int, err error) {
	cursor, headFailures, err := r.cursors.LoadCursor(ctx, r.cursorName)
	if err != nil {
		return 0, fmt.Errorf("load cursor %q: %w", r.cursorName, err)
	}
	batch, err := r.store.ReadAfter(ctx, cursor, limit)
	if err != nil {
		return 0, fmt.Errorf("read outbox after cursor: %w", err)
	}
	for _, rec := range batch {
		evt := eventFromRecord(rec)
		pos := persistence.OutboxCursor{CreatedTime: rec.CreatedTime, ID: rec.ID}
		msg := BusMessage{Key: r.keyOf(evt), Event: evt}
		if perr := r.bus.Publish(ctx, r.topicOf(evt), msg); perr != nil {
			// The head event failed to publish. Bump its sidecar head-failure count; do
			// NOT advance the cursor (advancing would skip the gap and lose the event —
			// write-only outbox, the cursor is the only progress). Stop the batch here so
			// order and the head-of-line property hold. After maxAttempts on the same head
			// event, dead-letter it and advance past it so a permanently un-publishable
			// event does not wedge the relay.
			headFailures++
			if headFailures >= r.maxAttempts {
				if dlErr := r.cursors.DeadLetter(ctx, r.cursorName, rec, perr.Error()); dlErr != nil {
					return published, fmt.Errorf("dead-letter %s: %w", rec.ID, dlErr)
				}
				if serr := r.cursors.SaveCursor(ctx, r.cursorName, pos, 0); serr != nil {
					return published, fmt.Errorf("advance past poison %s: %w", rec.ID, serr)
				}
				published++ // resolved by dead-lettering; the cursor moved past it
				return published, fmt.Errorf("dead-lettered un-publishable event %s after %d attempts: %w", rec.ID, headFailures, perr)
			}
			if serr := r.cursors.SaveCursor(ctx, r.cursorName, cursor, headFailures); serr != nil {
				return published, fmt.Errorf("save head-failure count: %w", serr)
			}
			return published, fmt.Errorf("publish %s: %w", rec.ID, perr)
		}
		// Published. Advance the cursor past this event and reset the head-failure count.
		// The outbox row is untouched.
		cursor = pos
		headFailures = 0
		if serr := r.cursors.SaveCursor(ctx, r.cursorName, cursor, 0); serr != nil {
			return published, fmt.Errorf("advance cursor past %s: %w", rec.ID, serr)
		}
		published++
	}
	return published, nil
}

// Run is the relay's leader-elected poll loop: it acquires the [Leader] claim, then runs
// PumpOnce on every tick of interval until ctx is cancelled, releasing the claim on exit.
// While another replica holds leadership, Run idles on the tick (re-trying the acquire)
// and pumps nothing — so exactly one relay per service drains the outbox. Errors from a
// pump are reported through onErr (nil to ignore) so one un-publishable event does not
// stop the loop.
//
// A service wires Run as a goroutine alongside the consumer's Consume loop. The relay must
// be the SOLE outbox→bus pump; the consumer is the SOLE bus→handler pump.
func (r *Relay) Run(ctx context.Context, interval time.Duration, batch int, onErr func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	if batch <= 0 {
		batch = 100
	}
	// Acquire leadership before pumping; release on exit. We re-check the claim each tick
	// so a relay that lost (or never won) leadership keeps trying and a winner that exits
	// frees it for the next replica.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.leader.Release(releaseCtx)
	}()

	t := time.NewTicker(interval)
	defer t.Stop()
	leading := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !leading {
				acquired, err := r.leader.TryAcquire(ctx)
				if err != nil {
					if onErr != nil {
						onErr(fmt.Errorf("relay leader acquire: %w", err))
					}
					continue
				}
				if !acquired {
					continue // another replica is the relay leader; idle this tick
				}
				leading = true
			}
			if _, err := r.PumpOnce(ctx, batch); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// Cursor returns the relay's current forward position (for tests/introspection): the
// (created_time, id) of the last event it published.
func (r *Relay) Cursor(ctx context.Context) (persistence.OutboxCursor, error) {
	c, _, err := r.cursors.LoadCursor(ctx, r.cursorName)
	return c, err
}
