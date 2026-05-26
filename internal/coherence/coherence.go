// Package coherence holds the subscribers that keep derived state consistent
// when the KB corpus mutates (doc updates, deletes, adapter swaps, KG entity
// merges, tenant purges). Phase 8 design.
//
// All subscribers run inside the existing in-process eventbus worker pool
// (internal/eventbus). They MUST be fast (sub-second) — handlers that do real
// work (re-embed a doc, refresh a derived index) enqueue rather than perform.
//
// The Phase 8 DoD is "p95 cache invalidation < 5s after a doc.updated event".
// For an in-process bus that's trivially sub-millisecond; the 5s budget exists
// for downstream effects (re-embed queuing, derived index refresh) which we
// keep cheap by deferring real work.
package coherence

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
)

// Invalidator is the per-tenant cache stamp store. Retrieve-side code that
// holds derived state (PPR cache, classifier-by-tenant cache, agent context
// snapshots) reads the latest stamp for a tenant and invalidates locally if it
// has advanced past the value the cached datum was tagged with.
//
// In-process and lock-light. The stamp itself is the monotonic time.UnixNano()
// of the most recent kb.* event for the tenant.
type Invalidator struct {
	mu     sync.RWMutex
	stamps map[string]int64 // tenantID -> latest event nanos
	// hitCounter is for benchmark/diagnostics. Each handler invocation bumps it.
	hitCounter atomic.Uint64
	// lastEventAt is the wall-clock when the last KB event observed a non-empty
	// tenantID; used by the latency benchmark to measure publish-to-observe.
	lastObserved atomic.Pointer[time.Time]
}

func NewInvalidator() *Invalidator {
	return &Invalidator{stamps: make(map[string]int64)}
}

// Stamp returns the latest event stamp for tenantID (zero if never bumped).
func (i *Invalidator) Stamp(tenantID string) int64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.stamps[tenantID]
}

// HitCount returns the cumulative number of KB events the subscriber set has
// handled. Exposed for diagnostics + the latency benchmark.
func (i *Invalidator) HitCount() uint64 { return i.hitCounter.Load() }

// LastObserved is the wall-clock when the most recent KB event with a
// non-empty tenant was handled. Used by the latency bench.
func (i *Invalidator) LastObserved() time.Time {
	p := i.lastObserved.Load()
	if p == nil {
		return time.Time{}
	}
	return *p
}

// Bump advances the stamp for tenantID to now-nanos (monotonic best-effort).
func (i *Invalidator) Bump(tenantID string) {
	now := time.Now().UnixNano()
	i.mu.Lock()
	if cur, ok := i.stamps[tenantID]; !ok || now > cur {
		i.stamps[tenantID] = now
	}
	i.mu.Unlock()
	if tenantID != "" {
		t := time.Now()
		i.lastObserved.Store(&t)
	}
	i.hitCounter.Add(1)
}

// Drop removes the tenant entry entirely. Used by the purge handler — after a
// tenant is GDPR-purged, holding its old stamp serves no purpose.
func (i *Invalidator) Drop(tenantID string) {
	i.mu.Lock()
	delete(i.stamps, tenantID)
	i.mu.Unlock()
	if tenantID != "" {
		t := time.Now()
		i.lastObserved.Store(&t)
	}
	i.hitCounter.Add(1)
}

// ReembedEnqueuer is the side-effectful "do real work" half of the
// doc.updated handler. The default implementation is a no-op; callers can
// wire a real implementation that writes to kb.kb_ingest_runs(kind=reembed)
// or pushes to an external queue. Kept as an interface so tests / phase-8 smoke
// don't need a DB.
type ReembedEnqueuer interface {
	Enqueue(ctx context.Context, tenantID, docID string, chunkIDs []string) error
}

// NoopReembed implements ReembedEnqueuer as a no-op.
type NoopReembed struct{}

func (NoopReembed) Enqueue(ctx context.Context, _, _ string, _ []string) error { return nil }

// PPRCacheBuster is wired against the HippoRAG layer (which today doesn't cache —
// P5.3 deferred). Kept as an interface so when the cache lands, swapping the
// implementation is a one-line change in the wiring.
type PPRCacheBuster interface {
	BustTenant(tenantID string)
	BustEntities(tenantID string, entityIDs []string)
}

// NoopPPR implements PPRCacheBuster as a no-op (current state — P5.3 deferred).
type NoopPPR struct{}

func (NoopPPR) BustTenant(string)                {}
func (NoopPPR) BustEntities(string, []string)    {}

// Subscriber wires KB event handlers onto an eventbus. It owns the side-effect
// dependencies (re-embed enqueuer, PPR cache buster) and the Invalidator.
type Subscriber struct {
	inv     *Invalidator
	reembed ReembedEnqueuer
	ppr     PPRCacheBuster
	unsubs  []func()
}

