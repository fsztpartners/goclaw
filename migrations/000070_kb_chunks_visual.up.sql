-- Phase 7 P7.4 — schema slot for ColPali/ColQwen multi-vector page embeddings.
--
-- ColPali represents a page IMAGE as a set of patch vectors (~1030 patches per
-- 1024×1024 page on ColQwen2.5). Retrieval = late-interaction MaxSim between
-- query patches and stored patches. Storage shape options:
--
--   (a) One vector(128) row per patch, ~1030 rows per chunk → 2M+ rows for a
--       2000-page tenant. Cheap per row, expensive to MaxSim.
--   (b) One JSONB-packed multi-vector per page-chunk → 1 row per chunk, but
--       you query via app-side computation (slower).
--   (c) Postgres pg_vector with `halfvec` + an unnest+JOIN MaxSim — pgvector
--       0.7+ supports halfvec, halves storage but still option (a) shape.
--
-- This migration lands the (a)-shape table so the wiring is ready. Choice between
-- (a)/(b)/(c) for the retrieval path is a Phase 7 P7.2 decision once we have
-- a working ColPali endpoint and can benchmark MaxSim latency.
--
-- The table coexists with kb.kb_chunks — same chunk_id (FK), one row per (chunk,
-- patch_idx). Visual queries JOIN kb_chunks_visual ON chunk_id and run MaxSim.

-- pg_vector halfvec (8-byte half-precision floats) — halves storage at minor
-- recall cost. The ColPali paper uses bf16 weights so half-precision is the
-- natural client dtype. If pgvector < 0.7 in this DB, fall back to vector(128).
CREATE TABLE IF NOT EXISTS kb.kb_chunks_visual (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    chunk_id        UUID NOT NULL REFERENCES kb.kb_chunks(id) ON DELETE CASCADE,
    -- 0-based patch index within the page image; ColQwen2.5 default is 1030
    -- patches per 1024×1024 page (32×32 grid + small head).
    patch_idx       INT NOT NULL,
    -- ColPali per-patch embedding. 128-d native; we don't truncate.
    embedding       halfvec(128) NOT NULL,
    -- For convenience — same model id concept as kb.kb_chunks.embedding_model_id.
    embedding_model_id TEXT NOT NULL,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX kb_chunks_visual_chunk_patch_idx
    ON kb.kb_chunks_visual (chunk_id, patch_idx, embedding_model_id);
CREATE INDEX kb_chunks_visual_tenant_idx ON kb.kb_chunks_visual (tenant_id);

-- HNSW on the half-precision vectors. ColPali MaxSim queries do nearest-neighbor
-- per query patch then aggregate; the per-patch ANN index is the fast path.
CREATE INDEX kb_chunks_visual_embedding_hnsw
    ON kb.kb_chunks_visual USING hnsw (embedding halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 64);
