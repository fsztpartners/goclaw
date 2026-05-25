-- Phase 3 — Trivia shadow loop tables.
--
-- A "trivia run" samples N chunks from kb.kb_chunks for one tenant, generates
-- 1-3 questions per chunk via Sonnet, embeds each question, scores retrieval
-- against the source chunk, and logs hits + hard negatives.
--
-- This is SHADOW MODE only — output is logged for analysis; it does not
-- influence production retrievals.

CREATE TABLE IF NOT EXISTS kb.trivia_runs (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    -- "shadow" | "scoring_only" | "training_prep"  -- shadow is Phase 3; others are P5.
    mode            text NOT NULL DEFAULT 'shadow' CHECK (mode IN ('shadow', 'scoring_only', 'training_prep')),
    sample_size     integer NOT NULL,
    generator_model text NOT NULL,
    embedding_model_id text NOT NULL,
    -- aggregate metrics for fast dashboard reads
    n_questions       integer NOT NULL DEFAULT 0,
    hit_at_5_count    integer NOT NULL DEFAULT 0,
    hit_at_10_count   integer NOT NULL DEFAULT 0,
    mean_rank         real,
    -- json blob: { config, sampled_chunk_ids[], errors[] }
    extra           jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- "ok" | "partial" | "failed"
    status          text NOT NULL DEFAULT 'ok' CHECK (status IN ('ok', 'partial', 'failed'))
);

CREATE INDEX IF NOT EXISTS trivia_runs_tenant_started ON kb.trivia_runs (tenant_id, started_at DESC);

-- Per-question outcomes. One row per generated question, regardless of how
-- many chunks were sampled (a chunk that produces 3 Qs writes 3 rows).
CREATE TABLE IF NOT EXISTS kb.trivia_questions (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    run_id          uuid NOT NULL REFERENCES kb.trivia_runs(id) ON DELETE CASCADE,
    tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    source_chunk_id uuid NOT NULL,
    -- We do NOT FK to kb_chunks(id) — if a chunk is later superseded we still
    -- want the historical trivia row to survive. Same pattern memory_docs uses.
    question        text NOT NULL,
    -- top-10 retrieved chunk_ids, in rank order; first 5 are the recall-at-5 cohort
    retrieved_top10 uuid[] NOT NULL,
    -- 1-based rank of the source chunk in retrieved_top10; 0 = miss beyond top-10
    source_rank     integer NOT NULL,
    -- chunk_ids in retrieved_top5 that are NOT the source — candidate hard negatives
    hard_negatives  uuid[] NOT NULL DEFAULT '{}',
    -- embedding cost tracker (cents × 100 for precision)
    embedding_tokens integer,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS trivia_questions_run ON kb.trivia_questions (run_id);
CREATE INDEX IF NOT EXISTS trivia_questions_tenant_created ON kb.trivia_questions (tenant_id, created_at DESC);
-- For Phase 5 hard-negative mining: find frequently-confused chunk pairs.
CREATE INDEX IF NOT EXISTS trivia_questions_source_chunk ON kb.trivia_questions (source_chunk_id);

COMMENT ON TABLE kb.trivia_runs IS
  'Phase 3 trivia shadow loop runs. One row per daily-cron firing per tenant.';
COMMENT ON TABLE kb.trivia_questions IS
  'Per-question outcomes from a trivia run. retrieved_top10 captures rank of the source chunk for recall@K metrics; hard_negatives feeds Phase 5 LoRA training.';
