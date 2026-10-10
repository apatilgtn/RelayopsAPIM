package apiops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompileDirectoryAndOverlays(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup structure
	apisDir := filepath.Join(tmpDir, "apis", "orders")
	if err := os.MkdirAll(apisDir, 0755); err != nil {
		t.Fatal(err)
	}
	plansDir := filepath.Join(tmpDir, "plans")
	if err := os.MkdirAll(plansDir, 0755); err != nil {
		t.Fatal(err)
	}
	envsDir := filepath.Join(tmpDir, "environments")
	if err := os.MkdirAll(envsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Write orders/config.yaml
	ordersConfig := `
name: orders
base_path: /orders
upstream_url: http://orders-dev:8080
auth_type: jwt
rate_limit_per_minute: 100
`
	if err := os.WriteFile(filepath.Join(apisDir, "config.yaml"), []byte(ordersConfig), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Write plans/gold.yaml
	goldPlan := `
name: gold
rate_limit_per_minute: 500
price_monthly_usd: 49.99
tier: premium
`
	if err := os.WriteFile(filepath.Join(plansDir, "gold.yaml"), []byte(goldPlan), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Write environments/prod.yaml overlay
	prodOverlay := `
upstream_urls:
  orders: https://orders.prod.internal
limits:
  orders: 1000
`
	if err := os.WriteFile(filepath.Join(envsDir, "prod.yaml"), []byte(prodOverlay), 0644); err != nil {
		t.Fatal(err)
	}

	// Test Dev compilation (no overlay)
	devBundle, err := CompileDirectory(tmpDir, "")
	if err != nil {
		t.Fatalf("dev compile failed: %v", err)
	}
	if len(devBundle.Config.APIs) != 1 || devBundle.Config.APIs[0].UpstreamURL != "http://orders-dev:8080" {
		t.Fatalf("unexpected dev api: %+v", devBundle.Config.APIs)
	}
	if len(devBundle.Config.Plans) != 1 || devBundle.Config.Plans[0].PriceMonthlyUSD != 49.99 {
		t.Fatalf("unexpected dev plan: %+v", devBundle.Config.Plans)
	}
	if devBundle.SourceHash == "" || devBundle.RenderedHash == "" {
		t.Fatalf("missing cryptographic digests in dev bundle")
	}

	// Test Prod compilation (with overlay)
	prodBundle, err := CompileDirectory(tmpDir, "prod")
	if err != nil {
		t.Fatalf("prod compile failed: %v", err)
	}
	if len(prodBundle.Config.APIs) != 1 {
		t.Fatalf("expected 1 api in prod bundle, got %d", len(prodBundle.Config.APIs))
	}
	api := prodBundle.Config.APIs[0]
	if api.UpstreamURL != "https://orders.prod.internal" {
		t.Fatalf("expected prod overlay upstream URL, got %s", api.UpstreamURL)
	}
	if api.RateLimitPerMinute != 1000 {
		t.Fatalf("expected prod overlay limit 1000, got %d", api.RateLimitPerMinute)
	}
	if prodBundle.RenderedHash == devBundle.RenderedHash {
		t.Fatalf("prod rendered hash must differ from dev rendered hash")
	}
}
