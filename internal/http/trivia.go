package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/trivia"
)

// TriviaHandler exposes a manual-trigger endpoint for the Phase 3 shadow loop.
// The daily ticker is the primary driver; this exists so the gateway operator
// can fire a run on demand (Phase-close smoke, dashboard refresh, debugging).
type TriviaHandler struct {
	runner *trivia.Runner
}

func NewTriviaHandler(r *trivia.Runner) *TriviaHandler { return &TriviaHandler{runner: r} }

// RegisterRoutes wires /v1/trivia/run on the mux.
func (h *TriviaHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/trivia/run", requireAuth("", h.handleRun))
	mux.HandleFunc("POST /v1/trivia/run_by_department", requireAuth("", h.handleRunByDepartment))
}

// handleRunByDepartment fires a Phase 8 trivia v2 per-role run. Body:
//
//	{ "department": "customer_care", "sample_size": 50 }
func (h *TriviaHandler) handleRunByDepartment(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var body struct {
		Department string `json:"department"`
		SampleSize int    `json:"sample_size"`
	}
	if !bindJSON(w, r, locale, &body) {
		return
	}
	if body.Department == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "department required"})
		return
	}
	if body.SampleSize <= 0 {
		body.SampleSize = trivia.DefaultSampleSize
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	run, err := h.runner.RunOnceForDepartment(ctx, body.Department, body.SampleSize)
	if err != nil {
		slog.Warn("trivia run-by-dept failed", "department", body.Department, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                 run.ID,
		"tenant_id":          run.TenantID,
		"department":         run.Department,
		"sample_size":        run.SampleSize,
		"n_questions":        run.NQuestions,
		"hit_at_5_count":     run.HitAt5Count,
		"hit_at_10_count":    run.HitAt10Count,
		"mean_rank":          run.MeanRank,
		"status":             string(run.Status),
		"generator_model":    run.GeneratorModel,
		"embedding_model_id": run.EmbeddingModelID,
		"started_at":         run.StartedAt,
		"finished_at":        run.FinishedAt,
	})
}

// handleRun fires one shadow trivia run for the caller's tenant_id (set by
// requireAuth → enrichContext). Body fields:
//
//	{ "sample_size": 100, "mode": "shadow" }
//
// Both optional. Returns the kb.trivia_runs row summary.
func (h *TriviaHandler) handleRun(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var body struct {
		SampleSize int    `json:"sample_size"`
		Mode       string `json:"mode"`
	}
	if !bindJSON(w, r, locale, &body) {
		return
	}
	if body.SampleSize <= 0 {
		body.SampleSize = trivia.DefaultSampleSize
	}
	mode := trivia.ModeShadow
	if body.Mode == string(trivia.ModeScoringOnly) {
		mode = trivia.ModeScoringOnly
	} else if body.Mode == string(trivia.ModeTrainingPrep) {
		mode = trivia.ModeTrainingPrep
	}

	// Long-running call — give it a generous budget. Daily ticker uses 20m.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()

	run, err := h.runner.RunOnce(ctx, body.SampleSize, mode)
	if err != nil {
		slog.Warn("trivia run failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                run.ID,
		"tenant_id":         run.TenantID,
		"sample_size":       run.SampleSize,
		"n_questions":       run.NQuestions,
		"hit_at_5_count":    run.HitAt5Count,
		"hit_at_10_count":   run.HitAt10Count,
		"mean_rank":         run.MeanRank,
		"status":            string(run.Status),
		"mode":              string(run.Mode),
		"generator_model":   run.GeneratorModel,
		"embedding_model_id": run.EmbeddingModelID,
		"started_at":        run.StartedAt,
		"finished_at":       run.FinishedAt,
	})
}
