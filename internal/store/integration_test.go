package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relayops/apim/internal/testdb"

	"github.com/jackc/pgx/v5"
)

// openTestStore migrates a throwaway database (see internal/testdb).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func mkAPI(name, path string) API {
	return API{Name: name, BasePath: path, UpstreamURL: "http://upstream.local", StripPath: true, AuthType: "api_key",
		TimeoutMS: 1000, Enabled: true, RateLimitPerMinute: 1000}
}

func TestIntegrationFreshInstallCanPublish(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, rev, err := s.CreateAPIAtomic(ctx, mkAPI("first", "/first"), "test", "first publish"); err != nil || rev < 2 {
		t.Fatalf("first publish on a fresh install failed: rev=%d err=%v", rev, err)
	}
}

func TestIntegrationRevisionIsolationOfPlanLimits(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	plan, _, err := s.CreatePlanAtomic(ctx, Plan{Name: "gold", RateLimitPerMinute: 100}, "test", "plan")
	if err != nil {
		t.Fatal(err)
	}
	api, _, err := s.CreateAPIAtomic(ctx, mkAPI("orders", "/orders"), "test", "api")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConsumer(ctx, Consumer{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.CreateAPIKey(ctx, c.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSubscription(ctx, c.ID, api.ID, &plan.ID); err != nil {
		t.Fatal(err)
	}
	stableRev, err := s.AtomicPublishConfig(ctx, "test", "baseline with subscription", "all", "active", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Canary changes the plan limit to 5/min and serves 25% of traffic.
	canaryRev, err := s.AtomicPublishCanary(ctx, "test", "tighten gold", CanarySplit{TrafficPercent: 25}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE plans SET rate_limit_per_minute=5 WHERE id=$1`, plan.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.LoadSnapshotDataForNode(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != stableRev || d.PolicySource != "revision" {
		t.Fatalf("standard node revision=%d source=%s, want stable %d from revision", d.Revision, d.PolicySource, stableRev)
	}
	if got := subLimit(d.Subs, c.ID); got != 100 {
		t.Fatalf("stable revision enforces %d/min, want the stable plan's 100", got)
	}
	if d.Canary == nil || d.Canary.Revision != canaryRev || d.Canary.TrafficPercent != 25 {
		t.Fatalf("canary not loaded for split: %+v", d.Canary)
	}
	if got := subLimit(d.Canary.Subs, c.ID); got != 5 {
		t.Fatalf("canary revision enforces %d/min, want 5", got)
	}

	// Dedicated canary nodes serve the canary for all traffic.
	cn, err := s.LoadSnapshotDataForNode(ctx, "canary", true)
	if err != nil {
		t.Fatal(err)
	}
	if cn.Revision != canaryRev || cn.Canary != nil || subLimit(cn.Subs, c.ID) != 5 {
		t.Fatalf("canary node: rev=%d canary=%v limit=%d", cn.Revision, cn.Canary, subLimit(cn.Subs, c.ID))
	}

	// Key revocation is live on both cohorts immediately, without any revision.
	if err := s.SetAPIKeyActive(ctx, key.ID, false); err != nil {
		t.Fatal(err)
	}
	d, _ = s.LoadSnapshotDataForNode(ctx, "default", false)
	if len(d.Keys) != 0 {
		t.Fatalf("revoked key still loaded: %+v", d.Keys)
	}

	// Publishing fleet-wide while the canary is in flight is refused.
	if _, _, err := s.CreateAPIAtomic(ctx, mkAPI("other", "/other"), "test", "would leak canary"); !IsCanaryInFlight(err) {
		t.Fatalf("publish during canary: err = %v, want ErrCanaryInFlight", err)
	}

	// Abort restores the stable plan in the tables and stops serving the canary.
	restored, err := s.AbortCanary(ctx, canaryRev, "test")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetPlan(ctx, plan.ID)
	if p.RateLimitPerMinute != 100 {
		t.Fatalf("plan table after abort = %d, want 100 restored", p.RateLimitPerMinute)
	}
	d, _ = s.LoadSnapshotDataForNode(ctx, "default", false)
	if d.Canary != nil || d.Revision != restored {
		t.Fatalf("after abort: rev=%d canary=%v, want rev %d and no canary", d.Revision, d.Canary, restored)
	}
	if _, _, err := s.CreateAPIAtomic(ctx, mkAPI("other", "/other"), "test", "after abort"); err != nil {
		t.Fatalf("publishing after abort: %v", err)
	}
}

func subLimit(subs []SubRecord, consumerID string) int {
	for _, s := range subs {
		if s.ConsumerID == consumerID && s.RateLimitPerMinute != nil {
			return *s.RateLimitPerMinute
		}
	}
	return -1
}

func TestIntegrationDeployCanaryRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, r1, _ := s.CreateAPIAtomic(ctx, mkAPI("a", "/a"), "t", "1")
	_, r2, _ := s.CreateAPIAtomic(ctx, mkAPI("b", "/b"), "t", "2")
	if err := s.DeployCanary(ctx, r1, CanarySplit{TrafficPercent: 10}); !errors.Is(err, ErrConflict) {
		t.Fatalf("canarying an older revision must be refused: %v", err)
	}
	if err := s.DeployCanary(ctx, r2, CanarySplit{TrafficPercent: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeployCanary(ctx, r2, CanarySplit{TrafficPercent: 50, Header: "X-Canary"}); err != nil {
		t.Fatalf("adjusting the split of the in-flight canary: %v", err)
	}
	sp, _ := s.GetCanarySplit(ctx, r2)
	if sp.TrafficPercent != 50 || sp.Header != "X-Canary" {
		t.Fatalf("split = %+v", sp)
	}
	stable, canary, _ := s.GetRolloutState(ctx)
	if stable != r1 || canary != r2 {
		t.Fatalf("rollout state stable=%d canary=%d, want %d/%d", stable, canary, r1, r2)
	}
	if err := s.PromoteCanary(ctx, r2); err != nil {
		t.Fatal(err)
	}
	stable, canary, _ = s.GetRolloutState(ctx)
	if stable != r2 || canary != 0 {
		t.Fatalf("after promote stable=%d canary=%d", stable, canary)
	}
	sp, _ = s.GetCanarySplit(ctx, r2)
	if sp.TrafficPercent != 0 || sp.Header != "" {
		t.Fatalf("promotion must clear the split: %+v", sp)
	}
}

func TestIntegrationAdvisoryLockIsExclusive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	var running, maxConcurrent, ran atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.WithAdvisoryLock(ctx, 4242, func(ctx context.Context) error {
				n := running.Add(1)
				for {
					m := maxConcurrent.Load()
					if n <= m || maxConcurrent.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
				running.Add(-1)
				ran.Add(1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			_ = got
		}()
	}
	wg.Wait()
	if maxConcurrent.Load() != 1 {
		t.Fatalf("lock holders ran concurrently: max %d", maxConcurrent.Load())
	}
	if ran.Load() < 1 {
		t.Fatalf("ran=%d", ran.Load())
	}
}

func TestIntegrationAutoRollbackSettingsPersist(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	cfg, err := s.GetAutoRollbackSettings(ctx)
	if err != nil || !cfg.Enabled || cfg.ErrorRateThresholdPercent != 5 {
		t.Fatalf("defaults = %+v err=%v", cfg, err)
	}
	cfg.ErrorRateThresholdPercent, cfg.MinRequests = 12.5, 40
	if _, err := s.UpdateAutoRollbackSettings(ctx, cfg, false, "ops@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAutoRollbackTrigger(ctx, 77, "bad release", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAutoRollbackSettings(ctx)
	if got.ErrorRateThresholdPercent != 12.5 || got.MinRequests != 40 || got.UpdatedBy != "ops@example.com" ||
		got.LastTriggeredRevision != 77 || got.CooldownUntil == nil || time.Until(*got.CooldownUntil) < 50*time.Second {
		t.Fatalf("persisted = %+v", got)
	}
	got, _ = s.UpdateAutoRollbackSettings(ctx, got, true, "ops@example.com")
	if got.CooldownUntil != nil {
		t.Fatal("reset_cooldown must clear the cooldown")
	}
}

func TestIntegrationRevisionErrorStatsAcrossNodes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	logs := []RequestLog{
		{TS: now, NodeID: "n1", RequestID: "1", Method: "GET", Path: "/", Status: 200, ConfigRevision: 7},
		{TS: now, NodeID: "n2", RequestID: "2", Method: "GET", Path: "/", Status: 502, ConfigRevision: 7},
		{TS: now, NodeID: "n3", RequestID: "3", Method: "GET", Path: "/", Status: 503, ConfigRevision: 7},
		{TS: now, NodeID: "n1", RequestID: "4", Method: "GET", Path: "/", Status: 200, ConfigRevision: 6},
		{TS: now.Add(-time.Hour), NodeID: "n1", RequestID: "5", Method: "GET", Path: "/", Status: 500, ConfigRevision: 6},
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	st, err := s.RevisionErrorStats(ctx, time.Minute, 7, 6, 99)
	if err != nil {
		t.Fatal(err)
	}
	if st[7].Requests != 3 || st[7].Errors != 2 || st[6].Requests != 1 || st[6].Errors != 0 || st[99].Requests != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A node that stopped reporting (scaled down, replaced) must not hold the
// fleet "not converged": only nodes seen within NodeLivenessWindow count.
func TestIntegrationFleetStatusDropsSilentNodes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, _, err := s.CreateAPIAtomic(ctx, mkAPI("first", "/first"), "t", "first"); err != nil {
		t.Fatal(err)
	}
	old, _, _ := s.GetRolloutState(ctx)
	if _, _, err := s.CreateAPIAtomic(ctx, mkAPI("second", "/second"), "t", "second"); err != nil {
		t.Fatal(err)
	}
	stable, _, err := s.GetRolloutState(ctx)
	if err != nil || stable == old {
		t.Fatalf("need two revisions: %d %d %v", old, stable, err)
	}
	if err := s.AcknowledgeRevisionWithCanary(ctx, "live", stable, 0, 1, 0, 1, "default", false); err != nil {
		t.Fatal(err)
	}
	// "gone" stopped on the old revision ten minutes ago.
	if err := s.AcknowledgeRevisionWithCanary(ctx, "gone", old, 0, 1, 0, 1, "default", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE node_acknowledgements SET applied_at = now() - interval '10 minutes' WHERE node_id = 'gone'`); err != nil {
		t.Fatal(err)
	}
	fs, err := s.GetFleetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Nodes) != 1 || fs.Nodes[0].NodeID != "live" || !fs.Converged {
		t.Fatalf("fleet = %+v (want only the live node, converged)", fs)
	}
}

// Sampled request logs: one kept success stands for SampleWeight requests, so
// revision stats and summaries count requests, not rows.
func TestIntegrationWeightedRevisionStatsAndSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	logs := []RequestLog{
		{TS: now, NodeID: "n1", RequestID: "a", LogID: "00000000-0000-4000-8000-00000000000a", Status: 200, ConfigRevision: 9, SampleWeight: 100, LatencyMS: 10, BytesOut: 5},
		{TS: now, NodeID: "n1", RequestID: "b", LogID: "00000000-0000-4000-8000-00000000000b", Status: 200, ConfigRevision: 9, SampleWeight: 100, LatencyMS: 10, BytesOut: 5},
		{TS: now, NodeID: "n2", RequestID: "c", LogID: "00000000-0000-4000-8000-00000000000c", Status: 503, ConfigRevision: 9, LatencyMS: 40},
		{TS: now, NodeID: "n2", RequestID: "d", LogID: "00000000-0000-4000-8000-00000000000d", Status: 404, ConfigRevision: 9, LatencyMS: 40},
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	st, err := s.RevisionErrorStats(ctx, time.Minute, 9)
	if err != nil {
		t.Fatal(err)
	}
	if st[9].Requests != 202 || st[9].Errors != 1 {
		t.Fatalf("weighted revision stats = %+v (want 202 requests, 1 error)", st[9])
	}
	sum, err := s.SummarizeScoped(ctx, time.Hour, "1h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 202 || sum.Errors5xx != 1 || sum.Errors4xx != 1 || sum.BytesOut != 1000 {
		t.Fatalf("weighted summary = total %d 5xx %d 4xx %d bytes %d", sum.Total, sum.Errors5xx, sum.Errors4xx, sum.BytesOut)
	}
	// Weighted mean latency: (200*10 + 2*40) / 202.
	if want := (200*10.0 + 2*40.0) / 202; sum.AvgLatency < want-0.01 || sum.AvgLatency > want+0.01 {
		t.Fatalf("weighted average latency %.3f, want %.3f", sum.AvgLatency, want)
	}
}

func TestIntegrationTrafficPolicyRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := mkAPI("lb", "/lb")
	a.TrafficPolicy = TrafficPolicy{
		Targets:        []UpstreamTarget{{URL: "http://a", Weight: 2}, {URL: "http://b", Weight: 0}},
		Retries:        &RetryPolicy{Attempts: 3},
		CircuitBreaker: &CircuitBreakerPolicy{FailureThreshold: 4, OpenSeconds: 9},
	}
	created, rev, err := s.CreateAPIAtomic(ctx, a, "t", "lb")
	if err != nil {
		t.Fatal(err)
	}
	if len(created.TrafficPolicy.Targets) != 2 || created.TrafficPolicy.Targets[1].Weight != 0 || created.TrafficPolicy.Retries.Attempts != 3 {
		t.Fatalf("stored policy = %+v", created.TrafficPolicy)
	}
	// Rollback restores the traffic policy (and every other field) from the revision.
	created.TrafficPolicy = TrafficPolicy{}
	created.OIDCAudience = "changed"
	if _, _, err := s.UpdateAPIAtomic(ctx, created, "t", "clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollbackConfigRevision(ctx, rev, "t"); err != nil {
		t.Fatal(err)
	}
	back, _ := s.GetAPI(ctx, created.ID)
	if back.TrafficPolicy.CircuitBreaker == nil || back.TrafficPolicy.CircuitBreaker.OpenSeconds != 9 || back.OIDCAudience != "" {
		t.Fatalf("rollback did not restore all fields: %+v audience=%q", back.TrafficPolicy, back.OIDCAudience)
	}
}

func TestIntegrationRequestLogRedeliveryIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	batch := []RequestLog{
		{TS: time.Now(), NodeID: "n", RequestID: "r1", Method: "GET", Path: "/a", Status: 200, LogID: "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"},
		{TS: time.Now(), NodeID: "n", RequestID: "r2", Method: "GET", Path: "/a", Status: 200, LogID: "7f9619ff-8b86-4d01-b42d-00cf4fc964ff"},
		{TS: time.Now(), NodeID: "n", RequestID: "legacy", Method: "GET", Path: "/a", Status: 200}, // no event ID (older writer)
	}
	for i := 0; i < 3; i++ { // delivered three times: original plus two replays
		if err := s.InsertLogs(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	var withID, withoutID int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE log_id IS NOT NULL), count(*) FILTER (WHERE log_id IS NULL) FROM request_logs`).Scan(&withID, &withoutID); err != nil {
		t.Fatal(err)
	}
	if withID != 2 {
		t.Fatalf("rows with event IDs = %d, want 2 (redelivery must not duplicate)", withID)
	}
	if withoutID != 3 {
		t.Fatalf("rows without event IDs = %d, want 3 (legacy rows are not deduplicated)", withoutID)
	}
}
