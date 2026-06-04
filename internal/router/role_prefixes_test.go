package router

import (
	"slices"
	"strings"
	"testing"
)

// AllRoles() and prefixTemplates are two hand-maintained lists that must stay
// in lockstep. A future role added to one but forgotten in the other drifts
// silently: ValidateRole would accept it (it checks the map) while AllRoles()
// would omit it from docs/eval/error messages. These tests close the loop.
func TestAllRolesCoversTemplates(t *testing.T) {
	all := AllRoles()
	for role := range prefixTemplates {
		if !slices.Contains(all, role) {
			t.Errorf("role %q present in prefixTemplates but missing from AllRoles()", role)
		}
	}
	for _, role := range all {
		if _, ok := prefixTemplates[role]; !ok {
			t.Errorf("role %q present in AllRoles() but missing from prefixTemplates", role)
		}
	}
}

// AllRoles() must have no duplicates — it's the canonical stable-order list
// used in docs and error messages.
func TestAllRolesNoDuplicates(t *testing.T) {
	seen := make(map[string]struct{}, len(AllRoles()))
	for _, role := range AllRoles() {
		if _, dup := seen[role]; dup {
			t.Errorf("role %q appears more than once in AllRoles()", role)
		}
		seen[role] = struct{}{}
	}
}

// ValidateRole is what gates request entry, and it consults prefixTemplates
// (not AllRoles()). If they ever drift, this asserts that ValidateRole's
// allow-list and the canonical AllRoles() list still agree.
func TestValidateRoleAcceptsEveryRoleInAllRoles(t *testing.T) {
	for _, role := range AllRoles() {
		if err := ValidateRole(role); err != nil {
			t.Errorf("ValidateRole(%q) returned %v; expected nil", role, err)
		}
	}
}

func TestValidateRoleRejectsUnknown(t *testing.T) {
	if err := ValidateRole("not_a_real_role"); err == nil {
		t.Error("ValidateRole accepted an unknown role; expected error")
	}
}

func TestValidateRoleAllowsEmpty(t *testing.T) {
	// Empty role means "caller doesn't want role-scoped retrieval" — must not error.
	if err := ValidateRole(""); err != nil {
		t.Errorf("ValidateRole(\"\") returned %v; expected nil", err)
	}
}

// FormatPrefix renders the template with the supplied or defaulted context;
// confirm the social role's template actually substitutes both placeholders.
func TestFormatPrefixSocialUsesDefaultContext(t *testing.T) {
	out := FormatPrefix(RoleSocial, "weekend post idea", "")
	if !strings.Contains(out, "the brand") {
		t.Errorf("FormatPrefix(social, q, \"\") did not insert defaultContextFor(social); got %q", out)
	}
	if !strings.Contains(out, "weekend post idea") {
		t.Errorf("FormatPrefix(social, q, \"\") did not insert {query}; got %q", out)
	}
}

func TestFormatPrefixUnknownRoleReturnsBareQuery(t *testing.T) {
	q := "hello"
	out := FormatPrefix("not_a_real_role", q, "ctx")
	if out != q {
		t.Errorf("FormatPrefix on unknown role returned %q; expected bare query %q", out, q)
	}
}
