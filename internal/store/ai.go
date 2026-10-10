package store

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
)

// ErrBudgetExceeded is returned when an atomic budget reservation cannot be granted.
var ErrBudgetExceeded = errors.New("monthly AI spend budget exceeded")

// ---------------------------------------------------------------------------
// Provider Connections & Model Deployments
// ---------------------------------------------------------------------------

func (s *Store) CreateAIProviderConnection(ctx context.Context, conn AIProviderConnection) (AIProviderConnection, error) {
	if conn.Capabilities == nil {
		conn.Capabilities = map[string]any{}
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ai_provider_connections (tenant_id, name, provider_type, base_url, api_key_secret_ref, capabilities)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, name, provider_type, base_url, api_key_secret_ref, capabilities, created_at, updated_at`,
		TenantOrDefault(conn.TenantID), conn.Name, conn.ProviderType, conn.BaseURL, conn.APIKeySecretRef, conn.Capabilities,
	).Scan(&conn.ID, &conn.TenantID, &conn.Name, &conn.ProviderType, &conn.BaseURL, &conn.APIKeySecretRef, &conn.Capabilities, &conn.CreatedAt, &conn.UpdatedAt)
	return conn, mapErr(err)
}

func (s *Store) ListAIProviderConnections(ctx context.Context, tenantID string) ([]AIProviderConnection, error) {
	query := `SELECT id, tenant_id, name, provider_type, base_url, api_key_secret_ref, capabilities, created_at, updated_at FROM ai_provider_connections`
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		rows, err = s.Pool.Query(ctx, query+` WHERE tenant_id = $1 ORDER BY name ASC`, tenantID)
	} else {
		rows, err = s.Pool.Query(ctx, query+` ORDER BY name ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AIProviderConnection
	for rows.Next() {
		var c AIProviderConnection
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.ProviderType, &c.BaseURL, &c.APIKeySecretRef, &c.Capabilities, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CreateAIModelDeployment(ctx context.Context, dep AIModelDeployment) (AIModelDeployment, error) {
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ai_model_deployments (tenant_id, connection_id, model_name, deployment_name, context_window_tokens, max_output_tokens, input_price_per_million, output_price_per_million, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, tenant_id, connection_id, model_name, deployment_name, context_window_tokens, max_output_tokens, input_price_per_million, output_price_per_million, enabled, created_at, updated_at`,
		TenantOrDefault(dep.TenantID), dep.ConnectionID, dep.ModelName, dep.DeploymentName, dep.ContextWindowTokens, dep.MaxOutputTokens, dep.InputPricePerMillion, dep.OutputPricePerMillion, dep.Enabled,
	).Scan(&dep.ID, &dep.TenantID, &dep.ConnectionID, &dep.ModelName, &dep.DeploymentName, &dep.ContextWindowTokens, &dep.MaxOutputTokens, &dep.InputPricePerMillion, &dep.OutputPricePerMillion, &dep.Enabled, &dep.CreatedAt, &dep.UpdatedAt)
	return dep, mapErr(err)
}

func (s *Store) ListAIModelDeployments(ctx context.Context, tenantID string) ([]AIModelDeployment, error) {
	query := `SELECT id, tenant_id, connection_id, model_name, deployment_name, context_window_tokens, max_output_tokens, input_price_per_million, output_price_per_million, enabled, created_at, updated_at FROM ai_model_deployments`
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		rows, err = s.Pool.Query(ctx, query+` WHERE tenant_id = $1 ORDER BY model_name ASC`, tenantID)
	} else {
		rows, err = s.Pool.Query(ctx, query+` ORDER BY model_name ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AIModelDeployment
	for rows.Next() {
		var d AIModelDeployment
		if err := rows.Scan(&d.ID, &d.TenantID, &d.ConnectionID, &d.ModelName, &d.DeploymentName, &d.ContextWindowTokens, &d.MaxOutputTokens, &d.InputPricePerMillion, &d.OutputPricePerMillion, &d.Enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// AI Services
// ---------------------------------------------------------------------------

func (s *Store) CreateAIService(ctx context.Context, svc AIService) (AIService, error) {
	if svc.AllowedModels == nil {
		svc.AllowedModels = []string{}
	}
	if svc.RoutingPolicy == "" {
		svc.RoutingPolicy = "single"
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ai_services (tenant_id, api_id, name, alias, primary_model_deployment_id, allowed_models, routing_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, api_id, name, alias, primary_model_deployment_id, allowed_models, routing_policy, created_at, updated_at`,
		TenantOrDefault(svc.TenantID), svc.APIID, svc.Name, svc.Alias, svc.PrimaryModelDeploymentID, svc.AllowedModels, svc.RoutingPolicy,
	).Scan(&svc.ID, &svc.TenantID, &svc.APIID, &svc.Name, &svc.Alias, &svc.PrimaryModelDeploymentID, &svc.AllowedModels, &svc.RoutingPolicy, &svc.CreatedAt, &svc.UpdatedAt)
	return svc, mapErr(err)
}

func (s *Store) ListAIServices(ctx context.Context, tenantID string) ([]AIService, error) {
	query := `SELECT id, tenant_id, api_id, name, alias, primary_model_deployment_id, allowed_models, routing_policy, created_at, updated_at FROM ai_services`
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		rows, err = s.Pool.Query(ctx, query+` WHERE tenant_id = $1 ORDER BY alias ASC`, tenantID)
	} else {
		rows, err = s.Pool.Query(ctx, query+` ORDER BY alias ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AIService
	for rows.Next() {
		var svc AIService
		if err := rows.Scan(&svc.ID, &svc.TenantID, &svc.APIID, &svc.Name, &svc.Alias, &svc.PrimaryModelDeploymentID, &svc.AllowedModels, &svc.RoutingPolicy, &svc.CreatedAt, &svc.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Atomic Budget Reservations & Settlement Ledger
// ---------------------------------------------------------------------------

func (s *Store) UpsertAIBudgetAccount(ctx context.Context, acc AIBudgetAccount) (AIBudgetAccount, error) {
	if acc.Currency == "" {
		acc.Currency = "USD"
	}
	if acc.ResetDayOfMonth <= 0 {
		acc.ResetDayOfMonth = 1
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ai_budget_accounts (tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month, created_at, updated_at`,
		TenantOrDefault(acc.TenantID), acc.ConsumerID, acc.Currency, acc.MonthlyBudgetCents, acc.CurrentSpendCents, acc.ReservedSpendCents, acc.StrictEnforcement, acc.ResetDayOfMonth,
	).Scan(&acc.ID, &acc.TenantID, &acc.ConsumerID, &acc.Currency, &acc.MonthlyBudgetCents, &acc.CurrentSpendCents, &acc.ReservedSpendCents, &acc.StrictEnforcement, &acc.ResetDayOfMonth, &acc.CreatedAt, &acc.UpdatedAt)
	return acc, mapErr(err)
}

func (s *Store) GetAIBudgetAccount(ctx context.Context, id string) (AIBudgetAccount, error) {
	var acc AIBudgetAccount
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month, created_at, updated_at
		FROM ai_budget_accounts WHERE id = $1`, id,
	).Scan(&acc.ID, &acc.TenantID, &acc.ConsumerID, &acc.Currency, &acc.MonthlyBudgetCents, &acc.CurrentSpendCents, &acc.ReservedSpendCents, &acc.StrictEnforcement, &acc.ResetDayOfMonth, &acc.CreatedAt, &acc.UpdatedAt)
	return acc, mapErr(err)
}

func (s *Store) GetAIBudgetAccountByConsumer(ctx context.Context, tenantID, consumerID string) (*AIBudgetAccount, error) {
	var acc AIBudgetAccount
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month, created_at, updated_at
		FROM ai_budget_accounts WHERE tenant_id = $1 AND consumer_id = $2`, TenantOrDefault(tenantID), consumerID,
	).Scan(&acc.ID, &acc.TenantID, &acc.ConsumerID, &acc.Currency, &acc.MonthlyBudgetCents, &acc.CurrentSpendCents, &acc.ReservedSpendCents, &acc.StrictEnforcement, &acc.ResetDayOfMonth, &acc.CreatedAt, &acc.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &acc, nil
}

func (s *Store) ListAIBudgetAccounts(ctx context.Context, tenantID string) ([]AIBudgetAccount, error) {
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		rows, err = s.Pool.Query(ctx, `
			SELECT id, tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month, created_at, updated_at
			FROM ai_budget_accounts
			WHERE tenant_id = $1
			ORDER BY created_at DESC`, TenantOrDefault(tenantID))
	} else {
		rows, err = s.Pool.Query(ctx, `
			SELECT id, tenant_id, consumer_id, currency, monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement, reset_day_of_month, created_at, updated_at
			FROM ai_budget_accounts
			ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []AIBudgetAccount
	for rows.Next() {
		var acc AIBudgetAccount
		if err := rows.Scan(&acc.ID, &acc.TenantID, &acc.ConsumerID, &acc.Currency, &acc.MonthlyBudgetCents, &acc.CurrentSpendCents, &acc.ReservedSpendCents, &acc.StrictEnforcement, &acc.ResetDayOfMonth, &acc.CreatedAt, &acc.UpdatedAt); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

// ReserveAIBudget atomically tests limits and reserves capacity in a single transaction.
func (s *Store) ReserveAIBudget(ctx context.Context, accountID, requestID string, reserveCents int64) (*AIBudgetReservation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var monthlyBudget, currentSpend, reservedSpend int64
	var strict bool
	err = tx.QueryRow(ctx, `
		SELECT monthly_budget_cents, current_spend_cents, reserved_spend_cents, strict_enforcement
		FROM ai_budget_accounts WHERE id = $1 FOR UPDATE`, accountID,
	).Scan(&monthlyBudget, &currentSpend, &reservedSpend, &strict)
	if err != nil {
		return nil, mapErr(err)
	}

	// If budget is set and strict enforcement is on, check limits
	if monthlyBudget > 0 && strict {
		if currentSpend+reservedSpend+reserveCents > monthlyBudget {
			return nil, ErrBudgetExceeded
		}
	}

	// Update reserved spend on account
	_, err = tx.Exec(ctx, `
		UPDATE ai_budget_accounts SET reserved_spend_cents = reserved_spend_cents + $2, updated_at = now()
		WHERE id = $1`, accountID, reserveCents)
	if err != nil {
		return nil, err
	}

	// Insert reservation record
	var res AIBudgetReservation
	err = tx.QueryRow(ctx, `
		INSERT INTO ai_budget_reservations (account_id, request_id, reserved_cents, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id, account_id, request_id, reserved_cents, settled_cents, status, created_at, expires_at`,
		accountID, requestID, reserveCents,
	).Scan(&res.ID, &res.AccountID, &res.RequestID, &res.ReservedCents, &res.SettledCents, &res.Status, &res.CreatedAt, &res.ExpiresAt)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &res, nil
}

// AIUsage is the metered usage of one AI request, settled against its reservation.
type AIUsage struct {
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	// EstimateCents is charged when the tenant has no priced deployment for Model.
	EstimateCents int64 `json:"estimate_cents"`
}

// SettleAIBudgetUsage prices usage with the reserving tenant's model deployment
// prices (per million tokens, in the account currency) and settles the
// reservation. Without a priced deployment for the model it charges the
// caller's estimate. Every settled request costs at least one cent. It returns
// the cents charged.
func (s *Store) SettleAIBudgetUsage(ctx context.Context, requestID string, u AIUsage) (int64, error) {
	cents := u.EstimateCents
	var in, out float64
	err := s.Pool.QueryRow(ctx, `
		SELECT d.input_price_per_million::float8, d.output_price_per_million::float8
		FROM ai_budget_reservations r
		JOIN ai_budget_accounts a ON a.id = r.account_id
		JOIN ai_model_deployments d ON d.tenant_id = a.tenant_id AND d.enabled
		     AND (d.model_name = $2 OR d.deployment_name = $2)
		WHERE r.request_id = $1
		ORDER BY d.updated_at DESC LIMIT 1`, requestID, u.Model).Scan(&in, &out)
	switch {
	case err == nil && (in > 0 || out > 0):
		cents = int64(math.Ceil((float64(u.PromptTokens)*in + float64(u.CompletionTokens)*out) / 1e6 * 100))
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return 0, err
	}
	if cents < 1 {
		cents = 1
	}
	return cents, s.SettleAIBudget(ctx, requestID, cents)
}

// SettleAIBudget reconciles actual usage against a pending reservation.
func (s *Store) SettleAIBudget(ctx context.Context, requestID string, actualCents int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var accountID string
	var reservedCents int64
	var status string
	err = tx.QueryRow(ctx, `
		SELECT account_id, reserved_cents, status
		FROM ai_budget_reservations WHERE request_id = $1 FOR UPDATE`, requestID,
	).Scan(&accountID, &reservedCents, &status)
	if err != nil {
		return mapErr(err)
	}

	if status != "pending" {
		return nil // already settled or released
	}

	// Adjust account: deduct reserved, add actual
	_, err = tx.Exec(ctx, `
		UPDATE ai_budget_accounts
		SET reserved_spend_cents = GREATEST(0, reserved_spend_cents - $2),
		    current_spend_cents = current_spend_cents + $3,
		    updated_at = now()
		WHERE id = $1`, accountID, reservedCents, actualCents)
	if err != nil {
		return err
	}

	// Mark reservation settled
	_, err = tx.Exec(ctx, `
		UPDATE ai_budget_reservations
		SET status = 'settled', settled_cents = $2
		WHERE request_id = $1`, requestID, actualCents)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ReleaseAIBudget releases a pending reservation without charging spend.
func (s *Store) ReleaseAIBudget(ctx context.Context, requestID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var accountID string
	var reservedCents int64
	var status string
	err = tx.QueryRow(ctx, `
		SELECT account_id, reserved_cents, status
		FROM ai_budget_reservations WHERE request_id = $1 FOR UPDATE`, requestID,
	).Scan(&accountID, &reservedCents, &status)
	if err != nil {
		return mapErr(err)
	}

	if status != "pending" {
		return nil
	}

	_, err = tx.Exec(ctx, `
		UPDATE ai_budget_accounts
		SET reserved_spend_cents = GREATEST(0, reserved_spend_cents - $2), updated_at = now()
		WHERE id = $1`, accountID, reservedCents)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE ai_budget_reservations SET status = 'released' WHERE request_id = $1`, requestID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// AI Release Manifests & Evaluation Suites
// ---------------------------------------------------------------------------

func (s *Store) CreateAIReleaseManifest(ctx context.Context, m AIReleaseManifest) (AIReleaseManifest, error) {
	if m.Version <= 0 {
		m.Version = 1
	}
	if m.QualificationStatus == "" {
		m.QualificationStatus = "draft"
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO ai_release_manifests (tenant_id, service_id, version, model_deployment_id, system_prompt, eval_run_id, qualification_status, evidence_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, tenant_id, service_id, version, model_deployment_id, system_prompt, eval_run_id, qualification_status, evidence_digest, created_at`,
		TenantOrDefault(m.TenantID), m.ServiceID, m.Version, m.ModelDeploymentID, m.SystemPrompt, m.EvalRunID, m.QualificationStatus, m.EvidenceDigest,
	).Scan(&m.ID, &m.TenantID, &m.ServiceID, &m.Version, &m.ModelDeploymentID, &m.SystemPrompt, &m.EvalRunID, &m.QualificationStatus, &m.EvidenceDigest, &m.CreatedAt)
	return m, mapErr(err)
}

func (s *Store) ListAIReleaseManifests(ctx context.Context, serviceID string) ([]AIReleaseManifest, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tenant_id, service_id, version, model_deployment_id, system_prompt, eval_run_id, qualification_status, evidence_digest, created_at
		FROM ai_release_manifests WHERE service_id = $1 ORDER BY version DESC`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AIReleaseManifest
	for rows.Next() {
		var m AIReleaseManifest
		if err := rows.Scan(&m.ID, &m.TenantID, &m.ServiceID, &m.Version, &m.ModelDeploymentID, &m.SystemPrompt, &m.EvalRunID, &m.QualificationStatus, &m.EvidenceDigest, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetAIReleaseManifest(ctx context.Context, id string) (AIReleaseManifest, error) {
	var m AIReleaseManifest
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, service_id, version, model_deployment_id, system_prompt, eval_run_id, qualification_status, evidence_digest, created_at
		FROM ai_release_manifests WHERE id = $1`, id,
	).Scan(&m.ID, &m.TenantID, &m.ServiceID, &m.Version, &m.ModelDeploymentID, &m.SystemPrompt, &m.EvalRunID, &m.QualificationStatus, &m.EvidenceDigest, &m.CreatedAt)
	return m, mapErr(err)
}

func (s *Store) QualifyAIReleaseManifest(ctx context.Context, manifestID, evidenceDigest, evalRunID string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE ai_release_manifests
		SET qualification_status = 'qualified', evidence_digest = $2, eval_run_id = $3
		WHERE id = $1`, manifestID, evidenceDigest, evalRunID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
