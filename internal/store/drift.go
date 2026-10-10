package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigDrift reports whether the apis/plans tables differ from the newest
// published (active or canary) revision. That happens when someone edits the
// tables directly in SQL instead of going through the admin API or apply.
type ConfigDrift struct {
	Drifted        bool     `json:"drifted"`
	Revision       int64    `json:"revision"`
	RevisionStatus string   `json:"revision_status"`
	ChangedAPIs    []string `json:"changed_apis,omitempty"`
	ChangedPlans   []string `json:"changed_plans,omitempty"`
}

// apiFingerprint and planFingerprint omit columns added after a revision was
// written (GraphQL/gRPC blobs, timestamps) so an upgrade is not reported as drift.
type apiFingerprint struct {
	ID, TenantID, Name, Description, BasePath, UpstreamURL, AuthType string
	StripPath                                                        bool
	RateLimitPerMinute, QuotaPerDay, QuotaPerMonth, TimeoutMS        int
	CORSEnabled, IsAI, RequireApproval, IsDraft, Enabled             bool
	Visibility, QuotaFailurePolicy, Protocol                         string
	RequestHeaders                                                   map[string]string
	TrafficPolicy                                                    TrafficPolicy
}

type planFingerprint struct {
	ID, TenantID, Name, Description, Tier          string
	RateLimitPerMinute, QuotaPerDay, QuotaPerMonth int
	PriceMonthlyUSD                                float64
}

// DetectConfigDrift compares the live tables with the newest published revision.
func (s *Store) DetectConfigDrift(ctx context.Context) (ConfigDrift, error) {
	var d ConfigDrift
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT revision, status, snapshot_data FROM config_revisions
			WHERE status IN ('active', 'canary') ORDER BY revision DESC LIMIT 1`).Scan(&d.Revision, &d.RevisionStatus, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var snap struct {
			APIs  []API  `json:"apis"`
			Plans []Plan `json:"plans"`
		}
		if err := json.Unmarshal(raw, &snap); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `SELECT `+apiCols+` FROM apis`)
		if err != nil {
			return err
		}
		var apis []API
		for rows.Next() {
			a, err := scanAPI(rows)
			if err != nil {
				rows.Close()
				return err
			}
			apis = append(apis, a)
		}
		rows.Close()
		rows, err = tx.Query(ctx, `SELECT `+planCols+` FROM plans`)
		if err != nil {
			return err
		}
		var plans []Plan
		for rows.Next() {
			p, err := scanPlan(rows)
			if err != nil {
				rows.Close()
				return err
			}
			plans = append(plans, p)
		}
		rows.Close()

		d.ChangedAPIs = diffByID(fingerprintAPIs(snap.APIs), fingerprintAPIs(apis))
		d.ChangedPlans = diffByID(fingerprintPlans(snap.Plans), fingerprintPlans(plans))
		d.Drifted = len(d.ChangedAPIs)+len(d.ChangedPlans) > 0
		return nil
	})
	return d, err
}

// fingerprintAPIs maps API ID -> (name, canonical JSON without timestamps).
func fingerprintAPIs(apis []API) map[string][2]string {
	out := make(map[string][2]string, len(apis))
	for _, a := range apis {
		a.CreatedAt, a.UpdatedAt = time.Time{}, time.Time{}
		// Revisions written before a field existed lack it: compare with the
		// value the upgrade gave existing rows, so an upgrade is not drift.
		a.TenantID = TenantOrDefault(a.TenantID)
		a.TrafficPolicy.Normalize()
		a.RequestHeaders = nonNilHeaders(a.RequestHeaders)
		a.OpenAPISpec = nonNilSpec(a.OpenAPISpec)
		a.Visibility = visibilityOrDefault(a.Visibility)
		a.QuotaFailurePolicy = quotaPolicyOrDefault(a.QuotaFailurePolicy)
		a.Protocol = protocolOrDefault(a.Protocol)
		// Revisions written before protocol/AI columns existed omit them. Compare
		// against the defaults migrations give existing rows so an upgrade is not drift.
		if len(a.GRPCDescriptorSet) == 0 {
			a.GRPCDescriptorSet = nil
		}
		raw, _ := json.Marshal(apiFingerprint{
			ID: a.ID, TenantID: a.TenantID, Name: a.Name, Description: a.Description,
			BasePath: a.BasePath, UpstreamURL: a.UpstreamURL, StripPath: a.StripPath,
			AuthType: a.AuthType, RateLimitPerMinute: a.RateLimitPerMinute,
			QuotaPerDay: a.QuotaPerDay, QuotaPerMonth: a.QuotaPerMonth, TimeoutMS: a.TimeoutMS,
			CORSEnabled: a.CORSEnabled, RequestHeaders: a.RequestHeaders, IsAI: a.IsAI,
			Visibility: a.Visibility, RequireApproval: a.RequireApproval, IsDraft: a.IsDraft,
			QuotaFailurePolicy: a.QuotaFailurePolicy, TrafficPolicy: a.TrafficPolicy,
			Protocol: a.Protocol, Enabled: a.Enabled,
		})
		out[a.ID] = [2]string{a.Name, string(raw)}
	}
	return out
}

func fingerprintPlans(plans []Plan) map[string][2]string {
	out := make(map[string][2]string, len(plans))
	for _, p := range plans {
		p.CreatedAt, p.UpdatedAt = time.Time{}, time.Time{}
		p.TenantID = TenantOrDefault(p.TenantID)
		p.Tier = tierOrDefault(p.Tier)
		raw, _ := json.Marshal(planFingerprint{
			ID: p.ID, TenantID: p.TenantID, Name: p.Name, Description: p.Description,
			RateLimitPerMinute: p.RateLimitPerMinute, QuotaPerDay: p.QuotaPerDay,
			QuotaPerMonth: p.QuotaPerMonth, PriceMonthlyUSD: p.PriceMonthlyUSD, Tier: p.Tier,
		})
		out[p.ID] = [2]string{p.Name, string(raw)}
	}
	return out
}

func tierOrDefault(tier string) string {
	if tier == "" {
		return "free"
	}
	return tier
}

// diffByID returns the names of entries added, removed or changed.
func diffByID(before, after map[string][2]string) []string {
	var names []string
	for id, a := range after {
		if b, ok := before[id]; !ok || b[1] != a[1] {
			names = append(names, a[0])
		}
	}
	for id, b := range before {
		if _, ok := after[id]; !ok {
			names = append(names, b[0])
		}
	}
	sort.Strings(names)
	return names
}
