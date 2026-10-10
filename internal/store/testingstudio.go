package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, fn)
}

// ---------------------------------------------------------------------------
// Test Studio Models
// ---------------------------------------------------------------------------

type TestSuite struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	APIID          *string   `json:"api_id,omitempty"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Ownership      string    `json:"ownership"` // personal, team
	CreatedBy      string    `json:"created_by"`
	Visibility     string    `json:"visibility"` // private, tenant, public
	CurrentVersion int       `json:"current_version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type TestSuiteVersion struct {
	ID            string          `json:"id"`
	SuiteID       string          `json:"suite_id"`
	Version       int             `json:"version"`
	SchemaVersion int             `json:"schema_version"`
	ContentHash   string          `json:"content_hash"`
	Definition    SuiteDefinition `json:"definition"`
	Author        string          `json:"author"`
	CreatedAt     time.Time       `json:"created_at"`
}

type SuiteDefinition struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Variables   []VariableDef    `json:"variables,omitempty"`
	Requests    []RequestDef     `json:"requests"`
	IgnorePaths []string         `json:"ignore_paths,omitempty"` // Exact case-sensitive JSON pointers; explicit subtree ignores
	Comparison  ComparisonPolicy `json:"comparison,omitempty"`
}

// ComparisonPolicy is versioned with the suite, so changing it invalidates old gate evidence.
// ComparisonEngineVersion invalidates pre-upgrade evidence when comparison semantics change.
const ComparisonEngineVersion = 2

type ComparisonPolicy struct {
	Headers                   []string `json:"headers,omitempty"`                      // Defaults to content-type; never compare sensitive or internal headers.
	MaxLatencyIncreasePercent float64  `json:"max_latency_increase_percent,omitempty"` // Zero disables the timing gate.
	MinLatencyIncreaseMS      float64  `json:"min_latency_increase_ms,omitempty"`      // Regression must exceed both thresholds.
}

type VariableDef struct {
	Key          string `json:"key"`
	DefaultValue string `json:"default_value"`
	Description  string `json:"description"`
}

type RequestDef struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Method      string            `json:"method"`
	Path        string            `json:"path"` // Path relative to API base_path or full relative path
	Headers     map[string]string `json:"headers,omitempty"`
	QueryParams map[string]string `json:"query_params,omitempty"`
	Body        string            `json:"body,omitempty"`
	Auth        RequestAuthDef    `json:"auth,omitempty"`
	Assertions  []AssertionDef    `json:"assertions,omitempty"`
	Extracts    []ExtractDef      `json:"extracts,omitempty"` // Variable extraction from response
}

type RequestAuthDef struct {
	Type          string `json:"type"` // none, inherit, api_key, bearer_token
	CredentialKey string `json:"credential_key,omitempty"`
	TokenValue    string `json:"token_value,omitempty"`
}

type AssertionDef struct {
	Type        string `json:"type"`             // status_code, header_equals, header_exists, json_path_equals, json_path_exists, body_contains, response_time_ms
	Target      string `json:"target,omitempty"` // header name, json path (e.g. /data/id)
	Expected    string `json:"expected"`
	Description string `json:"description,omitempty"`
}

type ExtractDef struct {
	Source  string `json:"source"` // json_path, header
	Target  string `json:"target"` // path or header name
	VarName string `json:"var_name"`
}

