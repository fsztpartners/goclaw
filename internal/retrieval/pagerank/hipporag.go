package pagerank

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// HippoRAG wraps PPR over the kb.kg_* tables, returning chunk_ids ranked by
// passage scores aggregated from PPR entity scores.
type HippoRAG struct {
	db *sql.DB
}

// New returns a HippoRAG retriever.
func New(db *sql.DB) *HippoRAG { return &HippoRAG{db: db} }

// RetrieveOpts controls the HippoRAG retrieve call.
type RetrieveOpts struct {
	K             int       // top-K chunk_ids returned
	Damping       float64   // PPR alpha
	MaxSeeds      int       // cap on seed entities (default 8)
	IncludeCooc   bool      // also add co-occurrence edges (entity_a, entity_b appear in same chunk)
	MaxEdges      int       // cap on total edges (default 20000)
}

// Result is one ranked chunk.
type Result struct {
	ChunkID string
	Score   float64
}

// Retrieve runs HippoRAG-style retrieval given a set of seed entity NAMES
// (typically extracted from the query). Pipeline:
//   1. Resolve seeds to entity_ids in this tenant
//   2. Load 1-hop relation edges + (optional) co-occurrence edges
//   3. Run PPR
//   4. For each chunk in kb.kg_entity_chunks, score = Σ weight(e,c) * ppr(e)
//   5. Return top-K chunks.
func (h *HippoRAG) Retrieve(ctx context.Context, seedNames []string, opts RetrieveOpts) ([]Result, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("hipporag: missing tenant_id")
	}
	if opts.K <= 0 {
		opts.K = 5
	}
	if opts.MaxSeeds <= 0 {
		opts.MaxSeeds = 8
	}
	if opts.MaxEdges <= 0 {
		opts.MaxEdges = 20000
	}
	if len(seedNames) == 0 {
		return nil, nil
	}

	// 1) Resolve seeds.
	seedIDs, err := h.resolveSeeds(ctx, tenantID, seedNames, opts.MaxSeeds)
	if err != nil {
		return nil, err
	}
	if len(seedIDs) == 0 {
		return nil, nil
	}
	seeds := make(map[string]float64, len(seedIDs))
	for _, id := range seedIDs {
		seeds[id] = 1.0
	}

	// 2) Load edges. Strategy: BFS-style — start from seed ids, fetch outbound
	// relations one or two hops, plus co-occurrence neighbors. For Phase 5 we
	// load a 2-hop frontier in one query to avoid round-trips.
	edges, err := h.loadEdges(ctx, tenantID, seedIDs, opts)
	if err != nil {
		return nil, err
	}

	// 3) PPR.
	pprOpts := DefaultOptions()
	if opts.Damping > 0 {
		pprOpts.Damping = opts.Damping
	}
	scores := Run(seeds, edges, pprOpts)
	if len(scores) == 0 {
		return nil, nil
	}

	// 4) Aggregate entity scores → chunk scores via kb.kg_entity_chunks.
	chunkScores, err := h.aggregateToChunks(ctx, tenantID, scores)
	if err != nil {
		return nil, err
	}

	// 5) Top-K.
	ranked := make([]Result, 0, len(chunkScores))
	for c, s := range chunkScores {
		ranked = append(ranked, Result{ChunkID: c, Score: s})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	if len(ranked) > opts.K {
		ranked = ranked[:opts.K]
	}
	return ranked, nil
}

