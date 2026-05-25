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

// Anthropic question generator. Thin direct HTTP wrapper — the existing
// providers/anthropic.go is wired into the agent runtime (model registry,
// schema profiles, etc.) and is heavier than we need for a single one-shot
// JSON-output call.

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	defaultSonnet    = "claude-sonnet-4-6"
)

// Generator produces 1-N trivia questions for one chunk. The contract: each
// returned question's answer MUST be derivable from the chunk text alone.
type Generator interface {
	GenerateQuestions(ctx context.Context, chunkText string, n int) ([]string, error)
	ModelID() string
}

type anthropicGen struct {
	apiKey string
	model  string
	client *http.Client
}

// NewAnthropicGenerator wires a generator backed by Claude Sonnet 4.6.
// Reads ANTHROPIC_API_KEY; model override via TRIVIA_GEN_MODEL (defaults to claude-sonnet-4-6).
func NewAnthropicGenerator() Generator {
	model := os.Getenv("TRIVIA_GEN_MODEL")
	if model == "" {
		model = defaultSonnet
	}
	return &anthropicGen{
		apiKey: os.Getenv("ANTHROPIC_API_KEY"),
		model:  model,
		client: &http.Client{Timeout: 45 * time.Second},
	}
}

func (g *anthropicGen) ModelID() string { return g.model }

func (g *anthropicGen) GenerateQuestions(ctx context.Context, chunkText string, n int) ([]string, error) {
	if g.apiKey == "" {
		return nil, fmt.Errorf("trivia: ANTHROPIC_API_KEY missing")
	}
	if n <= 0 {
		n = 1
	}
	prompt := fmt.Sprintf(`You are generating retrieval-eval trivia questions.

Rules:
- Each question's answer MUST be present in the chunk below.
- Phrase questions the way a real user would (a sales rep / HR recruiter / customer-care agent / marketer at GitLab) — do not quote chunk wording verbatim.
- Each question must be standalone (no "this chunk", "the document", etc.).
- Skip if the chunk is too short or generic to produce a good question — return an empty array.
- Output ONLY a JSON array of strings. No prose, no fences.

Generate up to %d questions for this chunk:

<chunk>
%s
</chunk>`, n, chunkText)

	body := map[string]any{
		"model":      g.model,
		"max_tokens": 600,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anthropicURL, bytes.NewReader(buf))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", g.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trivia anthropic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("trivia anthropic %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("trivia anthropic decode: %w", err)
	}
	var raw strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			raw.WriteString(b.Text)
		}
	}
	return parseQuestionsJSON(raw.String())
}

// parseQuestionsJSON tolerates fenced markdown and extra prose around the JSON.
func parseQuestionsJSON(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	// Strip ```json ... ``` fences if present.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// Find the first [ ... ]
	lb := strings.Index(s, "[")
	rb := strings.LastIndex(s, "]")
	if lb < 0 || rb < 0 || rb <= lb {
		return nil, fmt.Errorf("trivia gen: no JSON array in response: %s", truncate(s, 200))
	}
	var qs []string
	if err := json.Unmarshal([]byte(s[lb:rb+1]), &qs); err != nil {
		return nil, fmt.Errorf("trivia gen: json parse: %w (raw=%s)", err, truncate(s, 200))
	}
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		q = strings.TrimSpace(q)
		if q != "" {
			out = append(out, q)
		}
	}
	return out, nil
}
