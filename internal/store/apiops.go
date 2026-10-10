package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// APIOps Models
// ---------------------------------------------------------------------------

type APIOpsEnvironment struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	IsProduction     bool      `json:"is_production"`
	TargetGatewayURL string    `json:"target_gateway_url"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type APIOpsDeployment struct {
	ID                   string    `json:"id"`
	TenantID             string    `json:"tenant_id"`
	EnvironmentID        *string   `json:"environment_id,omitempty"`
	EnvironmentName      string    `json:"environment_name,omitempty"`
	Status               string    `json:"status"` // draft, planned, verifying, canary, promoted, aborted, recovering
	CommitSHA            string    `json:"commit_sha"`
	RepoURL              string    `json:"repo_url"`
	Branch               string    `json:"branch"`
	Actor                string    `json:"actor"`
	PlanHash             string    `json:"plan_hash"`
	ExpectedBaseRevision int64     `json:"expected_base_revision"`
	CandidateRevision    int64     `json:"candidate_revision"`
	PromotedRevision     int64     `json:"promoted_revision"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type APIOpsReleasePassport struct {
	ID             string         `json:"id"`
	DeploymentID   string         `json:"deployment_id"`
	TenantID       string         `json:"tenant_id"`
	Manifest       map[string]any `json:"manifest"`
	EvidenceDigest string         `json:"evidence_digest"`
	CreatedAt      time.Time      `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Store Operations
// ---------------------------------------------------------------------------

func (s *Store) CreateAPIOpsEnvironment(ctx context.Context, env APIOpsEnvironment) (APIOpsEnvironment, error) {
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO apiops_environments (tenant_id, name, description, is_production, target_gateway_url)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, tenant_id, name, description, is_production, target_gateway_url, created_at, updated_at`,
		TenantOrDefault(env.TenantID), env.Name, env.Description, env.IsProduction, env.TargetGatewayURL,
	).Scan(&env.ID, &env.TenantID, &env.Name, &env.Description, &env.IsProduction, &env.TargetGatewayURL, &env.CreatedAt, &env.UpdatedAt)
	return env, mapErr(err)
}

func (s *Store) ListAPIOpsEnvironments(ctx context.Context, tenantID string) ([]APIOpsEnvironment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tenant_id, name, description, is_production, target_gateway_url, created_at, updated_at
		FROM apiops_environments
		WHERE tenant_id = $1
		ORDER BY is_production ASC, name ASC`, TenantOrDefault(tenantID))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []APIOpsEnvironment
	for rows.Next() {
		var env APIOpsEnvironment
		if err := rows.Scan(&env.ID, &env.TenantID, &env.Name, &env.Description, &env.IsProduction, &env.TargetGatewayURL, &env.CreatedAt, &env.UpdatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

func (s *Store) GetAPIOpsEnvironment(ctx context.Context, id string) (APIOpsEnvironment, error) {
	var env APIOpsEnvironment
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, description, is_production, target_gateway_url, created_at, updated_at
		FROM apiops_environments WHERE id = $1`, id,
	).Scan(&env.ID, &env.TenantID, &env.Name, &env.Description, &env.IsProduction, &env.TargetGatewayURL, &env.CreatedAt, &env.UpdatedAt)
	return env, mapErr(err)
}

