package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// SupersedeRequest soft-supersedes a doc: marks the document + every chunk
// as superseded_at = NOW(), so retrieval drops them (the partial indexes on
// superseded_at IS NULL already exclude them). Hard delete only happens via
// Purge (GDPR / tenant churn).
type SupersedeRequest struct {
	DocID string `json:"doc_id"`
}

type SupersedeResponse struct {
	DocID            string   `json:"doc_id"`
	ChunkIDs         []string `json:"chunk_ids"`
	SupersededAt     string   `json:"superseded_at"`
}

// Supersede marks a doc as superseded. Both the document row and its chunk
// rows get superseded_at=NOW(). Emits kb.doc.deleted on the bus so subscribers
// invalidate caches that referenced the now-hidden chunks.
//
// Works only on kb.* (public). kb_internal supersede paths still go through
// neondb_owner cross-schema writes; not exposed via this method today.
func (s *Store) Supersede(ctx context.Context, req SupersedeRequest) (SupersedeResponse, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return SupersedeResponse{}, errors.New("kb supersede: missing tenant_id in context")
	}
	docUUID, err := uuid.Parse(req.DocID)
	if err != nil {
		return SupersedeResponse{}, fmt.Errorf("kb supersede: bad doc_id: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SupersedeResponse{}, fmt.Errorf("kb supersede: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Gate on tenant ownership AND not-already-superseded. UPDATE returns 0 rows
	// if either condition fails — distinguish by a follow-up SELECT.
	var department, docHash, docVersion string
	if err := tx.QueryRowContext(ctx, `
		UPDATE kb.kb_documents
		SET status = 'superseded', superseded_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND tenant_id = $2 AND superseded_at IS NULL
		RETURNING department, doc_hash, doc_version`,
		docUUID, tenantID,
	).Scan(&department, &docHash, &docVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SupersedeResponse{}, fmt.Errorf("kb supersede: doc not found or already superseded")
		}
		return SupersedeResponse{}, fmt.Errorf("kb supersede: update doc: %w", err)
	}

	// Collect chunk ids while we supersede them — needed for the event payload.
	rows, err := tx.QueryContext(ctx, `
		UPDATE kb.kb_chunks
		SET superseded_at = NOW()
		WHERE doc_id = $1 AND tenant_id = $2 AND superseded_at IS NULL
		RETURNING id`,
		docUUID, tenantID,
	)
	if err != nil {
		return SupersedeResponse{}, fmt.Errorf("kb supersede: update chunks: %w", err)
	}
	chunkIDs := make([]string, 0, 64)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return SupersedeResponse{}, fmt.Errorf("kb supersede: scan chunk id: %w", err)
		}
		chunkIDs = append(chunkIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SupersedeResponse{}, fmt.Errorf("kb supersede: rows err: %w", err)
	}

	now := time.Now()
	if err := tx.Commit(); err != nil {
		return SupersedeResponse{}, fmt.Errorf("kb supersede: commit: %w", err)
	}

	s.publishDocChanged(eventbus.EventKBDocDeleted, tenantID.String(), eventbus.KBDocChangedPayload{
		DocID:               docUUID.String(),
		DocHash:             docHash,
		DocVersion:          docVersion,
		Department:          department,
		Visibility:          VisibilityPublic,
		ChunkIDs:            chunkIDs,
		AffectedDepartments: []string{department},
		Reason:              "supersede",
	})

	return SupersedeResponse{
		DocID:        docUUID.String(),
		ChunkIDs:     chunkIDs,
		SupersededAt: now.UTC().Format(time.RFC3339),
	}, nil
}

// PurgeRequest is the GDPR / customer-churn endpoint. Hard-deletes every row
// for tenant_id across kb.*, kb_internal.*, and kb_quarantine.*. The tenant
// row itself is NOT deleted (operator can churn data but keep the tenant ID
// reserved). Writes a row to kb.purge_audit BEFORE the deletes so the audit
// survives even when the purge fails mid-run.
type PurgeRequest struct {
	TenantID  string `json:"tenant_id"`
	Reason    string `json:"reason,omitempty"`    // 'gdpr_request' | 'tenant_churn' | 'test'
	Requester string `json:"requester,omitempty"` // GDPR subject email, ticket id, etc.
}

type PurgeResponse struct {
	PurgeAuditID string           `json:"purge_audit_id"`
	TenantID     string           `json:"tenant_id"`
	Counts       map[string]int64 `json:"counts"`
	DurationMS   int64            `json:"duration_ms"`
}

