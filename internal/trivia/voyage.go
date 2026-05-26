package trivia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Voyage embedder targeted at the KB schema's vector(1024) column.
//
// Separate from internal/providers/embedding_voyage.go on purpose: that one is
// pinned to ExpectedEmbeddingDim = 1536 for the memory layer. The KB column is
// 1024 (plan decision #2), which we get via Voyage's output_dimension param.

const (
	voyageEmbedURL    = "https://api.voyageai.com/v1/embeddings"
	voyageDefaultMod  = "voyage-3-large"
	voyageInputDoc    = "document"
	voyageInputQuery  = "query"
)

// Embedder produces 1024-dim float32 embeddings, matching kb.kb_chunks.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, string, error)
	// ModelID returns the canonical "<model>@<dim>" string stored on each chunk.
	ModelID() string
}

type voyageEmbedder struct {
	apiKey string
	model  string
	client *http.Client
}

// NewVoyageEmbedder returns an Embedder reading the API key from VOYAGE_API_KEY.
// The model defaults to voyage-3-large (same as fzst-claw/lib/ingestion/embed.ts).
func NewVoyageEmbedder() Embedder {
	model := os.Getenv("TRIVIA_VOYAGE_MODEL")
	if model == "" {
		model = voyageDefaultMod
	}
	return &voyageEmbedder{
		apiKey: os.Getenv("VOYAGE_API_KEY"),
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *voyageEmbedder) ModelID() string {
	return fmt.Sprintf("%s@%d", e.model, EmbeddingDim)
}

func (e *voyageEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, string, error) {
	if e.apiKey == "" {
		return nil, "", fmt.Errorf("trivia: VOYAGE_API_KEY missing")
	}
	body := map[string]any{
		"input":            []string{text},
		"model":            e.model,
		"output_dimension": EmbeddingDim,
		"output_dtype":     "float",
		"input_type":       voyageInputQuery,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", voyageEmbedURL, bytes.NewReader(buf))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("trivia voyage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("trivia voyage %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("trivia voyage decode: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) != EmbeddingDim {
		return nil, "", fmt.Errorf("trivia voyage: expected %d dims, got %d", EmbeddingDim, len(out.Data[0].Embedding))
	}
	vec := make([]float32, EmbeddingDim)
	for i, v := range out.Data[0].Embedding {
		vec[i] = float32(v)
	}
	return vec, e.ModelID(), nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
