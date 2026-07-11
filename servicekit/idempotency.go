// idempotency.go — WS-043 / F048 Increment 2, Deliverable A: servicekit auto-wiring
// of the durable, exactly-once request-idempotency path plus a host-scheduled GC
// sweep.
//
// servicekit is ORM-free (it never imports gormtx), so — exactly like the event
// consumer seam ([ConsumerConfig.Tx]/[ConsumerConfig.Idem]) — a module supplies its
// own NAMESPACED durable store + tx runner (built from its [DatabaseNamespace]) via
// [App.EnableDurableIdempotency]. Because server.New bakes the interceptor chain
// before modules register, the host installs a late-bound [hostDurableDedup] holder as
// server.Config.DurableDedup at New and modules populate it during Register. The
// holder routes each request to the owning module's store by method (so a composed
// multi-module host with per-module-namespaced idempotency_keys tables is correct),
// and falls back to a correct in-process [memDurableStore] when no module registers a
// store (persistence-free dev) — durable idempotency is enabled without forcing a DB.
package servicekit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// DefaultIdempotencyGCInterval is the sweep period used when
// [DurableIdempotencyConfig.GCInterval] is zero.
const DefaultIdempotencyGCInterval = 15 * time.Minute

// DurableIdempotencyMode aliases [middleware.DurableDedupMode] so a host selects the
// transactional or reserve→remote→complete path without importing middleware.
type DurableIdempotencyMode = middleware.DurableDedupMode

const (
	// IdempotencyTransactional (default) claims/handler/completes in one transaction —
	// exactly-once for a LOCAL DB effect.
	IdempotencyTransactional = middleware.DurableModeTransactional
	// IdempotencyReserve reserves→runs the handler (a REMOTE effect) outside any
	// transaction→completes — no DB connection held across the remote call.
	IdempotencyReserve = middleware.DurableModeReserve
)

// DurableIdempotencyConfig is the HOST opt-in for durable request idempotency
// ([HostConfig.DurableIdempotency]). nil disables it (the best-effort in-memory
// DeduplicateUnary default is unchanged); non-nil installs the durable path and the
// host-scheduled GC sweep.
type DurableIdempotencyConfig struct {
	// TTL is the record retention; zero uses middleware.DefaultIdempotencyTTL (~24h).
	TTL time.Duration
	// DisableFingerprint turns OFF the param-fingerprint guard (on by default).
	DisableFingerprint bool
	// MaxResponseBytes, when > 0, rejects an over-large stored response (fail loud).
	MaxResponseBytes int
	// Mode selects the transactional (default) or reserve path for the WHOLE host —
	// one interceptor slot per server (single-service lane).
	Mode DurableIdempotencyMode
	// GCInterval is the expired-record sweep period; zero uses
	// DefaultIdempotencyGCInterval. Ignored when DisableGC is set.
	GCInterval time.Duration
	// DisableGC turns OFF the host-scheduled sweep (e.g. when an external cron owns
	// retention). Records still read as absent once expired; the table just grows.
	DisableGC bool
	// PartitionCount opts the durable idempotency_keys table into PostgreSQL HASH
	// partitioning by its full primary key (WS-043 Increment 3, DD-3), for a hot, high-
	// turnover table on a large deployment. 0/unset (the default) keeps the plain,
	// non-partitioned table — byte-for-byte unchanged, and unaffected on SQLite/dev.
	//
	// servicekit stays ORM-free, so this is the host-declared KNOB: the module's migrate
	// callback (which imports gormtx) reads it and passes it to
	// gormtx.MigrateOptions.IdempotencyPartitions, which creates the table hash-partitioned
	// at migrate time (create-time only; fails loud if the table already exists
	// non-partitioned). It is a PostgreSQL-only performance path; leave it 0 elsewhere.
	PartitionCount int
}

// DurableIdempotencyRegistration is what a module hands the host from Register via
// [App.EnableDurableIdempotency]. Both fields are required. It mirrors
// [ConsumerConfig]: the module builds its NAMESPACED store + tx runner from its
// [DatabaseNamespace] (e.g. gormtx.NewGormDurableDedupStore(db,
// gormtx.WithDurableDedupNamespace(ns)) and its GormTxRunner over the same db).
type DurableIdempotencyRegistration struct {
	// Store is the module's namespaced durable idempotency store.
	Store middleware.DurableIdempotencyStore
	// Tx is the module's tx runner over the SAME backend as Store (claim/complete bind
	// to it via persistence.TxFromContext).
	Tx persistence.TxRunner
}

