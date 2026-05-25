-- Phase 7 P7.6 — PageIndex tree for HR-internal handbook.
--
-- Vectorless retrieval: Sonnet builds a TOC tree for the handbook section;
-- queries navigate the tree via LLM rather than embedding-similarity. Good for
-- structured policy docs where the LLM can read "Compensation > Bands > Senior IC"
-- without an embedding match.
--
-- Reference: https://github.com/VectifyAI/PageIndex
--
-- Tree shape (jsonb): { title, doc_id, chunk_id?, children: [...] }
-- One row per (tenant, root_doc_or_section). LLM walks the tree at query time.

CREATE TABLE kb_internal.pageindex_trees (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- Logical name for the tree (e.g. 'handbook', 'comp-policy'). Multiple
    -- trees per tenant if the corpus is split.
    tree_name       TEXT NOT NULL,
    -- Sonnet 4.6 by default; let the model id evolve.
    builder_model   TEXT NOT NULL DEFAULT 'claude-sonnet-4-6',
    -- The full tree. Top-level is the root node; children are recursive.
    tree            JSONB NOT NULL,
    -- Snapshot of the doc set used to build the tree; for staleness checks.
    source_doc_ids  UUID[] NOT NULL DEFAULT '{}'::uuid[],
    source_doc_hash TEXT,
    -- Free-form notes about the build run (chunks-processed, $ cost).
    extra           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX pageindex_trees_tenant_name_idx
    ON kb_internal.pageindex_trees (tenant_id, tree_name);

GRANT SELECT, INSERT, UPDATE, DELETE ON kb_internal.pageindex_trees TO kb_internal_user;

ALTER TABLE kb_internal.pageindex_trees ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_internal.pageindex_trees FORCE ROW LEVEL SECURITY;
CREATE POLICY pageindex_trees_tenant_iso
    ON kb_internal.pageindex_trees
    FOR ALL TO kb_internal_user
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE OR REPLACE FUNCTION kb_internal.pageindex_trees_set_updated_at()
RETURNS TRIGGER AS $$ BEGIN NEW.updated_at = now(); RETURN NEW; END; $$ LANGUAGE plpgsql;

CREATE TRIGGER pageindex_trees_set_updated_at_trg
    BEFORE UPDATE ON kb_internal.pageindex_trees
    FOR EACH ROW EXECUTE FUNCTION kb_internal.pageindex_trees_set_updated_at();
