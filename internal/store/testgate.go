package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type GateRejectedError struct{ Reasons []string }

func (e *GateRejectedError) Error() string { return fmt.Sprintf("promotion blocked: %v", e.Reasons) }

type credentialReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func credentialVersions(ctx context.Context, q credentialReader, tenant string, bindings map[string]string) (map[string]any, error) {
	out := map[string]any{}
	for key, ref := range bindings {
		var id string
		var version int
		err := q.QueryRow(ctx, `SELECT id, credential_version FROM test_credentials WHERE tenant_id=$1 AND (id::text=$2 OR name=$2)`, tenant, ref).Scan(&id, &version)
		if err != nil {
			return nil, fmt.Errorf("credential binding %q is unavailable", key)
		}
		out[key] = map[string]any{"id": id, "version": version}
	}
	return out, nil
}

func (s *Store) TestCredentialVersions(ctx context.Context, env TestEnvironment) (map[string]any, error) {
	return credentialVersions(ctx, s.Pool, env.TenantID, env.CredentialBindings)
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Gate evaluation and the revision change share one short transaction. The
// SHARE locks keep policy, suite, environment, credentials and evidence stable
// until the promotion commits; no network requests occur inside it.
func (s *Store) PromoteCanaryWithTestGates(ctx context.Context, rev int64, override bool) ([]string, error) {
	var reasons []string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE config_revisions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE test_gate_policies, test_suites, test_suite_versions, test_environments, test_credentials, test_gate_evidence, test_runs IN SHARE MODE`); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1 FOR UPDATE`, rev).Scan(&status); err != nil {
			return mapErr(err)
		}
		if status != "canary" {
			return fmt.Errorf("%w: only an in-flight canary can be promoted", ErrConflict)
		}
		report, evalErr := evaluateGateReport(ctx, tx, rev)
		reasons = report.Reasons
		if evalErr != nil {
			return evalErr
		}
		if len(reasons) > 0 {
			return &GateRejectedError{Reasons: reasons}
		}
		if err := sealReleaseDecision(ctx, tx, rev, report); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET status='active', target_group='all', traffic_percent=0, canary_header='', canary_header_value='' WHERE revision=$1`, rev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table','config_revisions','op','PROMOTE','revision',$1::bigint,'at',now())::text)`, rev)
		return err
	})
	return reasons, err
}

// evaluateEnforcedGates is the single promotion-gate evaluator. Every promote
// path (classic canary and APIOps passport) must call this inside its transaction.
// GateReport is the authoritative decision used by review and promotion.
type GateReport struct {
	Revision            int64       `json:"revision"`
	Available           bool        `json:"available"`
	Enforced            bool        `json:"enforced"`
	Eligible            bool        `json:"eligible"`
	Suites              []GateSuite `json:"suites"`
	Reasons             []string    `json:"reasons"`
	BlockingReasonCodes []string    `json:"blocking_reason_codes"`
	RequiredSuites      []string    `json:"required_suites"`
}
type GateSuite struct {
	TenantID    string     `json:"tenant_id"`
	APIID       string     `json:"api_id"`
	SuiteID     string     `json:"suite_id"`
	EvidenceID  string     `json:"evidence_id,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Fingerprint string     `json:"input_fingerprint,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Eligible    bool       `json:"eligible"`
	ReasonCode  string     `json:"reason_code,omitempty"`
}

