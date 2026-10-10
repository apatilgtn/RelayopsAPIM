package apiops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

type PlanRequest struct {
	EnvironmentID        string `json:"environment_id"`
	CandidateRevision    int64  `json:"candidate_revision,omitempty"`
	CommitSHA            string `json:"commit_sha"`
	RepoURL              string `json:"repo_url"`
	Branch               string `json:"branch"`
	Actor                string `json:"actor"`
	PlanHash             string `json:"plan_hash"`
	ExpectedBaseRevision int64  `json:"expected_base_revision"`
}

type PlanResult struct {
	Deployment *store.APIOpsDeployment `json:"deployment"`
	PlanHash   string                  `json:"plan_hash"`
}

type Coordinator struct {
	store *store.Store
}

func NewCoordinator(s *store.Store) *Coordinator {
	return &Coordinator{store: s}
}

// PlanDeployment registers a planned deployment with its reviewed plan hash and base revision.
func (c *Coordinator) PlanDeployment(ctx context.Context, tenantID string, req PlanRequest) (*PlanResult, error) {
	if c.store == nil {
		return nil, errors.New("store not available")
	}

	dep := store.APIOpsDeployment{
		TenantID:             tenantID,
		Status:               "planned",
		CommitSHA:            req.CommitSHA,
		RepoURL:              req.RepoURL,
		Branch:               req.Branch,
		Actor:                req.Actor,
		PlanHash:             req.PlanHash,
		ExpectedBaseRevision: req.ExpectedBaseRevision,
		CandidateRevision:    req.CandidateRevision,
	}
	if req.EnvironmentID != "" {
		dep.EnvironmentID = &req.EnvironmentID
	}

	saved, err := c.store.CreateAPIOpsDeployment(ctx, dep)
	if err != nil {
		return nil, err
	}

	return &PlanResult{
		Deployment: &saved,
		PlanHash:   req.PlanHash,
	}, nil
}

// VerifyDeployment collects verification run evidence and updates deployment status.
// Missing, incomplete, or failing runs strictly reject verification.
func (c *Coordinator) VerifyDeployment(ctx context.Context, deploymentID string) (string, error) {
	dep, err := c.store.GetAPIOpsDeployment(ctx, deploymentID)
	if err != nil {
		return "", err
	}
	ev, err := c.collectEvidence(ctx, dep)
	if err != nil {
		return "", err
	}
	if err := c.store.UpdateAPIOpsDeploymentStatus(ctx, deploymentID, "verifying", dep.CandidateRevision, 0); err != nil {
		return "", err
	}
	return ev.Digest, nil
}

// verificationEvidence is the server-derived record of the Test Studio runs
// that verified a candidate revision.
type verificationEvidence struct {
	Digest  string
	Payload map[string]any
}

