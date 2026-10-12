package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"time"
)

type ReleaseDraft struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	Version           int             `json:"version"`
	Configuration     json.RawMessage `json:"configuration"`
	SuiteIDs          []string        `json:"suite_ids"`
	EnvironmentID     *string         `json:"environment_id,omitempty"`
	BaselineRevision  int64           `json:"baseline_revision"`
	PlanHash          string          `json:"plan_hash"`
	CandidateRevision int64           `json:"candidate_revision"`
	DeploymentID      *string         `json:"deployment_id,omitempty"`
	CreatedBy         string          `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

const draftCols = `id,tenant_id,version,configuration,suite_ids,environment_id,baseline_revision,plan_hash,candidate_revision,deployment_id,created_by,created_at,updated_at`

func scanDraft(row pgx.Row) (ReleaseDraft, error) {
	var d ReleaseDraft
	var suites []byte
	err := row.Scan(&d.ID, &d.TenantID, &d.Version, &d.Configuration, &suites, &d.EnvironmentID, &d.BaselineRevision, &d.PlanHash, &d.CandidateRevision, &d.DeploymentID, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err == nil {
		err = json.Unmarshal(suites, &d.SuiteIDs)
	}
	return d, mapErr(err)
}
func (s *Store) GetReleaseDraft(ctx context.Context, id string) (ReleaseDraft, error) {
	return scanDraft(s.Pool.QueryRow(ctx, `SELECT `+draftCols+` FROM release_drafts WHERE id=$1`, id))
}
func (s *Store) ListReleaseDrafts(ctx context.Context, tenants []string) ([]ReleaseDraft, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+draftCols+` FROM release_drafts WHERE ($1::uuid[] IS NULL OR tenant_id=ANY($1)) ORDER BY updated_at DESC LIMIT 100`, tenants)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReleaseDraft{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) SaveReleaseDraft(ctx context.Context, d ReleaseDraft) (ReleaseDraft, error) {
	suites, _ := json.Marshal(d.SuiteIDs)
	if d.SuiteIDs == nil {
		suites = []byte(`[]`)
	}
	if d.ID == "" {
		return scanDraft(s.Pool.QueryRow(ctx, `INSERT INTO release_drafts(tenant_id,configuration,suite_ids,environment_id,created_by) VALUES($1,$2,$3,$4,$5) RETURNING `+draftCols, d.TenantID, d.Configuration, suites, d.EnvironmentID, d.CreatedBy))
	}
	out, err := scanDraft(s.Pool.QueryRow(ctx, `UPDATE release_drafts SET configuration=$3,suite_ids=$4,environment_id=$5,version=version+1,plan_hash='',baseline_revision=0,updated_at=now() WHERE id=$1 AND tenant_id=$6 AND version=$2 AND candidate_revision=0 RETURNING `+draftCols, d.ID, d.Version, d.Configuration, suites, d.EnvironmentID, d.TenantID))
	if err == ErrNotFound {
		return out, fmt.Errorf("%w: draft changed or was already deployed; reload it", ErrConflict)
	}
	return out, err
}

type draftPublicationKey struct{}

func WithReleaseDraft(ctx context.Context, d ReleaseDraft) context.Context {
	return context.WithValue(ctx, draftPublicationKey{}, d)
}

// lockPublicationDraft runs within the publication transaction. Retried requests
// return the original candidate, even if the first HTTP response was lost.
func lockPublicationDraft(ctx context.Context, tx pgx.Tx) (int64, error) {
	wanted, ok := ctx.Value(draftPublicationKey{}).(ReleaseDraft)
	if !ok {
		return 0, nil
	}
	current, err := scanDraft(tx.QueryRow(ctx, `SELECT `+draftCols+` FROM release_drafts WHERE id=$1 FOR UPDATE`, wanted.ID))
	if err != nil {
		return 0, err
	}
	if current.TenantID != wanted.TenantID {
		return 0, ErrConflict
	}
	if current.CandidateRevision > 0 {
		return current.CandidateRevision, nil
	}
	if current.Version != wanted.Version || current.PlanHash == "" || current.PlanHash != wanted.PlanHash {
		return 0, fmt.Errorf("%w: reviewed draft changed", ErrConflict)
	}
	return 0, nil
}
func linkPublicationDraft(ctx context.Context, tx pgx.Tx, rev int64, actor string) error {
	d, ok := ctx.Value(draftPublicationKey{}).(ReleaseDraft)
	if !ok {
		return nil
	}
	var deployment string
	if err := tx.QueryRow(ctx, `INSERT INTO apiops_deployments(tenant_id,status,actor,plan_hash,expected_base_revision,candidate_revision) VALUES($1,'canary',$2,$3,$4,$5) RETURNING id`, d.TenantID, actor, d.PlanHash, d.BaselineRevision, rev).Scan(&deployment); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE release_drafts SET candidate_revision=$2,deployment_id=$3,updated_at=now() WHERE id=$1`, d.ID, rev, deployment)
	return err
}

