package store

import (
	"context"
	"encoding/json"
	"time"
)

// AIEvalRun is one server-executed evaluation of a suite against a release
// manifest's model, through the gateway.
type AIEvalRun struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id"`
	SuiteID        string         `json:"suite_id"`
	ManifestID     string         `json:"manifest_id"`
	Status         string         `json:"status"` // running, completed, failed
	MinPassRate    float64        `json:"min_pass_rate"`
	Summary        map[string]any `json:"summary"`
	Error          string         `json:"error,omitempty"`
	EvidenceDigest string         `json:"evidence_digest,omitempty"`
	CreatedBy      string         `json:"created_by"`
	StartedAt      time.Time      `json:"started_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}

func (s *Store) CreateAIEvalSuite(ctx context.Context, suite AIEvalSuite) (AIEvalSuite, error) {
	rubric, _ := json.Marshal(suite.Rubric)
	cases, _ := json.Marshal(suite.TestCases)
	err := s.Pool.QueryRow(ctx, `INSERT INTO ai_eval_suites (tenant_id, service_id, name, rubric, test_cases)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at, updated_at`,
		TenantOrDefault(suite.TenantID), suite.ServiceID, suite.Name, rubric, cases).Scan(&suite.ID, &suite.CreatedAt, &suite.UpdatedAt)
	return suite, mapErr(err)
}

func (s *Store) GetAIEvalSuite(ctx context.Context, id string) (AIEvalSuite, error) {
	var suite AIEvalSuite
	var rubric, cases []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, tenant_id, service_id, name, rubric, test_cases, created_at, updated_at
		FROM ai_eval_suites WHERE id = $1`, id).Scan(&suite.ID, &suite.TenantID, &suite.ServiceID, &suite.Name, &rubric, &cases, &suite.CreatedAt, &suite.UpdatedAt)
	if err != nil {
		return suite, mapErr(err)
	}
	_ = json.Unmarshal(rubric, &suite.Rubric)
	_ = json.Unmarshal(cases, &suite.TestCases)
	return suite, nil
}

func (s *Store) ListAIEvalSuites(ctx context.Context, tenantIDs []string) ([]AIEvalSuite, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, tenant_id, service_id, name, rubric, test_cases, created_at, updated_at
		FROM ai_eval_suites WHERE ($1::text[] IS NULL OR tenant_id::text = ANY($1)) ORDER BY created_at DESC`, tenantIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AIEvalSuite{}
	for rows.Next() {
		var suite AIEvalSuite
		var rubric, cases []byte
		if err := rows.Scan(&suite.ID, &suite.TenantID, &suite.ServiceID, &suite.Name, &rubric, &cases, &suite.CreatedAt, &suite.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(rubric, &suite.Rubric)
		_ = json.Unmarshal(cases, &suite.TestCases)
		out = append(out, suite)
	}
	return out, rows.Err()
}

const aiEvalRunCols = `id, tenant_id, suite_id, manifest_id, status, min_pass_rate, summary, error, evidence_digest, created_by, started_at, completed_at`

func scanAIEvalRun(row interface{ Scan(...any) error }) (AIEvalRun, error) {
	var r AIEvalRun
	var summary []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.SuiteID, &r.ManifestID, &r.Status, &r.MinPassRate, &summary, &r.Error, &r.EvidenceDigest, &r.CreatedBy, &r.StartedAt, &r.CompletedAt)
	if err == nil {
		_ = json.Unmarshal(summary, &r.Summary)
	}
	return r, mapErr(err)
}

func (s *Store) CreateAIEvalRun(ctx context.Context, run AIEvalRun) (AIEvalRun, error) {
	return scanAIEvalRun(s.Pool.QueryRow(ctx, `INSERT INTO ai_eval_runs (tenant_id, suite_id, manifest_id, min_pass_rate, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+aiEvalRunCols,
		TenantOrDefault(run.TenantID), run.SuiteID, run.ManifestID, run.MinPassRate, run.CreatedBy))
}

// FinishAIEvalRun records a run's outcome once; a finished run is immutable.
func (s *Store) FinishAIEvalRun(ctx context.Context, id, status string, summary map[string]any, digest, errMsg string) error {
	raw, _ := json.Marshal(summary)
	return s.execOne(ctx, `UPDATE ai_eval_runs SET status = $2, summary = $3, evidence_digest = $4, error = $5, completed_at = now()
		WHERE id = $1 AND status = 'running'`, id, status, raw, digest, errMsg)
}

// ListAIEvalRuns returns the newest runs in the given tenants (nil: all).
func (s *Store) ListAIEvalRuns(ctx context.Context, tenantIDs []string, limit int) ([]AIEvalRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+aiEvalRunCols+` FROM ai_eval_runs
		WHERE ($1::text[] IS NULL OR tenant_id::text = ANY($1)) ORDER BY started_at DESC LIMIT $2`, tenantIDs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AIEvalRun{}
	for rows.Next() {
		r, err := scanAIEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetAIEvalRun(ctx context.Context, id string) (AIEvalRun, error) {
	return scanAIEvalRun(s.Pool.QueryRow(ctx, `SELECT `+aiEvalRunCols+` FROM ai_eval_runs WHERE id = $1`, id))
}
