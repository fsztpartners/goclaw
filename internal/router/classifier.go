package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Classification is the Haiku output for one query.
// {entities, intent, is_multi_entity, needs_brand_voice}
type Classification struct {
	Entities       []string `json:"entities"`
	Intent         string   `json:"intent"` // factual | multi_hop | structural | visual | generative
	IsMultiEntity  bool     `json:"is_multi_entity"`
	NeedsBrandVoice bool    `json:"needs_brand_voice"`
	// Confidence in the classification 0..1; if absent or low the router
	// falls back to hybrid.
	Confidence float64 `json:"confidence"`
}

// Classifier returns a Classification for a query. Implementations cache by
// query hash.
type Classifier interface {
	Classify(ctx context.Context, query string) (Classification, error)
	ModelID() string
}

type haikuClassifier struct {
	apiKey string
	model  string
	client *http.Client
	cache  *classCache
}

const (
	defaultClassifierModel = "claude-haiku-4-5-20251001"
)

// NewHaikuClassifier returns a classifier backed by Claude Haiku 4.5.
// Reads ANTHROPIC_API_KEY; override model via ROUTER_CLASSIFIER_MODEL.
func NewHaikuClassifier() Classifier {
	model := os.Getenv("ROUTER_CLASSIFIER_MODEL")
	if model == "" {
		model = defaultClassifierModel
	}
	return &haikuClassifier{
		apiKey: os.Getenv("ANTHROPIC_API_KEY"),
		model:  model,
		client: &http.Client{Timeout: 4 * time.Second},
		cache:  newClassCache(time.Hour, 10000),
	}
}

func (c *haikuClassifier) ModelID() string { return c.model }

func (c *haikuClassifier) Classify(ctx context.Context, query string) (Classification, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return Classification{Intent: "factual"}, nil
	}
	if v, ok := c.cache.get(q); ok {
		return v, nil
	}
	if c.apiKey == "" {
		// Fall back to a heuristic classifier when no key is set — used in
		// tests and when running offline. Conservative: never claims multi-hop.
		out := heuristicClassify(q)
		c.cache.set(q, out)
		return out, nil
	}

	prompt := classifyPrompt(q)
	body := map[string]any{
		"model":      c.model,
		"max_tokens": 300,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(buf))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.client.Do(req)
	if err != nil {
		// Network error → fall back to heuristic; never fail the router on classifier outage.
		out := heuristicClassify(q)
		c.cache.set(q, out)
		return out, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = raw
		out := heuristicClassify(q)
		c.cache.set(q, out)
		return out, nil
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		out := heuristicClassify(q)
		c.cache.set(q, out)
		return out, nil
	}
	var raw strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			raw.WriteString(b.Text)
		}
	}
	parsed, err := parseClassification(raw.String())
	if err != nil {
		out := heuristicClassify(q)
		c.cache.set(q, out)
		return out, nil
	}
	c.cache.set(q, parsed)
	return parsed, nil
}

func classifyPrompt(q string) string {
	return `Classify the following retrieval query for a corporate-knowledge-base router.

Return ONE JSON object with these fields:
  entities          : array of named concepts (people, products, processes, places) mentioned in the query
  intent            : "factual" | "multi_hop" | "structural" | "visual" | "generative"
                       - factual: 1-entity, 1-fact lookup ("what is X?")
                       - multi_hop: needs to connect 2+ entities across docs ("how does X interact with Y?")
                       - structural: about doc layout / chapter / section ("where in the handbook ...")
                       - visual: asks about charts, diagrams, screenshots
                       - generative: needs brand voice / style / generation ("write a pitch about ...")
  is_multi_entity   : true if the query references 2+ distinct entities
  needs_brand_voice : true if the answer should be in the brand's voice (marketing copy / sales pitch / customer email)
  confidence        : 0..1 — your confidence in the classification

Examples:
  "What is MEDDPICC?"                          → factual, [MEDDPICC], multi=false, brand=false, conf=0.9
  "How does the SLA interact with on-call?"     → multi_hop, [SLA, on-call], multi=true, brand=false, conf=0.85
  "Show me the architecture diagram for X"      → visual, [X], multi=false, brand=false, conf=0.85
  "Where in the handbook is the comp policy?"   → structural, [compensation policy], multi=false, brand=false, conf=0.8
  "Write a pitch about our analytics product"   → generative, [analytics product], multi=false, brand=true, conf=0.9

Return ONLY the JSON object. No prose, no fences.

Query: ` + q
}

func parseClassification(s string) (Classification, error) {
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
		return Classification{}, fmt.Errorf("router: no JSON in response")
	}
	var out Classification
	if err := json.Unmarshal([]byte(s[lb:rb+1]), &out); err != nil {
		return Classification{}, fmt.Errorf("router: json parse: %w", err)
	}
	switch out.Intent {
	case "factual", "multi_hop", "structural", "visual", "generative":
	default:
		out.Intent = "factual"
	}
	if out.Confidence <= 0 {
		out.Confidence = 0.6
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return out, nil
}

// heuristicClassify is a cheap no-LLM fallback. Used when ANTHROPIC_API_KEY is
// missing OR Haiku is unreachable. Conservative — never claims multi-hop, so
// the router falls back to hybrid (which is the right behavior).
func heuristicClassify(q string) Classification {
	lq := strings.ToLower(q)
	visual := containsAny(lq, "diagram", "chart", "screenshot", "image", "figure")
	structural := containsAny(lq, "where in", "what section", "what chapter")
	generative := containsAny(lq, "write a", "draft a", "compose", "pitch", "email")
	intent := "factual"
	switch {
	case visual:
		intent = "visual"
	case structural:
		intent = "structural"
	case generative:
		intent = "generative"
	}
	return Classification{
		Intent:          intent,
		IsMultiEntity:   false,
		NeedsBrandVoice: generative,
		Confidence:      0.5,
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// --- cache ---

type classCache struct {
	mu   sync.Mutex
	data map[string]classEntry
	ttl  time.Duration
	cap  int
}

type classEntry struct {
	v   Classification
	exp time.Time
}

func newClassCache(ttl time.Duration, cap int) *classCache {
	return &classCache{data: make(map[string]classEntry), ttl: ttl, cap: cap}
}

func keyFor(q string) string {
	h := sha256.Sum256([]byte(q))
	return hex.EncodeToString(h[:16])
}

func (c *classCache) get(q string) (Classification, bool) {
	k := keyFor(q)
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[k]
	if !ok {
		return Classification{}, false
	}
	if time.Now().After(v.exp) {
		delete(c.data, k)
		return Classification{}, false
	}
	return v.v, true
}

func (c *classCache) set(q string, v Classification) {
	k := keyFor(q)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.cap {
		// Evict any one entry. Order doesn't matter — simple cap-bound LRU is overkill for 1hr TTL.
		for kk := range c.data {
			delete(c.data, kk)
			break
		}
	}
	c.data[k] = classEntry{v: v, exp: time.Now().Add(c.ttl)}
}