func ReleaseDraftFromContext(ctx context.Context) (ReleaseDraft, bool) {
	d, ok := ctx.Value(draftPublicationKey{}).(ReleaseDraft)
	return d, ok
}

// Declarative plan hashes are re-derived from these locked transaction reads.
func ConfigResourcesTx(ctx context.Context, tx pgx.Tx, tenant string) ([]API, []Plan, error) {
	rows, err := tx.Query(ctx, `SELECT `+apiCols+` FROM apis WHERE tenant_id=$1 ORDER BY name`, tenant)
	if err != nil {
		return nil, nil, err
	}
	var apis []API
	for rows.Next() {
		a, e := scanAPI(rows)
		if e != nil {
			rows.Close()
			return nil, nil, e
		}
		apis = append(apis, a)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = tx.Query(ctx, `SELECT `+planCols+` FROM plans WHERE tenant_id=$1 ORDER BY name`, tenant)
	if err != nil {
		return nil, nil, err
	}
	var plans []Plan
	for rows.Next() {
		p, e := scanPlan(rows)
		if e != nil {
			rows.Close()
			return nil, nil, e
		}
		plans = append(plans, p)
	}
	rows.Close()
	return apis, plans, rows.Err()
}

// RegisterPortalApplication commits identity, one-time credential and subscriptions
// together. Publication and approval policy are checked again under row locks.
func (s *Store) RegisterPortalApplication(ctx context.Context, c Consumer, apiIDs []string, planID *string) (Consumer, APIKey, bool, error) {
	var key APIKey
	var pending bool
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return c, key, false, err
	}
	raw := "rk_" + hex.EncodeToString(buf)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if planID != nil {
			var tenant string
			if err := tx.QueryRow(ctx, `SELECT tenant_id FROM plans WHERE id=$1 FOR SHARE`, *planID).Scan(&tenant); err != nil {
				return err
			}
			if tenant != c.TenantID {
				return ErrCrossTenant
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO consumers(tenant_id,name,email,status,registration_ip) VALUES($1,$2,$3,'active',$4) RETURNING id`, c.TenantID, c.Name, c.Email, c.RegistrationIP).Scan(&c.ID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO api_keys(consumer_id,name,key_prefix,key_hash) VALUES($1,'portal-key',$2,$3) RETURNING id`, c.ID, raw[:10], HashKey(raw)).Scan(&key.ID); err != nil {
			return err
		}
		for _, id := range apiIDs {
			var approval bool
			var tenant string
			if err := tx.QueryRow(ctx, `SELECT tenant_id,require_approval FROM apis WHERE id=$1 AND enabled=true AND is_draft=false AND visibility='public' FOR SHARE`, id).Scan(&tenant, &approval); err != nil {
				return err
			}
			if tenant != c.TenantID {
				return ErrCrossTenant
			}
			status := "approved"
			if approval {
				status = "pending"
				pending = true
			}
			if _, err := tx.Exec(ctx, `INSERT INTO subscriptions(consumer_id,api_id,plan_id,status,active) VALUES($1,$2,$3,$4,$5)`, c.ID, id, planID, status, !approval); err != nil {
				return err
			}
		}
		var err error
		c, err = scanConsumer(tx.QueryRow(ctx, consumerSelect+` WHERE c.id=$1`, c.ID))
		return err
	})
	key.Key = raw
	if err != nil {
		key = APIKey{}
	}
	return c, key, pending, mapErr(err)
}
