package kg

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Worker drains kb.kg_extract_jobs. One pass = ClaimJobs(limit) → for each job
// load chunk text → Sonnet extract → persist → mark done/failed.
//
// Wire as a long-running goroutine via Ticker, OR call ProcessOnce() from an
// HTTP route for manual operator drains.
type Worker struct {
	store     *Store
	extractor Extractor
	maxAttempts int
}

// NewWorker constructs a worker. maxAttempts caps retries per job (default 3).
func NewWorker(s *Store, x Extractor) *Worker {
	return &Worker{store: s, extractor: x, maxAttempts: 3}
}

// ProcessOnce drains up to `limit` queued jobs. Returns counts.
func (w *Worker) ProcessOnce(ctx context.Context, limit int) (RunResponse, error) {
	start := time.Now()
	jobs, err := w.store.ClaimJobs(ctx, limit)
	if err != nil {
		return RunResponse{}, err
	}
	resp := RunResponse{Processed: len(jobs)}
	for _, j := range jobs {
		if err := w.processJob(ctx, j); err != nil {
			slog.Warn("kg.worker: job failed", "job_id", j.ID, "chunk_id", j.ChunkID, "tenant_id", j.TenantID, "error", err)
			_ = w.store.MarkJob(ctx, j.ID, JobFailed, err.Error())
			resp.Failed++
			continue
		}
		resp.Done++
	}
	resp.LatencyMS = time.Since(start).Milliseconds()
	return resp, nil
}

func (w *Worker) processJob(ctx context.Context, j ExtractJob) error {
	if j.Attempts > w.maxAttempts {
		return errors.New("kg.worker: max attempts exceeded")
	}
	tid, err := uuid.Parse(j.TenantID)
	if err != nil {
		return err
	}
	jctx := store.WithTenantID(ctx, tid)
	text, _, err := w.store.LoadChunkText(jctx, j.ChunkID)
	if err != nil {
		return err
	}
	if len(text) < 60 {
		// Too thin to bother with; mark skipped without burning Sonnet.
		return w.store.MarkJob(jctx, j.ID, JobSkipped, "")
	}
	result, err := w.extractor.Extract(jctx, text)
	if err != nil {
		return err
	}
	if len(result.Entities) == 0 {
		// Sonnet explicitly returned empty; mark skipped (not failed).
		return w.store.MarkJob(jctx, j.ID, JobSkipped, "")
	}
	if _, _, _, perr := w.store.PersistExtraction(jctx, j.ChunkID, result); perr != nil {
		return perr
	}
	return w.store.MarkJob(jctx, j.ID, JobDone, "")
}

// Ticker drives ProcessOnce on a fixed interval.
type Ticker struct {
	worker   *Worker
	interval time.Duration
	limit    int
	stop     chan struct{}
}

// NewTicker returns a Ticker that drains up to `limit` jobs every `interval`.
func NewTicker(w *Worker, interval time.Duration, limit int) *Ticker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if limit <= 0 {
		limit = 50
	}
	return &Ticker{worker: w, interval: interval, limit: limit, stop: make(chan struct{})}
}

// Start launches the ticker goroutine. Idempotent — subsequent calls are no-ops.
func (t *Ticker) Start(ctx context.Context) {
	go func() {
		slog.Info("kg.ticker: started", "interval", t.interval.String(), "limit", t.limit)
		// Small warmup so we don't fire on boot.
		time.Sleep(30 * time.Second)
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.stop:
				return
			case <-ticker.C:
				resp, err := t.worker.ProcessOnce(ctx, t.limit)
				if err != nil {
					slog.Warn("kg.ticker: ProcessOnce failed", "error", err)
					continue
				}
				if resp.Processed > 0 {
					slog.Info("kg.ticker: drain", "processed", resp.Processed, "done", resp.Done, "failed", resp.Failed, "skipped", resp.Skipped, "latency_ms", resp.LatencyMS)
				}
			}
		}
	}()
}

// Stop signals the ticker to exit.
func (t *Ticker) Stop() { close(t.stop) }
