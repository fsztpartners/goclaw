package kb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
	"github.com/nextlevelbuilder/goclaw/internal/retrieval"
	"github.com/nextlevelbuilder/goclaw/internal/retrieval/pagerank"
	"github.com/nextlevelbuilder/goclaw/internal/router"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Store is the DB-facing API for the KB package. It is *not* GoClaw's MemoryStore — KB
// chunks live in their own schemas (kb / kb_internal), bypass dreaming/episodic/semantic
// workers, and use tenant_id (not agent_id) as the partition key.
type Store struct {
	db       *sql.DB
	reranker retrieval.Reranker
	// router decides which retrieval strategy to run for a given query.
	// nil → always hybrid (Phase 1/2 behavior).
	router *router.Router
	// hipporag is the Phase 5 PPR-based multi-hop retriever. nil disables
	// the hipporag route (caller falls back to hybrid).
	hipporag *pagerank.HippoRAG
	// bus publishes Phase 8 coherence events (doc.updated, doc.deleted, …).
	// nil disables publishing — handlers are best-effort, never fail the request.
	bus eventbus.DomainEventBus
}

// NewStore returns a Store wired to the given *sql.DB. The reranker is picked
// from env (see retrieval.NewRerankerFromEnv).
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, reranker: retrieval.NewRerankerFromEnv()}
}

// WithReranker overrides the reranker (for tests).
func (s *Store) WithReranker(r retrieval.Reranker) *Store {
	s.reranker = r
	return s
}

// WithRouter wires the intent-classifier-driven router (Phase 5). Pass nil to disable.
func (s *Store) WithRouter(r *router.Router) *Store {
	s.router = r
	return s
}

// WithHippoRAG wires the PPR retriever (Phase 5). Pass nil to fall back to hybrid.
func (s *Store) WithHippoRAG(h *pagerank.HippoRAG) *Store {
	s.hipporag = h
	return s
}

// WithEventBus wires Phase 8 coherence event publishing. Pass nil to disable.
// Events are emitted post-commit, fire-and-forget — bus errors never fail the
// ingest/supersede/purge request.
func (s *Store) WithEventBus(b eventbus.DomainEventBus) *Store {
	s.bus = b
	return s
}

