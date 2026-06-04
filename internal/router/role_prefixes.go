// Package router contains the per-role query instruction templates and (later)
// the intent classifier + router that picks a retrieval strategy.
//
// Phase 2 ships ONLY role_prefixes.go. The intent classifier (Haiku) lands in
// Phase 4 (P4.4).
//
// CARRY-FORWARD from phase-1-foundation/learnings.md: pre-embedded queries
// come in on POST /v1/kb/retrieve. The prefix MUST be baked into the query
// text the CLIENT embeds — applying it server-side after embedding would
// have no effect on the vector representation. This file is the canonical
// source of the template strings; lib/agents/role-prefixes.ts in fzst-claw
// MUST mirror it exactly. We keep the Go file authoritative because the
// query router and reranker (also Go) read from it.
package router

import (
	"fmt"
	"strings"
)

// Role identifiers. Match the values stored in kb_chunks.role_tags.
const (
	RoleSales        = "sales"
	RoleHRInternal   = "hr_internal"
	RoleHRRecruiter  = "hr_recruiter"
	RoleCustomerCare = "customer_care"
	// RoleMarketing is the umbrella / aggregator role — it synthesizes
	// across the sub-departments under marketing (social-media today;
	// more siblings landing later: ads, email/CRM, SEO, etc.) and produces
	// long-form brand copy at the org level. Think "marketing director
	// pulling a status update across the channels they own."
	RoleMarketing = "marketing"
	RolePublic    = "public"
	// RoleSocial is one of the leaf departments that sits under the
	// marketing umbrella (RoleMarketing). It owns the platform-native
	// short-form voice + the inbox/reply/growth-action pipeline; it has
	// its own dept_registry entry, brand profile, operator context, and
	// jobs/workers in fzst-claw. Kept as a distinct role (not a flavor of
	// marketing) so the prefix can carry the platform-voice framing and
	// so future sibling sub-depts (e.g. ads, email) plug in alongside it
	// without disturbing RoleMarketing's aggregator role.
	RoleSocial = "social"
)

// prefixTemplates maps a role to its instruction prefix. {query} is the
// caller's raw query; {context} is an optional caller-provided context blob
// (e.g. lead industry for sales). Templates without {context} ignore it.
var prefixTemplates = map[string]string{
	RoleSales:        "As a sales rep crafting outbound to {context}: {query}",
	RoleHRInternal:   "As an HR business partner answering an internal employee question: {query}",
	RoleHRRecruiter:  "As a recruiter answering a candidate question (public information only): {query}",
	RoleCustomerCare: "As a customer-care agent helping a customer with {context}: {query}",
	RoleMarketing:    "As a marketing copywriter producing brand-aligned copy about {context}: {query}",
	RolePublic:       "As a public-facing assistant answering a visitor question: {query}",
	RoleSocial:       "As the brand's social-media voice writing about {context}: {query}",
}

// FormatPrefix renders the prefix template for a role. Unknown roles return
// the bare query unchanged. {context} substitutes with the supplied context
// string or "this clinic" / "this customer" sensible defaults when empty.
func FormatPrefix(role, query, context string) string {
	t, ok := prefixTemplates[role]
	if !ok {
		return query
	}
	if context == "" {
		context = defaultContextFor(role)
	}
	out := strings.ReplaceAll(t, "{context}", context)
	out = strings.ReplaceAll(out, "{query}", query)
	return out
}

func defaultContextFor(role string) string {
	switch role {
	case RoleSales:
		return "a prospect"
	case RoleCustomerCare:
		return "their issue"
	case RoleMarketing:
		return "the brand"
	case RoleSocial:
		return "the brand"
	}
	return ""
}

// AllRoles returns the canonical list. Stable order (used by docs/eval).
func AllRoles() []string {
	return []string{
		RoleSales,
		RoleHRInternal,
		RoleHRRecruiter,
		RoleCustomerCare,
		RoleMarketing,
		RolePublic,
		RoleSocial,
	}
}

// ValidateRole returns an error if role is non-empty and not in the canonical list.
// An empty role is allowed (caller doesn't want role-scoped retrieval).
func ValidateRole(role string) error {
	if role == "" {
		return nil
	}
	if _, ok := prefixTemplates[role]; !ok {
		return fmt.Errorf("router: unknown role %q (allowed: %v)", role, AllRoles())
	}
	return nil
}
