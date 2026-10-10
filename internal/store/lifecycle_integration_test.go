package store

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/testdb"
)

// previousRelease is the last migration shipped before this release.
const previousRelease = "005_enterprise_session_and_oidc"

// TestIntegrationUpgradeFromPreviousRelease builds the previous release's schema,
// writes data the way that release did, upgrades, and checks that everything is
// still served, nothing is lost, and the upgrade's security fixes took effect.
func TestIntegrationUpgradeFromPreviousRelease(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MigrateTo(ctx, previousRelease); err != nil {
		t.Fatal(err)
	}

	// --- Data as the previous release wrote it ------------------------------
	var planID, apiID, consumerID, userID string
	mustQ := func(sql string, dst any, args ...any) {
		t.Helper()
		if err := s.Pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustQ(`INSERT INTO plans (name, rate_limit_per_minute) VALUES ('gold', 120) RETURNING id`, &planID)
	mustQ(`INSERT INTO apis (name, base_path, upstream_url, auth_type, timeout_ms) VALUES ('orders', '/orders', 'http://orders:8080', 'api_key', 5000) RETURNING id`, &apiID)
	mustQ(`INSERT INTO consumers (name) VALUES ('acme') RETURNING id`, &consumerID)
	exec1 := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec1(`INSERT INTO api_keys (consumer_id, key_prefix, key_hash) VALUES ($1, 'rk_old', $2)`, consumerID, HashKey("rk_old_key"))
	exec1(`INSERT INTO subscriptions (consumer_id, api_id, plan_id) VALUES ($1, $2, $3)`, consumerID, apiID, planID)
	// The previous release seeded revision 1 with an explicit ID; it published
	// revision snapshots without plans-aware limits or traffic policy.
	exec1(`INSERT INTO config_revisions (revision, created_by, description, status, target_group, snapshot_data)
		VALUES (2, 'admin', 'published by previous release', 'active', 'all',
		jsonb_build_object('apis', (SELECT jsonb_agg(to_jsonb(a)) FROM apis a), 'plans', (SELECT jsonb_agg(to_jsonb(p)) FROM plans p)))`)
	// An SSO-provisioned admin with the old shared default password, and a session.
	mustQ(`INSERT INTO admin_users (email, name, role, password_hash, token_hash, sso_provider)
		VALUES ('fed@corp.example', 'Fed', 'admin', $1, 'x', 'okta') RETURNING id`, &userID, HashKey("admin123"))
	exec1(`INSERT INTO admin_sessions (token_hash, user_id, expires_at) VALUES ('sess-hash', $1, now() + interval '1 day')`, userID)

	// --- Upgrade --------------------------------------------------------------
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}

	// Upgrading is not a configuration change: revisions written by the previous
	// release (without newer fields) must not look like drift.
	if d, err := s.DetectConfigDrift(ctx); err != nil || d.Drifted {
		t.Fatalf("upgrade reported drift: %+v err=%v", d, err)
	}

	// Served configuration and limits are intact.
	d, err := s.LoadSnapshotDataForNode(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := apiNamed(d.APIs, "orders"); d.Revision != 2 || !ok || o.TimeoutMS != 5000 {
		t.Fatalf("after upgrade: rev=%d orders=%+v found=%v", d.Revision, o, ok)
	}
	if got := subLimit(d.Subs, consumerID); got != 120 || len(d.Keys) != 1 {
		t.Fatalf("after upgrade: plan limit=%d keys=%d", got, len(d.Keys))
	}
	a, _ := s.GetAPI(ctx, apiID)
	a.TrafficPolicy.Normalize()
	if err := a.TrafficPolicy.Validate(); err != nil || len(a.TrafficPolicy.Targets) != 0 {
		t.Fatalf("existing API traffic policy after upgrade: %+v err=%v", a.TrafficPolicy, err)
	}

	// Publishing works (the revision sequence was behind the seeded rows).
	if _, rev, err := s.CreateAPIAtomic(ctx, API{Name: "new", BasePath: "/new", UpstreamURL: "http://n", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "first publish after upgrade"); err != nil || rev <= 2 {
		t.Fatalf("first publish after upgrade: rev=%d err=%v", rev, err)
	}

	// New tables and columns exist with safe defaults.
	cfg, err := s.GetAutoRollbackSettings(ctx)
	if err != nil || cfg.MinBaselineRequests != 20 || cfg.InsufficientBaselineAction != "absolute" {
		t.Fatalf("auto-rollback defaults after upgrade: %+v err=%v", cfg, err)
	}
	if err := s.InsertLogs(ctx, []RequestLog{{TS: time.Now(), NodeID: "n", RequestID: "r", Method: "GET", Path: "/", Status: 200, TraceID: "abc"}}); err != nil {
		t.Fatalf("request log with trace_id after upgrade: %v", err)
	}

	// Security fixes applied to existing data.
	var pw string
	var sessions int
	mustQ(`SELECT password_hash FROM admin_users WHERE id=$1`, &pw, userID)
	mustQ(`SELECT count(*) FROM admin_sessions WHERE user_id=$1`, &sessions, userID)
	if pw != "" || sessions != 0 {
		t.Fatalf("default password not removed: hash=%q sessions=%d", pw, sessions)
	}
	var seededToken string
	mustQ(`SELECT token_hash FROM admin_users WHERE email='admin@relayops.local'`, &seededToken)
	if seededToken == HashKey("relayops-admin") {
		t.Fatal("seeded admin still accepts the default token")
	}

	// Tenancy: existing data belongs to the default tenant, old request logs are
	// attributed to their API's tenant, and names are now unique per tenant.
	if a, _ := s.GetAPI(ctx, apiID); a.TenantID != DefaultTenantID {
		t.Fatalf("existing API tenant after upgrade = %q", a.TenantID)
	}
	if c, _ := s.GetConsumer(ctx, consumerID); c.TenantID != DefaultTenantID {
		t.Fatalf("existing consumer tenant after upgrade = %q", c.TenantID)
	}
	other, err := s.CreateTenant(ctx, "second", "Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlan(ctx, Plan{Name: "gold", RateLimitPerMinute: 1, TenantID: other.ID}); err != nil {
		t.Fatalf("same plan name in another tenant after upgrade: %v", err)
	}
	if _, err := s.CreatePlan(ctx, Plan{Name: "gold", RateLimitPerMinute: 1}); err == nil {
		t.Fatal("duplicate plan name within one tenant was accepted")
	}

	// Upgrading again is a no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-running migrations: %v", err)
	}
}

// TestIntegrationBackupAndRestore dumps a live database with pg_dump, restores it
// into a new database, and checks the restored control plane is equivalent.
func TestIntegrationBackupAndRestore(t *testing.T) {
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			if testdb.Required() {
				t.Fatalf("%s is required for the backup/restore test", tool)
			}
			t.Skipf("%s not found", tool)
		}
	}
	ctx := context.Background()
	srcDSN := testdb.New(t)
	src, err := Open(ctx, srcDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, _, _ := src.CreatePlanAtomic(ctx, Plan{Name: "gold", RateLimitPerMinute: 300}, "t", "plan")
	api, _, err := src.CreateAPIAtomic(ctx, API{Name: "orders", BasePath: "/orders", UpstreamURL: "http://o", StripPath: true,
		AuthType: "api_key", TimeoutMS: 1000, Enabled: true,
		TrafficPolicy: TrafficPolicy{Retries: &RetryPolicy{Attempts: 2}}}, "t", "api")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := src.CreateConsumer(ctx, Consumer{Name: "acme"})
	_, _ = src.CreateAPIKey(ctx, c.ID, "k")
	_, _ = src.CreateSubscription(ctx, c.ID, api.ID, &plan.ID)
	if _, err := src.AtomicPublishCanary(ctx, "t", "canary", CanarySplit{TrafficPercent: 15}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE plans SET rate_limit_per_minute=30 WHERE id=$1`, plan.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := src.LoadSnapshotDataForNode(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}

	dump := filepath.Join(t.TempDir(), "relayops.dump")
	if out, err := exec.Command("pg_dump", "-Fc", "-f", dump, "-d", srcDSN).CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
	if st, err := os.Stat(dump); err != nil || st.Size() == 0 {
		t.Fatalf("empty dump: %v", err)
	}

	name := testdb.Name()
	testdb.Create(t, testdb.ServerURL(t), name)
	dstDSN := replaceDB(t, srcDSN, name)
	if out, err := exec.Command("pg_restore", "--no-owner", "-d", dstDSN, dump).CombinedOutput(); err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, out)
	}
	dst, err := Open(ctx, dstDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.Migrate(ctx); err != nil {
		t.Fatalf("migrations on a restored database: %v", err)
	}

	after, err := dst.LoadSnapshotDataForNode(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || len(after.APIs) != len(before.APIs) || len(after.Keys) != len(before.Keys) ||
		after.Canary == nil || after.Canary.Revision != before.Canary.Revision || after.Canary.TrafficPercent != 15 {
		t.Fatalf("restored state differs:\nbefore rev=%d canary=%+v\nafter  rev=%d canary=%+v", before.Revision, before.Canary, after.Revision, after.Canary)
	}
	if subLimit(after.Subs, c.ID) != 300 || subLimit(after.Canary.Subs, c.ID) != 30 {
		t.Fatal("restored revisions must keep their own plan limits")
	}
	if o, ok := apiNamed(after.APIs, "orders"); !ok || o.TrafficPolicy.Retries == nil || o.TrafficPolicy.Retries.Attempts != 2 {
		t.Fatalf("traffic policy lost in restore: %+v", o.TrafficPolicy)
	}
	if d, _ := dst.DetectConfigDrift(ctx); d.Drifted {
		t.Fatalf("restore introduced drift: %+v", d)
	}
	// The restored control plane can operate: abort the canary, then publish.
	if _, err := dst.AbortCanary(ctx, before.Canary.Revision, "t"); err != nil {
		t.Fatal(err)
	}
	if _, rev, err := dst.CreateAPIAtomic(ctx, API{Name: "x", BasePath: "/x", UpstreamURL: "http://x", StripPath: true,
		AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "after restore"); err != nil || rev <= before.Canary.Revision {
		t.Fatalf("publish after restore: rev=%d err=%v", rev, err)
	}
}

func replaceDB(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func apiNamed(apis []API, name string) (API, bool) {
	for _, a := range apis {
		if a.Name == name {
			return a, true
		}
	}
	return API{}, false
}
