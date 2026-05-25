package http

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/kg"
)

// KGHandler exposes the KB-side knowledge-graph endpoints.
//   POST /v1/kb/kg/extract  — synchronous one-chunk extraction (for backfill scripts)
//   POST /v1/kb/kg/run      — drain queued kb.kg_extract_jobs
type KGHandler struct {
	store     *kg.Store
	worker    *kg.Worker
	extractor kg.Extractor
}

// NewKGHandler wires a handler with a store, worker (queue drainer), and extractor.
func NewKGHandler(s *kg.Store, w *kg.Worker, x kg.Extractor) *KGHandler {
	return &KGHandler{store: s, worker: w, extractor: x}
}

// RegisterRoutes registers /v1/kb/kg/* on the given mux.
func (h *KGHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/kb/kg/extract", h.auth(h.handleExtract))
	mux.HandleFunc("POST /v1/kb/kg/run", h.auth(h.handleRun))
}

func (h *KGHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

func (h *KGHandler) handleExtract(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kg.ExtractRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	if req.ChunkID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chunk_id required"})
		return
	}
	text := req.Text
	var tid string
	if text == "" {
		var err error
		text, tid, err = h.store.LoadChunkText(r.Context(), req.ChunkID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	start := time.Now()
	result, err := h.extractor.Extract(r.Context(), text)
	if err != nil {
		slog.Warn("kg.extract failed", "error", err, "chunk_id", req.ChunkID)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	resp := kg.ExtractResponse{
		ChunkID:    req.ChunkID,
		TenantID:   tid,
		Extraction: result,
		LatencyMS:  time.Since(start).Milliseconds(),
	}
	persist := req.Persist == nil || *req.Persist
	if persist {
		en, rel, lk, err := h.store.PersistExtraction(r.Context(), req.ChunkID, result)
		if err != nil {
			slog.Warn("kg.extract: persist failed", "error", err, "chunk_id", req.ChunkID)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		resp.EntitiesUpserted = en
		resp.RelationsUpserted = rel
		resp.LinksUpserted = lk
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *KGHandler) handleRun(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kg.RunRequest
	// Tolerate empty body.
	if r.ContentLength > 0 {
		if !bindJSON(w, r, locale, &req) {
			return
		}
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 500 {
		req.Limit = 500
	}
	resp, err := h.worker.ProcessOnce(r.Context(), req.Limit)
	if err != nil {
		slog.Warn("kg.run failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
