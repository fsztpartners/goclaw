package coherence

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
)

// TestSubscriber_HandlesAllKBEvents validates that publishing each KB event
// type causes the Invalidator to advance (or drop) its stamp for the tenant.
func TestSubscriber_HandlesAllKBEvents(t *testing.T) {
	bus := eventbus.NewDomainEventBus(eventbus.Config{QueueSize: 32, WorkerCount: 1})
	bus.Start(context.Background())
	t.Cleanup(func() { _ = bus.Drain(time.Second) })

	sub := NewSubscriber(nil, nil, nil)
	sub.Register(bus)
	t.Cleanup(sub.Close)

	tenant := uuid.New().String()
	cases := []struct {
		name    string
		evType  eventbus.EventType
		payload any
	}{
		{"doc_updated", eventbus.EventKBDocUpdated, eventbus.KBDocChangedPayload{DocID: uuid.New().String(), Reason: "ingest"}},
		{"doc_deleted", eventbus.EventKBDocDeleted, eventbus.KBDocChangedPayload{DocID: uuid.New().String(), Reason: "supersede"}},
		{"adapter_swapped", eventbus.EventKBAdapterSwapped, eventbus.KBAdapterSwappedPayload{AdapterID: "test", BaseModel: "qwen3-embed-4b@1024"}},
		{"entity_merged", eventbus.EventKBEntityMerged, eventbus.KBEntityMergedPayload{KeptEntityID: uuid.New().String(), MergedEntityIDs: []string{uuid.New().String()}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := sub.Invalidator().HitCount()
			bus.Publish(eventbus.DomainEvent{
				ID: uuid.New().String(), Type: tc.evType,
				SourceID: uuid.New().String(),
				TenantID: tenant, Timestamp: time.Now(), Payload: tc.payload,
			})
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if sub.Invalidator().HitCount() > before {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Fatalf("subscriber did not handle %s within 2s (hit count stuck at %d)", tc.name, before)
		})
	}

	if got := sub.Invalidator().Stamp(tenant); got == 0 {
		t.Fatalf("expected stamp > 0 after handling events; got 0")
	}
}

// TestSubscriber_TenantPurgedDropsStamp ensures a purge event clears the
// tenant's stamp (so a freshly-recreated tenant ID starts from zero).
func TestSubscriber_TenantPurgedDropsStamp(t *testing.T) {
	bus := eventbus.NewDomainEventBus(eventbus.Config{QueueSize: 16, WorkerCount: 1})
	bus.Start(context.Background())
	t.Cleanup(func() { _ = bus.Drain(time.Second) })

	sub := NewSubscriber(nil, nil, nil)
	sub.Register(bus)
	t.Cleanup(sub.Close)

	tenant := uuid.New().String()
	sub.Invalidator().Bump(tenant)
	if sub.Invalidator().Stamp(tenant) == 0 {
		t.Fatalf("seeded stamp lost")
	}
	bus.Publish(eventbus.DomainEvent{
		ID: uuid.New().String(), Type: eventbus.EventKBTenantPurged,
		SourceID: uuid.New().String(),
		TenantID: tenant, Timestamp: time.Now(),
		Payload: eventbus.KBTenantPurgedPayload{Reason: "test"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sub.Invalidator().Stamp(tenant) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected stamp dropped after purge; still %d", sub.Invalidator().Stamp(tenant))
}

// TestInvalidator_LatencyP95 publishes N events and measures publish→observe
// latency. With an in-process bus + no-op subscribers the p95 must be well
// under the Phase 8 5s budget.
func TestInvalidator_LatencyP95(t *testing.T) {
	bus := eventbus.NewDomainEventBus(eventbus.Config{QueueSize: 4096, WorkerCount: 4})
	bus.Start(context.Background())
	t.Cleanup(func() { _ = bus.Drain(time.Second) })

	sub := NewSubscriber(nil, nil, nil)
	sub.Register(bus)
	t.Cleanup(sub.Close)

	const N = 200
	samples := make([]time.Duration, 0, N)
	for i := range N {
		tenant := uuid.New().String()
		start := time.Now()
		bus.Publish(eventbus.DomainEvent{
			ID: uuid.New().String(), Type: eventbus.EventKBDocUpdated,
			SourceID: uuid.New().String(),
			TenantID: tenant, Timestamp: start,
			Payload: eventbus.KBDocChangedPayload{DocID: uuid.New().String(), Reason: "bench"},
		})
		// Spin until observed for this tenant.
		deadline := start.Add(5 * time.Second)
		for time.Now().Before(deadline) && sub.Invalidator().Stamp(tenant) == 0 {
			time.Sleep(100 * time.Microsecond)
		}
		if sub.Invalidator().Stamp(tenant) == 0 {
			t.Fatalf("iter %d: stamp never set within 5s", i)
		}
		samples = append(samples, time.Since(start))
	}
	// p95 sort.
	for i := 1; i < len(samples); i++ {
		for j := i; j > 0 && samples[j-1] > samples[j]; j-- {
			samples[j-1], samples[j] = samples[j], samples[j-1]
		}
	}
	p95 := samples[(len(samples)*95)/100]
	if p95 > 5*time.Second {
		t.Fatalf("p95 invalidation latency %v exceeds Phase 8 5s budget", p95)
	}
	t.Logf("invalidation latency p50=%v p95=%v p99=%v over N=%d",
		samples[len(samples)/2], p95, samples[(len(samples)*99)/100], N)
}
