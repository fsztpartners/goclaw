package trivia

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/kb"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Runner orchestrates one trivia run for one tenant.
type Runner struct {
	store   *Store
	kbStore *kb.Store
	embed   Embedder
	gen     Generator
}

// NewRunner wires the dependencies. db is used for sampling + writing trivia
// rows; kbStore handles the retrieve side.
func NewRunner(s *Store, k *kb.Store, e Embedder, g Generator) *Runner {
	return &Runner{store: s, kbStore: k, embed: e, gen: g}
}

// RunOnce executes one shadow trivia run for the tenant in ctx. sampleSize
// defaults to DefaultSampleSize when 0.
func (r *Runner) RunOnce(ctx context.Context, sampleSize int, mode RunMode) (*Run, error) {
	if sampleSize <= 0 {
		sampleSize = DefaultSampleSize
	}
	if mode == "" {
		mode = ModeShadow
	}

	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("trivia run: tenant_id missing from context")
	}

	chunks, err := r.store.SampleChunks(ctx, sampleSize)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("trivia run: no chunks to sample (tenant=%s)", tenantID)
	}

	// All chunks in one tenant currently use the same embedding model (we never
	// mix vector spaces — plan decision #12). Use the first chunk's model id to
	// gate the retrieve.
	modelID := chunks[0].EmbeddingModelID

	run := &Run{
		Mode:             mode,
		SampleSize:       len(chunks),
		GeneratorModel:   r.gen.ModelID(),
		EmbeddingModelID: modelID,
		Status:           StatusOK,
	}
	if err := r.store.InsertRun(ctx, run); err != nil {
		return nil, err
	}

	var (
		totalQ      int
		hit5        int
		hit10       int
		sumRankObs  int // sum of ranks where source landed in top-10
		rankObsCnt  int
		failedQ     int
	)
	for _, c := range chunks {
		questions, gerr := r.gen.GenerateQuestions(ctx, c.Text, QuestionsPerChunk)
		if gerr != nil {
			slog.Warn("trivia: gen failed", "chunk_id", c.ID, "err", gerr)
			failedQ++
			continue
		}
		for _, qText := range questions {
			outcome, err := r.scoreOne(ctx, c, qText, run.ID, modelID)
			if err != nil {
				slog.Warn("trivia: score failed", "chunk_id", c.ID, "err", err)
				failedQ++
				continue
			}
			if err := r.store.InsertQuestion(ctx, outcome); err != nil {
				slog.Warn("trivia: insert question failed", "err", err)
				failedQ++
				continue
			}
			totalQ++
			if outcome.SourceRank > 0 {
				rankObsCnt++
				sumRankObs += outcome.SourceRank
				if outcome.SourceRank <= 5 {
					hit5++
				}
				if outcome.SourceRank <= 10 {
					hit10++
				}
			}
		}
	}

	run.NQuestions = totalQ
	run.HitAt5Count = hit5
	run.HitAt10Count = hit10
	if rankObsCnt > 0 {
		run.MeanRank = float64(sumRankObs) / float64(rankObsCnt)
	}
	switch {
	case totalQ == 0:
		run.Status = StatusFailed
	case failedQ > 0:
		run.Status = StatusPartial
	default:
		run.Status = StatusOK
	}
	if err := r.store.FinalizeRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func (r *Runner) scoreOne(ctx context.Context, c SampledChunk, question, runID, modelID string) (*QuestionOutcome, error) {
	vec, _, err := r.embed.EmbedQuery(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("trivia embed: %w", err)
	}

	// Score against the public KB only — trivia loop does not generate from
	// kb_internal in Phase 3 (that schema is empty until Phase 6).
	resp, err := r.kbStore.Retrieve(ctx, kb.RetrieveRequest{
		Query:            question,
		Department:       c.Department,
		Visibility:       kb.VisibilityPublic,
		K:                RetrieveK,
		EmbeddingModelID: modelID,
		QueryEmbedding:   vec,
		// Rerank off for trivia — we want raw hybrid signal so the score isn't
		// confounded by the reranker. Phase 5 can re-evaluate.
		Rerank: boolPtr(false),
		Role:   "", // no per-role gating; sampled chunk's department is enough
	})
	if err != nil {
		return nil, fmt.Errorf("trivia retrieve: %w", err)
	}

	retrieved := make([]string, 0, len(resp.Chunks))
	rank := 0
	hardNegs := make([]string, 0, 4)
	for i, ch := range resp.Chunks {
		retrieved = append(retrieved, ch.ChunkID)
		if ch.ChunkID == c.ID {
			rank = i + 1
		} else if i < 5 && len(hardNegs) < 4 {
			hardNegs = append(hardNegs, ch.ChunkID)
		}
	}

	return &QuestionOutcome{
		RunID:          runID,
		SourceChunkID:  c.ID,
		Question:       question,
		RetrievedTop10: retrieved,
		SourceRank:     rank,
		HardNegatives:  hardNegs,
	}, nil
}

