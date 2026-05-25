// Package trivia implements the Phase 3 shadow-mode trivia loop.
//
// Daily per tenant:
//   1. Sample N kb.kb_chunks
//   2. Sonnet generates 1-3 questions per chunk
//   3. Embed each question (Voyage @ 1024 dim, matching corpus embedding model)
//   4. Call kb.Store.Retrieve with k=10
//   5. Score: source_chunk rank in top-10; collect top-4 non-source = hard negatives
//   6. Persist run + per-question rows to kb.trivia_runs / kb.trivia_questions
//
// Shadow-only: nothing here influences production retrievals. This data feeds
// Phase 5 LoRA training and the per-brand difficulty dashboard.
package trivia

import "time"

const (
	// EmbeddingDim must match kb.EmbeddingDim (1024).
	EmbeddingDim = 1024

	// DefaultSampleSize is the number of chunks sampled per run.
	DefaultSampleSize = 100

	// QuestionsPerChunk is the max Sonnet questions to generate per sampled chunk.
	QuestionsPerChunk = 2

	// RetrieveK is the depth to score against; top-10 lets us compute hit@5 and hit@10.
	RetrieveK = 10
)

// RunMode reflects the lifecycle stage. Phase 3 ships only "shadow".
type RunMode string

const (
	ModeShadow       RunMode = "shadow"
	ModeScoringOnly  RunMode = "scoring_only"
	ModeTrainingPrep RunMode = "training_prep"
)

// RunStatus mirrors the CHECK on kb.trivia_runs.status.
type RunStatus string

const (
	StatusOK      RunStatus = "ok"
	StatusPartial RunStatus = "partial"
	StatusFailed  RunStatus = "failed"
)

// Run captures one daily firing for one tenant.
type Run struct {
	ID               string
	TenantID         string
	StartedAt        time.Time
	FinishedAt       *time.Time
	Mode             RunMode
	SampleSize       int
	GeneratorModel   string
	EmbeddingModelID string
	NQuestions       int
	HitAt5Count      int
	HitAt10Count     int
	MeanRank         float64
	Status           RunStatus
	// Department is non-empty for Phase 8 trivia v2 "per-role" runs — the
	// department whose chunks were sampled (e.g. "customer_care"). The legacy
	// tenant-wide v1 run sets this to "".
	Department string
}

// QuestionOutcome is one row in kb.trivia_questions.
type QuestionOutcome struct {
	ID              string
	RunID           string
	TenantID        string
	SourceChunkID   string
	Question        string
	RetrievedTop10  []string
	SourceRank      int      // 1-based; 0 = miss beyond top-10
	HardNegatives   []string // top-5 retrieved that aren't the source
	EmbeddingTokens int
}

// SampledChunk is one of the N chunks sampled from kb.kb_chunks at the start of a run.
type SampledChunk struct {
	ID               string
	Department       string
	RoleTags         []string
	Text             string
	EmbeddingModelID string // every chunk in a single corpus uses the same model id
}
