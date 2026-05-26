// Package pagerank implements Personalized PageRank over the KB knowledge
// graph for HippoRAG-style multi-hop retrieval.
//
// Approach (HippoRAG 2-inspired, simplified):
//
//   1. Caller supplies a set of seed entities (matched from the query) and
//      an entity-edge graph (id → neighbors with weights). Edges come from
//      kb.kg_relations (entity ↔ entity) and from co-occurrence within the
//      same chunk via kb.kg_entity_chunks.
//   2. Construct a row-normalized transition matrix (sparse).
//   3. Run power iteration: p_{k+1} = (1-α) M p_k + α r, where r is the
//      teleport vector concentrated on seeds.
//   4. Stop when L1 change < tolerance, or max iterations hit (typically 30).
//
// The output is a score per entity. The caller aggregates entity scores into
// passage (chunk) scores using kb.kg_entity_chunks weights.
//
// Defaults: α=0.5 (HippoRAG release-time default), tolerance=1e-6,
// maxIters=50. Override via Options.
package pagerank

import (
	"math"
	"sort"
)

// Edge is one directed edge from src → dst with a soft weight.
// Caller supplies edges in both directions for an undirected graph.
type Edge struct {
	Src    string
	Dst    string
	Weight float64
}

// Options controls PPR convergence.
type Options struct {
	Damping     float64 // α; teleport probability. Default 0.5.
	Tolerance   float64 // L1 stop criterion. Default 1e-6.
	MaxIters    int     // hard cap. Default 50.
}

// DefaultOptions returns HippoRAG-style defaults.
func DefaultOptions() Options {
	return Options{Damping: 0.5, Tolerance: 1e-6, MaxIters: 50}
}

// Run computes personalized PageRank scores. `seeds` maps entity id → seed mass
// (typically 1.0 for each seed entity; will be L1-normalized). `edges` is the
// graph. Returns a map from every reachable entity id to its final score; the
// sum of scores in the returned map is 1.0.
//
// Entities not present as either a Src or Dst in `edges` and not in `seeds`
// are absent from the output.
func Run(seeds map[string]float64, edges []Edge, opts Options) map[string]float64 {
	if opts.Damping <= 0 || opts.Damping >= 1 {
		opts.Damping = 0.5
	}
	if opts.Tolerance <= 0 {
		opts.Tolerance = 1e-6
	}
	if opts.MaxIters <= 0 {
		opts.MaxIters = 50
	}

	// Index nodes for stable ordering.
	idx := make(map[string]int)
	addNode := func(id string) int {
		if i, ok := idx[id]; ok {
			return i
		}
		i := len(idx)
		idx[id] = i
		return i
	}
	for k := range seeds {
		addNode(k)
	}
	for _, e := range edges {
		addNode(e.Src)
		addNode(e.Dst)
	}
	n := len(idx)
	if n == 0 {
		return map[string]float64{}
	}
	rev := make([]string, n)
	for id, i := range idx {
		rev[i] = id
	}

	// Build row-stochastic transition matrix as a sparse adjacency list:
	// outgoing[src] = list of (dst, weight) with weights summing to 1.
	type weighted struct {
		dst int
		w   float64
	}
	out := make([][]weighted, n)
	rowSum := make([]float64, n)
	for _, e := range edges {
		s := idx[e.Src]
		d := idx[e.Dst]
		w := e.Weight
		if w <= 0 {
			w = 1.0
		}
		out[s] = append(out[s], weighted{dst: d, w: w})
		rowSum[s] += w
	}
	// Normalize.
	for i := range out {
		if rowSum[i] > 0 {
			for j := range out[i] {
				out[i][j].w /= rowSum[i]
			}
		}
	}

	// Teleport vector r. Seeds normalized to sum to 1.
	r := make([]float64, n)
	var seedTotal float64
	for k, v := range seeds {
		if v <= 0 {
			continue
		}
		seedTotal += v
		r[idx[k]] = v
	}
	if seedTotal == 0 {
		// No usable seeds — fall back to uniform teleport.
		for i := range r {
			r[i] = 1.0 / float64(n)
		}
	} else {
		for i := range r {
			r[i] /= seedTotal
		}
	}

	// Initialize p = r.
	p := make([]float64, n)
	copy(p, r)
	next := make([]float64, n)

	alpha := opts.Damping
	oneMinus := 1.0 - alpha

	for iter := 0; iter < opts.MaxIters; iter++ {
		// next = oneMinus * M^T p + alpha * r
		for i := range next {
			next[i] = alpha * r[i]
		}
		for src, row := range out {
			if len(row) == 0 || p[src] == 0 {
				continue
			}
			pv := oneMinus * p[src]
			for _, edge := range row {
				next[edge.dst] += pv * edge.w
			}
		}
		// Dangling-node mass: nodes with no out-edges leak probability. Catch it
		// by adding leaked mass uniformly across teleport.
		var leaked float64
		for src, row := range out {
			if len(row) == 0 {
				leaked += oneMinus * p[src]
			}
		}
		if leaked > 0 {
			for i := range next {
				next[i] += leaked * r[i]
			}
		}
		// L1 convergence check.
		var delta float64
		for i := range p {
			delta += math.Abs(next[i] - p[i])
		}
		p, next = next, p
		if delta < opts.Tolerance {
			break
		}
	}

	out2 := make(map[string]float64, n)
	for i, v := range p {
		out2[rev[i]] = v
	}
	return out2
}

// TopK returns the (id, score) pairs ranked by descending score, capped at K.
func TopK(scores map[string]float64, k int) []KV {
	out := make([]KV, 0, len(scores))
	for id, v := range scores {
		out = append(out, KV{ID: id, Score: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// KV is the output of TopK.
type KV struct {
	ID    string
	Score float64
}
