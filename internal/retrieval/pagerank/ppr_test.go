package pagerank

import (
	"math"
	"testing"
)

// Small synthetic graph:
//
//   A — B — C
//   |       |
//   D ————— E
//
// Seed A. Expect A highest; B/D second tier; C/E third.
func TestRun_SeedConcentrationDecays(t *testing.T) {
	edges := []Edge{
		{Src: "A", Dst: "B", Weight: 1}, {Src: "B", Dst: "A", Weight: 1},
		{Src: "B", Dst: "C", Weight: 1}, {Src: "C", Dst: "B", Weight: 1},
		{Src: "A", Dst: "D", Weight: 1}, {Src: "D", Dst: "A", Weight: 1},
		{Src: "D", Dst: "E", Weight: 1}, {Src: "E", Dst: "D", Weight: 1},
		{Src: "C", Dst: "E", Weight: 1}, {Src: "E", Dst: "C", Weight: 1},
	}
	scores := Run(map[string]float64{"A": 1}, edges, DefaultOptions())

	var sum float64
	for _, v := range scores {
		sum += v
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Fatalf("scores should sum to 1, got %v", sum)
	}
	if scores["A"] <= scores["B"] || scores["A"] <= scores["D"] {
		t.Fatalf("seed should dominate: %+v", scores)
	}
	// Both 1-hop neighbours should outscore 2-hop nodes.
	if scores["B"] <= scores["C"] {
		t.Fatalf("B (1-hop) should beat C (2-hop): %+v", scores)
	}
}

func TestRun_EmptyGraphReturnsEmpty(t *testing.T) {
	scores := Run(nil, nil, DefaultOptions())
	if len(scores) != 0 {
		t.Fatalf("expected empty, got %v", scores)
	}
}

func TestRun_NoSeedsFallsBackToUniform(t *testing.T) {
	edges := []Edge{
		{Src: "A", Dst: "B", Weight: 1},
		{Src: "B", Dst: "A", Weight: 1},
	}
	scores := Run(nil, edges, DefaultOptions())
	if len(scores) != 2 {
		t.Fatalf("expected 2 nodes, got %v", scores)
	}
	if math.Abs(scores["A"]-scores["B"]) > 1e-6 {
		t.Fatalf("symmetric graph + uniform teleport should yield equal scores: %+v", scores)
	}
}

func TestTopK(t *testing.T) {
	scores := map[string]float64{"a": 0.1, "b": 0.5, "c": 0.3}
	top := TopK(scores, 2)
	if len(top) != 2 || top[0].ID != "b" || top[1].ID != "c" {
		t.Fatalf("unexpected top: %+v", top)
	}
}
