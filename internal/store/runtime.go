package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Gateway snapshot data (everything the data plane needs, in one consistent read)
//
// Revision isolation contract
//
// Versioned with each config revision (what canaries and rollbacks change):
//   - API definitions: routing, auth settings, API-level limits, traffic policy.
//   - Plan definitions: per-plan rate limits and quotas. A node serving revision N
//     enforces the plan limits stored in revision N, even if the plans table has
//     since been edited for a newer (canary) revision.
//
// Always live (security-sensitive access state, effective everywhere immediately):
//   - API keys and their revocation / rotation grace periods.
//   - Consumer status (suspension).
//   - Subscription grants: which consumer may call which API, approval status,
//     enabled flag, and which plan they are assigned to.
//
// A revoked key must stop working on every node at once; it must never wait for
// a canary to be promoted or be resurrected by a stale revision.
// ---------------------------------------------------------------------------

type KeyRecord struct {
	KeyHash        string
	KeyID          string
	ConsumerID     string
	ConsumerName   string
	ConsumerStatus string
}

type SubRecord struct {
	ConsumerID         string
	APIID              string
	PlanID             string
	PlanName           string
	RateLimitPerMinute *int   // nil when no plan attached
	QuotaPerDay        *int   // nil when no plan attached
	QuotaPerMonth      *int   // nil when no plan attached
	Status             string // pending, approved, rejected
	Active             bool
	LimitsSource       string // "revision" when limits come from the revision's plans, "live" otherwise
}

// CanaryData is a traffic-split canary revision served alongside the stable one.
type CanaryData struct {
	Revision       int64
	TrafficPercent int    // 0-100 share of requests routed to the canary
	Header         string // optional routing header name; requests carrying it go to the canary
	HeaderValue    string // optional required header value ("" = any value)
	APIs           []API
	Subs           []SubRecord
}

type SnapshotData struct {
	Revision     int64
	TargetGroup  string
	PolicySource string // "revision" or "live" (legacy revisions without plan snapshots)
	APIs         []API
	Keys         []KeyRecord
	Subs         []SubRecord
	// MCPCatalog is the live MCP review state: definitions awaiting or
	// refused approval (blocked on every node) and approved tool schemas.
	MCPCatalog []MCPCatalogRecord `json:",omitempty"`
	Canary     *CanaryData        `json:",omitempty"`
}

func (s *Store) LoadSnapshotData(ctx context.Context) (SnapshotData, error) {
	return s.LoadSnapshotDataForNode(ctx, "default", false)
}

type revisionRow struct {
	Revision       int64
	TargetGroup    string
	Status         string
	TrafficPercent int
	Header         string
	HeaderValue    string
	Raw            []byte
}

const revisionRowCols = `revision, target_group, status, traffic_percent, canary_header, canary_header_value, snapshot_data`

func scanRevisionRow(row pgx.Row) (revisionRow, error) {
	var r revisionRow
	err := row.Scan(&r.Revision, &r.TargetGroup, &r.Status, &r.TrafficPercent, &r.Header, &r.HeaderValue, &r.Raw)
	return r, err
}

// revisionContent is the part of a revision snapshot the data plane serves.
type revisionContent struct {
	APIs     []API
	Plans    map[string]Plan // plan ID -> plan
	HasAPIs  bool
	HasPlans bool
}

func parseRevisionContent(raw []byte) revisionContent {
	var c revisionContent
	if len(raw) == 0 {
		return c
	}
	var snapObj struct {
		APIs  []API  `json:"apis"`
		Plans []Plan `json:"plans"`
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return c
	}
	if json.Unmarshal(raw, &snapObj) != nil {
		return c
	}
	if len(snapObj.APIs) > 0 {
		c.HasAPIs = true
		for _, a := range snapObj.APIs {
			if a.Enabled && !a.IsDraft {
				c.APIs = append(c.APIs, a)
			}
		}
	}
	if _, ok := probe["plans"]; ok {
		c.HasPlans = true
		c.Plans = make(map[string]Plan, len(snapObj.Plans))
		for _, p := range snapObj.Plans {
			c.Plans[p.ID] = p
		}
	}
	return c
}