// durableIdemRegistration is the resolved per-module registration the host records.
type durableIdemRegistration struct {
	moduleID string
	store    middleware.DurableIdempotencyStore
	tx       persistence.TxRunner
}

// hostDurableDedup is the late-bound host holder set as server.Config.DurableDedup at
// New and populated by modules during Register. It satisfies BOTH
// [middleware.DurableIdempotencyStore] (routing store calls by the request method) and
// [persistence.TxRunner] (routing the transaction by the gRPC method), so the ONE
// server-level interceptor dispatches each request to the owning module's store + tx.
// A method with no registered durable store routes to the in-process fallback.
type hostDurableDedup struct {
	methodToModule map[string]string                             // full gRPC method → moduleID
	stores         map[string]middleware.DurableIdempotencyStore // moduleID → store
	txs            map[string]persistence.TxRunner               // moduleID → tx
	single         *durableIdemRegistration                      // set when exactly one module registered
	fallback       *memDurableStore                              // dev fallback (no DB required)
}

func newHostDurableDedup() *hostDurableDedup {
	return &hostDurableDedup{
		methodToModule: map[string]string{},
		stores:         map[string]middleware.DurableIdempotencyStore{},
		txs:            map[string]persistence.TxRunner{},
		fallback:       newMemDurableStore(),
	}
}

// build resolves the method→module routing table from every module's descriptor and
// records each module's supplied store + tx. Called once in Run after all modules
// register and before Serve.
func (h *hostDurableDedup) build(modules []Module, regs []durableIdemRegistration) {
	for _, m := range modules {
		d := m.Descriptor()
		for _, meth := range d.Methods {
			h.methodToModule[meth] = d.ID
		}
	}
	for _, r := range regs {
		h.stores[r.moduleID] = r.store
		h.txs[r.moduleID] = r.tx
	}
	if len(regs) == 1 {
		only := regs[0]
		h.single = &only
	}
}

// storeFor routes a request to its owning module's durable store. If the method's
// owning module did NOT register a store, it routes to the isolated in-process
// fallback — NEVER to another module's store (which, under dedicated-DB isolation,
// would bind the effect to the wrong backend). A method owned by NO module (unknown)
// uses the sole registration when there is exactly one, else the fallback.
func (h *hostDurableDedup) storeFor(method string) middleware.DurableIdempotencyStore {
	if mod, ok := h.methodToModule[method]; ok {
		if s, ok := h.stores[mod]; ok {
			return s // owned + registered
		}
		return h.fallback // owned but the module did not register → isolated fallback
	}
	if h.single != nil {
		return h.single.store // unknown method, exactly one registration
	}
	return h.fallback
}

// txForMethod mirrors storeFor for the transaction runner (same routing rules, so the
// tx binds to the same backend the store call targets).
func (h *hostDurableDedup) txForMethod(method string) persistence.TxRunner {
	if mod, ok := h.methodToModule[method]; ok {
		if t, ok := h.txs[mod]; ok {
			return t // owned + registered
		}
		return h.fallback // owned but the module did not register → isolated fallback
	}
	if h.single != nil {
		return h.single.tx // unknown method, exactly one registration
	}
	return h.fallback
}