func boolPtr(b bool) *bool { return &b }

// RunOnceForDepartment runs a Phase 8 trivia v2 pass scoped to one department.
// The sample, Sonnet generation, and retrieve all happen against chunks where
// `department = $dept`. The persisted kb.trivia_runs row carries the
// department in `extra.department` so per-role drift can be tracked in the
// dashboard. Same shadow semantics as RunOnce (no production impact).
func (r *Runner) RunOnceForDepartment(ctx context.Context, department string, sampleSize int) (*Run, error) {
	if department == "" {
		return nil, fmt.Errorf("trivia run-by-dept: department required")
	}
	if sampleSize <= 0 {
		sampleSize = DefaultSampleSize
	}
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("trivia run-by-dept: tenant_id missing from context")
	}
	chunks, err := r.store.SampleChunksByDepartment(ctx, department, sampleSize)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("trivia run-by-dept: no chunks for tenant=%s dept=%s", tenantID, department)
	}
	modelID := chunks[0].EmbeddingModelID
	run := &Run{
		Mode:             ModeShadow,
		SampleSize:       len(chunks),
		GeneratorModel:   r.gen.ModelID(),
		EmbeddingModelID: modelID,
		Status:           StatusOK,
		Department:       department,
	}
	if err := r.store.InsertRun(ctx, run); err != nil {
		return nil, err
	}
	var (
		totalQ, hit5, hit10, sumRankObs, rankObsCnt, failedQ int
	)
	for _, c := range chunks {
		questions, gerr := r.gen.GenerateQuestions(ctx, c.Text, QuestionsPerChunk)
		if gerr != nil {
			slog.Warn("trivia v2: gen failed", "chunk_id", c.ID, "dept", department, "err", gerr)
			failedQ++
			continue
		}
		for _, qText := range questions {
			outcome, err := r.scoreOne(ctx, c, qText, run.ID, modelID)
			if err != nil {
				slog.Warn("trivia v2: score failed", "chunk_id", c.ID, "err", err)
				failedQ++
				continue
			}
			if err := r.store.InsertQuestion(ctx, outcome); err != nil {
				slog.Warn("trivia v2: insert question failed", "err", err)
				failedQ++
				continue
			}
			totalQ++
			if outcome.SourceRank > 0 {
				rankObsCnt++
				sumRankObs += outcome.SourceRank
				if outcome.SourceRank <= 5 {
					hit5++
				}
				if outcome.SourceRank <= 10 {
					hit10++
				}
			}
		}
	}
	run.NQuestions = totalQ
	run.HitAt5Count = hit5
	run.HitAt10Count = hit10
	if rankObsCnt > 0 {
		run.MeanRank = float64(sumRankObs) / float64(rankObsCnt)
	}
	switch {
	case totalQ == 0:
		run.Status = StatusFailed
	case failedQ > 0:
		run.Status = StatusPartial
	default:
		run.Status = StatusOK
	}
	if err := r.store.FinalizeRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}
