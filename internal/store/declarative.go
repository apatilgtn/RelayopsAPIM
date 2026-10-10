package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UpsertAPIByNameTx creates or fully replaces the API with a.Name inside tx.
// The API ID is preserved for existing APIs so subscriptions and logs stay attached.
func UpsertAPIByNameTx(ctx context.Context, tx pgx.Tx, a API) (API, error) {
	a.RequestHeaders = nonNilHeaders(a.RequestHeaders)
	a.OpenAPISpec = nonNilSpec(a.OpenAPISpec)
	a.Visibility = visibilityOrDefault(a.Visibility)
	a.QuotaFailurePolicy = quotaPolicyOrDefault(a.QuotaFailurePolicy)
	out, err := scanAPI(tx.QueryRow(ctx, `INSERT INTO apis
		(name, description, base_path, upstream_url, strip_path, auth_type, jwt_secret,
		 jwks_url, oidc_issuer, oidc_audience, rate_limit_per_minute, quota_per_day, quota_per_month,
		 timeout_ms, cors_enabled, request_headers, is_ai, openapi_spec, visibility, require_approval,
		 is_draft, quota_failure_policy, enabled, traffic_policy, tenant_id,
		 protocol, graphql_schema, graphql_policy, grpc_descriptor_set, grpc_policy, mcp_policy)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			description=$2, base_path=$3, upstream_url=$4, strip_path=$5, auth_type=$6, jwt_secret=$7,
			jwks_url=$8, oidc_issuer=$9, oidc_audience=$10, rate_limit_per_minute=$11, quota_per_day=$12,
			quota_per_month=$13, timeout_ms=$14, cors_enabled=$15, request_headers=$16, is_ai=$17,
			openapi_spec=$18, visibility=$19, require_approval=$20, is_draft=$21, quota_failure_policy=$22,
			enabled=$23, traffic_policy=$24, protocol=$26, graphql_schema=$27, graphql_policy=$28,
			grpc_descriptor_set=$29, grpc_policy=$30, mcp_policy=$31, updated_at=now()
		RETURNING `+apiCols,
		a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType, a.JWTSecret,
		a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay, a.QuotaPerMonth,
		a.TimeoutMS, a.CORSEnabled, a.RequestHeaders, a.IsAI, a.OpenAPISpec, a.Visibility, a.RequireApproval,
		a.IsDraft, a.QuotaFailurePolicy, a.Enabled, a.TrafficPolicy, TenantOrDefault(a.TenantID),
		protocolOrDefault(a.Protocol), a.GraphQLSchema, a.GraphQLPolicy, a.GRPCDescriptorSet, a.GRPCPolicy, a.MCPPolicy))
	if err != nil {
		return API{}, fmt.Errorf("apply api %q: %w", a.Name, err)
	}
	return out, nil
}

// UpsertPlanByNameTx creates or updates the plan with p.Name inside tx.
func UpsertPlanByNameTx(ctx context.Context, tx pgx.Tx, p Plan) (Plan, error) {
	tier := p.Tier
	if tier == "" {
		tier = "free"
	}
	out, err := scanPlan(tx.QueryRow(ctx, `INSERT INTO plans (name, description, rate_limit_per_minute, quota_per_day, quota_per_month, price_monthly_usd, tier, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, name) DO UPDATE SET description=$2, rate_limit_per_minute=$3, quota_per_day=$4, quota_per_month=$5, price_monthly_usd=$6, tier=$7, updated_at=now()
		RETURNING `+planCols, p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier, TenantOrDefault(p.TenantID)))
	if err != nil {
		return Plan{}, fmt.Errorf("apply plan %q: %w", p.Name, err)
	}
	return out, nil
}

// DeleteAPIByNameTx deletes a tenant's API (and, by cascade, its subscriptions).
func DeleteAPIByNameTx(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	_, err := tx.Exec(ctx, `DELETE FROM apis WHERE tenant_id=$1 AND name=$2`, TenantOrDefault(tenantID), name)
	return err
}

// DeletePlanByNameTx deletes a tenant's plan; subscriptions keep their grant but lose the plan.
func DeletePlanByNameTx(ctx context.Context, tx pgx.Tx, tenantID, name string) error {
	_, err := tx.Exec(ctx, `DELETE FROM plans WHERE tenant_id=$1 AND name=$2`, TenantOrDefault(tenantID), name)
	return err
}

// CountSubscriptionsByAPI and CountSubscriptionsByPlan support prune impact warnings.
func (s *Store) CountSubscriptionsByAPI(ctx context.Context) (map[string]int, error) {
	return s.countBy(ctx, `SELECT api_id::text, count(*) FROM subscriptions GROUP BY api_id`)
}

func (s *Store) CountSubscriptionsByPlan(ctx context.Context) (map[string]int, error) {
	return s.countBy(ctx, `SELECT plan_id::text, count(*) FROM subscriptions WHERE plan_id IS NOT NULL GROUP BY plan_id`)
}

func (s *Store) countBy(ctx context.Context, q string) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