// Ingest inserts a document + its chunks. Returns the IDs assigned. Caller-supplied
// embeddings must already be EmbeddingDim long; we do NOT call any embedding provider here.
func (s *Store) Ingest(ctx context.Context, req IngestRequest) (IngestResponse, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return IngestResponse{}, errors.New("kb: missing tenant_id in context")
	}

	if err := validateIngest(req); err != nil {
		return IngestResponse{}, err
	}

	schema, err := schemaFor(req.Visibility)
	if err != nil {
		return IngestResponse{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. ingest_run row — written to public kb.kb_ingest_runs BEFORE we drop privileges
	// to kb_internal_user (that role can't write to the public kb schema).
	runID := uuid.Must(uuid.NewV7())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kb.kb_ingest_runs (id, tenant_id, visibility, started_at, status)
		 VALUES ($1, $2, $3, NOW(), $4)`,
		runID, tenantID, req.Visibility, RunStatusRunning,
	); err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: insert run: %w", err)
	}

	// kb_internal requires (a) a non-BYPASSRLS role and (b) the tenant GUC,
	// both set on the same transaction. The default DB owner has rolbypassrls=true
	// on Neon, which would silently defeat the RLS policies in 000059+000060.
	// SET LOCAL ROLE is issued AFTER the ingest_run INSERT so we still have
	// neondb_owner privileges when writing to the public kb schema.
	if req.Visibility == VisibilityInternal {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE kb_internal_user"); err != nil {
			return IngestResponse{}, fmt.Errorf("kb ingest: set role: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return IngestResponse{}, fmt.Errorf("kb ingest: set tenant guc: %w", err)
		}
	}

	// 2. document row — UPSERT on (tenant_id, doc_hash).
	docID := uuid.Must(uuid.NewV7())
	docVersion := req.Doc.DocVersion
	if docVersion == "" {
		docVersion = "1"
	}

	insertDocSQL := fmt.Sprintf(`
		INSERT INTO %s.kb_documents (id, tenant_id, department, visibility, title, source_uri, source_kind_meta, doc_hash, doc_version, ingested_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (tenant_id, doc_hash)
		DO UPDATE SET title = EXCLUDED.title, source_uri = EXCLUDED.source_uri, doc_version = EXCLUDED.doc_version
		RETURNING id`, schema)

	if err := tx.QueryRowContext(ctx, insertDocSQL,
		docID, tenantID, req.Department, req.Visibility, req.Doc.Title, req.Doc.SourceURI,
		req.Doc.SourceKindMeta, req.Doc.DocHash, docVersion,
	).Scan(&docID); err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: upsert document: %w", err)
	}

	// Idempotency: when re-ingesting a doc (same tenant_id + doc_hash), wipe old chunks
	// before inserting new ones. Without this, repeated calls accumulate duplicate
	// chunks at the same chunk_idx — which we hit during Phase 1 smoke (see learnings).
	deleteChunksSQL := fmt.Sprintf(`DELETE FROM %s.kb_chunks WHERE doc_id = $1`, schema)
	if _, err := tx.ExecContext(ctx, deleteChunksSQL, docID); err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: clear existing chunks: %w", err)
	}

	// 3. chunk rows
	chunkIDs := make([]string, 0, len(req.Chunks))
	insertChunkSQL := fmt.Sprintf(`
		INSERT INTO %s.kb_chunks
		  (id, tenant_id, doc_id, department, visibility, source_kind, provenance_trust, role_tags,
		   doc_hash, doc_version, chunk_idx, page, timestamp_sec,
		   embedding_model_id, embedding_adapter_id, embedding,
		   text, pipeline_version)
		VALUES
		  ($1, $2, $3, $4, $5, $6, $7, $8,
		   $9, $10, $11, $12, $13,
		   $14, $15, $16::vector,
		   $17, $18)`, schema)

	for _, c := range req.Chunks {
		if len(c.Embedding) != EmbeddingDim {
			return IngestResponse{}, fmt.Errorf("kb ingest: chunk_idx=%d embedding has %d dims, expected %d", c.ChunkIdx, len(c.Embedding), EmbeddingDim)
		}
		chunkID := uuid.Must(uuid.NewV7())
		var adapter sql.NullString
		if c.EmbeddingAdapter != "" {
			adapter = sql.NullString{String: c.EmbeddingAdapter, Valid: true}
		}

		if _, err := tx.ExecContext(ctx, insertChunkSQL,
			chunkID, tenantID, docID, req.Department, req.Visibility, SourceKindDocumentChunk,
			req.ProvenanceTrust, pqStringArray(req.RoleTags),
			req.Doc.DocHash, docVersion, c.ChunkIdx, c.Page, c.TimestampSec,
			c.EmbeddingModelID, adapter, vectorLiteral(c.Embedding),
			c.Text, req.PipelineVersion,
		); err != nil {
			return IngestResponse{}, fmt.Errorf("kb ingest: insert chunk_idx=%d: %w", c.ChunkIdx, err)
		}
		chunkIDs = append(chunkIDs, chunkID.String())
	}

	// 4. close out ingest_run as ok
	if _, err := tx.ExecContext(ctx,
		`UPDATE kb.kb_ingest_runs
		 SET status = $1, finished_at = NOW(), chunk_count = $2, doc_id = $3
		 WHERE id = $4`,
		RunStatusOK, len(req.Chunks), docID, runID,
	); err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: finalize run: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return IngestResponse{}, fmt.Errorf("kb ingest: commit: %w", err)
	}

	// Phase 8: publish kb.doc.updated so coherence subscribers can invalidate
	// caches + queue re-embed work. Fire-and-forget — best effort.
	s.publishDocChanged(eventbus.EventKBDocUpdated, tenantID.String(), eventbus.KBDocChangedPayload{
		DocID:               docID.String(),
		DocHash:             req.Doc.DocHash,
		DocVersion:          docVersion,
		Department:          req.Department,
		Visibility:          req.Visibility,
		ChunkIDs:            chunkIDs,
		AffectedDepartments: []string{req.Department},
		Reason:              "ingest",
	})

	return IngestResponse{
		DocID:          docID.String(),
		ChunkIDs:       chunkIDs,
		IngestRunID:    runID.String(),
		SkippedWorkers: []string{"dreaming", "episodic", "semantic"},
	}, nil
}

// publishDocChanged emits a KB coherence event. No-op when no bus is wired.
func (s *Store) publishDocChanged(t eventbus.EventType, tenantID string, p eventbus.KBDocChangedPayload) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(eventbus.DomainEvent{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Type:      t,
		SourceID:  p.DocID + ":" + string(t),
		TenantID:  tenantID,
		AgentID:   "fzst-claw-system",
		Timestamp: time.Now(),
		Payload:   p,
	})
}

// Retrieve runs hybrid vector + BM25 retrieval, merges with RRF, and (optionally)
// reranks the top-N pool down to top-K.
func (s *Store) Retrieve(ctx context.Context, req RetrieveRequest) (RetrieveResponse, error) {
	start := time.Now()

	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return RetrieveResponse{}, errors.New("kb: missing tenant_id in context")
	}

	if req.K <= 0 {
		req.K = 5
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = VisibilityPublic
	}
	if len(req.QueryEmbedding) != EmbeddingDim {
		return RetrieveResponse{}, fmt.Errorf("kb retrieve: query_embedding has %d dims, expected %d", len(req.QueryEmbedding), EmbeddingDim)
	}
	if err := router.ValidateRole(req.Role); err != nil {
		return RetrieveResponse{}, err
	}

	// Phase 5: router decides which retrieval strategy to run. RouteOverride
	// bypasses the classifier (eval scripts use it for A/B). When the chosen
	// strategy is hipporag and we have a HippoRAG retriever wired, dispatch.
	// Otherwise fall through to the hybrid SQL path below.
	chosen := router.Route{Strategy: "hybrid", Reason: "default"}
	if req.RouteOverride != "" {
		chosen = router.Route{Strategy: req.RouteOverride, Reason: "override"}
	} else if s.router != nil {
		chosen = s.router.Pick(ctx, req.Query)
	}
	if chosen.Strategy == "hipporag" && s.hipporag != nil && visibility == VisibilityPublic {
		resp, err := s.retrieveHippoRAG(ctx, req, chosen, start)
		if err == nil {
			return resp, nil
		}
		// Fall back to hybrid on PPR failure — never let the router cost us availability.
		chosen = router.Route{Strategy: "hybrid", Reason: "hipporag_fallback:" + err.Error(), Classification: chosen.Classification}
	}

	// Default rerank policy: ON for sales role, OFF otherwise. Caller can force
	// either way via req.Rerank. RERANK_PROVIDER=off short-circuits regardless.
	wantRerank := req.Role == router.RoleSales
	if req.Rerank != nil {
		wantRerank = *req.Rerank
	}
	if s.reranker == nil || s.reranker.Provider() == "off" {
		wantRerank = false
	}
	// When reranking, pull a larger pool from SQL so the reranker has signal.
	sqlLimit := req.K
	if wantRerank && RetrievePoolSize > sqlLimit {
		sqlLimit = RetrievePoolSize
	}

	schema, err := schemaFor(visibility)
	if err != nil {
		return RetrieveResponse{}, err
	}

	// For kb_internal we run inside a tx with the GUC set; for kb (public) we can use the pool directly.
	var (
		rowsExec func(string, ...any) (*sql.Rows, error)
		commit   func() error
	)
	if visibility == VisibilityInternal {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return RetrieveResponse{}, fmt.Errorf("kb retrieve: begin tx: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE kb_internal_user"); err != nil {
			return RetrieveResponse{}, fmt.Errorf("kb retrieve: set role: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return RetrieveResponse{}, fmt.Errorf("kb retrieve: set tenant guc: %w", err)
		}
		if tags := strings.Join(req.RoleTags, ","); tags != "" {
			if _, err := tx.ExecContext(ctx, "SELECT set_config('app.role_tags', $1, true)", tags); err != nil {
				return RetrieveResponse{}, fmt.Errorf("kb retrieve: set role_tags guc: %w", err)
			}
		}
		rowsExec = func(q string, args ...any) (*sql.Rows, error) { return tx.QueryContext(ctx, q, args...) }
		commit = tx.Commit
	} else {
		rowsExec = func(q string, args ...any) (*sql.Rows, error) { return s.db.QueryContext(ctx, q, args...) }
		commit = func() error { return nil }
	}

	// One CTE-based query that does vector top-50, BM25 top-50, and RRF in SQL.
	// rrf_k = 60 (industry default). We rely on indexes (HNSW + GIN) to make each side fast.
	const rrfK = 60
	q := fmt.Sprintf(`
WITH params AS (
    SELECT $1::vector AS qvec, plainto_tsquery('simple', $2) AS qts
),
vec AS (
    SELECT c.id,
           ROW_NUMBER() OVER (ORDER BY c.embedding <=> p.qvec ASC) AS rk
    FROM %[1]s.kb_chunks c, params p
    WHERE c.tenant_id = $3
      AND c.superseded_at IS NULL
      AND ($4::text IS NULL OR c.department = $4)
      AND c.embedding_model_id = $5
    ORDER BY c.embedding <=> p.qvec ASC
    LIMIT 50
),
lex AS (
    SELECT c.id,
           ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.text_tsv, p.qts) DESC) AS rk
    FROM %[1]s.kb_chunks c, params p
    WHERE c.tenant_id = $3
      AND c.superseded_at IS NULL
      AND ($4::text IS NULL OR c.department = $4)
      AND c.text_tsv @@ p.qts
    ORDER BY ts_rank_cd(c.text_tsv, p.qts) DESC
    LIMIT 50
),
merged AS (
    SELECT id,
           COALESCE(vec.rk, 0)               AS vec_rk,
           COALESCE(lex.rk, 0)               AS lex_rk,
           COALESCE(1.0 / (%[2]d + vec.rk), 0) +
           COALESCE(1.0 / (%[2]d + lex.rk), 0) AS rrf_score
    FROM vec FULL OUTER JOIN lex USING (id)
)
SELECT c.id, c.doc_id, c.text, m.vec_rk, m.lex_rk, m.rrf_score,
       c.page, c.timestamp_sec, c.provenance_trust,
       d.title, d.source_uri
FROM merged m
JOIN %[1]s.kb_chunks c ON c.id = m.id
LEFT JOIN %[1]s.kb_documents d ON d.id = c.doc_id
ORDER BY m.rrf_score DESC
LIMIT $6`, schema, rrfK)

	var deptArg sql.NullString
	if req.Department != "" {
		deptArg = sql.NullString{String: req.Department, Valid: true}
	}

	rows, err := rowsExec(q, vectorLiteral(req.QueryEmbedding), req.Query, tenantID, deptArg, req.EmbeddingModelID, sqlLimit)
	if err != nil {
		return RetrieveResponse{}, fmt.Errorf("kb retrieve: query: %w", err)
	}
	defer rows.Close()

	out := make([]RetrieveChunk, 0, req.K)
	for rows.Next() {
		var (
			id, docID, text                          string
			vecRk, lexRk                             int
			rrf                                      float64
			page                                     sql.NullInt64
			tsSec                                    sql.NullFloat64
			trust, title, sourceURI                  sql.NullString
		)
		if err := rows.Scan(&id, &docID, &text, &vecRk, &lexRk, &rrf, &page, &tsSec, &trust, &title, &sourceURI); err != nil {
			return RetrieveResponse{}, fmt.Errorf("kb retrieve: scan: %w", err)
		}
		prov := map[string]any{}
		if title.Valid {
			prov["title"] = title.String
		}
		if sourceURI.Valid {
			prov["source_uri"] = sourceURI.String
		}
		if page.Valid {
			prov["page"] = page.Int64
		}
		if tsSec.Valid {
			prov["timestamp_sec"] = tsSec.Float64
		}
		if trust.Valid {
			prov["trust"] = trust.String
		}
		out = append(out, RetrieveChunk{
			ChunkID:    id,
			DocID:      docID,
			Text:       text,
			VecRank:    vecRk,
			LexRank:    lexRk,
			RRFScore:   rrf,
			Provenance: prov,
		})
	}
	if err := rows.Err(); err != nil {
		return RetrieveResponse{}, fmt.Errorf("kb retrieve: rows: %w", err)
	}
	if err := commit(); err != nil {
		return RetrieveResponse{}, fmt.Errorf("kb retrieve: commit ro tx: %w", err)
	}

	// Default route: hybrid. Rerank flips it to hybrid+rerank:<provider>.
	route := "hybrid"
	provider := ""
	if wantRerank && len(out) > 0 {
		cands := make([]retrieval.Candidate, len(out))
		byID := make(map[string]int, len(out))
		for i, c := range out {
			cands[i] = retrieval.Candidate{ID: c.ChunkID, Text: c.Text}
			byID[c.ChunkID] = i
		}
		ranked, rerr := s.reranker.Rerank(ctx, req.Query, cands, req.K)
		if rerr != nil {
			// Don't fail the whole retrieve on a reranker outage — log + fall
			// back to RRF order. Phase 2 prioritises uptime over rerank quality.
			route = "hybrid+rerank_failed:" + s.reranker.Provider()
			if len(out) > req.K {
				out = out[:req.K]
			}
		} else {
			provider = s.reranker.Provider()
			route = "hybrid+rerank:" + provider
			reordered := make([]RetrieveChunk, 0, len(ranked))
			for _, r := range ranked {
				idx, ok := byID[r.ID]
				if !ok {
					continue
				}
				chunk := out[idx]
				score := r.Score
				chunk.RerankScore = &score
				reordered = append(reordered, chunk)
			}
			out = reordered
		}
	} else if len(out) > req.K {
		out = out[:req.K]
	}

	resp := RetrieveResponse{
		Chunks:           out,
		EmbeddingModelID: req.EmbeddingModelID,
		LatencyMS:        time.Since(start).Milliseconds(),
		Route:            route,
		RerankProvider:   provider,
	}
	if visibility == VisibilityInternal {
		s.writeRetrievalAudit(ctx, tenantID, req, resp)
	}
	return resp, nil
}

// writeRetrievalAudit inserts one row into kb_internal.retrieval_audit.
// Errors are intentionally swallowed — audit failure must never fail a retrieve.
func (s *Store) writeRetrievalAudit(ctx context.Context, tenantID uuid.UUID, req RetrieveRequest, resp RetrieveResponse) {
	agentID := req.AgentID
	if agentID == "" {
		agentID = "fzst-claw-system"
	}
	chunkIDs := make([]string, len(resp.Chunks))
	for i, c := range resp.Chunks {
		chunkIDs[i] = c.ChunkID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE kb_internal_user"); err != nil {
		return
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return
	}
	rerankProvider := sql.NullString{String: resp.RerankProvider, Valid: resp.RerankProvider != ""}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kb_internal.retrieval_audit
		  (tenant_id, agent_id, query, role, role_tags, chunk_ids, route, rerank_provider, latency_ms)
		VALUES ($1, $2, $3, $4, $5::text[], $6::uuid[], $7, $8, $9)`,
		tenantID, agentID, req.Query, req.Role,
		pqStringArray(req.RoleTags), pagerankUUIDArray(chunkIDs),
		resp.Route, rerankProvider, resp.LatencyMS,
	); err != nil {
		return
	}
	_ = tx.Commit()
}

