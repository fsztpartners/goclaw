-- KB-side Knowledge Graph for HippoRAG 2 multi-hop retrieval.
-- Phase 5 of plans/brand-knowledge-base/ (fzst-claw repo).
--
-- DESIGN — separate kb.kg_* tables, NOT a reuse of legacy kg_entities.
--   Legacy kg_entities is agent-keyed (UNIQUE(agent_id, user_id, external_id))
--   with tenant_id ALTER'd in 000027 but the constraint never moved. KB-side
--   KG MUST partition by tenant_id at the UNIQUE constraint level so the
--   P5.8 audit invariant ("two brands, same product name, entities do NOT
--   merge") is enforced structurally — not at query time.
--
-- TABLES
--   kb.kg_entities         — phrase nodes; UNIQUE(tenant_id, lower(name), entity_type)
--   kb.kg_relations        — triples; UNIQUE(tenant_id, src, predicate, dst)
--   kb.kg_entity_chunks    — entity ↔ chunk evidence join; PRIMARY KEY (tenant_id, entity_id, chunk_id)
--
-- Tenant-id is denormalized onto every row to make scope filters trivial and
-- to let cascading deletes on tenants() take everything in one shot.

-- ============================================================
-- kb.kg_entities
-- ============================================================

CREATE TABLE kb.kg_entities (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    -- Lower-cased, whitespace-collapsed key used for UNIQUE dedup.
    -- Stored as a generated column so app code never has to compute it.
    normalized_name  TEXT NOT NULL GENERATED ALWAYS AS (
        regexp_replace(lower(trim(name)), '\s+', ' ', 'g')
    ) STORED,
    entity_type      TEXT NOT NULL DEFAULT 'concept',
    description      TEXT NOT NULL DEFAULT '',
    aliases          TEXT[] NOT NULL DEFAULT '{}',
    embedding        vector(1024),
    -- Provenance: how many chunks reference this entity (denormalized count,
    -- bumped by app code on insert into kg_entity_chunks; recomputable).
    chunk_count      INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    tsv              tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', coalesce(name, '') || ' ' || coalesce(description, ''))
    ) STORED
);

-- THE security gate. Two tenants with the same entity name produce DIFFERENT rows.
-- Equivalent legacy table is UNIQUE(agent_id, user_id, external_id) — see P5.8
-- audit doc for why that's not enough.
CREATE UNIQUE INDEX kb_kg_entities_tenant_name
    ON kb.kg_entities (tenant_id, normalized_name, entity_type);

CREATE INDEX kb_kg_entities_tenant       ON kb.kg_entities (tenant_id);
CREATE INDEX kb_kg_entities_tsv          ON kb.kg_entities USING gin (tsv);
CREATE INDEX kb_kg_entities_vec          ON kb.kg_entities USING hnsw (embedding vector_cosine_ops);
CREATE INDEX kb_kg_entities_aliases      ON kb.kg_entities USING gin (aliases);

-- ============================================================
-- kb.kg_relations
-- ============================================================

