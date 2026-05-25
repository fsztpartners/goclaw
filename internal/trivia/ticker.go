package trivia

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Ticker runs the trivia loop daily for every tenant that has ingested chunks.
//
// Design choice (Phase 3): we DON'T reuse internal/cron because that service
// dispatches Payload to agent_turn (scheduler.LaneCron + agent.RunRequest), and
// trivia is programmatic, not an LLM agent run. A dedicated ticker here keeps
// the trivia loop independent of agent runtime changes.
type Ticker struct {
	runner   *Runner
	store    *Store
	interval time.Duration
	mu       sync.Mutex
	running  bool
	stop     chan struct{}
}

// NewTicker schedules trivia.RunOnce at the given interval (default 24h).
// For tests and the manual-trigger HTTP endpoint, interval can be shorter.
func NewTicker(r *Runner, s *Store, interval time.Duration) *Ticker {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Ticker{runner: r, store: s, interval: interval, stop: make(chan struct{})}
}

// Start fires once after a short warm-up delay (to avoid a thundering herd at
// gateway boot) and then once per interval. Idempotent.
func (t *Ticker) Start(ctx context.Context) {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	t.running = true
	t.mu.Unlock()

	go t.loop(ctx)
}

// Stop ends the loop. Safe to call multiple times.
func (t *Ticker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running {
		return
	}
	t.running = false
	close(t.stop)
	t.stop = make(chan struct{})
}

func (t *Ticker) loop(parent context.Context) {
	// Warm-up: 5 min after boot is the first run so any gateway init noise
	// settles. After that, fire on the interval.
	warmup := 5 * time.Minute
	select {
	case <-time.After(warmup):
	case <-parent.Done():
		return
	case <-t.stop:
		return
	}

	t.fireAllTenants(parent)

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.fireAllTenants(parent)
		case <-parent.Done():
			return
		case <-t.stop:
			return
		}
	}
}

func (t *Ticker) fireAllTenants(parent context.Context) {
	tenants, err := t.store.ListActiveTenants(parent)
	if err != nil {
		slog.Error("trivia: list active tenants failed", "err", err)
		return
	}
	if len(tenants) == 0 {
		slog.Info("trivia: no active tenants to run for; skipping")
		return
	}
	slog.Info("trivia: firing daily run", "tenants", len(tenants))
	for _, tID := range tenants {
		tu, perr := uuid.Parse(tID)
		if perr != nil {
			slog.Warn("trivia: bad tenant id", "tenant", tID, "err", perr)
			continue
		}
		// One run per tenant, bounded to avoid leaking goroutines if a run hangs.
		runCtx, cancel := context.WithTimeout(parent, 20*time.Minute)
		runCtx = store.WithTenantID(runCtx, tu)
		started := time.Now()
		run, err := t.runner.RunOnce(runCtx, DefaultSampleSize, ModeShadow)
		cancel()
		if err != nil {
			slog.Error("trivia: tenant run failed", "tenant", tID, "err", err, "elapsed", time.Since(started))
			continue
		}
		slog.Info("trivia: tenant run done", "tenant", tID,
			"run_id", run.ID, "questions", run.NQuestions,
			"hit5", run.HitAt5Count, "hit10", run.HitAt10Count,
			"elapsed", time.Since(started))
	}
}