func (h *HippoRAG) resolveSeeds(ctx context.Context, tenantID uuid.UUID, names []string, max int) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	// One query: any entity in this tenant whose normalized_name equals any
	// of the lowercased names OR whose aliases contain a match.
	rows, err := h.db.QueryContext(ctx, `
		SELECT id::text
		FROM kb.kg_entities
		WHERE tenant_id = $1
		  AND (
		      normalized_name = ANY($2::text[])
		   OR aliases && $2::text[]
		  )
		ORDER BY chunk_count DESC
		LIMIT $3`,
		tenantID, normalizeAll(names), max)
	if err != nil {
		return nil, fmt.Errorf("hipporag resolve seeds: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loadEdges returns a 2-hop edge expansion around seedIDs.
func (h *HippoRAG) loadEdges(ctx context.Context, tenantID uuid.UUID, seedIDs []string, opts RetrieveOpts) ([]Edge, error) {
	// 1-hop relation edges (both directions, treat as undirected weighted).
	rows, err := h.db.QueryContext(ctx, `
		WITH frontier AS (
		  SELECT unnest($2::uuid[]) AS id
		),
		hop1 AS (
		  SELECT id FROM frontier
		  UNION
		  SELECT r.dst_entity_id AS id FROM kb.kg_relations r WHERE r.tenant_id=$1 AND r.src_entity_id::text = ANY($2::text[])
		  UNION
		  SELECT r.src_entity_id AS id FROM kb.kg_relations r WHERE r.tenant_id=$1 AND r.dst_entity_id::text = ANY($2::text[])
		),
		-- bound the second hop by the number we got from hop1
		hop2 AS (
		  SELECT DISTINCT r.src_entity_id AS src, r.dst_entity_id AS dst, r.confidence AS w
		  FROM kb.kg_relations r
		  WHERE r.tenant_id=$1
		    AND (r.src_entity_id IN (SELECT id FROM hop1) OR r.dst_entity_id IN (SELECT id FROM hop1))
		  LIMIT $3
		)
		SELECT src::text, dst::text, w FROM hop2`,
		tenantID, pqUUIDArray(seedIDs), opts.MaxEdges,
	)
	if err != nil {
		return nil, fmt.Errorf("hipporag load edges: %w", err)
	}
	defer rows.Close()
	var edges []Edge
	for rows.Next() {
		var src, dst string
		var w float64
		if err := rows.Scan(&src, &dst, &w); err != nil {
			return edges, err
		}
		if w <= 0 {
			w = 1.0
		}
		edges = append(edges, Edge{Src: src, Dst: dst, Weight: w})
		edges = append(edges, Edge{Src: dst, Dst: src, Weight: w})
	}
	if err := rows.Err(); err != nil {
		return edges, err
	}

	// Co-occurrence edges: any two entities sharing a chunk get an edge.
	// Bounded by per-chunk co-occurrence (squared in chunk-entity count).
	if opts.IncludeCooc {
		coocRows, err := h.db.QueryContext(ctx, `
			WITH frontier AS (SELECT unnest($2::uuid[]) AS id),
			seed_chunks AS (
			  SELECT DISTINCT chunk_id
			  FROM kb.kg_entity_chunks
			  WHERE tenant_id=$1 AND entity_id IN (SELECT id FROM frontier)
			),
			pairs AS (
			  SELECT a.entity_id AS src, b.entity_id AS dst, COUNT(*) AS w
			  FROM kb.kg_entity_chunks a
			  JOIN kb.kg_entity_chunks b USING (chunk_id, tenant_id)
			  WHERE a.tenant_id=$1
			    AND a.chunk_id IN (SELECT chunk_id FROM seed_chunks)
			    AND a.entity_id < b.entity_id
			  GROUP BY a.entity_id, b.entity_id
			  LIMIT $3
			)
			SELECT src::text, dst::text, w::float FROM pairs`,
			tenantID, pqUUIDArray(seedIDs), opts.MaxEdges,
		)
		if err != nil {
			return edges, fmt.Errorf("hipporag load cooc: %w", err)
		}
		defer coocRows.Close()
		for coocRows.Next() {
			var src, dst string
			var w float64
			if err := coocRows.Scan(&src, &dst, &w); err != nil {
				return edges, err
			}
			// Co-occurrence edges weighted 0.3× relation edges (relation is a stronger signal).
			weight := 0.3 * w
			edges = append(edges, Edge{Src: src, Dst: dst, Weight: weight})
			edges = append(edges, Edge{Src: dst, Dst: src, Weight: weight})
		}
		if err := coocRows.Err(); err != nil {
			return edges, err
		}
	}

	return edges, nil
}

func (h *HippoRAG) aggregateToChunks(ctx context.Context, tenantID uuid.UUID, entityScores map[string]float64) (map[string]float64, error) {
	if len(entityScores) == 0 {
		return nil, nil
	}
	// Stream rows for entities in the score map. Use a temp-table-free approach
	// by passing two arrays (ids, scores) keyed by index.
	ids := make([]string, 0, len(entityScores))
	for id := range entityScores {
		ids = append(ids, id)
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT ec.entity_id::text, ec.chunk_id::text, ec.weight
		FROM kb.kg_entity_chunks ec
		WHERE ec.tenant_id=$1 AND ec.entity_id = ANY($2::uuid[])`,
		tenantID, pqUUIDArray(ids),
	)
	if err != nil {
		return nil, fmt.Errorf("hipporag aggregate: %w", err)
	}
	defer rows.Close()
	chunkScores := make(map[string]float64)
	for rows.Next() {
		var eid, cid string
		var w float64
		if err := rows.Scan(&eid, &cid, &w); err != nil {
			return chunkScores, err
		}
		chunkScores[cid] += entityScores[eid] * w
	}
	return chunkScores, rows.Err()
}
