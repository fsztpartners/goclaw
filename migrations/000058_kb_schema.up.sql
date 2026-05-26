-- KB (Knowledge Base) schemas + tables.
-- Phase 1 of plans/brand-knowledge-base/ (see fzst-claw repo).
--
-- Two schemas:
--   kb            — visibility = 'public' chunks (default-readable in app layer)
--   kb_internal   — visibility = 'internal' chunks (Postgres RLS in 000059)
--
-- Tables identical in both schemas. Tenant column is tenant_id (REFERENCES tenants(id)),
-- reusing GoClaw's existing tenant infrastructure (000027_tenant_foundation).

CREATE SCHEMA IF NOT EXISTS kb;
CREATE SCHEMA IF NOT EXISTS kb_internal;

-- ============================================================
-- kb_documents — one row per ingested document
-- ============================================================

CREATE TABLE kb.kb_documents (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    department       TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility = 'public'),
    title            TEXT,
    source_uri       TEXT,
    source_kind_meta TEXT NOT NULL CHECK (source_kind_meta IN ('pdf','pdf_visual','video','web','upload')),
    doc_hash         VARCHAR(64) NOT NULL,
    doc_version      INT NOT NULL DEFAULT 1,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX kb_documents_tenant_hash ON kb.kb_documents(tenant_id, doc_hash);
CREATE INDEX        kb_documents_tenant_dept ON kb.kb_documents(tenant_id, department) WHERE superseded_at IS NULL;

CREATE TABLE kb_internal.kb_documents (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    department       TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility = 'internal'),
    title            TEXT,
    source_uri       TEXT,
    source_kind_meta TEXT NOT NULL CHECK (source_kind_meta IN ('pdf','pdf_visual','video','web','upload')),
    doc_hash         VARCHAR(64) NOT NULL,
    doc_version      INT NOT NULL DEFAULT 1,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX kb_internal_documents_tenant_hash ON kb_internal.kb_documents(tenant_id, doc_hash);
CREATE INDEX        kb_internal_documents_tenant_dept ON kb_internal.kb_documents(tenant_id, department) WHERE superseded_at IS NULL;

-- ============================================================
-- kb_chunks — one row per chunk; vector + tsvector for hybrid retrieval
-- ============================================================

CREATE TABLE kb.kb_chunks (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    doc_id               UUID NOT NULL REFERENCES kb.kb_documents(id) ON DELETE CASCADE,
    department           TEXT NOT NULL,
    visibility           TEXT NOT NULL CHECK (visibility = 'public'),
    source_kind          TEXT NOT NULL DEFAULT 'document_chunk' CHECK (source_kind = 'document_chunk'),
    provenance_trust     TEXT NOT NULL CHECK (provenance_trust IN ('first_party','customer','crawled')),
    role_tags            TEXT[] NOT NULL DEFAULT '{}',

    doc_hash             VARCHAR(64) NOT NULL,
    doc_version          INT NOT NULL DEFAULT 1,
    chunk_idx            INT NOT NULL,
    page                 INT,
    timestamp_sec        REAL,

    embedding_model_id   TEXT NOT NULL,
    embedding_adapter_id TEXT,
    embedding            vector(1024) NOT NULL,

    text                 TEXT NOT NULL,
    text_tsv             tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,

    superseded_at        TIMESTAMPTZ,
    ingested_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pipeline_version     TEXT NOT NULL
);

CREATE INDEX kb_chunks_tenant_dept       ON kb.kb_chunks(tenant_id, department) WHERE superseded_at IS NULL;
CREATE INDEX kb_chunks_tenant_visibility ON kb.kb_chunks(tenant_id, visibility) WHERE superseded_at IS NULL;
CREATE INDEX kb_chunks_tsv               ON kb.kb_chunks USING GIN(text_tsv);
CREATE INDEX kb_chunks_vec               ON kb.kb_chunks USING hnsw(embedding vector_cosine_ops);
CREATE INDEX kb_chunks_doc               ON kb.kb_chunks(doc_id);
CREATE INDEX kb_chunks_role_tags         ON kb.kb_chunks USING GIN(role_tags);

CREATE TABLE kb_internal.kb_chunks (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    doc_id               UUID NOT NULL REFERENCES kb_internal.kb_documents(id) ON DELETE CASCADE,
    department           TEXT NOT NULL,
    visibility           TEXT NOT NULL CHECK (visibility = 'internal'),
    source_kind          TEXT NOT NULL DEFAULT 'document_chunk' CHECK (source_kind = 'document_chunk'),
    provenance_trust     TEXT NOT NULL CHECK (provenance_trust IN ('first_party','customer','crawled')),
    role_tags            TEXT[] NOT NULL DEFAULT '{}',

    doc_hash             VARCHAR(64) NOT NULL,
    doc_version          INT NOT NULL DEFAULT 1,
    chunk_idx            INT NOT NULL,
    page                 INT,
    timestamp_sec        REAL,

    embedding_model_id   TEXT NOT NULL,
    embedding_adapter_id TEXT,
    embedding            vector(1024) NOT NULL,

    text                 TEXT NOT NULL,
    text_tsv             tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,

    superseded_at        TIMESTAMPTZ,
    ingested_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    pipeline_version     TEXT NOT NULL
);

CREATE INDEX kb_internal_chunks_tenant_dept ON kb_internal.kb_chunks(tenant_id, department) WHERE superseded_at IS NULL;
CREATE INDEX kb_internal_chunks_tsv         ON kb_internal.kb_chunks USING GIN(text_tsv);
CREATE INDEX kb_internal_chunks_vec         ON kb_internal.kb_chunks USING hnsw(embedding vector_cosine_ops);
CREATE INDEX kb_internal_chunks_doc         ON kb_internal.kb_chunks(doc_id);
CREATE INDEX kb_internal_chunks_role_tags   ON kb_internal.kb_chunks USING GIN(role_tags);

-- ============================================================
-- kb_ingest_runs — per-ingest cost + status log
-- ============================================================

CREATE TABLE kb.kb_ingest_runs (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    doc_id      UUID,                                      -- nullable; may not have a doc_id on failure
    visibility  TEXT NOT NULL CHECK (visibility IN ('public','internal')),
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    status      TEXT NOT NULL CHECK (status IN ('running','ok','failed')),
    error       TEXT,
    chunk_count INT,
    cost_usd    NUMERIC(10,4)
);

CREATE INDEX kb_ingest_runs_tenant_started ON kb.kb_ingest_runs(tenant_id, started_at DESC);
