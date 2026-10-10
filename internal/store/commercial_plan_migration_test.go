package store

import (
	"context"
	"github.com/relayops/apim/internal/testdb"
	"testing"
)

func TestIntegrationCommercialPlansRequireExplicitConfiguration(t *testing.T) {
	s := openTestStore(t)
	var count int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM plans WHERE name IN ('Developer Tier','Pro Tier')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("fresh installation unexpectedly created priced commercial offerings")
	}
	p, _, err := s.CreatePlanAtomic(context.Background(), Plan{Name: "Pro Tier", PriceMonthlyUSD: 42, Tier: "custom", RateLimitPerMinute: 75}, "test", "explicit offering")
	if err != nil {
		t.Fatal(err)
	}
	if p.PriceMonthlyUSD != 42 || p.Tier != "custom" {
		t.Fatalf("explicit customer offering changed: %+v", p)
	}
}

func TestIntegrationCommercialMigrationPreservesCustomerPlan(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MigrateTo(ctx, "010_tenants_and_onboarding"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO plans (name,description,rate_limit_per_minute,quota_per_day,quota_per_month) VALUES ('Pro Tier','Customer-owned description',77,88,99) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPlan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "Customer-owned description" || p.RateLimitPerMinute != 77 || p.QuotaPerDay != 88 || p.QuotaPerMonth != 99 || p.PriceMonthlyUSD != 0 || p.Tier != "free" {
		t.Fatalf("upgrade changed customer plan: %+v", p)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE plans SET price_monthly_usd=43,tier='customer' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, err = s.GetPlan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.PriceMonthlyUSD != 43 || p.Tier != "customer" {
		t.Fatal("repeat migration overwrote customer pricing")
	}
}
