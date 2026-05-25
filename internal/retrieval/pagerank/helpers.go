package pagerank

import (
	"regexp"
	"strings"
)

var wsRe = regexp.MustCompile(`\s+`)

// normalizeOne returns the same lower-cased / whitespace-collapsed form that
// the generated kb.kg_entities.normalized_name column produces.
func normalizeOne(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return wsRe.ReplaceAllString(s, " ")
}

func normalizeAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		n := normalizeOne(s)
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// pqUUIDArray formats a Go []string of UUIDs as a Postgres array literal.
func pqUUIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		// UUIDs are safe to write verbatim. Reject anything not matching the
		// uuid charset to be defensive.
		for _, r := range id {
			if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-' {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('}')
	return b.String()
}
