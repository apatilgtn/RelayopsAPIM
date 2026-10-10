package store

import (
	"context"
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
		var evalErr error
		reasons, evalErr = evaluateEnforcedGates(ctx, tx, rev)
		if evalErr != nil {
			return evalErr
		}
		if len(reasons) > 0 && !override {
			return &GateRejectedError{Reasons: reasons}
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
func evaluateEnforcedGates(ctx context.Context, tx pgx.Tx, rev int64) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, api_id, required_suite_ids, freshness_seconds FROM test_gate_policies WHERE enforcement_enabled=true`)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		if err := json.Unmarshal(raw, &p.suites); err != nil {
			rows.Close()
			return nil, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var reasons []string
	for _, p := range policies {
		if len(p.suites) == 0 {
			reasons = append(reasons, "gate must specify required suites")
			continue
		}
		for _, suite := range p.suites {
			var runID, fingerprint string
			var eligible bool
			var verified time.Time
			err := tx.QueryRow(ctx, `SELECT e.run_id, e.target_fingerprint, e.eligible, e.verified_at FROM test_gate_evidence e JOIN test_runs r ON r.id=e.run_id WHERE e.tenant_id=$1 AND e.api_id=$2 AND e.revision=$3 AND r.suite_id=$4 ORDER BY e.verified_at DESC LIMIT 1`, p.tenant, p.api, rev, suite).Scan(&runID, &fingerprint, &eligible, &verified)
			if err == pgx.ErrNoRows {
				reasons = append(reasons, "suite "+suite+": missing evidence")
				continue
			}
			if err != nil {
				return nil, err
			}
			run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM test_runs WHERE id=$1`, runID))
			if err != nil {
				return nil, err
			}
			var hash string
			var definitionJSON []byte
			if err := tx.QueryRow(ctx, `SELECT v.content_hash, v.definition FROM test_suites s JOIN test_suite_versions v ON v.suite_id=s.id AND v.version=s.current_version WHERE s.id=$1 AND s.tenant_id=$2 AND s.api_id=$3`, suite, p.tenant, p.api).Scan(&hash, &definitionJSON); err != nil {
				return nil, err
			}
			valid := eligible && run.TenantID == p.tenant && run.LifecycleState == "completed" && run.TotalSteps > 0 && run.PassedSteps == run.TotalSteps && run.FailedSteps == 0 && run.SkippedSteps == 0 && run.CompletedAt != nil && p.freshness > 0 && time.Since(*run.CompletedAt) <= time.Duration(p.freshness)*time.Second && time.Since(verified) <= time.Duration(p.freshness)*time.Second && hash == run.SuiteContentHash && fingerprint == run.SuiteContentHash
			var definition SuiteDefinition
			if err := json.Unmarshal(definitionJSON, &definition); err != nil {
				return nil, err
			}
			if raw := run.ImmutableInputs["ignore_paths"]; raw != nil {
				var paths []string
				encoded, _ := json.Marshal(raw)
				if err := json.Unmarshal(encoded, &paths); err != nil {
					return nil, err
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
					return nil, err
				} else {
					versions, err := credentialVersions(ctx, tx, p.tenant, env.CredentialBindings)
					if err != nil {
						valid = false
					}
					valid = valid && sameJSON(snap["revision"], env.Revision) && sameJSON(snap["gateway_target"], env.GatewayTarget) && sameJSON(snap["variables"], env.Variables) && sameJSON(snap["credential_bindings"], env.CredentialBindings) && sameJSON(snap["credential_versions"], versions)
				}
			}
			if !valid {
				reasons = append(reasons, "suite "+suite+": failed, expired or changed test inputs")
			}
		}
	}
	return reasons, nil
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
