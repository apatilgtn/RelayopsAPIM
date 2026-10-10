package store

import (
	"context"
	"testing"
)

func TestIntegrationAIBudgetAndManifestLifecycle(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 1. Create a Provider Connection
	conn, err := st.CreateAIProviderConnection(ctx, AIProviderConnection{
		TenantID:        DefaultTenantID,
		Name:            "OpenAI Production",
		ProviderType:    "openai",
		BaseURL:         "https://api.openai.com/v1",
		APIKeySecretRef: "${secret:OPENAI_API_KEY}",
		Capabilities:    map[string]any{"streaming": true, "tools": true},
	})
	if err != nil {
		t.Fatalf("CreateAIProviderConnection: %v", err)
	}

	// 2. Create Model Deployments
	dep, err := st.CreateAIModelDeployment(ctx, AIModelDeployment{
		TenantID:              DefaultTenantID,
		ConnectionID:          conn.ID,
		ModelName:             "gpt-4o",
		DeploymentName:        "gpt-4o-primary",
		ContextWindowTokens:   128000,
		MaxOutputTokens:       4096,
		InputPricePerMillion:  2.50,
		OutputPricePerMillion: 10.00,
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateAIModelDeployment: %v", err)
	}

	// 3. Create an AI Service
	svc, err := st.CreateAIService(ctx, AIService{
		TenantID:                 DefaultTenantID,
		Name:                     "Customer Support AI",
		Alias:                    "support-ai",
		PrimaryModelDeploymentID: &dep.ID,
		AllowedModels:            []string{"gpt-4o", "gpt-4o-mini"},
		RoutingPolicy:            "single",
	})
	if err != nil {
		t.Fatalf("CreateAIService: %v", err)
	}

	// 4. Test Atomic Spend Budget Account
	// Set monthly budget to $10.00 (1000 cents)
	acc, err := st.UpsertAIBudgetAccount(ctx, AIBudgetAccount{
		TenantID:           DefaultTenantID,
		Currency:           "USD",
		MonthlyBudgetCents: 1000,
		StrictEnforcement:  true,
	})
	if err != nil {
		t.Fatalf("UpsertAIBudgetAccount: %v", err)
	}

	// First reservation: $4.00 (400 cents) -> Should succeed
	res1, err := st.ReserveAIBudget(ctx, acc.ID, "req-101", 400)
	if err != nil {
		t.Fatalf("ReserveAIBudget 400 cents failed: %v", err)
	}
	if res1.Status != "pending" {
		t.Fatalf("expected pending status, got %s", res1.Status)
	}

	// Check account state
	accCheck, err := st.GetAIBudgetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAIBudgetAccount: %v", err)
	}
	if accCheck.ReservedSpendCents != 400 || accCheck.CurrentSpendCents != 0 {
		t.Fatalf("unexpected spend balances: reserved=%d, current=%d", accCheck.ReservedSpendCents, accCheck.CurrentSpendCents)
	}

	// Second reservation: $7.00 (700 cents) -> Total would be 400+700 = 1100 > 1000 -> Should fail with ErrBudgetExceeded
	_, err = st.ReserveAIBudget(ctx, acc.ID, "req-102", 700)
	if err != ErrBudgetExceeded {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}

	// Settle first reservation with actual usage of $3.50 (350 cents)
	if err := st.SettleAIBudget(ctx, "req-101", 350); err != nil {
		t.Fatalf("SettleAIBudget failed: %v", err)
	}

	accCheck2, err := st.GetAIBudgetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAIBudgetAccount: %v", err)
	}
	if accCheck2.ReservedSpendCents != 0 || accCheck2.CurrentSpendCents != 350 {
		t.Fatalf("unexpected spend after settlement: reserved=%d, current=%d", accCheck2.ReservedSpendCents, accCheck2.CurrentSpendCents)
	}

	// Third reservation: $2.00 (200 cents) -> Should succeed now that 350+200 <= 1000
	_, err = st.ReserveAIBudget(ctx, acc.ID, "req-103", 200)
	if err != nil {
		t.Fatalf("ReserveAIBudget 200 cents failed: %v", err)
	}

	// Release third reservation without charging
	if err := st.ReleaseAIBudget(ctx, "req-103"); err != nil {
		t.Fatalf("ReleaseAIBudget failed: %v", err)
	}

	accCheck3, err := st.GetAIBudgetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAIBudgetAccount: %v", err)
	}
	if accCheck3.ReservedSpendCents != 0 || accCheck3.CurrentSpendCents != 350 {
		t.Fatalf("unexpected spend after release: reserved=%d, current=%d", accCheck3.ReservedSpendCents, accCheck3.CurrentSpendCents)
	}

	// Usage is priced with the tenant's deployment prices: 200k prompt tokens at
	// $2.50/M plus 50k completion tokens at $10/M is $1.00 (100 cents), not the estimate.
	if _, err := st.ReserveAIBudget(ctx, acc.ID, "req-104", 2); err != nil {
		t.Fatalf("ReserveAIBudget req-104: %v", err)
	}
	cents, err := st.SettleAIBudgetUsage(ctx, "req-104", AIUsage{Model: "gpt-4o", PromptTokens: 200_000, CompletionTokens: 50_000, EstimateCents: 2})
	if err != nil || cents != 100 {
		t.Fatalf("priced settlement: %d cents, err %v (want 100)", cents, err)
	}
	// A model without a priced deployment is charged the caller's estimate.
	if _, err := st.ReserveAIBudget(ctx, acc.ID, "req-105", 2); err != nil {
		t.Fatalf("ReserveAIBudget req-105: %v", err)
	}
	if cents, err := st.SettleAIBudgetUsage(ctx, "req-105", AIUsage{Model: "unpriced-model", PromptTokens: 10, EstimateCents: 7}); err != nil || cents != 7 {
		t.Fatalf("estimate settlement: %d cents, err %v (want 7)", cents, err)
	}
	accCheck4, _ := st.GetAIBudgetAccount(ctx, acc.ID)
	if accCheck4.ReservedSpendCents != 0 || accCheck4.CurrentSpendCents != 457 {
		t.Fatalf("spend after priced settlements: reserved=%d, current=%d (want 0, 457)", accCheck4.ReservedSpendCents, accCheck4.CurrentSpendCents)
	}

	// 5. Test AI Release Manifest Lifecycle
	manifest, err := st.CreateAIReleaseManifest(ctx, AIReleaseManifest{
		TenantID:            DefaultTenantID,
		ServiceID:           svc.ID,
		Version:             1,
		ModelDeploymentID:   dep.ID,
		SystemPrompt:        "You are a helpful customer support agent.",
		QualificationStatus: "draft",
	})
	if err != nil {
		t.Fatalf("CreateAIReleaseManifest: %v", err)
	}
	if manifest.QualificationStatus != "draft" {
		t.Fatalf("expected draft status, got %s", manifest.QualificationStatus)
	}

	// Qualify release manifest with evaluation evidence digest
	digest := "sha256:7f83b1657ff1fc53b92dc18148a1d65dfc2d4b1fa3d677284addd200126d9069"
	if err := st.QualifyAIReleaseManifest(ctx, manifest.ID, digest, "eval-run-123"); err != nil {
		t.Fatalf("QualifyAIReleaseManifest failed: %v", err)
	}

	list, err := st.ListAIReleaseManifests(ctx, svc.ID)
	if err != nil {
		t.Fatalf("ListAIReleaseManifests: %v", err)
	}
	if len(list) != 1 || list[0].QualificationStatus != "qualified" || list[0].EvidenceDigest != digest {
		t.Fatalf("unexpected manifest after qualification: %+v", list[0])
	}
}
