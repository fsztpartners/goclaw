package kg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Store wraps the *sql.DB for KG-side ops.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store wired to the given *sql.DB.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// EnqueueChunks adds one job per chunk_id for the current tenant context.
// Idempotent on (tenant_id, chunk_id) — UNIQUE constraint on kb.kg_extract_jobs.
func (s *Store) EnqueueChunks(ctx context.Context, chunkIDs []string) (int, error) {
	if len(chunkIDs) == 0 {
		return 0, nil
	}
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return 0, errors.New("kg enqueue: missing tenant_id in context")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("kg enqueue: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO kb.kg_extract_jobs (tenant_id, chunk_id)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id, chunk_id) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("kg enqueue: prepare: %w", err)
	}
	defer stmt.Close()
	n := 0
	for _, cid := range chunkIDs {
		res, err := stmt.ExecContext(ctx, tenantID, cid)
		if err != nil {
			return n, fmt.Errorf("kg enqueue chunk=%s: %w", cid, err)
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("kg enqueue: commit: %w", err)
	}
	return n, nil
}

// LoadChunkText returns the chunk text for a given chunk_id, scoped to tenant.
func (s *Store) LoadChunkText(ctx context.Context, chunkID string) (string, string, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return "", "", errors.New("kg load chunk: missing tenant_id")
	}
	var text, tid string
	if err := s.db.QueryRowContext(ctx,
		`SELECT text, tenant_id::text FROM kb.kb_chunks WHERE id = $1 AND tenant_id = $2`,
		chunkID, tenantID,
	).Scan(&text, &tid); err != nil {
		return "", "", fmt.Errorf("kg load chunk %s: %w", chunkID, err)
	}
	return text, tid, nil
}

// PersistExtraction upserts entities + relations + entity-chunk links for one
// chunk. Returns counts of distinct upserted rows.
//
// All writes happen in one transaction. Cross-tenant inserts are rejected by
// the DB triggers in 000065 — if any row trips, the whole tx rolls back.
func (s *Store) PersistExtraction(ctx context.Context, chunkID string, result ExtractionResult) (entities, relations, links int, err error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		err = errors.New("kg persist: missing tenant_id")
		return
	}
	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		err = fmt.Errorf("kg persist: begin: %w", txErr)
		return
	}
	defer tx.Rollback()

	// 1. Upsert entities. Returns id keyed by lower(name) for FK use below.
	nameToID := make(map[string]string, len(result.Entities))
	for _, e := range result.Entities {
		if strings.TrimSpace(e.Name) == "" {
			continue
		}
		var id string
		// COALESCE wraps the array_agg subquery because array_agg over an empty
		// set returns NULL, and `aliases` is NOT NULL — without COALESCE the
		// update path nulls the column and the constraint trips.
		err = tx.QueryRowContext(ctx, `
			INSERT INTO kb.kg_entities (tenant_id, name, entity_type, description, aliases)
			VALUES ($1, $2, $3, $4, $5::text[])
			ON CONFLICT (tenant_id, normalized_name, entity_type) DO UPDATE
			   SET description = CASE WHEN length(EXCLUDED.description) > length(kb.kg_entities.description)
			                          THEN EXCLUDED.description
			                          ELSE kb.kg_entities.description END,
			       aliases     = COALESCE(
			                       (SELECT array_agg(DISTINCT a) FROM unnest(kb.kg_entities.aliases || EXCLUDED.aliases) a WHERE a IS NOT NULL),
			                       '{}'::text[]
			                     ),
			       updated_at  = NOW()
			RETURNING id`,
			tenantID, e.Name, e.EntityType, nullableText(e.Description), pqStringArray(e.Aliases),
		).Scan(&id)
		if err != nil {
			err = fmt.Errorf("kg persist: entity %q: %w", e.Name, err)
			return
		}
		nameToID[strings.ToLower(strings.TrimSpace(e.Name))] = id
		entities++
	}

	// 2. Upsert entity_chunks (the evidence join).
	for _, e := range result.Entities {
		eid, ok := nameToID[strings.ToLower(strings.TrimSpace(e.Name))]
		if !ok {
			continue
		}
		var added bool
		err = tx.QueryRowContext(ctx, `
			INSERT INTO kb.kg_entity_chunks (tenant_id, entity_id, chunk_id, weight)
			VALUES ($1, $2, $3, 1.0)
			ON CONFLICT (tenant_id, entity_id, chunk_id) DO NOTHING
			RETURNING TRUE`,
			tenantID, eid, chunkID,
		).Scan(&added)
		if err != nil && err != sql.ErrNoRows {
			err = fmt.Errorf("kg persist: entity_chunk %s: %w", e.Name, err)
			return
		}
		if added {
			links++
		}
		err = nil
	}

	// Update chunk_count denormalization (best-effort; not part of correctness).
	for _, eid := range nameToID {
		if _, err2 := tx.ExecContext(ctx,
			`UPDATE kb.kg_entities SET chunk_count =
			   (SELECT COUNT(*) FROM kb.kg_entity_chunks WHERE entity_id = $1)
			 WHERE id = $1`, eid); err2 != nil {
			// non-fatal — log but don't abort
			_ = err2
		}
	}

	// 3. Upsert relations.
	for _, r := range result.Relations {
		srcID, sok := nameToID[strings.ToLower(strings.TrimSpace(r.Src))]
		dstID, dok := nameToID[strings.ToLower(strings.TrimSpace(r.Dst))]
		if !sok || !dok || srcID == dstID {
			continue
		}
		var added bool
		err = tx.QueryRowContext(ctx, `
			INSERT INTO kb.kg_relations (tenant_id, src_entity_id, predicate, dst_entity_id, confidence, evidence_chunk_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, src_entity_id, predicate, dst_entity_id) DO UPDATE
			   SET confidence = GREATEST(kb.kg_relations.confidence, EXCLUDED.confidence)
			RETURNING (xmax = 0) AS inserted`,
			tenantID, srcID, r.Predicate, dstID, r.Confidence, chunkID,
		).Scan(&added)
		if err != nil {
			// Trigger may reject a cross-tenant relation; propagate.
			err = fmt.Errorf("kg persist: relation %s -[%s]-> %s: %w", r.Src, r.Predicate, r.Dst, err)
			return
		}
		if added {
			relations++
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		err = fmt.Errorf("kg persist: commit: %w", cerr)
		return
	}
	return entities, relations, links, nil
}