// unregisteredModules returns the IDs of modules that declare methods but did NOT call
// EnableDurableIdempotency, when at least one module DID — a mixed configuration whose
// unregistered modules fall back to non-durable, per-pod idempotency. Run logs it so
// the downgrade is not silent.
func (h *hostDurableDedup) unregisteredModules(modules []Module) []string {
	if len(h.stores) == 0 {
		return nil // pure dev fallback (no module registered) — expected, not mixed
	}
	var missing []string
	for _, m := range modules {
		id := m.Descriptor().ID
		if _, ok := h.stores[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// Lookup implements middleware.DurableIdempotencyStore (routed by key.Method).
func (h *hostDurableDedup) Lookup(ctx context.Context, key persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	return h.storeFor(key.Method).Lookup(ctx, key)
}

// Claim implements middleware.DurableIdempotencyStore (routed by key.Method).
func (h *hostDurableDedup) Claim(ctx context.Context, key persistence.IdempotencyKey, fp string, ttl time.Duration) (persistence.IdempotencyRecord, bool, error) {
	return h.storeFor(key.Method).Claim(ctx, key, fp, ttl)
}

// Complete implements middleware.DurableIdempotencyStore (routed by key.Method).
func (h *hostDurableDedup) Complete(ctx context.Context, key persistence.IdempotencyKey, responseType string, response []byte) error {
	return h.storeFor(key.Method).Complete(ctx, key, responseType, response)
}

// Abandon implements middleware.DurableIdempotencyStore (routed by key.Method).
func (h *hostDurableDedup) Abandon(ctx context.Context, key persistence.IdempotencyKey) (bool, error) {
	return h.storeFor(key.Method).Abandon(ctx, key)
}

// GC sweeps EVERY registered module store plus the fallback, summing the counts. It
// does not stop on a per-store error — it continues so one failing store cannot starve
// the others (or the fallback, always swept last) of sweeps — and joins the errors.
func (h *hostDurableDedup) GC(ctx context.Context, now time.Time) (int64, error) {
	var total int64
	var errs []error
	for _, s := range h.stores {
		n, err := s.GC(ctx, now)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	n, err := h.fallback.GC(ctx, now)
	total += n
	if err != nil {
		errs = append(errs, err)
	}
	return total, errors.Join(errs...)
}

// Atomically implements persistence.TxRunner: it routes the transaction to the module
// that owns the current request's method (so claim/complete bind to that module's
// backend), keyed by the SAME method the store call uses (stashed on ctx by the
// interceptor), falling back to the sole registration or the in-memory fallback.
func (h *hostDurableDedup) Atomically(ctx context.Context, fn func(context.Context) error) error {
	return h.txFor(ctx).Atomically(ctx, fn)
}

func (h *hostDurableDedup) txFor(ctx context.Context) persistence.TxRunner {
	if method, ok := middleware.IdempotencyMethodFromContext(ctx); ok {
		return h.txForMethod(method)
	}
	if h.single != nil {
		return h.single.tx
	}
	return h.fallback
}

// verifyMigrated probes each registered store with a sentinel Lookup so an
// enabled-but-un-migrated idempotency_keys table fails LOUDLY at boot (DA-6), naming
// the migration models, rather than surfacing a raw driver error per request. The
// in-memory fallback needs no probe.
func (h *hostDurableDedup) verifyMigrated(ctx context.Context) error {
	probe := persistence.IdempotencyKey{Method: "__servicekit_idempotency_boot_probe__", RequestID: "__probe__"}
	for mod, s := range h.stores {
		if _, _, err := s.Lookup(ctx, probe); err != nil {
			return fmt.Errorf("servicekit: module %q enabled durable idempotency but its idempotency_keys table is not migrated — "+
				"include gormtx.RequestIdempotencyMigrationModels() in the host migration (MigrateOptions.FrameworkModels): %w", mod, err)
		}
	}
	return nil
}

// ---- memDurableStore: the in-process, transactional dev fallback ----------------

// memDurableStore is a correct, in-process middleware.DurableIdempotencyStore + its
// own persistence.TxRunner. It is the safe fallback when durable idempotency is opted
// in but no module registers a DB-backed store (persistence-free dev): idempotency
// still holds per-pod, with rollback-on-error, TTL, and GC — no DB required.
//
// Atomically snapshots the map and restores it on error (so a claim rolls back with a
// failed handler, matching the durable contract) and enrolls a sentinel handle on ctx
// so the mutating methods know they run inside a transaction (and fail loud otherwise).
//
// DEV-ONLY caveats (it is not a production store): in DurableModeTransactional the
// handler runs INSIDE Atomically, so its global mutex is held for the handler's whole
// duration — every idempotent request is serialized. And the map is only pruned by GC
// (and expired-row reclaim), so a host that disables GC lets it grow with each unique
// request_id (a RAM bound of roughly request-rate × TTL). Register a DB-backed store
// for production.
type memDurableStore struct {
	mu  sync.Mutex
	m   map[string]memDurableRecord
	now func() time.Time
}

type memDurableRecord struct {
	rec       persistence.IdempotencyRecord
	expiresAt time.Time
}

// memTxHandle is the sentinel transaction handle memDurableStore.Atomically enrolls.
type memTxHandle struct{}

func newMemDurableStore() *memDurableStore {
	return &memDurableStore{m: map[string]memDurableRecord{}, now: time.Now}
}

func memKey(k persistence.IdempotencyKey) string {
	return k.Tenant + "\x00" + k.Method + "\x00" + k.RequestID
}

func inMemTx(ctx context.Context) bool {
	if h, ok := persistence.TxFromContext(ctx); ok {
		_, ok = h.(memTxHandle)
		return ok
	}
	return false
}

func errMemNoTx(op string) error {
	return fmt.Errorf("servicekit: memDurableStore.%s called outside Atomically", op)
}

// Atomically implements persistence.TxRunner with snapshot/rollback semantics.
func (s *memDurableStore) Atomically(ctx context.Context, fn func(context.Context) error) error {
	if inMemTx(ctx) {
		return fn(ctx) // nested: the outer call owns the lock, snapshot, and commit
	}
	s.mu.Lock()
	snap := make(map[string]memDurableRecord, len(s.m))
	for k, v := range s.m {
		snap[k] = v
	}
	committed := false
	defer func() {
		if !committed {
			s.m = snap // rollback
		}
		s.mu.Unlock()
	}()
	if err := fn(persistence.WithTx(ctx, memTxHandle{})); err != nil {
		return err
	}
	committed = true
	return nil
}

// Lookup implements middleware.DurableIdempotencyStore (non-transactional fast path).
func (s *memDurableStore) Lookup(_ context.Context, key persistence.IdempotencyKey) (persistence.IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[memKey(key)]
	if !ok || !cur.expiresAt.After(s.now()) {
		return persistence.IdempotencyRecord{}, false, nil // absent or expired
	}
	return cur.rec, true, nil
}

// Claim implements middleware.DurableIdempotencyStore. It must run inside Atomically.
func (s *memDurableStore) Claim(ctx context.Context, key persistence.IdempotencyKey, fp string, ttl time.Duration) (persistence.IdempotencyRecord, bool, error) {
	if !inMemTx(ctx) {
		return persistence.IdempotencyRecord{}, false, errMemNoTx("Claim")
	}
	now := s.now()
	k := memKey(key)
	if cur, ok := s.m[k]; ok && cur.expiresAt.After(now) {
		return cur.rec, false, nil // live conflict
	}
	// Fresh or expired: (re)claim.
	s.m[k] = memDurableRecord{
		rec:       persistence.IdempotencyRecord{Status: persistence.StatusInProgress, Fingerprint: fp},
		expiresAt: now.Add(ttl),
	}
	return persistence.IdempotencyRecord{}, true, nil
}

// Complete implements middleware.DurableIdempotencyStore. It must run inside Atomically.
func (s *memDurableStore) Complete(ctx context.Context, key persistence.IdempotencyKey, responseType string, response []byte) error {
	if !inMemTx(ctx) {
		return errMemNoTx("Complete")
	}
	k := memKey(key)
	cur, ok := s.m[k]
	if !ok {
		return fmt.Errorf("servicekit: memDurableStore complete: no claimed record for request_id %q", key.RequestID)
	}
	cur.rec.Status = persistence.StatusCompleted
	cur.rec.ResponseType = responseType
	cur.rec.Response = response
	s.m[k] = cur
	return nil
}

// Abandon implements middleware.DurableIdempotencyStore. It must run inside Atomically
// and is guarded to in_progress records (never erases a completed response).
func (s *memDurableStore) Abandon(ctx context.Context, key persistence.IdempotencyKey) (bool, error) {
	if !inMemTx(ctx) {
		return false, errMemNoTx("Abandon")
	}
	k := memKey(key)
	cur, ok := s.m[k]
	if !ok || cur.rec.Status != persistence.StatusInProgress {
		return false, nil
	}
	delete(s.m, k)
	return true, nil
}

// GC implements middleware.DurableIdempotencyStore (non-transactional sweep).
func (s *memDurableStore) GC(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, v := range s.m {
		if !v.expiresAt.After(now) {
			delete(s.m, k)
			n++
		}
	}
	return n, nil
}

// Compile-time contract checks.
var (
	_ middleware.DurableIdempotencyStore = (*hostDurableDedup)(nil)
	_ persistence.TxRunner               = (*hostDurableDedup)(nil)
	_ middleware.DurableIdempotencyStore = (*memDurableStore)(nil)
	_ persistence.TxRunner               = (*memDurableStore)(nil)
)
