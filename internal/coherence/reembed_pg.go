package coherence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// PgReembedEnqueuer writes a row into kb.kb_ingest_runs(kind='reembed') so the
// existing run-monitoring UI and CLI pickers naturally surface the queue.
// The actual re-embed worker (lib/finetune/reembed.ts in fzst-claw) polls this
// table — Phase 8 only enqueues; it does not perform the embedding.
//
// Schema note: kb_ingest_runs.kind was added in migration 064. Older runs may
// have NULL kind; the worker filters on kind='reembed' + status='queued'.
type PgReembedEnqueuer struct {
	db *sql.DB
}

func NewPgReembedEnqueuer(db *sql.DB) *PgReembedEnqueuer {
	return &PgReembedEnqueuer{db: db}
}

func (p *PgReembedEnqueuer) Enqueue(ctx context.Context, tenantID, docID string, chunkIDs []string) error {
	tu, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("reembed enqueue: bad tenant_id %q: %w", tenantID, err)
	}
	du, err := uuid.Parse(docID)
	if err != nil {
		return fmt.Errorf("reembed enqueue: bad doc_id %q: %w", docID, err)
	}
	stats, _ := json.Marshal(map[string]any{
		"trigger":      "doc.updated",
		"chunk_count":  len(chunkIDs),
		"chunk_id_sample": sampleIDs(chunkIDs, 8),
	})
	runID := uuid.Must(uuid.NewV7())
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO kb.kb_ingest_runs
		  (id, tenant_id, visibility, doc_id, status, kind, stats, started_at)
		VALUES
		  ($1, $2, 'public', $3, 'queued', 'reembed', $4::jsonb, NOW())`,
		runID, tu, du, string(stats),
	)
	return err
}

func sampleIDs(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	return ids[:n]
}