func (s *Store) EvaluateTestGates(ctx context.Context, rev int64) (GateReport, error) {
	var report GateReport
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE test_gate_policies, test_suites, test_suite_versions, test_environments, test_credentials, test_gate_evidence, test_runs IN SHARE MODE`); err != nil {
			return err
		}
		var err error
		report, err = evaluateGateReport(ctx, tx, rev)
		return err
	})
	if err != nil {
		report.Available = false
		report.Eligible = false
	}
	return report, err
}
func evaluateEnforcedGates(ctx context.Context, tx pgx.Tx, rev int64) ([]string, error) {
	report, err := evaluateGateReport(ctx, tx, rev)
	return report.Reasons, err
}

func evaluateGateReport(ctx context.Context, tx pgx.Tx, rev int64) (GateReport, error) {
	report := GateReport{Revision: rev, Eligible: true, Available: true, Suites: []GateSuite{}, Reasons: []string{}}
	rows, err := tx.Query(ctx, `SELECT tenant_id, api_id, required_suite_ids, freshness_seconds FROM test_gate_policies WHERE enforcement_enabled=true`)
	if err != nil {
		return report, err
	}
	type policy struct {
		tenant, api string
		suites      []string
		freshness   int
	}
	var policies []policy
	for rows.Next() {
		var p policy
		var raw []byte
		if err := rows.Scan(&p.tenant, &p.api, &raw, &p.freshness); err != nil {
			rows.Close()
			return report, err
		}
		if err := json.Unmarshal(raw, &p.suites); err != nil {
			rows.Close()
			return report, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	reasons := []string{}
	for _, p := range policies {
		report.Enforced = true
		if len(p.suites) == 0 {
			reasons = append(reasons, "gate must specify required suites")
			continue
		}
		for _, suite := range p.suites {
			report.RequiredSuites = append(report.RequiredSuites, suite)
			detail := GateSuite{TenantID: p.tenant, APIID: p.api, SuiteID: suite, ReasonCode: "missing_evidence"}
			var runID, fingerprint string
			var eligible bool
			var verified time.Time
			err := tx.QueryRow(ctx, `SELECT e.id, e.run_id, e.target_fingerprint, e.eligible, e.verified_at FROM test_gate_evidence e JOIN test_runs r ON r.id=e.run_id WHERE e.tenant_id=$1 AND e.api_id=$2 AND e.revision=$3 AND r.suite_id=$4 AND r.id=(SELECT latest.id FROM test_runs latest WHERE latest.tenant_id=$1 AND latest.suite_id=$4 AND (latest.actual_revision=$3 OR latest.actual_candidate_revision=$3 OR latest.immutable_inputs->>'candidate_revision'=$3::text OR latest.immutable_inputs->>'target_revision'=$3::text) ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1) ORDER BY e.verified_at DESC, e.id DESC LIMIT 1`, p.tenant, p.api, rev, suite).Scan(&detail.EvidenceID, &runID, &fingerprint, &eligible, &verified)
			if err == pgx.ErrNoRows {
				report.BlockingReasonCodes = append(report.BlockingReasonCodes, "missing_evidence")
				report.Suites = append(report.Suites, detail)
				reasons = append(reasons, "suite "+suite+": missing evidence")
				continue
			}
			if err != nil {
				return report, err
			}
			run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM test_runs WHERE id=$1`, runID))
			if err != nil {
				return report, err
			}
			detail.RunID = runID
			inputs, _ := json.Marshal(map[string]any{"suite_hash": run.SuiteContentHash, "inputs": run.ImmutableInputs, "revision": rev, "comparison": run.ComparisonSummary})
			sum := sha256.Sum256(inputs)
			detail.Fingerprint = "sha256:" + hex.EncodeToString(sum[:])
			expires := verified.Add(time.Duration(p.freshness) * time.Second)
			detail.ExpiresAt = &expires
			if run.CompletedAt != nil && run.CompletedAt.Add(time.Duration(p.freshness)*time.Second).Before(expires) {
				expires = run.CompletedAt.Add(time.Duration(p.freshness) * time.Second)
			}
			var hash string
			var definitionJSON []byte
			if err := tx.QueryRow(ctx, `SELECT v.content_hash, v.definition FROM test_suites s JOIN test_suite_versions v ON v.suite_id=s.id AND v.version=s.current_version WHERE s.id=$1 AND s.tenant_id=$2 AND s.api_id=$3`, suite, p.tenant, p.api).Scan(&hash, &definitionJSON); err != nil {
				return report, err
			}
			valid := eligible && run.TenantID == p.tenant && run.LifecycleState == "completed" && run.TotalSteps > 0 && run.PassedSteps == run.TotalSteps && run.FailedSteps == 0 && run.SkippedSteps == 0 && run.CompletedAt != nil && p.freshness > 0 && time.Since(*run.CompletedAt) <= time.Duration(p.freshness)*time.Second && time.Since(verified) <= time.Duration(p.freshness)*time.Second && hash == run.SuiteContentHash && fingerprint == run.SuiteContentHash
			var definition SuiteDefinition
			if err := json.Unmarshal(definitionJSON, &definition); err != nil {
				return report, err
			}
			if raw := run.ImmutableInputs["ignore_paths"]; raw != nil {
				var paths []string
				encoded, _ := json.Marshal(raw)
				if err := json.Unmarshal(encoded, &paths); err != nil {
					return report, err
				}
				if len(paths) > 0 && !sameJSON(paths, definition.IgnorePaths) {
					valid = false
				}
			}
			if run.Mode == "comparison" || run.Mode == "canary_gate" {
				valid = valid && run.ActualCandidateRevision == rev && sameJSON(run.ComparisonSummary["engine_version"], ComparisonEngineVersion)
			} else {
				requiresComparison := len(definition.Comparison.Headers) > 0 || definition.Comparison.MaxLatencyIncreasePercent > 0
				valid = valid && run.ActualRevision == rev && !requiresComparison
			}
			snap, ok := run.ImmutableInputs["environment_snapshot"].(map[string]any)
			valid = valid && ok && run.EnvironmentID != nil
			if ok && run.EnvironmentID != nil {
				env, err := scanEnvironment(tx.QueryRow(ctx, `SELECT `+envCols+` FROM test_environments WHERE id=$1 AND tenant_id=$2`, *run.EnvironmentID, p.tenant))
				if err == pgx.ErrNoRows || err == ErrNotFound {
					valid = false
				} else if err != nil {
					return report, err
				} else {
					versions, err := credentialVersions(ctx, tx, p.tenant, env.CredentialBindings)
					if err != nil {
						valid = false
					}
					valid = valid && sameJSON(snap["revision"], env.Revision) && sameJSON(snap["gateway_target"], env.GatewayTarget) && sameJSON(snap["variables"], env.Variables) && sameJSON(snap["credential_bindings"], env.CredentialBindings) && sameJSON(snap["credential_versions"], versions)
				}
			}
			detail.Eligible = valid
			detail.ReasonCode = ""
			if !valid {
				detail.ReasonCode = "failed_or_changed_inputs"
				if p.freshness <= 0 || time.Now().After(expires) {
					detail.ReasonCode = "expired_evidence"
				}
				if run.SkippedSteps > 0 {
					detail.ReasonCode = "skipped_steps"
				}
				if run.LifecycleState != "completed" || run.TotalSteps <= 0 || run.PassedSteps != run.TotalSteps {
					detail.ReasonCode = "incomplete_or_failed_run"
				}
				reasons = append(reasons, "suite "+suite+": failed, expired or changed test inputs")
				report.BlockingReasonCodes = append(report.BlockingReasonCodes, detail.ReasonCode)
			}
			report.Suites = append(report.Suites, detail)
		}
	}
	if !report.Enforced {
		reasons = append(reasons, "required release gates are not configured")
		report.BlockingReasonCodes = append(report.BlockingReasonCodes, "gates_not_configured")
	}
	report.Reasons = reasons
	report.Eligible = len(reasons) == 0
	return report, nil
}

// HasEnforcedTestGates reports whether any of the specified APIs has active test gate enforcement.
func (s *Store) HasEnforcedTestGates(ctx context.Context, tenant string, apiIDs ...string) (bool, error) {
	if len(apiIDs) == 0 {
		return false, nil
	}
	var count int
	err := s.Pool.QueryRow(ctx, `
		SELECT count(*) FROM test_gate_policies
		WHERE tenant_id = $1 AND enforcement_enabled = true AND api_id = ANY($2)`,
		TenantOrDefault(tenant), apiIDs,
	).Scan(&count)
	if err != nil {
		return false, mapErr(err)
	}
	return count > 0, nil
}

func sealReleaseDecision(ctx context.Context, tx pgx.Tx, rev int64, report GateReport) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	_, err = tx.Exec(ctx, `INSERT INTO release_decisions(revision,evidence,evidence_digest) VALUES($1,$2,$3)`, rev, raw, "sha256:"+hex.EncodeToString(sum[:]))
	return err
}
