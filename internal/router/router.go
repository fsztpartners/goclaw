package router

import (
	"context"
)

// Route is the chosen retrieval strategy for one query. The Reason is logged
// to retrieval_traces.route alongside the actual retrieval path that ran.
type Route struct {
	Strategy  string         // "hipporag" | "pageindex" | "colpali" | "hybrid"
	Reason    string         // short text for the trace ("multi_hop_high_conf", etc.)
	Classification Classification
}

// Decide picks a retrieval strategy from a Classification.
//
// Policy (Phase 5 v1):
//   - confidence < threshold        → hybrid (fallback)
//   - intent=multi_hop AND is_multi_entity → hipporag
//   - intent=visual                  → colpali (stub)
//   - intent=structural              → pageindex (stub)
//   - otherwise                      → hybrid
//
// Stub strategies fall through to hybrid until their modules are wired
// (colpali Phase 7, pageindex Phase 7). The router still emits the named
// strategy in the trace so we can see how often we'd want to use them once
// available — informs Phase 7 prioritization.
const ConfidenceFloor = 0.55

func Decide(c Classification) Route {
	if c.Confidence < ConfidenceFloor {
		return Route{Strategy: "hybrid", Reason: "low_confidence", Classification: c}
	}
	switch {
	case c.Intent == "multi_hop" && c.IsMultiEntity:
		return Route{Strategy: "hipporag", Reason: "multi_hop_multi_entity", Classification: c}
	case c.Intent == "visual":
		return Route{Strategy: "colpali", Reason: "visual_intent", Classification: c}
	case c.Intent == "structural":
		return Route{Strategy: "pageindex", Reason: "structural_intent", Classification: c}
	default:
		return Route{Strategy: "hybrid", Reason: "default", Classification: c}
	}
}

// Router wires a classifier to Decide. Pure plumbing; provided so kb.Store can
// hold one Router rather than the raw classifier + policy fn.
type Router struct {
	classifier Classifier
}

// NewRouter returns a Router using the provided classifier. Pass
// NewHaikuClassifier() for the production path.
func NewRouter(c Classifier) *Router { return &Router{classifier: c} }

// Pick classifies the query and applies Decide(). On classifier error, falls
// back to the heuristic which yields a hybrid route.
func (r *Router) Pick(ctx context.Context, query string) Route {
	c, _ := r.classifier.Classify(ctx, query)
	return Decide(c)
}