// retrieveHippoRAG runs the PPR retrieval and fetches chunk text + provenance.
// Returns an error so the caller can fall back to hybrid gracefully.
func (s *Store) retrieveHippoRAG(ctx context.Context, req RetrieveRequest, chosen router.Route, start time.Time) (RetrieveResponse, error) {
	// Seeds come from the classifier's extracted entities. If the override path
	// skipped the classifier, fall back to a tokenized version of the query.
	seeds := chosen.Classification.Entities
	if len(seeds) == 0 {
		seeds = seedsFromQuery(req.Query)
	}
	if len(seeds) == 0 {
		return RetrieveResponse{}, fmt.Errorf("hipporag: no seed entities for query %q", req.Query)
	}

	// Pool size for hipporag is RetrievePoolSize so the reranker (if on) has signal.
	K := req.K
	pool := K
	wantRerank := s.reranker != nil && s.reranker.Provider() != "off"
	if req.Rerank != nil {
		wantRerank = *req.Rerank
	}
	if wantRerank && RetrievePoolSize > pool {
		pool = RetrievePoolSize
	}
	results, err := s.hipporag.Retrieve(ctx, seeds, pagerank.RetrieveOpts{
		K:           pool,
		IncludeCooc: true,
	})
	if err != nil {
		return RetrieveResponse{}, fmt.Errorf("hipporag retrieve: %w", err)
	}
	if len(results) == 0 {
		return RetrieveResponse{}, fmt.Errorf("hipporag: empty result")
	}

	// Fetch chunk text + provenance for the returned chunk_ids in one query.
	ids := make([]string, len(results))
	scoreByID := make(map[string]float64, len(results))
	for i, r := range results {
		ids[i] = r.ChunkID
		scoreByID[r.ChunkID] = r.Score
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id::text, c.doc_id::text, c.text, c.page, c.timestamp_sec, c.provenance_trust,
		       d.title, d.source_uri
		FROM kb.kb_chunks c LEFT JOIN kb.kb_documents d ON d.id = c.doc_id
		WHERE c.id = ANY($1::uuid[]) AND c.tenant_id = $2 AND c.superseded_at IS NULL`,
		pagerankUUIDArray(ids), store.TenantIDFromContext(ctx),
	)
	if err != nil {
		return RetrieveResponse{}, fmt.Errorf("hipporag: fetch chunks: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]RetrieveChunk, len(ids))
	for rows.Next() {
		var (
			id, docID, text                  string
			page                             sql.NullInt64
			tsSec                            sql.NullFloat64
			trust, title, sourceURI          sql.NullString
		)
		if err := rows.Scan(&id, &docID, &text, &page, &tsSec, &trust, &title, &sourceURI); err != nil {
			return RetrieveResponse{}, fmt.Errorf("hipporag: scan: %w", err)
		}
		prov := map[string]any{}
		if title.Valid {
			prov["title"] = title.String
		}
		if sourceURI.Valid {
			prov["source_uri"] = sourceURI.String
		}
		if page.Valid {
			prov["page"] = page.Int64
		}
		if tsSec.Valid {
			prov["timestamp_sec"] = tsSec.Float64
		}
		if trust.Valid {
			prov["trust"] = trust.String
		}
		byID[id] = RetrieveChunk{
			ChunkID:    id,
			DocID:      docID,
			Text:       text,
			RRFScore:   scoreByID[id],
			Provenance: prov,
		}
	}

	// Preserve PPR rank order.
	out := make([]RetrieveChunk, 0, len(results))
	for _, r := range results {
		if c, ok := byID[r.ChunkID]; ok {
			out = append(out, c)
		}
	}

	route := "hipporag"
	provider := ""
	if wantRerank && len(out) > 0 {
		cands := make([]retrieval.Candidate, len(out))
		bm := make(map[string]int, len(out))
		for i, c := range out {
			cands[i] = retrieval.Candidate{ID: c.ChunkID, Text: c.Text}
			bm[c.ChunkID] = i
		}
		ranked, rerr := s.reranker.Rerank(ctx, req.Query, cands, req.K)
		if rerr != nil {
			route = "hipporag+rerank_failed:" + s.reranker.Provider()
			if len(out) > req.K {
				out = out[:req.K]
			}
		} else {
			provider = s.reranker.Provider()
			route = "hipporag+rerank:" + provider
			reordered := make([]RetrieveChunk, 0, len(ranked))
			for _, r := range ranked {
				idx, ok := bm[r.ID]
				if !ok {
					continue
				}
				chunk := out[idx]
				score := r.Score
				chunk.RerankScore = &score
				reordered = append(reordered, chunk)
			}
			out = reordered
		}
	} else if len(out) > req.K {
		out = out[:req.K]
	}

	return RetrieveResponse{
		Chunks:           out,
		EmbeddingModelID: req.EmbeddingModelID,
		LatencyMS:        time.Since(start).Milliseconds(),
		Route:            route,
		RerankProvider:   provider,
	}, nil
}

// seedsFromQuery is a no-LLM fallback: split the query into stop-filtered tokens
// of length >= 3 and dedupe. Used when the router runs in override mode and
// callers pass a raw query without going through the classifier.
func seedsFromQuery(q string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "from": true,
		"that": true, "this": true, "what": true, "how": true, "why": true,
		"who": true, "where": true, "when": true, "does": true, "did": true,
		"are": true, "can": true, "should": true, "will": true, "would": true,
		"about": true, "between": true, "into": true, "than": true, "then": true,
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tokens {
		if len(t) < 3 || stop[t] {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// pagerankUUIDArray formats []string of UUIDs as a Postgres array literal.
// Duplicates the pagerank package helper to avoid an import cycle.
func pagerankUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		for _, r := range id {
			if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-' {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('}')
	return b.String()
}

// validateIngest checks the request body for invariants the DB CHECKs and RLS rely on.
func validateIngest(req IngestRequest) error {
	switch req.Visibility {
	case VisibilityPublic, VisibilityInternal:
	default:
		return fmt.Errorf("kb ingest: visibility must be 'public' or 'internal', got %q", req.Visibility)
	}
	switch req.ProvenanceTrust {
	case TrustFirstParty, TrustCustomer, TrustCrawled:
	default:
		return fmt.Errorf("kb ingest: provenance_trust must be one of first_party|customer|crawled, got %q", req.ProvenanceTrust)
	}
	if req.Department == "" {
		return errors.New("kb ingest: department is required")
	}
	if req.PipelineVersion == "" {
		return errors.New("kb ingest: pipeline_version is required")
	}
	switch req.Doc.SourceKindMeta {
	case "pdf", "pdf_visual", "video", "web", "upload":
	default:
		return fmt.Errorf("kb ingest: doc.source_kind_meta must be pdf|pdf_visual|video|web|upload, got %q", req.Doc.SourceKindMeta)
	}
	if req.Doc.DocHash == "" {
		return errors.New("kb ingest: doc.doc_hash is required")
	}
	if len(req.Chunks) == 0 {
		return errors.New("kb ingest: chunks must be non-empty")
	}
	return nil
}

func schemaFor(visibility string) (string, error) {
	switch visibility {
	case VisibilityPublic:
		return "kb", nil
	case VisibilityInternal:
		return "kb_internal", nil
	default:
		return "", fmt.Errorf("kb: unknown visibility %q", visibility)
	}
}

// vectorLiteral formats a float32 slice as pgvector's text literal '[a,b,c]'.
// Same algorithm as internal/store/pg/memory_docs.go:vectorToString, duplicated here so
// the kb package has no internal/store/pg dependency.
func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}

// pqStringArray returns a database/sql-friendly representation of a string slice for a
// TEXT[] column. We use the Postgres array literal `{a,b,c}` form — values are not
// caller-controlled here (role_tags come from a controlled vocabulary in fzst-claw), so
// the escaping is conservative: reject quotes and commas.
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
		safe := strings.Map(func(r rune) rune {
			switch r {
			case '"', '\\', ',', '{', '}':
				return -1
			}
			return r
		}, v)
		b.WriteString(safe)
	}
	b.WriteByte('}')
	return b.String()
}
