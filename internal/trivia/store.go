package trivia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Store persists trivia runs + per-question outcomes.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// SampleChunks pulls N random non-superseded chunks for the tenant. The tenant_id
// is read from the context (same pattern as kb.Store). Returns at most N rows.
func (s *Store) SampleChunks(ctx context.Context, n int) ([]SampledChunk, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, errors.New("trivia: missing tenant_id in context")
	}
	if n <= 0 {
		n = DefaultSampleSize
	}
	// TABLESAMPLE BERNOULLI gives a fast, statistically-acceptable random subset
	// without scanning the full table. Phase 3 corpus is small (~2k chunks per
	// tenant) so the sampling rate is high; tune later if a tenant grows.
	q := `
		SELECT id, department, role_tags, text, embedding_model_id
		FROM kb.kb_chunks
		WHERE tenant_id = $1
		  AND superseded_at IS NULL
		ORDER BY random()
		LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, tenantID, n)
	if err != nil {
		return nil, fmt.Errorf("trivia sample: %w", err)
	}
	defer rows.Close()

	out := make([]SampledChunk, 0, n)
	for rows.Next() {
		var c SampledChunk
		var roleTagsRaw string
		if err := rows.Scan(&c.ID, &c.Department, &roleTagsRaw, &c.Text, &c.EmbeddingModelID); err != nil {
			return nil, fmt.Errorf("trivia sample scan: %w", err)
		}
		c.RoleTags = parsePgTextArray(roleTagsRaw)
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertRun persists a new kb.trivia_runs row. The id is generated server-side.
// If run.Department is non-empty (Phase 8 v2 per-role runs), it is stored in
// extra.department alongside the normal columns.
func (s *Store) InsertRun(ctx context.Context, run *Run) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return errors.New("trivia: missing tenant_id in context")
	}
	runID := uuid.Must(uuid.NewV7())
	run.ID = runID.String()
	run.TenantID = tenantID.String()
	if run.Mode == "" {
		run.Mode = ModeShadow
	}
	if run.Status == "" {
		run.Status = StatusOK
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	extra := "{}"
	if run.Department != "" {
		// Phase 8 trivia v2 — tag run with its target department so dashboards
		// can break out per-role retrieval-drift.
		extra = fmt.Sprintf(`{"department":%q,"trivia_version":2}`, run.Department)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kb.trivia_runs
		  (id, tenant_id, started_at, mode, sample_size, generator_model, embedding_model_id, status, extra)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		runID, tenantID, run.StartedAt, string(run.Mode), run.SampleSize,
		run.GeneratorModel, run.EmbeddingModelID, string(run.Status), extra)
	if err != nil {
		return fmt.Errorf("trivia insert run: %w", err)
	}
	return nil
}

// SampleChunksByDepartment is the Phase 8 trivia v2 sampler — same shape as
// SampleChunks but constrained to one department. Used by RunOnceForDepartment
// to fan out per-role runs from the daily ticker.
func (s *Store) SampleChunksByDepartment(ctx context.Context, department string, n int) ([]SampledChunk, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, errors.New("trivia: missing tenant_id in context")
	}
	if n <= 0 {
		n = DefaultSampleSize
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, department, role_tags, text, embedding_model_id
		FROM kb.kb_chunks
		WHERE tenant_id = $1
		  AND department = $2
		  AND superseded_at IS NULL
		ORDER BY random()
		LIMIT $3`,
		tenantID, department, n)
	if err != nil {
		return nil, fmt.Errorf("trivia sample by dept: %w", err)
	}
	defer rows.Close()
	out := make([]SampledChunk, 0, n)
	for rows.Next() {
		var c SampledChunk
		var roleTagsRaw string
		if err := rows.Scan(&c.ID, &c.Department, &roleTagsRaw, &c.Text, &c.EmbeddingModelID); err != nil {
			return nil, fmt.Errorf("trivia sample-by-dept scan: %w", err)
		}
		c.RoleTags = parsePgTextArray(roleTagsRaw)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DepartmentsWithChunks lists departments that have at least one non-superseded
// chunk for the tenant in ctx. The v2 ticker uses this to know which per-role
// runs to fire.
func (s *Store) DepartmentsWithChunks(ctx context.Context) ([]string, error) {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, errors.New("trivia: missing tenant_id in context")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT department FROM kb.kb_chunks
		WHERE tenant_id = $1 AND superseded_at IS NULL
		ORDER BY department`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("trivia depts: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertQuestion persists one outcome. Question id is server-generated.
func (s *Store) InsertQuestion(ctx context.Context, q *QuestionOutcome) error {
	tenantID := store.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return errors.New("trivia: missing tenant_id in context")
	}
	id := uuid.Must(uuid.NewV7())
	q.ID = id.String()
	q.TenantID = tenantID.String()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kb.trivia_questions
		  (id, run_id, tenant_id, source_chunk_id, question, retrieved_top10, source_rank, hard_negatives, embedding_tokens)
		VALUES ($1, $2, $3, $4, $5, $6::uuid[], $7, $8::uuid[], $9)`,
		id, q.RunID, tenantID, q.SourceChunkID, q.Question,
		uuidArrayLiteral(q.RetrievedTop10), q.SourceRank,
		uuidArrayLiteral(q.HardNegatives), q.EmbeddingTokens)
	if err != nil {
		return fmt.Errorf("trivia insert question: %w", err)
	}
	return nil
}

// FinalizeRun updates the aggregate counts + status when the run finishes.
func (s *Store) FinalizeRun(ctx context.Context, run *Run) error {
	now := time.Now().UTC()
	run.FinishedAt = &now
	_, err := s.db.ExecContext(ctx, `
		UPDATE kb.trivia_runs
		   SET finished_at   = $1,
		       n_questions   = $2,
		       hit_at_5_count = $3,
		       hit_at_10_count = $4,
		       mean_rank     = $5,
		       status        = $6
		 WHERE id = $7`,
		now, run.NQuestions, run.HitAt5Count, run.HitAt10Count, run.MeanRank, string(run.Status), run.ID)
	if err != nil {
		return fmt.Errorf("trivia finalize: %w", err)
	}
	return nil
}

// ListActiveTenants returns tenant_ids that have at least one non-superseded
// chunk in kb.kb_chunks. The daily ticker uses this to know which tenants to
// run trivia for.
func (s *Store) ListActiveTenants(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT tenant_id::text
		  FROM kb.kb_chunks
		 WHERE superseded_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("trivia list tenants: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// uuidArrayLiteral builds the Postgres literal `{u1,u2,...}` cast as uuid[].
// Caller-supplied IDs are uuid strings, never user input, so escaping is trivial.
func uuidArrayLiteral(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(id)
	}
	b.WriteByte('}')
	return b.String()
}

// parsePgTextArray converts `{a,b,c}` to []string. Tolerant of empty arrays.
func parsePgTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
