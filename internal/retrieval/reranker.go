// Package retrieval holds rerank + (future) specialized retrieval primitives
// (HippoRAG, PageIndex, ColPali). Phase 2 ships only the reranker.
//
// Provider selection (env RERANK_PROVIDER):
//
//	voyage   — Voyage rerank-2.5 hosted API (default; matches plan decision #5 logic
//	           for embeddings — hosted while Qwen3 pod isn't up)
//	qwen3    — Self-hosted vLLM endpoint at RERANK_QWEN3_URL (Phase 5+)
//	off      — Skip reranking; return candidates in input order, top-K
//
// The reranker takes RRF top-N candidates and reorders them by relevance.
// Default N=20 in (top-20 → top-K caller-specified).
package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Candidate is one chunk passed into the reranker.
type Candidate struct {
	ID   string
	Text string
}

// Ranked is one chunk after reranking — keeps the ID + the rerank score so
// callers can carry both back through their result type.
type Ranked struct {
	ID    string
	Score float64
}

// Reranker is the interface the store uses.
type Reranker interface {
	// Rerank reorders candidates by relevance to query and returns the top-k.
	// Implementations MUST preserve order ties deterministically (by ID asc).
	Rerank(ctx context.Context, query string, candidates []Candidate, topK int) ([]Ranked, error)
	// Provider returns a short label for logging ("voyage" | "qwen3" | "off").
	Provider() string
}

// NewRerankerFromEnv picks a provider based on env. Falls back to voyage when
// RERANK_PROVIDER is unset (server-side default ON for sales).
func NewRerankerFromEnv() Reranker {
	switch strings.ToLower(os.Getenv("RERANK_PROVIDER")) {
	case "off":
		return offReranker{}
	case "qwen3":
		return &qwen3Reranker{
			endpoint: os.Getenv("RERANK_QWEN3_URL"),
			model:    envOr("RERANK_QWEN3_MODEL", "Qwen/Qwen3-Reranker-4B"),
			client:   &http.Client{Timeout: 8 * time.Second},
		}
	default:
		return &voyageReranker{
			apiKey: os.Getenv("VOYAGE_API_KEY"),
			model:  envOr("RERANK_VOYAGE_MODEL", "rerank-2.5"),
			client: &http.Client{Timeout: 8 * time.Second},
		}
	}
}

func envOr(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

// offReranker returns input order, truncated to top-K. Used when RERANK_PROVIDER=off.
type offReranker struct{}

func (offReranker) Provider() string { return "off" }

func (offReranker) Rerank(_ context.Context, _ string, candidates []Candidate, topK int) ([]Ranked, error) {
	if topK <= 0 || topK > len(candidates) {
		topK = len(candidates)
	}
	out := make([]Ranked, 0, topK)
	for i := 0; i < topK; i++ {
		// Use 1/(rank+1) as a placeholder score so callers can sort sensibly.
		out = append(out, Ranked{ID: candidates[i].ID, Score: 1.0 / float64(i+1)})
	}
	return out, nil
}

// voyageReranker calls Voyage rerank-2.5 hosted API.
// Docs: https://docs.voyageai.com/reference/reranker-api
type voyageReranker struct {
	apiKey string
	model  string
	client *http.Client
}

func (v *voyageReranker) Provider() string { return "voyage" }

type voyageReq struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Model     string   `json:"model"`
	TopK      int      `json:"top_k,omitempty"`
}

type voyageResp struct {
	Data []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"data"`
}

func (v *voyageReranker) Rerank(ctx context.Context, query string, candidates []Candidate, topK int) ([]Ranked, error) {
	if v.apiKey == "" {
		return nil, fmt.Errorf("voyage rerank: VOYAGE_API_KEY missing")
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if topK <= 0 || topK > len(candidates) {
		topK = len(candidates)
	}
	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Text
	}
	body, _ := json.Marshal(voyageReq{Query: query, Documents: docs, Model: v.model, TopK: topK})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.voyageai.com/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("voyage rerank: build req: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+v.apiKey)
	res, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage rerank: do: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		return nil, fmt.Errorf("voyage rerank: status %d: %s", res.StatusCode, truncate(buf.String(), 500))
	}
	var vr voyageResp
	if err := json.NewDecoder(res.Body).Decode(&vr); err != nil {
		return nil, fmt.Errorf("voyage rerank: decode: %w", err)
	}
	out := make([]Ranked, 0, len(vr.Data))
	for _, d := range vr.Data {
		if d.Index < 0 || d.Index >= len(candidates) {
			continue
		}
		out = append(out, Ranked{ID: candidates[d.Index].ID, Score: d.RelevanceScore})
	}
	return out, nil
}

// qwen3Reranker is a thin client for a self-hosted vLLM Qwen3-Reranker endpoint.
// Wire shape follows the vLLM /v1/rerank surface; revisit when the pod lands.
type qwen3Reranker struct {
	endpoint string
	model    string
	client   *http.Client
}

func (q *qwen3Reranker) Provider() string { return "qwen3" }

func (q *qwen3Reranker) Rerank(ctx context.Context, query string, candidates []Candidate, topK int) ([]Ranked, error) {
	if q.endpoint == "" {
		return nil, fmt.Errorf("qwen3 rerank: RERANK_QWEN3_URL missing")
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if topK <= 0 || topK > len(candidates) {
		topK = len(candidates)
	}
	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Text
	}
	body, _ := json.Marshal(map[string]any{
		"query":     query,
		"documents": docs,
		"model":     q.model,
		"top_k":     topK,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(q.endpoint, "/")+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("qwen3 rerank: build req: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	res, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen3 rerank: do: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(res.Body)
		return nil, fmt.Errorf("qwen3 rerank: status %d: %s", res.StatusCode, truncate(buf.String(), 500))
	}
	// vLLM mirrors Voyage shape closely enough — reuse the decoder.
	var vr voyageResp
	if err := json.NewDecoder(res.Body).Decode(&vr); err != nil {
		return nil, fmt.Errorf("qwen3 rerank: decode: %w", err)
	}
	out := make([]Ranked, 0, len(vr.Data))
	for _, d := range vr.Data {
		if d.Index < 0 || d.Index >= len(candidates) {
			continue
		}
		out = append(out, Ranked{ID: candidates[d.Index].ID, Score: d.RelevanceScore})
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
