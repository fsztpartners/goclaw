// Package kb implements the knowledge-base ingest and retrieve endpoints.
//
// Two endpoints, two tables, two schemas:
//
//   POST /v1/kb/ingest_chunks  → INSERT into {kb,kb_internal}.kb_documents + .kb_chunks
//   POST /v1/kb/retrieve       → hybrid (vector cosine + tsvector BM25) + RRF
//
// Invariants enforced by this package:
//
//   - source_kind is always 'document_chunk' (no observation/episodic/semantic path)
//   - Workers (dreaming, episodic, semantic) are NEVER enqueued from here
//   - tenant_id comes from the auth context — callers can't override
//   - visibility=internal writes go to kb_internal.* (RLS-protected); public to kb.*
//   - vector dim is fixed at 1024 (MRL-truncated; see plans/brand-knowledge-base)
package kb

import "time"

// EmbeddingDim is the locked vector dimension. Change requires a destructive migration.
const EmbeddingDim = 1024

// Visibility values.
const (
	VisibilityPublic   = "public"
	VisibilityInternal = "internal"
)

// SourceKind value (only one for this package).
const SourceKindDocumentChunk = "document_chunk"

// Provenance-trust values.
const (
	TrustFirstParty = "first_party"
	TrustCustomer   = "customer"
	TrustCrawled    = "crawled"
)

// IngestDocument represents the document metadata in an ingest request.
type IngestDocument struct {
	Title          string `json:"title,omitempty"`
	SourceURI      string `json:"source_uri,omitempty"`
	SourceKindMeta string `json:"source_kind_meta"` // pdf | pdf_visual | video | web | upload
	DocHash        string `json:"doc_hash"`
	// DocVersion is a freeform version string (git SHA, semver, monotonic int) — defaults to "1".
	DocVersion string `json:"doc_version,omitempty"`
}

// IngestChunk is one chunk's payload.
type IngestChunk struct {
	ChunkIdx         int       `json:"chunk_idx"`
	Page             *int      `json:"page,omitempty"`
	TimestampSec     *float32  `json:"timestamp_sec,omitempty"`
	Text             string    `json:"text"`
	EmbeddingModelID string    `json:"embedding_model_id"`
	EmbeddingAdapter string    `json:"embedding_adapter_id,omitempty"`
	Embedding        []float32 `json:"embedding"` // MUST be EmbeddingDim long
}

// IngestRequest is the body of POST /v1/kb/ingest_chunks.
type IngestRequest struct {
	Department      string         `json:"department"`
	Visibility      string         `json:"visibility"`       // public | internal
	ProvenanceTrust string         `json:"provenance_trust"` // first_party | customer | crawled
	RoleTags        []string       `json:"role_tags,omitempty"`
	Doc             IngestDocument `json:"doc"`
	Chunks          []IngestChunk  `json:"chunks"`
	PipelineVersion string         `json:"pipeline_version"`
}

// IngestResponse is what /v1/kb/ingest_chunks returns.
type IngestResponse struct {
	DocID          string   `json:"doc_id"`
	ChunkIDs       []string `json:"chunk_ids"`
	SkippedWorkers []string `json:"skipped_workers"` // confirmation of invariant — always {dreaming, episodic, semantic}
	IngestRunID    string   `json:"ingest_run_id"`
}

// RetrieveRequest is the body of POST /v1/kb/retrieve.
type RetrieveRequest struct {
	Query            string    `json:"query"`
	Department       string    `json:"department,omitempty"`
	Visibility       string    `json:"visibility,omitempty"` // default 'public'
	Role             string    `json:"role,omitempty"`       // single role tag; multi-tag is comma-separated in RoleTags
	RoleTags         []string  `json:"role_tags,omitempty"`
	K                int       `json:"k"`
	EmbeddingModelID string    `json:"embedding_model_id"`
	QueryEmbedding   []float32 `json:"query_embedding"` // pre-embedded by caller; MUST be EmbeddingDim long

	// Rerank toggles the rerank pass. nil = use server default (ON for sales role,
	// OFF otherwise — keeps Phase 1 callers untouched). *false = force off;
	// *true = force on. Pool size is RetrievePoolSize (top-20).
	Rerank *bool `json:"rerank,omitempty"`

	// RouteOverride forces a specific retrieval strategy. Empty = let the
	// router decide. Values: "hybrid" | "hipporag". Reserved (stub-only): "colpali" | "pageindex".
	// Used by eval scripts to A/B routes against the same golden set.
	RouteOverride string `json:"route_override,omitempty"`

	// AgentID is written to kb_internal.retrieval_audit for internal visibility
	// retrievals. Defaults to "fzst-claw-system" when empty.
	AgentID string `json:"agent_id,omitempty"`
}

// RetrieveChunk is one ranked result.
type RetrieveChunk struct {
	ChunkID      string         `json:"chunk_id"`
	DocID        string         `json:"doc_id"`
	Text         string         `json:"text"`
	VecRank      int            `json:"vec_rank"` // 0 if not in vector top-50
	LexRank      int            `json:"lex_rank"` // 0 if not in BM25 top-50
	RRFScore     float64        `json:"rrf_score"`
	RerankScore  *float64       `json:"rerank_score,omitempty"`  // present iff route includes rerank
	Provenance   map[string]any `json:"provenance"`
}

// RetrieveResponse is what /v1/kb/retrieve returns.
type RetrieveResponse struct {
	Chunks           []RetrieveChunk `json:"chunks"`
	EmbeddingModelID string          `json:"embedding_model_id"`
	LatencyMS        int64           `json:"latency_ms"`
	// Route describes the retrieval strategy used end-to-end. Phase 2 emits
	// "hybrid" or "hybrid+rerank:<provider>". Future phases add "hipporag",
	// "pageindex", "colpali". Logged into retrieval_traces.route.
	Route string `json:"route"`
	// RerankProvider is "voyage" | "qwen3" | "off" iff a rerank pass ran.
	RerankProvider string `json:"rerank_provider,omitempty"`
}

// RetrievePoolSize is the top-N pulled from RRF before reranking (top-N → top-K).
const RetrievePoolSize = 20

// IngestRunStatus values.
const (
	RunStatusRunning = "running"
	RunStatusOK      = "ok"
	RunStatusFailed  = "failed"
)

// internalIngestRun mirrors the DB row for kb.kb_ingest_runs.
type internalIngestRun struct {
	ID         string
	TenantID   string
	DocID      *string
	Visibility string
	StartedAt  time.Time
	Status     string
	ChunkCount int
}