// ResolveSubLimits applies a revision's plan definitions to live subscription grants.
// Subscriptions whose plan is unknown to the revision (created later) keep live limits.
func ResolveSubLimits(live []SubRecord, plans map[string]Plan, hasPlans bool) []SubRecord {
	out := make([]SubRecord, len(live))
	for i, sub := range live {
		sub.LimitsSource = "live"
		if hasPlans && sub.PlanID != "" {
			if p, ok := plans[sub.PlanID]; ok {
				rl, qd, qm := p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth
				sub.PlanName = p.Name
				sub.RateLimitPerMinute, sub.QuotaPerDay, sub.QuotaPerMonth = &rl, &qd, &qm
				sub.LimitsSource = "revision"
			}
		}
		out[i] = sub
	}
	return out
}

// LoadSnapshotDataForNode loads, in one repeatable-read transaction, the stable
// revision this node should serve, an optional traffic-split canary revision, and
// the live access state (keys, consumer status, subscription grants).
//
// Revision selection:
//   - stable: newest status='active' revision targeted at 'all' or this node group.
//   - canary: newest status='canary' revision newer than stable.
//   - Canary nodes (RELAYOPS_CANARY / group "canary") serve the canary revision for
//     100% of their traffic, as before.
//   - Other nodes serve it for traffic_percent of requests and/or requests carrying
//     the canary routing header. A node-group-only canary (no split) is ignored.
func (s *Store) LoadSnapshotDataForNode(ctx context.Context, nodeGroup string, isCanary bool) (SnapshotData, error) {
	if nodeGroup == "" {
		nodeGroup = "default"
	}
	canaryNode := isCanary || nodeGroup == "canary"
	var d SnapshotData
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		stable, stableErr := scanRevisionRow(tx.QueryRow(ctx, `SELECT `+revisionRowCols+` FROM config_revisions
			WHERE status = 'active' AND (target_group = 'all' OR target_group = $1)
			ORDER BY revision DESC LIMIT 1`, nodeGroup))
		if stableErr != nil && !errors.Is(stableErr, pgx.ErrNoRows) {
			return stableErr
		}
		hasStable := stableErr == nil

		canary, canaryErr := scanRevisionRow(tx.QueryRow(ctx, `SELECT `+revisionRowCols+` FROM config_revisions
			WHERE status = 'canary' AND revision > $1
			ORDER BY revision DESC LIMIT 1`, stable.Revision))
		if canaryErr != nil && !errors.Is(canaryErr, pgx.ErrNoRows) {
			return canaryErr
		}
		hasCanary := canaryErr == nil

		primary := stable
		if hasCanary && canaryNode && (canary.TargetGroup == "canary" || canary.TargetGroup == "all" || canary.TargetGroup == nodeGroup) {
			primary = canary
			hasCanary = false // the whole node serves it; no split on top
		} else if hasCanary && canary.TrafficPercent <= 0 && canary.Header == "" {
			hasCanary = false // node-group-only canary: not served by standard nodes
		}

		if hasStable || primary.Revision > 0 {
			d.Revision = primary.Revision
			d.TargetGroup = primary.TargetGroup
		} else {
			d.Revision = 1
			d.TargetGroup = "all"
		}

		content := parseRevisionContent(primary.Raw)
		if content.HasAPIs {
			d.APIs = content.APIs
		} else {
			// Legacy or empty revision: serve enabled, published APIs from the live table.
			rows, err := tx.Query(ctx, `SELECT `+apiCols+` FROM apis WHERE enabled AND NOT is_draft`)
			if err != nil {
				return err
			}
			for rows.Next() {
				a, err := scanAPI(rows)
				if err != nil {
					rows.Close()
					return err
				}
				d.APIs = append(d.APIs, a)
			}
			rows.Close()
		}

		// Live access state: active keys (or secondary keys within their rotation
		// grace period) belonging to active, non-suspended consumers.
		rows, err := tx.Query(ctx, `SELECT k.key_hash, k.id, c.id, c.name, c.status
			FROM api_keys k JOIN consumers c ON c.id = k.consumer_id
			WHERE (k.active OR (k.grace_until IS NOT NULL AND k.grace_until > now())) AND c.status = 'active'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k KeyRecord
			if err := rows.Scan(&k.KeyHash, &k.KeyID, &k.ConsumerID, &k.ConsumerName, &k.ConsumerStatus); err != nil {
				rows.Close()
				return err
			}
			d.Keys = append(d.Keys, k)
		}
		rows.Close()

		// Live subscription grants, with live plan limits as the fallback.
		rows, err = tx.Query(ctx, `SELECT s.consumer_id, s.api_id, COALESCE(s.plan_id::text, ''), COALESCE(p.name,''),
			p.rate_limit_per_minute, p.quota_per_day, p.quota_per_month, s.status, s.active
			FROM subscriptions s LEFT JOIN plans p ON p.id = s.plan_id`)
		if err != nil {
			return err
		}
		var liveSubs []SubRecord
		for rows.Next() {
			var x SubRecord
			if err := rows.Scan(&x.ConsumerID, &x.APIID, &x.PlanID, &x.PlanName, &x.RateLimitPerMinute,
				&x.QuotaPerDay, &x.QuotaPerMonth, &x.Status, &x.Active); err != nil {
				rows.Close()
				return err
			}
			liveSubs = append(liveSubs, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Live MCP catalog state (ordered, so the config fingerprint is stable).
		rows, err = tx.Query(ctx, `SELECT api_id, kind, name, fingerprint, status,
				CASE WHEN status = 'approved' AND kind = 'tool' THEN definition->'inputSchema' END
			FROM mcp_catalog WHERE status IN ('pending', 'rejected', 'approved')
			ORDER BY api_id, kind, name, fingerprint`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m MCPCatalogRecord
			var schema []byte
			if err := rows.Scan(&m.APIID, &m.Kind, &m.Name, &m.Fingerprint, &m.Status, &schema); err != nil {
				rows.Close()
				return err
			}
			if len(schema) > 0 && string(schema) != "null" {
				m.InputSchema = schema
			}
			d.MCPCatalog = append(d.MCPCatalog, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		d.Subs = ResolveSubLimits(liveSubs, content.Plans, content.HasPlans)
		d.PolicySource = "live"
		if content.HasPlans {
			d.PolicySource = "revision"
		}

		if hasCanary {
			cc := parseRevisionContent(canary.Raw)
			cd := &CanaryData{
				Revision:       canary.Revision,
				TrafficPercent: canary.TrafficPercent,
				Header:         canary.Header,
				HeaderValue:    canary.HeaderValue,
				APIs:           cc.APIs,
				Subs:           ResolveSubLimits(liveSubs, cc.Plans, cc.HasPlans),
			}
			if cc.HasAPIs {
				d.Canary = cd
			}
		}
		return nil
	})
	return d, err
}

// ---------------------------------------------------------------------------
// Request logs
// ---------------------------------------------------------------------------

type RequestLog struct {
	ID                 int64          `json:"id"`
	TS                 time.Time      `json:"ts"`
	NodeID             string         `json:"node_id"`
	RequestID          string         `json:"request_id"`
	APIID              *string        `json:"api_id"`
	APIName            string         `json:"api_name"`
	ConsumerID         *string        `json:"consumer_id"`
	ConsumerName       string         `json:"consumer_name"`
	Method             string         `json:"method"`
	Path               string         `json:"path"`
	Status             int            `json:"status"`
	LatencyMS          float64        `json:"latency_ms"`
	BytesOut           int64          `json:"bytes_out"`
	ClientIP           string         `json:"client_ip"`
	Error              string         `json:"error"`
	Model              string         `json:"model,omitempty"`
	TokensPrompt       int            `json:"tokens_prompt,omitempty"`
	TokensCompletion   int            `json:"tokens_completion,omitempty"`
	TokensTotal        int            `json:"tokens_total,omitempty"`
	DecisionReason     string         `json:"decision_reason,omitempty"`
	AuthStatus         string         `json:"auth_status,omitempty"`
	SubscriptionStatus string         `json:"subscription_status,omitempty"`
	RateLimitStatus    string         `json:"rate_limit_status,omitempty"`
	UpstreamDurationMS float64        `json:"upstream_duration_ms,omitempty"`
	ConfigRevision     int64          `json:"config_revision"`
	MatchedRoute       string         `json:"matched_route,omitempty"`
	PolicyEvaluations  map[string]any `json:"policy_evaluations,omitempty"`
	TraceID            string         `json:"trace_id,omitempty"`
	LogID              string         `json:"log_id,omitempty"` // unique event ID; makes redelivery idempotent
	TenantID           string         `json:"tenant_id,omitempty"`
	// SampleWeight is how many requests this row stands for when successful
	// requests are sampled; 0 is stored as 1.
	SampleWeight float64 `json:"sample_weight,omitempty"`
}

var logColumns = []string{
	"ts", "node_id", "request_id", "api_id", "api_name", "consumer_id", "consumer_name",
	"method", "path", "status", "latency_ms", "bytes_out", "client_ip", "error",
	"model", "tokens_prompt", "tokens_completion", "tokens_total",
	"decision_reason", "auth_status", "subscription_status", "rate_limit_status", "upstream_duration_ms",
	"config_revision", "matched_route", "policy_evaluations", "trace_id", "log_id", "tenant_id", "sample_weight",
}

// InsertLogs bulk-loads request logs. Rows are COPYed into a temporary table and
// moved with ON CONFLICT DO NOTHING on log_id, so redelivering a batch (for
// example a spool replay whose acknowledgement was lost) never duplicates rows.
func (s *Store) InsertLogs(ctx context.Context, logs []RequestLog) error {
	if len(logs) == 0 {
		return nil
	}
	cols := strings.Join(logColumns, ", ")
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE request_logs_in (LIKE request_logs INCLUDING DEFAULTS) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"request_logs_in"}, logColumns,
			pgx.CopyFromSlice(len(logs), func(i int) ([]any, error) {
				l := logs[i]
				policyJSON, _ := json.Marshal(l.PolicyEvaluations)
				if l.ConfigRevision <= 0 {
					l.ConfigRevision = 1
				}
				var logID any
				if l.LogID != "" {
					logID = l.LogID
				}
				weight := l.SampleWeight
				if weight <= 0 {
					weight = 1
				}
				return []any{
					l.TS, l.NodeID, l.RequestID, l.APIID, l.APIName, l.ConsumerID, l.ConsumerName,
					l.Method, l.Path, l.Status, l.LatencyMS, l.BytesOut, l.ClientIP, l.Error,
					l.Model, l.TokensPrompt, l.TokensCompletion, l.TokensTotal,
					l.DecisionReason, l.AuthStatus, l.SubscriptionStatus, l.RateLimitStatus, l.UpstreamDurationMS,
					l.ConfigRevision, l.MatchedRoute, policyJSON, l.TraceID, logID, nullableUUID(l.TenantID), float32(weight),
				}, nil
			})); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO request_logs (`+cols+`) SELECT `+cols+` FROM request_logs_in
			ON CONFLICT (log_id, ts) DO NOTHING`)
		return err
	})
}