// NewSubscriber returns a Subscriber not yet attached to a bus. Call Register
// to wire it onto the eventbus and Close to unsubscribe.
func NewSubscriber(inv *Invalidator, reembed ReembedEnqueuer, ppr PPRCacheBuster) *Subscriber {
	if inv == nil {
		inv = NewInvalidator()
	}
	if reembed == nil {
		reembed = NoopReembed{}
	}
	if ppr == nil {
		ppr = NoopPPR{}
	}
	return &Subscriber{inv: inv, reembed: reembed, ppr: ppr}
}

func (s *Subscriber) Invalidator() *Invalidator { return s.inv }

// Register subscribes the KB event handlers onto bus. Idempotent across
// repeat calls only if the caller calls Close first.
func (s *Subscriber) Register(bus eventbus.DomainEventBus) {
	s.unsubs = append(s.unsubs, bus.Subscribe(eventbus.EventKBDocUpdated, s.handleDocChanged))
	s.unsubs = append(s.unsubs, bus.Subscribe(eventbus.EventKBDocDeleted, s.handleDocChanged))
	s.unsubs = append(s.unsubs, bus.Subscribe(eventbus.EventKBAdapterSwapped, s.handleAdapterSwapped))
	s.unsubs = append(s.unsubs, bus.Subscribe(eventbus.EventKBEntityMerged, s.handleEntityMerged))
	s.unsubs = append(s.unsubs, bus.Subscribe(eventbus.EventKBTenantPurged, s.handleTenantPurged))
}

// Close unsubscribes every handler the Subscriber registered.
func (s *Subscriber) Close() {
	for _, u := range s.unsubs {
		u()
	}
	s.unsubs = nil
}

func (s *Subscriber) handleDocChanged(ctx context.Context, ev eventbus.DomainEvent) error {
	payload, ok := ev.Payload.(eventbus.KBDocChangedPayload)
	if !ok {
		// Tolerate JSON-decoded payloads (the HTTP publish endpoint deserializes
		// from JSON), which arrive as a map.
		if m, mok := ev.Payload.(map[string]any); mok {
			payload = decodeChangedFromMap(m)
		} else {
			return nil
		}
	}
	s.inv.Bump(ev.TenantID)
	// doc.updated re-embed is async, fire-and-forget. doc.deleted does not
	// re-embed (the chunks are superseded/gone).
	if ev.Type == eventbus.EventKBDocUpdated && payload.DocID != "" {
		// Detach from request context (bus dispatch context is fine to honor).
		if err := s.reembed.Enqueue(ctx, ev.TenantID, payload.DocID, payload.ChunkIDs); err != nil {
			slog.Warn("coherence: re-embed enqueue failed", "tenant", ev.TenantID, "doc", payload.DocID, "err", err)
		}
	}
	return nil
}

func (s *Subscriber) handleAdapterSwapped(ctx context.Context, ev eventbus.DomainEvent) error {
	s.inv.Bump(ev.TenantID)
	// Adapter swap implies every chunk under the previous adapter is candidate
	// for re-embed. Today we record the stamp and let the re-embed pipeline
	// pick up the work; explicit per-doc enqueue would require iterating the
	// tenant's docs, which is properly an offline job.
	return nil
}

func (s *Subscriber) handleEntityMerged(ctx context.Context, ev eventbus.DomainEvent) error {
	s.inv.Bump(ev.TenantID)
	payload, ok := ev.Payload.(eventbus.KBEntityMergedPayload)
	if !ok {
		if m, mok := ev.Payload.(map[string]any); mok {
			payload = decodeMergedFromMap(m)
		}
	}
	if len(payload.MergedEntityIDs) > 0 {
		s.ppr.BustEntities(ev.TenantID, payload.MergedEntityIDs)
	} else {
		s.ppr.BustTenant(ev.TenantID)
	}
	return nil
}

func (s *Subscriber) handleTenantPurged(ctx context.Context, ev eventbus.DomainEvent) error {
	s.inv.Drop(ev.TenantID)
	s.ppr.BustTenant(ev.TenantID)
	return nil
}

// decodeChangedFromMap recovers KBDocChangedPayload from a JSON-decoded map.
// Tolerant of missing fields — coherence handlers should never error on a
// minor schema drift.
func decodeChangedFromMap(m map[string]any) eventbus.KBDocChangedPayload {
	p := eventbus.KBDocChangedPayload{}
	b, err := json.Marshal(m)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(b, &p)
	return p
}

func decodeMergedFromMap(m map[string]any) eventbus.KBEntityMergedPayload {
	p := eventbus.KBEntityMergedPayload{}
	b, err := json.Marshal(m)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(b, &p)
	return p
}