CREATE TABLE kb.kg_relations (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    src_entity_id    UUID NOT NULL REFERENCES kb.kg_entities(id) ON DELETE CASCADE,
    predicate        TEXT NOT NULL,
    dst_entity_id    UUID NOT NULL REFERENCES kb.kg_entities(id) ON DELETE CASCADE,
    confidence       REAL NOT NULL DEFAULT 1.0,
    -- Optional: chunk that first asserted this triple. Not FK'd — chunks
    -- can be superseded and we keep the relation.
    evidence_chunk_id UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Cross-row invariant: src + dst must belong to the same tenant_id.
    -- Enforced via trigger below (a CHECK across FK rows isn't expressible).
    CHECK (src_entity_id <> dst_entity_id)
);

CREATE UNIQUE INDEX kb_kg_relations_tenant_triple
    ON kb.kg_relations (tenant_id, src_entity_id, predicate, dst_entity_id);
CREATE INDEX kb_kg_relations_src   ON kb.kg_relations (src_entity_id);
CREATE INDEX kb_kg_relations_dst   ON kb.kg_relations (dst_entity_id);
CREATE INDEX kb_kg_relations_tenant ON kb.kg_relations (tenant_id);

-- Enforce src.tenant_id = dst.tenant_id = relation.tenant_id.
CREATE OR REPLACE FUNCTION kb.kg_relations_tenant_check() RETURNS trigger AS $$
DECLARE
    src_tid uuid;
    dst_tid uuid;
BEGIN
    SELECT tenant_id INTO src_tid FROM kb.kg_entities WHERE id = NEW.src_entity_id;
    SELECT tenant_id INTO dst_tid FROM kb.kg_entities WHERE id = NEW.dst_entity_id;
    IF src_tid IS NULL OR dst_tid IS NULL THEN
        RAISE EXCEPTION 'kb.kg_relations: src or dst entity not found';
    END IF;
    IF src_tid <> NEW.tenant_id OR dst_tid <> NEW.tenant_id THEN
        RAISE EXCEPTION 'kb.kg_relations: cross-tenant relation rejected (relation=%, src=%, dst=%)',
            NEW.tenant_id, src_tid, dst_tid;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kb_kg_relations_tenant_check
    BEFORE INSERT OR UPDATE ON kb.kg_relations
    FOR EACH ROW EXECUTE FUNCTION kb.kg_relations_tenant_check();

-- ============================================================
-- kb.kg_entity_chunks — entity occurrences per chunk
-- ============================================================

CREATE TABLE kb.kg_entity_chunks (
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    entity_id     UUID NOT NULL REFERENCES kb.kg_entities(id) ON DELETE CASCADE,
    chunk_id      UUID NOT NULL REFERENCES kb.kb_chunks(id) ON DELETE CASCADE,
    -- Soft weight: how strongly this entity appears in this chunk
    -- (count, tfidf-style, or 1.0 if app code doesn't compute one).
    weight        REAL NOT NULL DEFAULT 1.0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, entity_id, chunk_id)
);

CREATE INDEX kb_kg_entity_chunks_chunk ON kb.kg_entity_chunks (chunk_id);
CREATE INDEX kb_kg_entity_chunks_entity ON kb.kg_entity_chunks (entity_id);

-- Same tenant-consistency trigger as relations: entity.tenant_id = chunk.tenant_id = row.tenant_id.
CREATE OR REPLACE FUNCTION kb.kg_entity_chunks_tenant_check() RETURNS trigger AS $$
DECLARE
    ent_tid uuid;
    chk_tid uuid;
BEGIN
    SELECT tenant_id INTO ent_tid FROM kb.kg_entities WHERE id = NEW.entity_id;
    SELECT tenant_id INTO chk_tid FROM kb.kb_chunks WHERE id = NEW.chunk_id;
    IF ent_tid IS NULL OR chk_tid IS NULL THEN
        RAISE EXCEPTION 'kb.kg_entity_chunks: entity or chunk not found';
    END IF;
    IF ent_tid <> NEW.tenant_id OR chk_tid <> NEW.tenant_id THEN
        RAISE EXCEPTION 'kb.kg_entity_chunks: cross-tenant link rejected (row=%, ent=%, chk=%)',
            NEW.tenant_id, ent_tid, chk_tid;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kb_kg_entity_chunks_tenant_check
    BEFORE INSERT OR UPDATE ON kb.kg_entity_chunks
    FOR EACH ROW EXECUTE FUNCTION kb.kg_entity_chunks_tenant_check();

-- ============================================================
-- kb.kg_extract_jobs — per-chunk extraction queue
-- ============================================================
-- The GoClaw KG extractor worker drains this table. Inserts happen at
-- /v1/kb/ingest_chunks time (one row per new chunk). Worker picks queued
-- rows, calls Sonnet OpenIE, upserts entities/relations/links, marks done.

CREATE TABLE kb.kg_extract_jobs (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    chunk_id    UUID NOT NULL REFERENCES kb.kb_chunks(id) ON DELETE CASCADE,
    status      TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed','skipped')),
    attempts    INT NOT NULL DEFAULT 0,
    last_error  TEXT,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (tenant_id, chunk_id)
);

CREATE INDEX kb_kg_extract_jobs_pending ON kb.kg_extract_jobs (status, enqueued_at)
    WHERE status IN ('queued','running');
