// Package kg implements KB-side knowledge graph extraction and queries.
//
// The KG is tenant-keyed at the DB constraint level (kb.kg_entities UNIQUE
// is (tenant_id, normalized_name, entity_type)); see plans/brand-knowledge-base/
// phase-5-hipporag-multirole/p5.8-kg-tenant-audit.md.
//
// Flow:
//
//   /v1/kb/ingest_chunks  →  enqueue one row in kb.kg_extract_jobs per chunk
//   Worker drains the queue → Sonnet OpenIE → upsert entities + relations
//   /v1/kb/retrieve (HippoRAG route) → PPR over kb.kg_entity_chunks
package kg

import "time"

// JobStatus values.
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
	JobSkipped = "skipped"
)

// ExtractJob is one row in kb.kg_extract_jobs.
type ExtractJob struct {
	ID          string
	TenantID    string
	ChunkID     string
	Status      string
	Attempts    int
	LastError   string
	EnqueuedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

// Entity is one extracted phrase node.
type Entity struct {
	Name        string   `json:"name"`
	EntityType  string   `json:"entity_type"`         // 'person' | 'org' | 'product' | 'concept' | 'process' | 'place' | 'metric' | 'doc' | 'other'
	Description string   `json:"description,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// Relation is one extracted (src, predicate, dst) triple, referencing entity
// names that must appear in the same response's Entities slice.
type Relation struct {
	Src        string  `json:"src"`        // matches Entity.Name
	Predicate  string  `json:"predicate"`
	Dst        string  `json:"dst"`        // matches Entity.Name
	Confidence float64 `json:"confidence"` // 0..1; default 1.0
}

// ExtractionResult is what the OpenIE extractor returns per chunk.
type ExtractionResult struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// ExtractRequest is the body of POST /v1/kb/kg/extract.
type ExtractRequest struct {
	ChunkID string `json:"chunk_id"`
	// Optional override; if empty, the chunk's text is loaded from DB.
	Text string `json:"text,omitempty"`
	// If true, persist the result. Default true.
	Persist *bool `json:"persist,omitempty"`
}

// ExtractResponse mirrors ExtractionResult + persistence stats.
type ExtractResponse struct {
	ChunkID         string           `json:"chunk_id"`
	TenantID        string           `json:"tenant_id"`
	Extraction      ExtractionResult `json:"extraction"`
	EntitiesUpserted int             `json:"entities_upserted"`
	RelationsUpserted int            `json:"relations_upserted"`
	LinksUpserted    int             `json:"links_upserted"`
	LatencyMS        int64            `json:"latency_ms"`
}

// RunRequest is the body of POST /v1/kb/kg/run (drains queued jobs).
type RunRequest struct {
	Limit int `json:"limit,omitempty"` // default 50, max 500
}

// RunResponse summarizes one drain pass.
type RunResponse struct {
	Tenant     string `json:"tenant"`
	Processed  int    `json:"processed"`
	Done       int    `json:"done"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	LatencyMS  int64  `json:"latency_ms"`
}

// MaxChunkChars caps chunk text sent to Sonnet — over 4000 chars rarely
// produces better extractions, and capping bounds Sonnet cost.
const MaxChunkChars = 4000