func (s *Store) CreateAPIOpsDeployment(ctx context.Context, dep APIOpsDeployment) (APIOpsDeployment, error) {
	if dep.Status == "" {
		dep.Status = "draft"
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO apiops_deployments (tenant_id, environment_id, status, commit_sha, repo_url, branch, actor, plan_hash, expected_base_revision, candidate_revision, promoted_revision)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, environment_id, status, commit_sha, repo_url, branch, actor, plan_hash, expected_base_revision, candidate_revision, promoted_revision, created_at, updated_at`,
		TenantOrDefault(dep.TenantID), dep.EnvironmentID, dep.Status, dep.CommitSHA, dep.RepoURL, dep.Branch, dep.Actor, dep.PlanHash, dep.ExpectedBaseRevision, dep.CandidateRevision, dep.PromotedRevision,
	).Scan(&dep.ID, &dep.TenantID, &dep.EnvironmentID, &dep.Status, &dep.CommitSHA, &dep.RepoURL, &dep.Branch, &dep.Actor, &dep.PlanHash, &dep.ExpectedBaseRevision, &dep.CandidateRevision, &dep.PromotedRevision, &dep.CreatedAt, &dep.UpdatedAt)
	return dep, mapErr(err)
}

func (s *Store) GetAPIOpsDeployment(ctx context.Context, id string) (APIOpsDeployment, error) {
	var dep APIOpsDeployment
	var envName *string
	err := s.Pool.QueryRow(ctx, `
		SELECT d.id, d.tenant_id, d.environment_id, d.status, d.commit_sha, d.repo_url, d.branch, d.actor,
		       d.plan_hash, d.expected_base_revision, d.candidate_revision, d.promoted_revision, d.created_at, d.updated_at,
		       e.name
		FROM apiops_deployments d
		LEFT JOIN apiops_environments e ON e.id = d.environment_id
		WHERE d.id = $1`, id,
	).Scan(&dep.ID, &dep.TenantID, &dep.EnvironmentID, &dep.Status, &dep.CommitSHA, &dep.RepoURL, &dep.Branch, &dep.Actor,
		&dep.PlanHash, &dep.ExpectedBaseRevision, &dep.CandidateRevision, &dep.PromotedRevision, &dep.CreatedAt, &dep.UpdatedAt, &envName)
	if envName != nil {
		dep.EnvironmentName = *envName
	}
	return dep, mapErr(err)
}

func (s *Store) ListAPIOpsDeployments(ctx context.Context, tenantID string, limit int) ([]APIOpsDeployment, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT d.id, d.tenant_id, d.environment_id, d.status, d.commit_sha, d.repo_url, d.branch, d.actor,
		       d.plan_hash, d.expected_base_revision, d.candidate_revision, d.promoted_revision, d.created_at, d.updated_at,
		       e.name
		FROM apiops_deployments d
		LEFT JOIN apiops_environments e ON e.id = d.environment_id
		WHERE d.tenant_id = $1
		ORDER BY d.created_at DESC LIMIT $2`, TenantOrDefault(tenantID), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []APIOpsDeployment
	for rows.Next() {
		var dep APIOpsDeployment
		var envName *string
		if err := rows.Scan(&dep.ID, &dep.TenantID, &dep.EnvironmentID, &dep.Status, &dep.CommitSHA, &dep.RepoURL, &dep.Branch, &dep.Actor,
			&dep.PlanHash, &dep.ExpectedBaseRevision, &dep.CandidateRevision, &dep.PromotedRevision, &dep.CreatedAt, &dep.UpdatedAt, &envName); err != nil {
			return nil, mapErr(err)
		}
		if envName != nil {
			dep.EnvironmentName = *envName
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAPIOpsDeploymentStatus(ctx context.Context, id, status string, candidateRev, promotedRev int64) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE apiops_deployments
		SET status = $2,
		    candidate_revision = CASE WHEN $3 > 0 THEN $3 ELSE candidate_revision END,
		    promoted_revision = CASE WHEN $4 > 0 THEN $4 ELSE promoted_revision END,
		    updated_at = now()
		WHERE id = $1`, id, status, candidateRev, promotedRev)
	return mapErr(err)
}

func (s *Store) CreateAPIOpsReleasePassport(ctx context.Context, pass APIOpsReleasePassport) (APIOpsReleasePassport, error) {
	manifestJSON, err := json.Marshal(pass.Manifest)
	if err != nil {
		return pass, err
	}
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO apiops_release_passports (deployment_id, tenant_id, manifest, evidence_digest)
		VALUES ($1, $2, $3, $4)
		RETURNING id, deployment_id, tenant_id, manifest, evidence_digest, created_at`,
		pass.DeploymentID, TenantOrDefault(pass.TenantID), manifestJSON, pass.EvidenceDigest,
	).Scan(&pass.ID, &pass.DeploymentID, &pass.TenantID, &manifestJSON, &pass.EvidenceDigest, &pass.CreatedAt)
	if err == nil {
		_ = json.Unmarshal(manifestJSON, &pass.Manifest)
	}
	return pass, mapErr(err)
}

func (s *Store) GetAPIOpsReleasePassport(ctx context.Context, deploymentID string) (APIOpsReleasePassport, error) {
	var pass APIOpsReleasePassport
	var raw []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT id, deployment_id, tenant_id, manifest, evidence_digest, created_at
		FROM apiops_release_passports
		WHERE deployment_id = $1
		ORDER BY created_at DESC LIMIT 1`, deploymentID,
	).Scan(&pass.ID, &pass.DeploymentID, &pass.TenantID, &raw, &pass.EvidenceDigest, &pass.CreatedAt)
	if err != nil {
		return pass, mapErr(err)
	}
	_ = json.Unmarshal(raw, &pass.Manifest)
	return pass, nil
}

// PromoteCanaryAndSealPassport atomically verifies test gates, promotes the canary revision to active,
// records the sealed Release Passport, and marks the deployment as promoted.
func (s *Store) PromoteCanaryAndSealPassport(ctx context.Context, deploymentID string, rev int64, override bool, pass APIOpsReleasePassport) (APIOpsReleasePassport, []string, error) {
	var reasons []string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE test_gate_policies, test_suites, test_suite_versions, test_environments, test_credentials, test_gate_evidence, test_runs, apiops_deployments, apiops_release_passports IN SHARE MODE`); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1 FOR UPDATE`, rev).Scan(&status); err != nil {
			return mapErr(err)
		}
		if status != "canary" {
			return fmt.Errorf("%w: only an in-flight canary can be promoted", ErrConflict)
		}

		var evalErr error
		reasons, evalErr = evaluateEnforcedGates(ctx, tx, rev)
		if evalErr != nil {
			return evalErr
		}
		if len(reasons) > 0 && !override {
			return &GateRejectedError{Reasons: reasons}
		}

		// 1. Promote canary revision
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET status='active', target_group='all',
			traffic_percent=0, canary_header='', canary_header_value='' WHERE revision=$1`, rev); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'PROMOTE', 'revision', $1::bigint, 'at', now())::text)`, rev); err != nil {
			return err
		}

		// 2. Insert Release Passport
		manifestJSON, err := json.Marshal(pass.Manifest)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO apiops_release_passports (deployment_id, tenant_id, manifest, evidence_digest)
			VALUES ($1, $2, $3, $4)
			RETURNING id, created_at`,
			pass.DeploymentID, TenantOrDefault(pass.TenantID), manifestJSON, pass.EvidenceDigest,
		).Scan(&pass.ID, &pass.CreatedAt)
		if err != nil {
			return err
		}

		// 3. Update Deployment
		_, err = tx.Exec(ctx, `
			UPDATE apiops_deployments
			SET status='promoted', promoted_revision=$2, updated_at=now()
			WHERE id=$1`, deploymentID, rev)
		return err
	})

	if err != nil {
		return pass, nil, mapErr(err)
	}
	return pass, reasons, nil
}