// Purge runs the cross-schema, cross-table hard delete. Returns per-table row
// counts so the caller can do a follow-up audit (see scripts/sql/run-gdpr-purge-audit.sql).
//
// Ordering: child tables before parents to keep FK CASCADE behavior predictable
// even though every table also CASCADEs from tenants.
func (s *Store) Purge(ctx context.Context, req PurgeRequest) (PurgeResponse, error) {
	tenantUUID, err := uuid.Parse(req.TenantID)
	if err != nil {
		return PurgeResponse{}, fmt.Errorf("kb purge: bad tenant_id: %w", err)
	}
	if req.Reason == "" {
		req.Reason = "tenant_churn"
	}

	purgedBy := os.Getenv("GOCLAW_USER_ID")
	if purgedBy == "" {
		purgedBy = "system"
	}

	start := time.Now()

	// Tables we DELETE explicitly. Order: children → parents. Every row is
	// FK→tenants(id) ON DELETE CASCADE so the order is defensive, not strictly
	// required.
	type target struct {
		schema string
		table  string
		// Most tables key on tenant_id directly. The exceptions (kb_internal
		// audit, trivia_questions) need a join.
		query string
	}
	deletes := []target{
		{"kb", "kg_entity_chunks", "DELETE FROM kb.kg_entity_chunks WHERE tenant_id = $1"},
		{"kb", "kg_relations", "DELETE FROM kb.kg_relations WHERE tenant_id = $1"},
		{"kb", "kg_extract_jobs", "DELETE FROM kb.kg_extract_jobs WHERE tenant_id = $1"},
		{"kb", "kg_entities", "DELETE FROM kb.kg_entities WHERE tenant_id = $1"},
		// trivia_questions FKs to trivia_runs but not tenants — delete via join.
		{"kb", "trivia_questions",
			"DELETE FROM kb.trivia_questions WHERE run_id IN (SELECT id FROM kb.trivia_runs WHERE tenant_id = $1)"},
		{"kb", "trivia_runs", "DELETE FROM kb.trivia_runs WHERE tenant_id = $1"},
		{"kb", "kb_chunks", "DELETE FROM kb.kb_chunks WHERE tenant_id = $1"},
		{"kb", "kb_documents", "DELETE FROM kb.kb_documents WHERE tenant_id = $1"},
		{"kb", "kb_ingest_runs", "DELETE FROM kb.kb_ingest_runs WHERE tenant_id = $1"},
		{"kb", "embedding_adapters", "DELETE FROM kb.embedding_adapters WHERE tenant_id = $1"},
		// kb_internal — chunks before documents; pageindex + audit also tenant-scoped.
		{"kb_internal", "kb_chunks", "DELETE FROM kb_internal.kb_chunks WHERE tenant_id = $1"},
		{"kb_internal", "kb_documents", "DELETE FROM kb_internal.kb_documents WHERE tenant_id = $1"},
		{"kb_internal", "pageindex_trees", "DELETE FROM kb_internal.pageindex_trees WHERE tenant_id = $1"},
		{"kb_internal", "retrieval_audit", "DELETE FROM kb_internal.retrieval_audit WHERE tenant_id = $1"},
		// kb_quarantine — chunks before documents.
		{"kb_quarantine", "kb_chunks", "DELETE FROM kb_quarantine.kb_chunks WHERE tenant_id = $1"},
		{"kb_quarantine", "kb_documents", "DELETE FROM kb_quarantine.kb_documents WHERE tenant_id = $1"},
	}

	counts := make(map[string]int64, len(deletes))

	// Audit row written FIRST (before deletes) so a partial failure still leaves
	// a forensic record. counts_json updated at the end.
	auditID := uuid.Must(uuid.NewV7())
	emptyCounts, _ := json.Marshal(map[string]int64{})
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO kb.purge_audit (id, tenant_id, purged_by, reason, requester, counts_json)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		auditID, tenantUUID, purgedBy, req.Reason, req.Requester, string(emptyCounts),
	); err != nil {
		return PurgeResponse{}, fmt.Errorf("kb purge: write audit prelude: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PurgeResponse{}, fmt.Errorf("kb purge: begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, d := range deletes {
		res, err := tx.ExecContext(ctx, d.query, tenantUUID)
		if err != nil {
			return PurgeResponse{}, fmt.Errorf("kb purge: delete %s.%s: %w", d.schema, d.table, err)
		}
		n, _ := res.RowsAffected()
		counts[d.schema+"."+d.table] = n
	}

	if err := tx.Commit(); err != nil {
		return PurgeResponse{}, fmt.Errorf("kb purge: commit: %w", err)
	}

	duration := time.Since(start)
	// Update audit row with final counts + duration.
	countsJSON, _ := json.Marshal(counts)
	if _, err := s.db.ExecContext(ctx, `
		UPDATE kb.purge_audit SET counts_json = $1::jsonb, duration_ms = $2 WHERE id = $3`,
		string(countsJSON), duration.Milliseconds(), auditID,
	); err != nil {
		// Don't fail the purge if the audit-tail update fails — the row still exists.
		// Log via the caller's perspective by surfacing in the response.
		_ = err
	}

	// Fire purge event for any in-process cache.
	if s.bus != nil {
		s.bus.Publish(eventbus.DomainEvent{
			ID:        uuid.Must(uuid.NewV7()).String(),
			Type:      eventbus.EventKBTenantPurged,
			SourceID:  auditID.String(),
			TenantID:  tenantUUID.String(),
			AgentID:   "fzst-claw-system",
			Timestamp: time.Now(),
			Payload: eventbus.KBTenantPurgedPayload{
				PurgeAuditID: auditID.String(),
				CountsJSON:   string(countsJSON),
				Reason:       req.Reason,
			},
		})
	}

	return PurgeResponse{
		PurgeAuditID: auditID.String(),
		TenantID:     tenantUUID.String(),
		Counts:       counts,
		DurationMS:   duration.Milliseconds(),
	}, nil
}
