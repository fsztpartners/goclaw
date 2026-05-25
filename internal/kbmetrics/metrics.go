// Package kbmetrics owns the Prometheus instruments for the KB substrate.
//
// Phase 9 (Production Hardening). Co-located with kb/router/retrieval so that
// any new code path can call kbmetrics.* without pulling in every other
// subsystem's instruments. Registered against the default registry — the
// Server's /metrics handler picks them up automatically.
package kbmetrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Histogram buckets in milliseconds, tuned to the latency range we actually
// see: hybrid ~50-400ms, hybrid+rerank ~600-2700ms, hipporag ~1500-3000ms.
// Buckets stop at 30s — anything slower is effectively a timeout.
var latencyBucketsMS = []float64{
	5, 10, 25, 50, 100, 200, 400, 800,
	1500, 2500, 4000, 6000, 10000, 15000, 30000,
}

var (
	RetrieveRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_retrieve_requests_total",
			Help: "Total /v1/kb/retrieve requests, labeled by brand/route/role/outcome.",
		},
		[]string{"brand", "route", "role", "outcome"},
	)

	RetrieveDurationMS = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kb_retrieve_duration_ms",
			Help:    "End-to-end /v1/kb/retrieve latency (server-side, includes rerank when on).",
			Buckets: latencyBucketsMS,
		},
		[]string{"brand", "route", "role"},
	)

	RerankDurationMS = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kb_rerank_duration_ms",
			Help:    "Reranker call duration in ms, by provider.",
			Buckets: latencyBucketsMS,
		},
		[]string{"brand", "provider"},
	)

	IngestRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_ingest_requests_total",
			Help: "Total /v1/kb/ingest_chunks calls.",
		},
		[]string{"brand", "visibility", "outcome"},
	)

	IngestChunksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_ingest_chunks_total",
			Help: "Total chunks accepted by /v1/kb/ingest_chunks.",
		},
		[]string{"brand", "visibility"},
	)

	EmbeddingCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_embedding_calls_total",
			Help: "Embedding provider calls (caller-side, recorded by GoClaw when it embeds). Labels: provider, outcome.",
		},
		[]string{"provider", "outcome"},
	)

	// Gauges populated by kb-trace-exporter (Phase 9 polling script).
	// Kept here so the metric names are checked against the dashboards in one place.
	CanaryRecallAt5 = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kb_canary_recall_at_5",
			Help: "Latest canary recall@5 per brand (1.0 = PASS).",
		},
		[]string{"brand"},
	)

	TriviaHitAt5Ratio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kb_trivia_hit_at_5_ratio",
			Help: "Latest daily trivia hit@5 ratio per brand.",
		},
		[]string{"brand"},
	)

	JudgeGroundedRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kb_judge_grounded_ratio",
			Help: "Latest LLM-judge grounded ratio per brand (eval rig).",
		},
		[]string{"brand"},
	)

	BrandMonthlyBudgetUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kb_brand_monthly_budget_usd",
			Help: "Configured monthly KB spend budget per brand.",
		},
		[]string{"brand"},
	)

	CostUSDTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_cost_usd_total",
			Help: "Cumulative KB-related spend per brand and component (embedding | rerank | judge | trivia | kg | sonnet | other).",
		},
		[]string{"brand", "component"},
	)

	AdapterABRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_adapter_ab_runs_total",
			Help: "Adapter A/B runs completed per brand.",
		},
		[]string{"brand"},
	)

	AdapterABWinsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kb_adapter_ab_wins_total",
			Help: "Adapter A/B runs where candidate won.",
		},
		[]string{"brand"},
	)

	EmbedCacheHits = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kb_embed_cache_hits_total",
			Help: "Query-embedding cache hits (Redis-backed).",
		},
	)

	EmbedCacheMisses = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "kb_embed_cache_misses_total",
			Help: "Query-embedding cache misses (Redis-backed).",
		},
	)
)

// MustRegister registers all KB instruments with the default registry. Safe to
// call once at gateway boot; panics on duplicate registration so we'd notice.
func MustRegister() {
	prometheus.MustRegister(
		RetrieveRequestsTotal,
		RetrieveDurationMS,
		RerankDurationMS,
		IngestRequestsTotal,
		IngestChunksTotal,
		EmbeddingCallsTotal,
		CanaryRecallAt5,
		TriviaHitAt5Ratio,
		JudgeGroundedRatio,
		BrandMonthlyBudgetUSD,
		CostUSDTotal,
		AdapterABRunsTotal,
		AdapterABWinsTotal,
		EmbedCacheHits,
		EmbedCacheMisses,
	)
}

// Handler returns the /metrics http.Handler. Wired into the gateway mux by a
// thin Handler type satisfying RegisterRoutes(mux *http.ServeMux).
func Handler() http.Handler { return promhttp.Handler() }

// RouteHandler is the RegisterRoutes shim so the existing gateway loop can
// pick this up like any other handler.
type RouteHandler struct{}

func (RouteHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /metrics", Handler())
}