type TestEnvironment struct {
	ID                 string            `json:"id"`
	TenantID           string            `json:"tenant_id"`
	Name               string            `json:"name"`
	GatewayTarget      string            `json:"gateway_target"` // e.g. http://127.0.0.1:8080
	Variables          map[string]string `json:"variables"`
	CredentialBindings map[string]string `json:"credential_bindings"`
	Revision           int               `json:"revision"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

type TestCredential struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	Name              string    `json:"name"`
	ProviderRef       string    `json:"provider_ref"`
	CredentialVersion int       `json:"credential_version"`
	Permissions       []string  `json:"permissions"`
	MaskedPreview     string    `json:"masked_preview"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type TestRun struct {
	ID                      string         `json:"id"`
	TenantID                string         `json:"tenant_id"`
	SuiteID                 string         `json:"suite_id"`
	SuiteVersionID          *string        `json:"suite_version_id,omitempty"`
	SuiteContentHash        string         `json:"suite_content_hash"`
	EnvironmentID           *string        `json:"environment_id,omitempty"`
	Actor                   string         `json:"actor"`
	Mode                    string         `json:"mode"`            // standard, comparison, canary_gate
	LifecycleState          string         `json:"lifecycle_state"` // queued, running, completed, failed, cancelled, interrupted
	TotalSteps              int            `json:"total_steps"`
	PassedSteps             int            `json:"passed_steps"`
	FailedSteps             int            `json:"failed_steps"`
	SkippedSteps            int            `json:"skipped_steps"`
	StartedAt               *time.Time     `json:"started_at,omitempty"`
	CompletedAt             *time.Time     `json:"completed_at,omitempty"`
	CancelledAt             *time.Time     `json:"cancelled_at,omitempty"`
	FailureReason           string         `json:"failure_reason,omitempty"`
	ImmutableInputs         map[string]any `json:"immutable_inputs,omitempty"`
	ActualRevision          int64          `json:"actual_revision"`
	ActualCandidateRevision int64          `json:"actual_candidate_revision,omitempty"`
	ComparisonSummary       map[string]any `json:"comparison_summary,omitempty"`
	ExpiresAt               *time.Time     `json:"expires_at,omitempty"`
	CreatedAt               time.Time      `json:"created_at"`
}

type TestRunStep struct {
	ID               string         `json:"id"`
	RunID            string         `json:"run_id"`
	StepIndex        int            `json:"step_index"`
	Cohort           string         `json:"cohort"` // baseline, candidate, standard
	RequestName      string         `json:"request_name"`
	Method           string         `json:"method"`
	URL              string         `json:"url"`
	TargetRevision   int64          `json:"target_revision"`
	ObservedRevision int64          `json:"observed_revision"`
	DurationMS       float64        `json:"duration_ms"`
	StatusCode       int            `json:"status_code"`
	AssertionResults []AssertionRes `json:"assertion_results"`
	DecisionPolicy   string         `json:"decision_policy"`
	DecisionReason   string         `json:"decision_reason"`
	RedactedRequest  map[string]any `json:"redacted_request,omitempty"`
	RedactedResponse map[string]any `json:"redacted_response,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
}

type AssertionRes struct {
	Type        string `json:"type"`
	Target      string `json:"target"`
	Expected    string `json:"expected"`
	Actual      string `json:"actual"`
	Passed      bool   `json:"passed"`
	Description string `json:"description,omitempty"`
	Error       string `json:"error,omitempty"`
}

type TestJob struct {
	ID              string     `json:"id"`
	RunID           string     `json:"run_id"`
	TenantID        string     `json:"tenant_id"`
	Status          string     `json:"status"` // pending, leased, done, failed, cancelled
	ClaimLeaseUntil *time.Time `json:"claim_lease_until,omitempty"`
	HeartbeatAt     *time.Time `json:"heartbeat_at,omitempty"`
	Attempts        int        `json:"attempts"`
	WorkerID        string     `json:"worker_id"`
	NextAvailableAt time.Time  `json:"next_available_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

type TestGatePolicy struct {
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	APIID              *string   `json:"api_id,omitempty"`
	TargetEnvironment  string    `json:"target_environment"`
	RequiredSuiteIDs   []string  `json:"required_suite_ids"`
	FreshnessSeconds   int       `json:"freshness_seconds"`
	EnforcementEnabled bool      `json:"enforcement_enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type TestGateEvidence struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	APIID             *string   `json:"api_id,omitempty"`
	Revision          int64     `json:"revision"`
	RunID             *string   `json:"run_id,omitempty"`
	TargetFingerprint string    `json:"target_fingerprint"`
	Eligible          bool      `json:"eligible"`
	Reasons           []string  `json:"reasons"`
	VerifiedAt        time.Time `json:"verified_at"`
}

// ---------------------------------------------------------------------------
// Store Methods: Test Suites & Versions
// ---------------------------------------------------------------------------

const suiteCols = `id, tenant_id, api_id, name, description, ownership, created_by, visibility, current_version, created_at, updated_at`

func scanSuite(row pgx.Row) (TestSuite, error) {
	var s TestSuite
	err := row.Scan(&s.ID, &s.TenantID, &s.APIID, &s.Name, &s.Description, &s.Ownership, &s.CreatedBy, &s.Visibility, &s.CurrentVersion, &s.CreatedAt, &s.UpdatedAt)
	return s, mapErr(err)
}

func (s *Store) CreateTestSuite(ctx context.Context, suite TestSuite, def SuiteDefinition, author string) (TestSuite, TestSuiteVersion, error) {
	var created TestSuite
	var version TestSuiteVersion

	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		row := tx.QueryRow(ctx, `INSERT INTO test_suites (tenant_id, api_id, name, description, ownership, created_by, visibility, current_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1) RETURNING `+suiteCols,
			TenantOrDefault(suite.TenantID), suite.APIID, suite.Name, suite.Description, suite.Ownership, suite.CreatedBy, suite.Visibility)
		created, err = scanSuite(row)
		if err != nil {
			return err
		}

		defBytes, _ := json.Marshal(def)
		h := sha256.Sum256(defBytes)
		contentHash := hex.EncodeToString(h[:])

		vRow := tx.QueryRow(ctx, `INSERT INTO test_suite_versions (suite_id, version, schema_version, content_hash, definition, author)
			VALUES ($1, 1, 1, $2, $3, $4) RETURNING id, suite_id, version, schema_version, content_hash, definition, author, created_at`,
			created.ID, contentHash, defBytes, author)

		var defJSON []byte
		err = vRow.Scan(&version.ID, &version.SuiteID, &version.Version, &version.SchemaVersion, &version.ContentHash, &defJSON, &version.Author, &version.CreatedAt)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(defJSON, &version.Definition)
		return nil
	})

	return created, version, err
}

func (s *Store) GetTestSuite(ctx context.Context, id string) (TestSuite, error) {
	return scanSuite(s.Pool.QueryRow(ctx, `SELECT `+suiteCols+` FROM test_suites WHERE id=$1`, id))
}

func (s *Store) ListTestSuites(ctx context.Context, tenantID string) ([]TestSuite, error) {
	query := `SELECT ` + suiteCols + ` FROM test_suites`
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		query += ` WHERE tenant_id=$1 ORDER BY created_at DESC`
		rows, err = s.Pool.Query(ctx, query, tenantID)
	} else {
		query += ` ORDER BY created_at DESC`
		rows, err = s.Pool.Query(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TestSuite{}
	for rows.Next() {
		item, err := scanSuite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTestSuite(ctx context.Context, suite TestSuite, def *SuiteDefinition, author string) (TestSuite, *TestSuiteVersion, error) {
	var updated TestSuite
	var newVer *TestSuiteVersion

	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		if def != nil {
			// Increment version
			row := tx.QueryRow(ctx, `UPDATE test_suites SET name=$2, description=$3, ownership=$4, visibility=$5, current_version=current_version+1, updated_at=now()
				WHERE id=$1 RETURNING `+suiteCols,
				suite.ID, suite.Name, suite.Description, suite.Ownership, suite.Visibility)
			updated, err = scanSuite(row)
			if err != nil {
				return err
			}

			defBytes, _ := json.Marshal(*def)
			h := sha256.Sum256(defBytes)
			contentHash := hex.EncodeToString(h[:])

			var ver TestSuiteVersion
			vRow := tx.QueryRow(ctx, `INSERT INTO test_suite_versions (suite_id, version, schema_version, content_hash, definition, author)
				VALUES ($1, $2, 1, $3, $4, $5) RETURNING id, suite_id, version, schema_version, content_hash, definition, author, created_at`,
				updated.ID, updated.CurrentVersion, contentHash, defBytes, author)

			var defJSON []byte
			err = vRow.Scan(&ver.ID, &ver.SuiteID, &ver.Version, &ver.SchemaVersion, &ver.ContentHash, &defJSON, &ver.Author, &ver.CreatedAt)
			if err != nil {
				return err
			}
			_ = json.Unmarshal(defJSON, &ver.Definition)
			newVer = &ver
		} else {
			row := tx.QueryRow(ctx, `UPDATE test_suites SET name=$2, description=$3, ownership=$4, visibility=$5, updated_at=now()
				WHERE id=$1 RETURNING `+suiteCols,
				suite.ID, suite.Name, suite.Description, suite.Ownership, suite.Visibility)
			updated, err = scanSuite(row)
			if err != nil {
				return err
			}
		}
		return nil
	})

	return updated, newVer, err
}

func (s *Store) DeleteTestSuite(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM test_suites WHERE id=$1`, id)
}

func (s *Store) GetTestSuiteVersion(ctx context.Context, suiteID string, version int) (TestSuiteVersion, error) {
	var v TestSuiteVersion
	var defJSON []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, suite_id, version, schema_version, content_hash, definition, author, created_at
		FROM test_suite_versions WHERE suite_id=$1 AND version=$2`, suiteID, version).
		Scan(&v.ID, &v.SuiteID, &v.Version, &v.SchemaVersion, &v.ContentHash, &defJSON, &v.Author, &v.CreatedAt)
	if err != nil {
		return v, mapErr(err)
	}
	_ = json.Unmarshal(defJSON, &v.Definition)
	return v, nil
}

func (s *Store) GetLatestTestSuiteVersion(ctx context.Context, suiteID string) (TestSuiteVersion, error) {
	var v TestSuiteVersion
	var defJSON []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, suite_id, version, schema_version, content_hash, definition, author, created_at
		FROM test_suite_versions WHERE suite_id=$1 ORDER BY version DESC LIMIT 1`, suiteID).
		Scan(&v.ID, &v.SuiteID, &v.Version, &v.SchemaVersion, &v.ContentHash, &defJSON, &v.Author, &v.CreatedAt)
	if err != nil {
		return v, mapErr(err)
	}
	_ = json.Unmarshal(defJSON, &v.Definition)
	return v, nil
}

