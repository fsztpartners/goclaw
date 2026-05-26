package kg

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

// Sonnet 4.6 OpenIE extractor. Returns entities + relations per chunk.
// Keep this self-contained (direct anthropic HTTP) — same rationale as
// internal/trivia/generator.go: avoid the agent-runtime provider stack
// for a single one-shot JSON-output call.

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	defaultModel     = "claude-sonnet-4-6"
)

// Extractor produces an ExtractionResult for one chunk.
type Extractor interface {
	Extract(ctx context.Context, chunkText string) (ExtractionResult, error)
	ModelID() string
}

type sonnetExtractor struct {
	apiKey string
	model  string
	client *http.Client
}

// NewSonnetExtractor wires an extractor backed by ANTHROPIC_API_KEY.
// Model override via KG_EXTRACT_MODEL (defaults to claude-sonnet-4-6).
func NewSonnetExtractor() Extractor {
	model := os.Getenv("KG_EXTRACT_MODEL")
	if model == "" {
		model = defaultModel
	}
	return &sonnetExtractor{
		apiKey: os.Getenv("ANTHROPIC_API_KEY"),
		model:  model,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (e *sonnetExtractor) ModelID() string { return e.model }

func (e *sonnetExtractor) Extract(ctx context.Context, chunkText string) (ExtractionResult, error) {
	if e.apiKey == "" {
		return ExtractionResult{}, fmt.Errorf("kg: ANTHROPIC_API_KEY missing")
	}
	if len(chunkText) > MaxChunkChars {
		chunkText = chunkText[:MaxChunkChars]
	}
	prompt := openIEPrompt(chunkText)

	body := map[string]any{
		"model":      e.model,
		// 4096: empirically Sonnet OpenIE on a 4K-char chunk produces 8-15
		// entities + 5-15 triples ≈ 2-3K tokens. 2K caused truncation on
		// chunks with rich entity vocabularies (B30 — see Phase 5 learnings).
		"max_tokens": 4096,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anthropicURL, bytes.NewReader(buf))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", e.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	resp, err := e.client.Do(req)
	if err != nil {
		return ExtractionResult{}, fmt.Errorf("kg extract: anthropic: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return ExtractionResult{}, fmt.Errorf("kg extract: anthropic %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return ExtractionResult{}, fmt.Errorf("kg extract: decode: %w", err)
	}
	var raw strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			raw.WriteString(b.Text)
		}
	}
	return parseExtraction(raw.String())
}

func openIEPrompt(chunkText string) string {
	return `You are an open information extraction system. Read the passage below
and return a JSON object describing the named concepts, organizations, products,
people, and processes it mentions, plus the relations between them.

Rules:
- "entities" is an array. Each entity has:
    name         : the canonical phrase as it appears (or its normalized form)
    entity_type  : one of "person" | "org" | "product" | "concept" | "process" | "place" | "metric" | "doc" | "other"
    description  : short description, max 30 words (optional)
    aliases      : array of surface forms / acronyms (optional)
- "relations" is an array. Each relation has:
    src         : an entity name (must appear in "entities")
    predicate   : a short verb phrase (e.g. "uses", "depends on", "is part of", "owned by")
    dst         : another entity name (must appear in "entities")
    confidence  : 0..1 (default 1.0)
- Extract ONLY concepts the passage clearly establishes — do NOT add background knowledge.
- Skip generic stop-phrases ("the team", "the page", "the company") unless they refer to a clearly-named entity.
- Skip if the passage is mostly boilerplate / TOC / table-of-contents — return {"entities":[],"relations":[]}.
- Return ONLY one JSON object. No prose, no fences.

Passage:
<passage>
` + chunkText + `
</passage>`
}

// parseExtraction tolerates fenced markdown and accepts {"entities":[...],"relations":[...]}.
func parseExtraction(s string) (ExtractionResult, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	lb := strings.Index(s, "{")
	rb := strings.LastIndex(s, "}")
	if lb < 0 || rb < 0 || rb <= lb {
		return ExtractionResult{}, fmt.Errorf("kg extract: no JSON object in response: %s", truncate(s, 200))
	}
	var out ExtractionResult
	if err := json.Unmarshal([]byte(s[lb:rb+1]), &out); err != nil {
		return ExtractionResult{}, fmt.Errorf("kg extract: json parse: %w (raw=%s)", err, truncate(s, 200))
	}
	// Defaults + light validation.
	for i := range out.Entities {
		out.Entities[i].Name = strings.TrimSpace(out.Entities[i].Name)
		if out.Entities[i].EntityType == "" {
			out.Entities[i].EntityType = "concept"
		}
	}
	// Drop relations referencing unknown entity names.
	names := map[string]bool{}
	for _, e := range out.Entities {
		names[strings.ToLower(e.Name)] = true
	}
	clean := out.Relations[:0]
	for _, r := range out.Relations {
		r.Src = strings.TrimSpace(r.Src)
		r.Dst = strings.TrimSpace(r.Dst)
		r.Predicate = strings.TrimSpace(r.Predicate)
		if r.Src == "" || r.Dst == "" || r.Predicate == "" {
			continue
		}
		if !names[strings.ToLower(r.Src)] || !names[strings.ToLower(r.Dst)] {
			continue
		}
		if strings.EqualFold(r.Src, r.Dst) {
			continue
		}
		if r.Confidence <= 0 {
			r.Confidence = 1.0
		}
		clean = append(clean, r)
	}
	out.Relations = clean
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
