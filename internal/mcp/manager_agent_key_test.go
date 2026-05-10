package mcp

import (
	"context"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// resolveServerCredentials should inject X-GoClaw-Agent-Key when an agentKey
// is supplied, and force per-agent connection mode so the header doesn't leak
// across pooled connections.
func TestResolveServerCredentials_AgentKeyHeader(t *testing.T) {
	tests := []struct {
		name             string
		agentKey         string
		existingHeaders  map[string]string
		wantHeader       string
		wantHasUserCreds bool
	}{
		{
			name:             "injects when agentKey present and no existing header",
			agentKey:         "sales-orchestrator",
			existingHeaders:  nil,
			wantHeader:       "sales-orchestrator",
			wantHasUserCreds: true,
		},
		{
			name:             "does not overwrite an explicit server header",
			agentKey:         "sales-orchestrator",
			existingHeaders:  map[string]string{"X-GoClaw-Agent-Key": "operator-set"},
			wantHeader:       "operator-set",
			wantHasUserCreds: true, // still per-agent — headers may differ across calls
		},
		{
			name:             "skips injection when agentKey empty",
			agentKey:         "",
			existingHeaders:  nil,
			wantHeader:       "",
			wantHasUserCreds: false,
		},
		{
			name:             "preserves other headers when injecting",
			agentKey:         "hr-recruiter",
			existingHeaders:  map[string]string{"X-Custom": "v1"},
			wantHeader:       "hr-recruiter",
			wantHasUserCreds: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			info := store.MCPAccessInfo{
				Server: store.MCPServerData{
					Name:    "test-server",
					Enabled: true,
					Headers: marshalJSON(t, tt.existingHeaders),
				},
			}
			rs := m.resolveServerCredentials(context.Background(), info, "", tt.agentKey)
			if rs == nil {
				t.Fatal("resolveServerCredentials returned nil")
			}
			gotHeader := rs.headers["X-GoClaw-Agent-Key"]
			if gotHeader != tt.wantHeader {
				t.Errorf("X-GoClaw-Agent-Key: got %q, want %q", gotHeader, tt.wantHeader)
			}
			if rs.hasUserCreds != tt.wantHasUserCreds {
				t.Errorf("hasUserCreds: got %v, want %v", rs.hasUserCreds, tt.wantHasUserCreds)
			}
			// Ensure pre-existing headers are preserved.
			for k, v := range tt.existingHeaders {
				if k == "X-GoClaw-Agent-Key" {
					continue
				}
				if rs.headers[k] != v {
					t.Errorf("preserved header %q: got %q, want %q", k, rs.headers[k], v)
				}
			}
		})
	}
}

func marshalJSON(t *testing.T, m map[string]string) []byte {
	t.Helper()
	if m == nil {
		return nil
	}
	// Hand-roll a tiny JSON object to avoid pulling encoding/json into the
	// header of this test file; the production resolver uses
	// jsonBytesToStringMap which we exercise indirectly.
	buf := []byte("{")
	first := true
	for k, v := range m {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = append(buf, k...)
		buf = append(buf, '"', ':', '"')
		buf = append(buf, v...)
		buf = append(buf, '"')
	}
	buf = append(buf, '}')
	return buf
}