type LogFilter struct {
	TenantIDs  []string // nil = all tenants
	APIID      string
	ConsumerID string
	StatusMin  int
	StatusMax  int
	Search     string
	Limit      int
}

func (s *Store) QueryLogs(ctx context.Context, f LogFilter) ([]RequestLog, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.TenantIDs != nil {
		add("tenant_id::text = ANY(?)", f.TenantIDs)
	}
	if f.APIID != "" {
		add("api_id = ?", f.APIID)
	}
	if f.ConsumerID != "" {
		add("consumer_id = ?", f.ConsumerID)
	}
	if f.StatusMin > 0 {
		add("status >= ?", f.StatusMin)
	}
	if f.StatusMax > 0 {
		add("status <= ?", f.StatusMax)
	}
	if f.Search != "" {
		args = append(args, "%"+f.Search+"%", f.Search)
		like, exact := "$"+strconv.Itoa(len(args)-1), "$"+strconv.Itoa(len(args))
		where = append(where, "(path ILIKE "+like+" OR error ILIKE "+like+" OR model ILIKE "+like+" OR decision_reason ILIKE "+like+" OR request_id = "+exact+" OR trace_id = "+exact+")")
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	q := `SELECT id, ts, node_id, request_id, api_id, api_name, consumer_id, consumer_name, method, path,
		status, latency_ms, bytes_out, client_ip, error, model, tokens_prompt, tokens_completion, tokens_total,
		decision_reason, auth_status, subscription_status, rate_limit_status, upstream_duration_ms,
		config_revision, matched_route, policy_evaluations, trace_id, COALESCE(tenant_id::text, '') FROM request_logs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ts DESC LIMIT " + strconv.Itoa(f.Limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []RequestLog{}
	for rows.Next() {
		var l RequestLog
		var policyJSON []byte
		if err := rows.Scan(&l.ID, &l.TS, &l.NodeID, &l.RequestID, &l.APIID, &l.APIName, &l.ConsumerID,
			&l.ConsumerName, &l.Method, &l.Path, &l.Status, &l.LatencyMS, &l.BytesOut, &l.ClientIP, &l.Error,
			&l.Model, &l.TokensPrompt, &l.TokensCompletion, &l.TokensTotal,
			&l.DecisionReason, &l.AuthStatus, &l.SubscriptionStatus, &l.RateLimitStatus, &l.UpstreamDurationMS,
			&l.ConfigRevision, &l.MatchedRoute, &policyJSON, &l.TraceID, &l.TenantID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(policyJSON, &l.PolicyEvaluations)
		out = append(out, l)
	}
	return out, rows.Err()
}

var logPartitionName = regexp.MustCompile(`^request_logs_p(\d{8})$`)

// EnsureLogPartitions creates the daily request_logs partitions from today
// (UTC) through daysAhead days ahead. Safe to run concurrently on every node.
func (s *Store) EnsureLogPartitions(ctx context.Context, daysAhead int) error {
	_, err := s.Pool.Exec(ctx, `SELECT relayops_ensure_log_partitions((now() AT TIME ZONE 'UTC')::date,
		(now() AT TIME ZONE 'UTC')::date + $1::int)`, daysAhead)
	return err
}

// PurgeLogs removes request logs older than olderThan. Daily partitions that
// end before the cutoff are dropped whole (no row-by-row DELETE, no bloat);
// older rows left in the boundary or default partition are deleted. It also
// keeps partitions created a few days ahead, and returns the rows removed.
func (s *Store) PurgeLogs(ctx context.Context, olderThan time.Duration) (int64, error) {
	if err := s.EnsureLogPartitions(ctx, 3); err != nil {
		slog.Warn("could not create request log partitions ahead of time", "err", err)
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := s.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'request_logs'`)
	if err != nil {
		return 0, err
	}
	var drop []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		m := logPartitionName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		day, err := time.Parse("20060102", m[1])
		if err == nil && !day.AddDate(0, 0, 1).After(cutoff) {
			drop = append(drop, name)
		}
	}
	rows.Close()
	var removed int64
	for _, name := range drop {
		ident := pgx.Identifier{name}.Sanitize()
		var n int64
		_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM `+ident).Scan(&n)
		if _, err := s.Pool.Exec(ctx, `DROP TABLE IF EXISTS `+ident); err != nil {
			return removed, err
		}
		removed += n
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM request_logs WHERE ts < $1`, cutoff)
	return removed + tag.RowsAffected(), err
}