func (s *Store) ListTestSuiteVersions(ctx context.Context, suiteID string) ([]TestSuiteVersion, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, suite_id, version, schema_version, content_hash, definition, author, created_at
		FROM test_suite_versions WHERE suite_id=$1 ORDER BY version DESC`, suiteID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	out := []TestSuiteVersion{}
	for rows.Next() {
		var v TestSuiteVersion
		var defJSON []byte
		if err := rows.Scan(&v.ID, &v.SuiteID, &v.Version, &v.SchemaVersion, &v.ContentHash, &defJSON, &v.Author, &v.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(defJSON, &v.Definition)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PurgeExpiredTestStudioData(ctx context.Context) (int64, error) {
	// 7 days retention for detailed steps, artifacts, and terminal jobs
	_, _ = s.Pool.Exec(ctx, `DELETE FROM test_run_steps WHERE created_at < now() - INTERVAL '7 days'`)
	_, _ = s.Pool.Exec(ctx, `DELETE FROM test_run_artifacts WHERE created_at < now() - INTERVAL '7 days'`)
	_, _ = s.Pool.Exec(ctx, `DELETE FROM test_jobs WHERE status IN ('completed', 'failed', 'cancelled') AND created_at < now() - INTERVAL '7 days'`)

	// 30 days retention for runs
	tag, err := s.Pool.Exec(ctx, `DELETE FROM test_runs WHERE created_at < now() - INTERVAL '30 days'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Store Methods: Test Environments
// ---------------------------------------------------------------------------

const envCols = `id, tenant_id, name, gateway_target, variables, credential_bindings, revision, created_at, updated_at`

func scanEnvironment(row pgx.Row) (TestEnvironment, error) {
	var e TestEnvironment
	var varsJSON, credsJSON []byte
	err := row.Scan(&e.ID, &e.TenantID, &e.Name, &e.GatewayTarget, &varsJSON, &credsJSON, &e.Revision, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return e, mapErr(err)
	}
	_ = json.Unmarshal(varsJSON, &e.Variables)
	_ = json.Unmarshal(credsJSON, &e.CredentialBindings)
	if e.Variables == nil {
		e.Variables = map[string]string{}
	}
	if e.CredentialBindings == nil {
		e.CredentialBindings = map[string]string{}
	}
	return e, nil
}

func (s *Store) CreateTestEnvironment(ctx context.Context, env TestEnvironment) (TestEnvironment, error) {
	varsJSON, _ := json.Marshal(env.Variables)
	credsJSON, _ := json.Marshal(env.CredentialBindings)
	return scanEnvironment(s.Pool.QueryRow(ctx, `INSERT INTO test_environments (tenant_id, name, gateway_target, variables, credential_bindings, revision)
		VALUES ($1, $2, $3, $4, $5, 1) RETURNING `+envCols,
		TenantOrDefault(env.TenantID), env.Name, env.GatewayTarget, varsJSON, credsJSON))
}

func (s *Store) GetTestEnvironment(ctx context.Context, id string) (TestEnvironment, error) {
	return scanEnvironment(s.Pool.QueryRow(ctx, `SELECT `+envCols+` FROM test_environments WHERE id=$1`, id))
}

func (s *Store) ListTestEnvironments(ctx context.Context, tenantID string) ([]TestEnvironment, error) {
	query := `SELECT ` + envCols + ` FROM test_environments`
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		query += ` WHERE tenant_id=$1 ORDER BY name ASC`
		rows, err = s.Pool.Query(ctx, query, tenantID)
	} else {
		query += ` ORDER BY name ASC`
		rows, err = s.Pool.Query(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TestEnvironment{}
	for rows.Next() {
		item, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTestEnvironment(ctx context.Context, env TestEnvironment) (TestEnvironment, error) {
	varsJSON, _ := json.Marshal(env.Variables)
	credsJSON, _ := json.Marshal(env.CredentialBindings)
	return scanEnvironment(s.Pool.QueryRow(ctx, `UPDATE test_environments SET name=$2, gateway_target=$3, variables=$4, credential_bindings=$5, revision=revision+1, updated_at=now()
		WHERE id=$1 RETURNING `+envCols,
		env.ID, env.Name, env.GatewayTarget, varsJSON, credsJSON))
}

func (s *Store) DeleteTestEnvironment(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM test_environments WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// Store Methods: Test Runs & Steps
// ---------------------------------------------------------------------------

const runCols = `id, tenant_id, suite_id, suite_version_id, suite_content_hash, environment_id, actor, mode,
	lifecycle_state, total_steps, passed_steps, failed_steps, skipped_steps, started_at, completed_at,
	cancelled_at, failure_reason, immutable_inputs, actual_revision, actual_candidate_revision,
	comparison_summary, expires_at, created_at`

func scanRun(row pgx.Row) (TestRun, error) {
	var r TestRun
	var inputsJSON, compJSON []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.SuiteID, &r.SuiteVersionID, &r.SuiteContentHash, &r.EnvironmentID,
		&r.Actor, &r.Mode, &r.LifecycleState, &r.TotalSteps, &r.PassedSteps, &r.FailedSteps, &r.SkippedSteps,
		&r.StartedAt, &r.CompletedAt, &r.CancelledAt, &r.FailureReason, &inputsJSON, &r.ActualRevision,
		&r.ActualCandidateRevision, &compJSON, &r.ExpiresAt, &r.CreatedAt)
	if err != nil {
		return r, mapErr(err)
	}
	_ = json.Unmarshal(inputsJSON, &r.ImmutableInputs)
	_ = json.Unmarshal(compJSON, &r.ComparisonSummary)
	return r, nil
}

func (s *Store) CreateTestRun(ctx context.Context, r TestRun) (TestRun, error) {
	inputsJSON, _ := json.Marshal(r.ImmutableInputs)
	compJSON, _ := json.Marshal(r.ComparisonSummary)
	if r.Mode == "" {
		r.Mode = "standard"
	}
	if r.LifecycleState == "" {
		r.LifecycleState = "queued"
	}
	expires := time.Now().Add(30 * 24 * time.Hour) // 30-day default retention

	return scanRun(s.Pool.QueryRow(ctx, `INSERT INTO test_runs (tenant_id, suite_id, suite_version_id, suite_content_hash, environment_id, actor, mode, lifecycle_state, total_steps, immutable_inputs, actual_revision, actual_candidate_revision, comparison_summary, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+runCols,
		TenantOrDefault(r.TenantID), r.SuiteID, r.SuiteVersionID, r.SuiteContentHash, r.EnvironmentID, r.Actor, r.Mode, r.LifecycleState, r.TotalSteps, inputsJSON, r.ActualRevision, r.ActualCandidateRevision, compJSON, expires))
}

func (s *Store) GetTestRun(ctx context.Context, id string) (TestRun, error) {
	return scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM test_runs WHERE id=$1`, id))
}

func (s *Store) ListTestRuns(ctx context.Context, tenantID, suiteID string, limit int) ([]TestRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if suiteID != "" {
		rows, err = s.Pool.Query(ctx, `SELECT `+runCols+` FROM test_runs WHERE suite_id=$1 ORDER BY created_at DESC LIMIT $2`, suiteID, limit)
	} else if tenantID != "" {
		rows, err = s.Pool.Query(ctx, `SELECT `+runCols+` FROM test_runs WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	} else {
		rows, err = s.Pool.Query(ctx, `SELECT `+runCols+` FROM test_runs ORDER BY created_at DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TestRun{}
	for rows.Next() {
		item, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTestRunProgress(ctx context.Context, id string, state string, passed, failed, skipped int, failureReason string, actualRev, candRev int64, compSummary map[string]any) error {
	var compJSON []byte
	if compSummary != nil {
		compJSON, _ = json.Marshal(compSummary)
	} else {
		compJSON = []byte("{}")
	}

	var completedAt *time.Time
	if state == "completed" || state == "failed" || state == "cancelled" || state == "interrupted" {
		now := time.Now()
		completedAt = &now
	}

	var startedAt *time.Time
	if state == "running" {
		now := time.Now()
		startedAt = &now
	}

	tag, err := s.Pool.Exec(ctx, `UPDATE test_runs SET 
		lifecycle_state=$2, passed_steps=$3, failed_steps=$4, skipped_steps=$5, 
		failure_reason=$6, actual_revision=$7, actual_candidate_revision=$8, 
		comparison_summary=$9,
		started_at=COALESCE(started_at, $10),
		completed_at=COALESCE(completed_at, $11)
		WHERE id=$1 AND (lifecycle_state NOT IN ('cancelled','interrupted') OR lifecycle_state=$2)`,
		id, state, passed, failed, skipped, failureReason, actualRev, candRev, compJSON, startedAt, completedAt)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

func (s *Store) CancelTestRun(ctx context.Context, id string) error {
	now := time.Now()
	_, err := s.Pool.Exec(ctx, `UPDATE test_runs SET lifecycle_state='cancelled', cancelled_at=$2, completed_at=$2 WHERE id=$1 AND lifecycle_state IN ('queued', 'running')`, id, now)
	return err
}

const stepCols = `id, run_id, step_index, cohort, request_name, method, url, target_revision, observed_revision,
	duration_ms, status_code, assertion_results, decision_policy, decision_reason, redacted_request, redacted_response, created_at`

func (s *Store) RecordTestRunStep(ctx context.Context, step TestRunStep) (TestRunStep, error) {
	assertJSON, _ := json.Marshal(step.AssertionResults)
	reqJSON, _ := json.Marshal(step.RedactedRequest)
	resJSON, _ := json.Marshal(step.RedactedResponse)

	cohort := strings.TrimSpace(step.Cohort)
	if cohort == "" {
		cohort = "baseline"
	}

	var created TestRunStep
	var aJSON, rJSON, sJSON []byte
	err := s.Pool.QueryRow(ctx, `INSERT INTO test_run_steps (run_id, step_index, cohort, request_name, method, url, target_revision, observed_revision, duration_ms, status_code, assertion_results, decision_policy, decision_reason, redacted_request, redacted_response)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING `+stepCols,
		step.RunID, step.StepIndex, cohort, step.RequestName, step.Method, step.URL, step.TargetRevision, step.ObservedRevision, step.DurationMS, step.StatusCode, assertJSON, step.DecisionPolicy, step.DecisionReason, reqJSON, resJSON).
		Scan(&created.ID, &created.RunID, &created.StepIndex, &created.Cohort, &created.RequestName, &created.Method, &created.URL, &created.TargetRevision, &created.ObservedRevision, &created.DurationMS, &created.StatusCode, &aJSON, &created.DecisionPolicy, &created.DecisionReason, &rJSON, &sJSON, &created.CreatedAt)

	if err != nil {
		return created, mapErr(err)
	}
	_ = json.Unmarshal(aJSON, &created.AssertionResults)
	_ = json.Unmarshal(rJSON, &created.RedactedRequest)
	_ = json.Unmarshal(sJSON, &created.RedactedResponse)
	return created, nil
}

func (s *Store) ListTestRunSteps(ctx context.Context, runID string) ([]TestRunStep, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+stepCols+` FROM test_run_steps WHERE run_id=$1 ORDER BY step_index ASC, cohort ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TestRunStep{}
	for rows.Next() {
		var item TestRunStep
		var aJSON, rJSON, sJSON []byte
		err := rows.Scan(&item.ID, &item.RunID, &item.StepIndex, &item.Cohort, &item.RequestName, &item.Method, &item.URL, &item.TargetRevision, &item.ObservedRevision, &item.DurationMS, &item.StatusCode, &aJSON, &item.DecisionPolicy, &item.DecisionReason, &rJSON, &sJSON, &item.CreatedAt)
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(aJSON, &item.AssertionResults)
		_ = json.Unmarshal(rJSON, &item.RedactedRequest)
		_ = json.Unmarshal(sJSON, &item.RedactedResponse)
		out = append(out, item)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Store Methods: Test Jobs Queue (FOR UPDATE SKIP LOCKED)
// ---------------------------------------------------------------------------

func (s *Store) EnqueueTestJob(ctx context.Context, runID, tenantID string) (TestJob, error) {
	var job TestJob
	err := s.Pool.QueryRow(ctx, `INSERT INTO test_jobs (run_id, tenant_id, status, next_available_at)
		VALUES ($1, $2, 'pending', now())
		RETURNING id, run_id, tenant_id, status, claim_lease_until, heartbeat_at, attempts, worker_id, next_available_at, created_at`,
		runID, TenantOrDefault(tenantID)).
		Scan(&job.ID, &job.RunID, &job.TenantID, &job.Status, &job.ClaimLeaseUntil, &job.HeartbeatAt, &job.Attempts, &job.WorkerID, &job.NextAvailableAt, &job.CreatedAt)
	if err == nil {
		_, _ = s.Pool.Exec(ctx, `NOTIFY relayops_test_jobs, '`+runID+`'`)
	}
	return job, mapErr(err)
}

func (s *Store) ClaimTestJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*TestJob, error) {
	now := time.Now()
	leaseUntil := now.Add(leaseDuration)

	var job TestJob
	claimed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		// Only check/expire leased jobs if any exist
		tag, err := tx.Exec(ctx, `UPDATE test_jobs SET status = 'interrupted' WHERE status = 'leased' AND claim_lease_until < $1`, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `UPDATE test_runs SET lifecycle_state = 'interrupted', failure_reason = 'worker lease expired (interrupted)', completed_at = $1 WHERE lifecycle_state IN ('queued','running') AND id IN (SELECT run_id FROM test_jobs WHERE status = 'interrupted')`, now); err != nil {
				return err
			}
		}

		row := tx.QueryRow(ctx, `SELECT id, run_id, tenant_id, status, attempts
			FROM test_jobs
			WHERE status = 'pending' AND next_available_at <= $1
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1`, now)

		var id, runID, tenantID, status string
		var attempts int
		if err := row.Scan(&id, &runID, &tenantID, &status, &attempts); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		claimed = true

		// Update to leased
		uRow := tx.QueryRow(ctx, `UPDATE test_jobs SET 
			status='leased', claim_lease_until=$2, heartbeat_at=$1, attempts=attempts+1, worker_id=$3
			WHERE id=$4
			RETURNING id, run_id, tenant_id, status, claim_lease_until, heartbeat_at, attempts, worker_id, next_available_at, created_at`,
			now, leaseUntil, workerID, id)

		return uRow.Scan(&job.ID, &job.RunID, &job.TenantID, &job.Status, &job.ClaimLeaseUntil, &job.HeartbeatAt, &job.Attempts, &job.WorkerID, &job.NextAvailableAt, &job.CreatedAt)
	})

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // No work available
	}
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, nil
	}
	return &job, nil
}

func (s *Store) HeartbeatTestJob(ctx context.Context, jobID, workerID string, leaseDuration time.Duration) error {
	now := time.Now()
	leaseUntil := now.Add(leaseDuration)
	return s.execOne(ctx, `UPDATE test_jobs SET heartbeat_at=$1, claim_lease_until=$2 WHERE id=$3 AND worker_id=$4 AND status='leased'`,
		now, leaseUntil, jobID, workerID)
}

func (s *Store) CompleteTestJob(ctx context.Context, jobID, status string) error {
	return s.execOne(ctx, `UPDATE test_jobs SET status=$1 WHERE id=$2`, status, jobID)
}

// ---------------------------------------------------------------------------
// Store Methods: Test Gate Policies & Evidence
// ---------------------------------------------------------------------------

func (s *Store) GetTestGatePolicy(ctx context.Context, tenantID, apiID string) (*TestGatePolicy, error) {
	var p TestGatePolicy
	var reqJSON []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, tenant_id, api_id, target_environment, required_suite_ids, freshness_seconds, enforcement_enabled, created_at, updated_at
		FROM test_gate_policies WHERE tenant_id=$1 AND api_id=$2`,
		TenantOrDefault(tenantID), apiID).
		Scan(&p.ID, &p.TenantID, &p.APIID, &p.TargetEnvironment, &reqJSON, &p.FreshnessSeconds, &p.EnforcementEnabled, &p.CreatedAt, &p.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal(reqJSON, &p.RequiredSuiteIDs)
	return &p, nil
}

func (s *Store) UpsertTestGatePolicy(ctx context.Context, p TestGatePolicy) (TestGatePolicy, error) {
	reqJSON, _ := json.Marshal(p.RequiredSuiteIDs)
	if p.FreshnessSeconds <= 0 {
		p.FreshnessSeconds = 3600
	}
	if p.TargetEnvironment == "" {
		p.TargetEnvironment = "canary"
	}

	var saved TestGatePolicy
	var outJSON []byte
	err := s.Pool.QueryRow(ctx, `INSERT INTO test_gate_policies (tenant_id, api_id, target_environment, required_suite_ids, freshness_seconds, enforcement_enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, api_id, target_environment) DO UPDATE SET
			required_suite_ids=$4, freshness_seconds=$5, enforcement_enabled=$6, updated_at=now()
		RETURNING id, tenant_id, api_id, target_environment, required_suite_ids, freshness_seconds, enforcement_enabled, created_at, updated_at`,
		TenantOrDefault(p.TenantID), p.APIID, p.TargetEnvironment, reqJSON, p.FreshnessSeconds, p.EnforcementEnabled).
		Scan(&saved.ID, &saved.TenantID, &saved.APIID, &saved.TargetEnvironment, &outJSON, &saved.FreshnessSeconds, &saved.EnforcementEnabled, &saved.CreatedAt, &saved.UpdatedAt)

	if err != nil {
		return saved, mapErr(err)
	}
	_ = json.Unmarshal(outJSON, &saved.RequiredSuiteIDs)
	return saved, nil
}

func (s *Store) RecordTestGateEvidence(ctx context.Context, ev TestGateEvidence) error {
	reasonsJSON, _ := json.Marshal(ev.Reasons)
	_, err := s.Pool.Exec(ctx, `INSERT INTO test_gate_evidence (tenant_id, api_id, revision, run_id, target_fingerprint, eligible, reasons, verified_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (tenant_id, api_id, revision, run_id) DO UPDATE SET
			target_fingerprint=$5, eligible=$6, reasons=$7, verified_at=now()`,
		TenantOrDefault(ev.TenantID), ev.APIID, ev.Revision, ev.RunID, ev.TargetFingerprint, ev.Eligible, reasonsJSON)
	return err
}

func (s *Store) GetLatestEligibleEvidence(ctx context.Context, tenantID, apiID string, revision int64) (*TestGateEvidence, error) {
	var ev TestGateEvidence
	var reasonsJSON []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, tenant_id, api_id, revision, run_id, target_fingerprint, eligible, reasons, verified_at
		FROM test_gate_evidence
		WHERE tenant_id=$1 AND api_id=$2 AND revision=$3 AND eligible=true
		ORDER BY verified_at DESC LIMIT 1`,
		TenantOrDefault(tenantID), apiID, revision).
		Scan(&ev.ID, &ev.TenantID, &ev.APIID, &ev.Revision, &ev.RunID, &ev.TargetFingerprint, &ev.Eligible, &reasonsJSON, &ev.VerifiedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal(reasonsJSON, &ev.Reasons)
	return &ev, nil
}
