package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/eventbus"
	"github.com/nextlevelbuilder/goclaw/internal/kb"
	"github.com/nextlevelbuilder/goclaw/internal/kbmetrics"
	"github.com/nextlevelbuilder/goclaw/internal/kg"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// tenantIDFromContext stringifies the tenant UUID for Prometheus labels.
// Returns "unknown" when no tenant has been set so series stay well-formed
// even on misrouted requests (which itself becomes a useful signal).
func tenantIDFromContext(ctx context.Context) string {
	id := store.TenantIDFromContext(ctx)
	if id == uuid.Nil {
		return "unknown"
	}
	return id.String()
}

// KBHandler is the HTTP surface for the knowledge-base endpoints.
type KBHandler struct {
	store   *kb.Store
	kgStore *kg.Store // optional; if set, ingest enqueues KG extraction per chunk
	bus     eventbus.DomainEventBus
}

// NewKBHandler wires a KB store to HTTP routes.
func NewKBHandler(s *kb.Store) *KBHandler { return &KBHandler{store: s} }

// WithKGStore enables auto-enqueue of KG extraction jobs after successful ingest.
// Pass nil to disable. Caller wires this in cmd/gateway_http_wiring.
func (h *KBHandler) WithKGStore(k *kg.Store) *KBHandler {
	h.kgStore = k
	return h
}

// WithEventBus wires the bus that backs /v1/kb/events (the publish ingress that
// fzst-claw / external coherence emitters POST to). Pass nil to disable that
// route — internal kb.Store events still flow through the bus injected into
// the store via kb.Store.WithEventBus.
func (h *KBHandler) WithEventBus(b eventbus.DomainEventBus) *KBHandler {
	h.bus = b
	return h
}

// RegisterRoutes registers /v1/kb/* on the given mux.
func (h *KBHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/kb/ingest_chunks", h.auth(h.handleIngest))
	mux.HandleFunc("POST /v1/kb/retrieve", h.auth(h.handleRetrieve))
	mux.HandleFunc("POST /v1/kb/supersede", h.auth(h.handleSupersede))
	mux.HandleFunc("POST /v1/kb/purge", h.auth(h.handlePurge))
	mux.HandleFunc("POST /v1/kb/events", h.auth(h.handlePublishEvent))
}

func (h *KBHandler) auth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

func (h *KBHandler) handleIngest(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kb.IngestRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	resp, err := h.store.Ingest(r.Context(), req)
	brandLabel := tenantIDFromContext(r.Context())
	visLabel := req.Visibility
	if visLabel == "" {
		visLabel = "public"
	}
	if err != nil {
		kbmetrics.IngestRequestsTotal.WithLabelValues(brandLabel, visLabel, "error").Inc()
		slog.Warn("kb.ingest failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	kbmetrics.IngestRequestsTotal.WithLabelValues(brandLabel, visLabel, "ok").Inc()
	kbmetrics.IngestChunksTotal.WithLabelValues(brandLabel, visLabel).Add(float64(len(resp.ChunkIDs)))
	// Phase 5: enqueue KG extraction jobs for the new chunks. Best-effort —
	// never fail the ingest if enqueue fails. Public visibility only for now
	// (kb_internal KG is Phase 7).
	if h.kgStore != nil && req.Visibility == kb.VisibilityPublic && len(resp.ChunkIDs) > 0 {
		if n, err := h.kgStore.EnqueueChunks(r.Context(), resp.ChunkIDs); err != nil {
			slog.Warn("kb.ingest: kg enqueue failed", "error", err, "doc_id", resp.DocID)
		} else if n > 0 {
			slog.Info("kb.ingest: kg jobs enqueued", "doc_id", resp.DocID, "n", n)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *KBHandler) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kb.RetrieveRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	start := time.Now()
	resp, err := h.store.Retrieve(r.Context(), req)
	durMS := float64(time.Since(start).Milliseconds())
	brandLabel := tenantIDFromContext(r.Context())
	roleLabel := req.Role
	if roleLabel == "" {
		roleLabel = "unknown"
	}
	if err != nil {
		kbmetrics.RetrieveRequestsTotal.WithLabelValues(brandLabel, "error", roleLabel, "error").Inc()
		slog.Warn("kb.retrieve failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	routeLabel := resp.Route
	if routeLabel == "" {
		routeLabel = "unknown"
	}
	kbmetrics.RetrieveRequestsTotal.WithLabelValues(brandLabel, routeLabel, roleLabel, "ok").Inc()
	kbmetrics.RetrieveDurationMS.WithLabelValues(brandLabel, routeLabel, roleLabel).Observe(durMS)
	if resp.RerankProvider != "" {
		kbmetrics.RerankDurationMS.WithLabelValues(brandLabel, resp.RerankProvider).Observe(durMS)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSupersede soft-supersedes a doc + its chunks + emits kb.doc.deleted.
func (h *KBHandler) handleSupersede(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kb.SupersedeRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	resp, err := h.store.Supersede(r.Context(), req)
	if err != nil {
		slog.Warn("kb.supersede failed", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePurge runs the GDPR / churn purge for a tenant. Hard-deletes across
// kb.*, kb_internal.*, kb_quarantine.* and writes a row to kb.purge_audit.
func (h *KBHandler) handlePurge(w http.ResponseWriter, r *http.Request) {
	locale := extractLocale(r)
	var req kb.PurgeRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	resp, err := h.store.Purge(r.Context(), req)
	if err != nil {
		slog.Error("kb.purge failed", "tenant_id", req.TenantID, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	slog.Info("kb.purge ok", "tenant_id", resp.TenantID, "audit_id", resp.PurgeAuditID, "duration_ms", resp.DurationMS)
	writeJSON(w, http.StatusOK, resp)
}

// publishEventRequest is the body of POST /v1/kb/events. fzst-claw posts here
// when it does an out-of-band write (e.g. the Next.js supersede UI path) so
// the coherence subscribers in goclaw can still react.
type publishEventRequest struct {
	Type     string         `json:"type"`     // "kb.doc.updated" | "kb.doc.deleted" | "kb.adapter.swapped" | "kb.entity.merged"
	TenantID string         `json:"tenant_id"`
	SourceID string         `json:"source_id,omitempty"`
	Payload  map[string]any `json:"payload"`
}

func (h *KBHandler) handlePublishEvent(w http.ResponseWriter, r *http.Request) {
	if h.bus == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "event bus not wired"})
		return
	}
	locale := extractLocale(r)
	var req publishEventRequest
	if !bindJSON(w, r, locale, &req) {
		return
	}
	switch eventbus.EventType(req.Type) {
	case eventbus.EventKBDocUpdated, eventbus.EventKBDocDeleted,
		eventbus.EventKBAdapterSwapped, eventbus.EventKBEntityMerged,
		eventbus.EventKBTenantPurged:
		// ok
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported event type: " + req.Type})
		return
	}
	if req.TenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant_id required"})
		return
	}
	if req.SourceID == "" {
		req.SourceID = uuid.Must(uuid.NewV7()).String()
	}
	h.bus.Publish(eventbus.DomainEvent{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Type:      eventbus.EventType(req.Type),
		SourceID:  req.SourceID,
		TenantID:  req.TenantID,
		AgentID:   "fzst-claw-system",
		Timestamp: time.Now(),
		Payload:   req.Payload,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