// Vacuum executes VACUUM ANALYZE on specified tables to reclaim disk space and update query planner statistics.
func (s *Store) Vacuum(ctx context.Context, tables ...string) error {
	for _, t := range tables {
		switch t {
		case "request_logs", "test_run_steps", "test_run_artifacts", "test_jobs", "test_runs", "admin_sessions", "audit_logs":
			if _, err := s.Pool.Exec(ctx, "VACUUM ANALYZE "+t); err != nil {
				return fmt.Errorf("vacuum %s: %w", t, err)
			}
		default:
			return fmt.Errorf("table %q not eligible for vacuum", t)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Analytics
// ---------------------------------------------------------------------------

type Summary struct {
	Window       string           `json:"window"`
	Total        int64            `json:"total"`
	Errors4xx    int64            `json:"errors_4xx"`
	Errors5xx    int64            `json:"errors_5xx"`
	AvgLatency   float64          `json:"avg_latency_ms"`
	P50Latency   float64          `json:"p50_latency_ms"`
	P95Latency   float64          `json:"p95_latency_ms"`
	P99Latency   float64          `json:"p99_latency_ms"`
	BytesOut     int64            `json:"bytes_out"`
	TotalTokens  int64            `json:"total_tokens"`
	TopAPIs      []TopItem        `json:"top_apis"`
	TopConsumers []TopItem        `json:"top_consumers"`
	TopModels    []TopItem        `json:"top_models"`
	ByStatus     map[string]int64 `json:"by_status"`
	// Series has 60 equal buckets across the window (empty buckets are 0).
	Series []SeriesPoint `json:"series"`
}

// SeriesPoint is one time bucket of traffic.
type SeriesPoint struct {
	Bucket     time.Time `json:"bucket"`
	Count      int64     `json:"count"`
	Errors     int64     `json:"errors"` // 5xx
	AvgLatency float64   `json:"avg_latency_ms"`
}

type TopItem struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Count      int64   `json:"count"`
	Errors     int64   `json:"errors"` // 5xx
	AvgLatency float64 `json:"avg_latency_ms"`
	Tokens     int64   `json:"tokens"`
}

func (s *Store) Summarize(ctx context.Context, window time.Duration, label string) (Summary, error) {
	return s.SummarizeScoped(ctx, window, label, nil)
}

// SummarizeScoped summarises traffic; tenantIDs (when non-nil) restricts it to those tenants.
func (s *Store) SummarizeScoped(ctx context.Context, window time.Duration, label string, tenantIDs []string) (Summary, error) {
	var sum Summary
	sum.Window = label
	sum.ByStatus = make(map[string]int64)

	secs := window.Seconds()

	err := s.Pool.QueryRow(ctx, `
		SELECT
			COALESCE(round(sum(sample_weight)), 0)::bigint,
			COALESCE(round(sum(sample_weight) FILTER (WHERE status >= 400 AND status < 500)), 0)::bigint,
			COALESCE(round(sum(sample_weight) FILTER (WHERE status >= 500)), 0)::bigint,
			COALESCE(sum(latency_ms * sample_weight) / NULLIF(sum(sample_weight), 0), 0),
			-- percentiles are over persisted rows; with sampling, errors are over-represented
			COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms), 0),
			COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0),
			COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms), 0),
			COALESCE(round(sum(bytes_out * sample_weight)), 0)::bigint,
			COALESCE(round(sum(tokens_total * sample_weight)), 0)::bigint
		FROM request_logs
		WHERE ts >= now() - make_interval(secs => $1) AND ($2::text[] IS NULL OR tenant_id::text = ANY($2))
	`, secs, tenantIDs).Scan(&sum.Total, &sum.Errors4xx, &sum.Errors5xx,
		&sum.AvgLatency, &sum.P50Latency, &sum.P95Latency, &sum.P99Latency, &sum.BytesOut, &sum.TotalTokens)
	if err != nil {
		return sum, err
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT status / 100 || 'xx', round(sum(sample_weight))::bigint
		FROM request_logs
		WHERE ts >= now() - make_interval(secs => $1) AND ($2::text[] IS NULL OR tenant_id::text = ANY($2))
		GROUP BY 1`, secs, tenantIDs)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var bucket string
			var count int64
			if rows.Scan(&bucket, &count) == nil {
				sum.ByStatus[bucket] = count
			}
		}
	}

	sum.Series = s.series(ctx, window, tenantIDs)
	sum.TopAPIs = s.top(ctx, secs, "api_id", "api_name", tenantIDs)
	sum.TopConsumers = s.top(ctx, secs, "consumer_id", "consumer_name", tenantIDs)
	sum.TopModels = s.top(ctx, secs, "model", "model", tenantIDs)

	return sum, nil
}

func (s *Store) top(ctx context.Context, secs float64, idCol, nameCol string, tenantIDs []string) []TopItem {
	return s.topN(ctx, secs, idCol, nameCol, tenantIDs, 5)
}

// APIStats is per-API traffic over a window for every API with traffic (up to
// 200), busiest first. Orbit signals compare it across windows.
// RateLimitExposure says what a per-client rate limit on an API would have
// done to its recent traffic: the busiest single minute any one client
// (consumer, or client IP for anonymous callers) sent, how many requests over
// the limit would have been refused, and from how many clients.
type RateLimitExposure struct {
	PeakPerClientMinute int64 `json:"peak_per_client_minute"`
	WouldRefuse         int64 `json:"would_refuse"`
	ClientsAffected     int64 `json:"clients_affected"`
}

func (s *Store) RateLimitExposure(ctx context.Context, apiID string, limit int, d time.Duration) (RateLimitExposure, error) {
	var out RateLimitExposure
	err := s.Pool.QueryRow(ctx, `WITH per AS (
			SELECT date_trunc('minute', ts) AS m, COALESCE(consumer_id::text, NULLIF(client_ip, ''), 'anonymous') AS k,
			       sum(sample_weight) AS c
			FROM request_logs
			WHERE api_id::text = $1 AND ts >= now() - make_interval(secs => $2)
			GROUP BY 1, 2)
		SELECT COALESCE(round(max(c)), 0)::bigint,
		       COALESCE(round(sum(GREATEST(c - $3, 0))), 0)::bigint,
		       count(DISTINCT k) FILTER (WHERE c > $3)
		FROM per`, apiID, d.Seconds(), limit).Scan(&out.PeakPerClientMinute, &out.WouldRefuse, &out.ClientsAffected)
	return out, err
}

func (s *Store) APIStats(ctx context.Context, d time.Duration, tenantIDs []string) []TopItem {
	return s.topN(ctx, d.Seconds(), "api_id", "api_name", tenantIDs, 200)
}

func (s *Store) topN(ctx context.Context, secs float64, idCol, nameCol string, tenantIDs []string, limit int) []TopItem {
	q := `SELECT coalesce(` + idCol + `::text, ''), ` + nameCol + `, round(sum(sample_weight))::bigint,
			COALESCE(round(sum(sample_weight) FILTER (WHERE status >= 500)), 0)::bigint,
			COALESCE(sum(latency_ms * sample_weight) / NULLIF(sum(sample_weight), 0), 0),
			COALESCE(round(sum(tokens_total * sample_weight)), 0)::bigint
		FROM request_logs
		WHERE ts >= now() - make_interval(secs => $1) AND ` + nameCol + ` <> ''
		  AND ($2::text[] IS NULL OR tenant_id::text = ANY($2))
		GROUP BY 1, 2 ORDER BY 3 DESC LIMIT $3`
	rows, err := s.Pool.Query(ctx, q, secs, tenantIDs, limit)
	if err != nil {
		return []TopItem{}
	}
	defer rows.Close()
	out := make([]TopItem, 0)
	for rows.Next() {
		var it TopItem
		if rows.Scan(&it.ID, &it.Name, &it.Count, &it.Errors, &it.AvgLatency, &it.Tokens) == nil {
			out = append(out, it)
		}
	}
	return out
}

// series buckets the window into 60 points.
func (s *Store) series(ctx context.Context, window time.Duration, tenantIDs []string) []SeriesPoint {
	step := window / 60
	if step < 5*time.Second {
		step = 5 * time.Second
	}
	end := time.Now().UTC().Truncate(step).Add(step)
	start := end.Add(-60 * step)
	points := make([]SeriesPoint, 60)
	for i := range points {
		points[i].Bucket = start.Add(time.Duration(i) * step)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT date_bin(make_interval(secs => $3), ts, $4::timestamptz),
			round(sum(sample_weight))::bigint,
			COALESCE(round(sum(sample_weight) FILTER (WHERE status >= 500)), 0)::bigint,
			COALESCE(sum(latency_ms * sample_weight) / NULLIF(sum(sample_weight), 0), 0)
		FROM request_logs
		WHERE ts >= $4 AND ts < $1 AND ($2::text[] IS NULL OR tenant_id::text = ANY($2))
		GROUP BY 1`, end, tenantIDs, step.Seconds(), start)
	if err != nil {
		return points
	}
	defer rows.Close()
	for rows.Next() {
		var p SeriesPoint
		if rows.Scan(&p.Bucket, &p.Count, &p.Errors, &p.AvgLatency) != nil {
			continue
		}
		if i := int(p.Bucket.Sub(start) / step); i >= 0 && i < len(points) {
			points[i].Count, points[i].Errors, points[i].AvgLatency = p.Count, p.Errors, p.AvgLatency
		}
	}
	return points
}

type Counts struct {
	APIs          int `json:"apis"`
	EnabledAPIs   int `json:"enabled_apis"`
	Consumers     int `json:"consumers"`
	ActiveKeys    int `json:"active_keys"`
	Subscriptions int `json:"subscriptions"`
	Plans         int `json:"plans"`
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM apis), (SELECT count(*) FROM apis WHERE enabled),
		(SELECT count(*) FROM consumers), (SELECT count(*) FROM api_keys WHERE active),
		(SELECT count(*) FROM subscriptions), (SELECT count(*) FROM plans)`).
		Scan(&c.APIs, &c.EnabledAPIs, &c.Consumers, &c.ActiveKeys, &c.Subscriptions, &c.Plans)
	return c, err
}

func (s *Store) GetRequestLogByRequestID(ctx context.Context, requestID string) (RequestLog, error) {
	logs, err := s.QueryLogs(ctx, LogFilter{Search: requestID, Limit: 1})
	if err != nil {
		return RequestLog{}, err
	}
	if len(logs) == 0 {
		return RequestLog{}, ErrNotFound
	}
	return logs[0], nil
}