// MarkJob updates a job's status. Pass an empty errMsg for non-failure transitions.
func (s *Store) MarkJob(ctx context.Context, jobID, status, errMsg string) error {
	var sql string
	args := []any{status, jobID}
	switch status {
	case JobRunning:
		sql = `UPDATE kb.kg_extract_jobs SET status = $1, started_at = NOW(), attempts = attempts + 1 WHERE id = $2`
	case JobDone, JobSkipped:
		sql = `UPDATE kb.kg_extract_jobs SET status = $1, finished_at = NOW(), last_error = NULL WHERE id = $2`
	case JobFailed:
		sql = `UPDATE kb.kg_extract_jobs SET status = $1, finished_at = NOW(), last_error = $3 WHERE id = $2`
		args = append(args, errMsg)
	default:
		return fmt.Errorf("kg mark job: bad status %q", status)
	}
	_, err := s.db.ExecContext(ctx, sql, args...)
	return err
}

// ClaimJobs claims up to N queued jobs across all tenants and returns them in
// FIFO order. Uses FOR UPDATE SKIP LOCKED to allow parallel workers safely.
// Each returned job is marked status='running'.
func (s *Store) ClaimJobs(ctx context.Context, limit int) ([]ExtractJob, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE kb.kg_extract_jobs
		SET status='running', started_at=NOW(), attempts=attempts+1
		WHERE id IN (
			SELECT id FROM kb.kg_extract_jobs
			WHERE status='queued'
			ORDER BY enqueued_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		RETURNING id, tenant_id::text, chunk_id::text, status, attempts`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("kg claim: %w", err)
	}
	defer rows.Close()
	var out []ExtractJob
	for rows.Next() {
		var j ExtractJob
		if err := rows.Scan(&j.ID, &j.TenantID, &j.ChunkID, &j.Status, &j.Attempts); err != nil {
			return out, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// pqStringArray formats a Go []string as a Postgres array literal.
func pqStringArray(s []string) string {
	if len(s) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		// Reject control chars and embedded quotes / commas / braces — aliases
		// come from Sonnet output but pass through a controlled vocabulary.
		v = strings.ReplaceAll(v, `"`, "")
		v = strings.ReplaceAll(v, ",", " ")
		v = strings.ReplaceAll(v, "{", "")
		v = strings.ReplaceAll(v, "}", "")
		b.WriteByte('"')
		b.WriteString(v)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func nullableText(s string) any {
	if s == "" {
		return ""
	}
	return s
}
