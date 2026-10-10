package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

func TestAPIOpsLifecycleAndReleasePassport(t *testing.T) {
	// A throwaway database: the fixtures use fixed names, so a shared one
	// conflicts on the second run.
	dbURL := testdb.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tenantID := store.DefaultTenantID

	// 1. Create an APIOps Environment
	env, err := s.CreateAPIOpsEnvironment(ctx, store.APIOpsEnvironment{
		TenantID:         tenantID,
		Name:             "staging-primary",
		Description:      "Staging gateway target",
		IsProduction:     false,
		TargetGatewayURL: "http://staging.relayops.internal:8080",
	})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if env.ID == "" || env.Name != "staging-primary" {
		t.Fatalf("unexpected env: %+v", env)
	}

	// 2. Create an APIOps Deployment
	dep, err := s.CreateAPIOpsDeployment(ctx, store.APIOpsDeployment{
		TenantID:             tenantID,
		EnvironmentID:        &env.ID,
		Status:               "planned",
		CommitSHA:            "a1b2c3d4e5f6",
		RepoURL:              "https://github.com/org/relayops-config",
		Branch:               "main",
		Actor:                "ci-bot",
		PlanHash:             "sha256:112233445566",
		ExpectedBaseRevision: 195,
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if dep.Status != "planned" || dep.CommitSHA != "a1b2c3d4e5f6" {
		t.Fatalf("unexpected deployment: %+v", dep)
	}

	// 3. Update Status to canary
	err = s.UpdateAPIOpsDeploymentStatus(ctx, dep.ID, "canary", 196, 0)
	if err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	gotDep, err := s.GetAPIOpsDeployment(ctx, dep.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if gotDep.Status != "canary" || gotDep.CandidateRevision != 196 || gotDep.EnvironmentName != "staging-primary" {
		t.Fatalf("unexpected gotDep: %+v", gotDep)
	}

	// 4. Create Release Passport
	passport, err := s.CreateAPIOpsReleasePassport(ctx, store.APIOpsReleasePassport{
		DeploymentID:   dep.ID,
		TenantID:       tenantID,
		EvidenceDigest: "sha256:fedcba9876543210",
		Manifest: map[string]any{
			"commit_sha":        "a1b2c3d4e5f6",
			"candidate_rev":     196,
			"promoted_rev":      197,
			"verified_suites":   []string{"suite-smoke", "suite-regression"},
			"evidence_eligible": true,
			"approved_by":       "lead-operator",
		},
	})
	if err != nil {
		t.Fatalf("create release passport: %v", err)
	}
	if passport.ID == "" || passport.EvidenceDigest != "sha256:fedcba9876543210" {
		t.Fatalf("unexpected passport: %+v", passport)
	}

	// 5. Query Passport by Deployment ID
	fetchedPass, err := s.GetAPIOpsReleasePassport(ctx, dep.ID)
	if err != nil {
		t.Fatalf("get release passport: %v", err)
	}
	if fetchedPass.EvidenceDigest != passport.EvidenceDigest {
		t.Fatalf("digest mismatch: got %s, want %s", fetchedPass.EvidenceDigest, passport.EvidenceDigest)
	}
	if fetchedPass.Manifest["approved_by"] != "lead-operator" {
		t.Fatalf("manifest content mismatch: %+v", fetchedPass.Manifest)
	}
}