// collectEvidence gathers the Test Studio runs executed against the
// deployment's candidate revision. It fails when there are none, or when any
// run is incomplete, has failures, or passed no steps.
func (c *Coordinator) collectEvidence(ctx context.Context, dep store.APIOpsDeployment) (*verificationEvidence, error) {
	deploymentID := dep.ID
	if dep.CandidateRevision <= 0 {
		return nil, fmt.Errorf("deployment %s has no active candidate revision to verify (deploy candidate canary first)", deploymentID)
	}

	// 1. Collect actual Test Studio runs executed against this candidate revision
	rows, err := c.store.Pool.Query(ctx, `
		SELECT r.id, r.suite_id, r.lifecycle_state, r.passed_steps, r.failed_steps, r.skipped_steps, r.completed_at
		FROM test_runs r
		WHERE r.tenant_id = $1 AND r.actual_revision = $2
		ORDER BY r.created_at DESC`, dep.TenantID, dep.CandidateRevision)
	if err != nil {
		return nil, fmt.Errorf("query test runs: %w", err)
	}
	defer rows.Close()

	type runSummary struct {
		ID          string    `json:"id"`
		SuiteID     string    `json:"suite_id"`
		State       string    `json:"state"`
		PassedSteps int       `json:"passed_steps"`
		FailedSteps int       `json:"failed_steps"`
		CompletedAt time.Time `json:"completed_at"`
	}

	var runs []runSummary
	for rows.Next() {
		var r runSummary
		var comp *time.Time
		var skipped int
		if err := rows.Scan(&r.ID, &r.SuiteID, &r.State, &r.PassedSteps, &r.FailedSteps, &skipped, &comp); err != nil {
			return nil, err
		}
		if comp != nil {
			r.CompletedAt = *comp
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(runs) == 0 {
		return nil, fmt.Errorf("verification rejected: no Test Studio runs observed against candidate rev_%d", dep.CandidateRevision)
	}

	for _, r := range runs {
		if r.FailedSteps > 0 || r.State != "completed" {
			return nil, fmt.Errorf("verification rejected: test run %s for suite %s has status %s with %d failed steps", r.ID, r.SuiteID, r.State, r.FailedSteps)
		}
		if r.PassedSteps == 0 {
			return nil, fmt.Errorf("verification rejected: test run %s for suite %s passed no steps", r.ID, r.SuiteID)
		}
	}

	// 2. Compute canonical server-derived evidence digest over actual test runs
	evidencePayload := map[string]any{
		"deployment_id":   dep.ID,
		"tenant_id":       dep.TenantID,
		"commit_sha":      dep.CommitSHA,
		"plan_hash":       dep.PlanHash,
		"candidate_rev":   dep.CandidateRevision,
		"verified_at":     time.Now().UTC().Format(time.RFC3339),
		"test_runs_count": len(runs),
		"test_runs":       runs,
		"verification_ok": true,
	}

	raw, err := json.Marshal(evidencePayload)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &verificationEvidence{Digest: "sha256:" + hex.EncodeToString(sum[:]), Payload: evidencePayload}, nil
}

// PromoteDeployment promotes a verified release candidate and seals the immutable Release Passport atomically.
func (c *Coordinator) PromoteDeployment(ctx context.Context, deploymentID string, actor string, isSuperadmin bool, overrideReason string) (*store.APIOpsReleasePassport, error) {
	dep, err := c.store.GetAPIOpsDeployment(ctx, deploymentID)
	if err != nil {
		return nil, err
	}

	if dep.Status == "promoted" {
		// Idempotent retry: return existing passport if already promoted
		pass, perr := c.store.GetAPIOpsReleasePassport(ctx, deploymentID)
		if perr == nil {
			return &pass, nil
		}
	}

	if dep.CandidateRevision <= 0 {
		return nil, fmt.Errorf("deployment %s does not have an active candidate revision to promote", deploymentID)
	}

	allowOverride := isSuperadmin && strings.TrimSpace(overrideReason) != ""
	if overrideReason != "" && !isSuperadmin {
		return nil, fmt.Errorf("forbidden: superadmin role is required to override test gate policies")
	}

	// The passport must be backed by verification evidence collected now, on
	// the server, for the revision being promoted. Only an authorized,
	// recorded override promotes without it.
	ev, evErr := c.collectEvidence(ctx, dep)
	if evErr != nil && !allowOverride {
		return nil, evErr
	}

	// Prepare manifest
	manifest := map[string]any{
		"deployment_id":      dep.ID,
		"commit_sha":         dep.CommitSHA,
		"repo_url":           dep.RepoURL,
		"branch":             dep.Branch,
		"promoted_by":        actor,
		"candidate_revision": dep.CandidateRevision,
		"promoted_revision":  dep.CandidateRevision,
		"plan_hash":          dep.PlanHash,
		"gate_override":      allowOverride,
		"override_reason":    overrideReason,
		"promoted_at":        time.Now().UTC().Format(time.RFC3339),
	}
	if ev != nil {
		manifest["verification_evidence_digest"] = ev.Digest
		manifest["verification"] = ev.Payload
	} else {
		manifest["verification_error"] = evErr.Error() // promoted under override without evidence
	}

	raw, _ := json.Marshal(manifest)
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	passIn := store.APIOpsReleasePassport{
		DeploymentID:   dep.ID,
		TenantID:       dep.TenantID,
		Manifest:       manifest,
		EvidenceDigest: digest,
	}

	passport, reasons, err := c.store.PromoteCanaryAndSealPassport(ctx, dep.ID, dep.CandidateRevision, allowOverride, passIn)
	if err != nil {
		return nil, fmt.Errorf("promotion gate blocked: %w", err)
	}
	if len(reasons) > 0 {
		manifest["gate_warning_reasons"] = reasons
	}

	return &passport, nil
}

// AbortDeployment aborts an in-flight canary and marks the deployment aborted.
func (c *Coordinator) AbortDeployment(ctx context.Context, deploymentID string, actor string) error {
	dep, err := c.store.GetAPIOpsDeployment(ctx, deploymentID)
	if err != nil {
		return err
	}

	if dep.CandidateRevision > 0 {
		if _, err := c.store.AbortCanary(ctx, dep.CandidateRevision, actor); err != nil {
			return fmt.Errorf("abort canary failed: %w", err)
		}
	}

	return c.store.UpdateAPIOpsDeploymentStatus(ctx, deploymentID, "aborted", 0, 0)
}
