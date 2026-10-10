package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

type Plan struct {
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	RateLimitPerMinute int       `json:"rate_limit_per_minute"`
	QuotaPerDay        int       `json:"quota_per_day"`
	QuotaPerMonth      int       `json:"quota_per_month"`
	PriceMonthlyUSD    float64   `json:"price_monthly_usd"`
	Tier               string    `json:"tier"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type API struct {
	ID                 string            `json:"id"`
	TenantID           string            `json:"tenant_id"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	BasePath           string            `json:"base_path"`
	UpstreamURL        string            `json:"upstream_url"`
	StripPath          bool              `json:"strip_path"`
	AuthType           string            `json:"auth_type"` // none, api_key, jwt, oidc
	JWTSecret          string            `json:"jwt_secret,omitempty"`
	JWKSURL            string            `json:"jwks_url,omitempty"`
	OIDCIssuer         string            `json:"oidc_issuer,omitempty"`
	OIDCAudience       string            `json:"oidc_audience,omitempty"`
	RateLimitPerMinute int               `json:"rate_limit_per_minute"`
	QuotaPerDay        int               `json:"quota_per_day"`
	QuotaPerMonth      int               `json:"quota_per_month"`
	TimeoutMS          int               `json:"timeout_ms"`
	CORSEnabled        bool              `json:"cors_enabled"`
	RequestHeaders     map[string]string `json:"request_headers"`
	IsAI               bool              `json:"is_ai"`
	OpenAPISpec        map[string]any    `json:"openapi_spec,omitempty"`
	Visibility         string            `json:"visibility"`           // public, private, internal
	RequireApproval    bool              `json:"require_approval"`     // if true, subscriptions start as pending
	IsDraft            bool              `json:"is_draft"`             // if true, not loaded to data plane until published
	QuotaFailurePolicy string            `json:"quota_failure_policy"` // fail_open, fail_closed
	TrafficPolicy      TrafficPolicy     `json:"traffic_policy"`
	Protocol           string            `json:"protocol"` // http, graphql, grpc, mcp
	GraphQLSchema      string            `json:"graphql_schema,omitempty"`
	GraphQLPolicy      GraphQLPolicy     `json:"graphql_policy"`
	GRPCDescriptorSet  []byte            `json:"grpc_descriptor_set,omitempty"`
	GRPCPolicy         GRPCPolicy        `json:"grpc_policy"`
	MCPPolicy          MCPPolicy         `json:"mcp_policy"`
	Enabled            bool              `json:"enabled"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

type GraphQLPolicy struct {
	MaxDepth           int      `json:"max_depth"`                     // e.g. 10 (0 = unlimited)
	MaxCost            int      `json:"max_cost"`                      // e.g. 500 (0 = unlimited)
	AllowIntrospection bool     `json:"allow_introspection"`           // whether __schema queries are permitted
	AllowMutations     bool     `json:"allow_mutations"`               // if false, only queries allowed
	OperationAllowlist []string `json:"operation_allowlist,omitempty"` // operation names or "sha256:<hex>" query hashes
	MaxAliases         int      `json:"max_aliases,omitempty"`         // 0 = unlimited
	MaxBatchSize       int      `json:"max_batch_size,omitempty"`      // operations per batched request; 0 = batching refused
	// ValidateAgainstSchema checks every request against graphql_schema with
	// the specification's validation rules (unknown fields, wrong argument
	// types, ...) before it reaches the upstream.
	ValidateAgainstSchema bool `json:"validate_against_schema,omitempty"`
	// ListSizeArguments name the arguments that multiply a field's cost
	// (default first, last, limit, pageSize).
	ListSizeArguments []string `json:"list_size_arguments,omitempty"`
}

type GRPCPolicy struct {
	AllowReflection     bool `json:"allow_reflection"`
	MaxMessageSizeBytes int  `json:"max_message_size_bytes"` // per message, both directions; 0 = default 4 MiB
	// DefaultAction applies when no rule matches: "allow" (default) or "deny".
	DefaultAction string `json:"default_action,omitempty"`
	// Rules decide which methods each caller may call; the first match wins.
	Rules []GRPCMethodRule `json:"rules,omitempty"`
	// JSONTranscoding lets HTTP clients call unary methods with JSON
	// (POST /package.Service/Method); needs grpc_descriptor_set.
	JSONTranscoding bool `json:"json_transcoding,omitempty"`
}

// GRPCMethodRule allows or denies methods ("package.Service/Method", globs
// allowed: "package.Service/*") for a set of callers. Empty consumers or
// plans match any caller.
type GRPCMethodRule struct {
	Methods   []string `json:"methods"`
	Consumers []string `json:"consumers,omitempty"`
	Plans     []string `json:"plans,omitempty"`
	Action    string   `json:"action"`
}

type StreamService struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	ListenPort       int       `json:"listen_port"`
	TargetAddresses  []string  `json:"target_addresses"`
	TLSMode          string    `json:"tls_mode"` // none, terminate, passthrough
	MaxConnections   int       `json:"max_connections"`
	ConnectTimeoutMS int       `json:"connect_timeout_ms"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Consumer struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	Name           string    `json:"name"`
	Email          string    `json:"email"`
	Status         string    `json:"status"` // active, pending_approval, suspended
	RegistrationIP string    `json:"registration_ip,omitempty"`
	KeyCount       int       `json:"key_count"`
	SubCount       int       `json:"subscription_count"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type AdminUser struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	Role         string     `json:"role"` // superadmin, admin, operator, auditor, developer
	PasswordHash string     `json:"-"`
	TokenHash    string     `json:"-"`
	SSOProvider  string     `json:"sso_provider,omitempty"`
	SSOSub       string     `json:"sso_sub,omitempty"`
	Team         string     `json:"team"`
	Active       bool       `json:"active"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type DeveloperApp struct {
	ID          string    `json:"id"`
	ConsumerID  string    `json:"consumer_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Environment string    `json:"environment"` // production, staging, development
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type APIKey struct {
	ID            string     `json:"id"`
	ConsumerID    string     `json:"consumer_id"`
	AppID         *string    `json:"app_id,omitempty"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"key_prefix"`
	Active        bool       `json:"active"`
	IsSecondary   bool       `json:"is_secondary"`
	GraceUntil    *time.Time `json:"grace_until,omitempty"`
	RotatedFromID *string    `json:"rotated_from_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	// Key is only populated once, in the response to key creation or rotation.
	Key string `json:"key,omitempty"`
}

type Subscription struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	ConsumerID      string    `json:"consumer_id"`
	ConsumerName    string    `json:"consumer_name"`
	AppID           *string   `json:"app_id,omitempty"`
	APIID           string    `json:"api_id"`
	APIName         string    `json:"api_name"`
	PlanID          *string   `json:"plan_id"`
	PlanName        string    `json:"plan_name"`
	Status          string    `json:"status"` // pending, approved, rejected
	RejectionReason string    `json:"rejection_reason,omitempty"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
}

type AuditLog struct {
	ID           int64          `json:"id"`
	TS           time.Time      `json:"ts"`
	Actor        string         `json:"actor"`
	ActorID      string         `json:"actor_id,omitempty"`
	ActorEmail   string         `json:"actor_email,omitempty"`
	ActorRole    string         `json:"actor_role,omitempty"`
	Action       string         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	ResourceName string         `json:"resource_name,omitempty"`
	BeforeState  map[string]any `json:"before_state,omitempty"`
	AfterState   map[string]any `json:"after_state,omitempty"`
	StateDiff    map[string]any `json:"state_diff,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
	ClientIP     string         `json:"client_ip"`
	UserAgent    string         `json:"user_agent,omitempty"`
	TenantID     string         `json:"tenant_id,omitempty"`
}

type ConfigRevision struct {
	Revision       int64          `json:"revision"`
	CreatedAt      time.Time      `json:"created_at"`
	CreatedBy      string         `json:"created_by"`
	Description    string         `json:"description"`
	Status         string         `json:"status"` // active, canary, rolled_back
	TargetGroup    string         `json:"target_group"`
	ParentRevision *int64         `json:"parent_revision,omitempty"`
	RollbackOf     *int64         `json:"rollback_of,omitempty"`
	SnapshotData   map[string]any `json:"snapshot_data,omitempty"`
}

type NodeAcknowledgement struct {
	NodeID      string    `json:"node_id"`
	Revision    int64     `json:"revision"`
	AppliedAt   time.Time `json:"applied_at"`
	RoutesCount int       `json:"routes_count"`
	KeysCount   int       `json:"keys_count"`
	TookMS      float64   `json:"took_ms"`
	NodeGroup   string    `json:"node_group"`
	IsCanary    bool      `json:"is_canary"`
	CanaryRev   int64     `json:"canary_revision,omitempty"`
}

type FleetStatus struct {
	TargetRevision int64                 `json:"target_revision"`           // newest active (stable) revision
	CanaryRevision int64                 `json:"canary_revision,omitempty"` // in-flight canary revision, if any
	Nodes          []NodeAcknowledgement `json:"nodes"`
	Converged      bool                  `json:"converged"`
}

// GetRolloutState returns the newest active revision targeted at the whole fleet
// and the newest in-flight canary revision newer than it (0 when none).
func (s *Store) GetRolloutState(ctx context.Context) (stable, canary int64, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT max(revision) FROM config_revisions WHERE status='active' AND target_group='all'),
		         (SELECT max(revision) FROM config_revisions), 1)`).Scan(&stable)
	if err != nil {
		return 0, 0, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM config_revisions WHERE status='canary' AND revision > $1`, stable).Scan(&canary)
	return stable, canary, err
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

const planCols = `id, name, description, rate_limit_per_minute, quota_per_day, quota_per_month, price_monthly_usd, tier, created_at, updated_at, tenant_id`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.RateLimitPerMinute, &p.QuotaPerDay, &p.QuotaPerMonth, &p.PriceMonthlyUSD, &p.Tier, &p.CreatedAt, &p.UpdatedAt, &p.TenantID)
	return p, mapErr(err)
}

func (s *Store) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+planCols+` FROM plans ORDER BY rate_limit_per_minute = 0, rate_limit_per_minute, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPlan(ctx context.Context, id string) (Plan, error) {
	return scanPlan(s.Pool.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE id=$1`, id))
}

func (s *Store) CreatePlan(ctx context.Context, p Plan) (Plan, error) {
	tier := p.Tier
	if tier == "" {
		tier = "free"
	}
	return scanPlan(s.Pool.QueryRow(ctx,
		`INSERT INTO plans (name, description, rate_limit_per_minute, quota_per_day, quota_per_month, price_monthly_usd, tier, tenant_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+planCols,
		p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier, TenantOrDefault(p.TenantID)))
}

func (s *Store) UpdatePlan(ctx context.Context, p Plan) (Plan, error) {
	tier := p.Tier
	if tier == "" {
		tier = "free"
	}
	return scanPlan(s.Pool.QueryRow(ctx,
		`UPDATE plans SET name=$2, description=$3, rate_limit_per_minute=$4, quota_per_day=$5, quota_per_month=$6, price_monthly_usd=$7, tier=$8, updated_at=now() WHERE id=$1 RETURNING `+planCols,
		p.ID, p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier))
}

func (s *Store) DeletePlan(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM plans WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// APIs
// ---------------------------------------------------------------------------

const apiCols = `id, name, description, base_path, upstream_url, strip_path, auth_type, jwt_secret,
	jwks_url, oidc_issuer, oidc_audience, rate_limit_per_minute, quota_per_day, quota_per_month,
	timeout_ms, cors_enabled, request_headers, is_ai, openapi_spec, visibility, require_approval,
	is_draft, quota_failure_policy, enabled, created_at, updated_at, traffic_policy, tenant_id,
	protocol, graphql_schema, graphql_policy, grpc_descriptor_set, grpc_policy, mcp_policy`

func protocolOrDefault(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "graphql":
		return "graphql"
	case "grpc":
		return "grpc"
	case "mcp":
		return "mcp"
	default:
		return "http"
	}
}

func scanAPI(row pgx.Row) (API, error) {
	var a API
	err := row.Scan(&a.ID, &a.Name, &a.Description, &a.BasePath, &a.UpstreamURL, &a.StripPath, &a.AuthType,
		&a.JWTSecret, &a.JWKSURL, &a.OIDCIssuer, &a.OIDCAudience, &a.RateLimitPerMinute, &a.QuotaPerDay,
		&a.QuotaPerMonth, &a.TimeoutMS, &a.CORSEnabled, &a.RequestHeaders, &a.IsAI, &a.OpenAPISpec,
		&a.Visibility, &a.RequireApproval, &a.IsDraft, &a.QuotaFailurePolicy, &a.Enabled,
		&a.CreatedAt, &a.UpdatedAt, &a.TrafficPolicy, &a.TenantID,
		&a.Protocol, &a.GraphQLSchema, &a.GraphQLPolicy, &a.GRPCDescriptorSet, &a.GRPCPolicy, &a.MCPPolicy)
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}
	if a.Visibility == "" {
		a.Visibility = "public"
	}
	if a.QuotaFailurePolicy == "" {
		a.QuotaFailurePolicy = "fail_open"
	}
	a.Protocol = protocolOrDefault(a.Protocol)
	return a, mapErr(err)
}

func (s *Store) ListAPIs(ctx context.Context) ([]API, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+apiCols+` FROM apis ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []API{}
	for rows.Next() {
		a, err := scanAPI(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAPI(ctx context.Context, id string) (API, error) {
	return scanAPI(s.Pool.QueryRow(ctx, `SELECT `+apiCols+` FROM apis WHERE id=$1`, id))
}

func (s *Store) CreateAPI(ctx context.Context, a API) (API, error) {
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}
	if a.Visibility == "" {
		a.Visibility = "public"
	}
	if a.QuotaFailurePolicy == "" {
		a.QuotaFailurePolicy = "fail_open"
	}
	return scanAPI(s.Pool.QueryRow(ctx, `INSERT INTO apis
		(name, description, base_path, upstream_url, strip_path, auth_type, jwt_secret,
		 jwks_url, oidc_issuer, oidc_audience, rate_limit_per_minute, quota_per_day, quota_per_month,
		 timeout_ms, cors_enabled, request_headers, is_ai, openapi_spec, visibility, require_approval,
		 is_draft, quota_failure_policy, enabled, traffic_policy, tenant_id,
		 protocol, graphql_schema, graphql_policy, grpc_descriptor_set, grpc_policy, mcp_policy)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31) RETURNING `+apiCols,
		a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType, a.JWTSecret,
		a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay, a.QuotaPerMonth,
		a.TimeoutMS, a.CORSEnabled, a.RequestHeaders, a.IsAI, a.OpenAPISpec, a.Visibility, a.RequireApproval,
		a.IsDraft, a.QuotaFailurePolicy, a.Enabled, a.TrafficPolicy, TenantOrDefault(a.TenantID),
		protocolOrDefault(a.Protocol), a.GraphQLSchema, a.GraphQLPolicy, a.GRPCDescriptorSet, a.GRPCPolicy, a.MCPPolicy))
}

func (s *Store) UpdateAPI(ctx context.Context, a API) (API, error) {
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}
	if a.Visibility == "" {
		a.Visibility = "public"
	}
	if a.QuotaFailurePolicy == "" {
		a.QuotaFailurePolicy = "fail_open"
	}
	return scanAPI(s.Pool.QueryRow(ctx, `UPDATE apis SET
		name=$2, description=$3, base_path=$4, upstream_url=$5, strip_path=$6, auth_type=$7, jwt_secret=$8,
		jwks_url=$9, oidc_issuer=$10, oidc_audience=$11, rate_limit_per_minute=$12, quota_per_day=$13, quota_per_month=$14,
		timeout_ms=$15, cors_enabled=$16, request_headers=$17, is_ai=$18, openapi_spec=$19, visibility=$20,
		require_approval=$21, is_draft=$22, quota_failure_policy=$23, enabled=$24, traffic_policy=$25,
		protocol=$26, graphql_schema=$27, graphql_policy=$28, grpc_descriptor_set=$29, grpc_policy=$30, mcp_policy=$31,
		updated_at=now() WHERE id=$1 RETURNING `+apiCols,
		a.ID, a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType, a.JWTSecret,
		a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay, a.QuotaPerMonth,
		a.TimeoutMS, a.CORSEnabled, a.RequestHeaders, a.IsAI, a.OpenAPISpec, a.Visibility,
		a.RequireApproval, a.IsDraft, a.QuotaFailurePolicy, a.Enabled, a.TrafficPolicy,
		protocolOrDefault(a.Protocol), a.GraphQLSchema, a.GraphQLPolicy, a.GRPCDescriptorSet, a.GRPCPolicy, a.MCPPolicy))
}

func (s *Store) DeleteAPI(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM apis WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// Consumers
// ---------------------------------------------------------------------------

const consumerSelect = `SELECT c.id, c.name, c.email, c.status, c.registration_ip,
	(SELECT count(*) FROM api_keys k WHERE k.consumer_id=c.id AND k.active),
	(SELECT count(*) FROM subscriptions s WHERE s.consumer_id=c.id),
	c.created_at, c.updated_at, c.tenant_id FROM consumers c`

func scanConsumer(row pgx.Row) (Consumer, error) {
	var c Consumer
	err := row.Scan(&c.ID, &c.Name, &c.Email, &c.Status, &c.RegistrationIP, &c.KeyCount, &c.SubCount, &c.CreatedAt, &c.UpdatedAt, &c.TenantID)
	return c, mapErr(err)
}

func (s *Store) ListConsumers(ctx context.Context) ([]Consumer, error) {
	rows, err := s.Pool.Query(ctx, consumerSelect+` ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Consumer{}
	for rows.Next() {
		c, err := scanConsumer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetConsumer(ctx context.Context, id string) (Consumer, error) {
	return scanConsumer(s.Pool.QueryRow(ctx, consumerSelect+` WHERE c.id=$1`, id))
}

func (s *Store) CreateConsumer(ctx context.Context, c Consumer) (Consumer, error) {
	var id string
	if c.Status == "" {
		c.Status = "active"
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO consumers (name, email, status, registration_ip, tenant_id) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		c.Name, c.Email, c.Status, c.RegistrationIP, TenantOrDefault(c.TenantID)).Scan(&id); err != nil {
		return Consumer{}, mapErr(err)
	}
	return s.GetConsumer(ctx, id)
}

func (s *Store) UpdateConsumer(ctx context.Context, c Consumer) (Consumer, error) {
	if c.Status == "" {
		c.Status = "active"
	}
	if err := s.execOne(ctx, `UPDATE consumers SET name=$2, email=$3, status=$4, updated_at=now() WHERE id=$1`,
		c.ID, c.Name, c.Email, c.Status); err != nil {
		return Consumer{}, err
	}
	return s.GetConsumer(ctx, c.ID)
}

func (s *Store) DeleteConsumer(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM consumers WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Store) ListAPIKeys(ctx context.Context, consumerID string) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, consumer_id, name, key_prefix, active, created_at
		FROM api_keys WHERE consumer_id=$1 ORDER BY created_at DESC`, consumerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.ConsumerID, &k.Name, &k.KeyPrefix, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, consumerID, name string) (APIKey, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return APIKey{}, err
	}
	raw := "rk_" + hex.EncodeToString(buf)
	if name == "" {
		name = "default"
	}
	k := APIKey{Key: raw}
	err := s.Pool.QueryRow(ctx, `INSERT INTO api_keys (consumer_id, name, key_prefix, key_hash)
		VALUES ($1,$2,$3,$4) RETURNING id, consumer_id, name, key_prefix, active, created_at`,
		consumerID, name, raw[:10], HashKey(raw)).
		Scan(&k.ID, &k.ConsumerID, &k.Name, &k.KeyPrefix, &k.Active, &k.CreatedAt)
	return k, mapErr(err)
}

func (s *Store) SetAPIKeyActive(ctx context.Context, id string, active bool) error {
	return s.execOne(ctx, `UPDATE api_keys SET active=$2 WHERE id=$1`, id, active)
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM api_keys WHERE id=$1`, id)
}

func (s *Store) GetAPIKey(ctx context.Context, id string) (APIKey, error) {
	var k APIKey
	err := s.Pool.QueryRow(ctx, `SELECT id, consumer_id, app_id, name, key_prefix, active, is_secondary, grace_until, rotated_from_id, created_at
		FROM api_keys WHERE id=$1`, id).
		Scan(&k.ID, &k.ConsumerID, &k.AppID, &k.Name, &k.KeyPrefix, &k.Active, &k.IsSecondary, &k.GraceUntil, &k.RotatedFromID, &k.CreatedAt)
	return k, mapErr(err)
}

func (s *Store) GetConsumerByRawKey(ctx context.Context, rawKey string) (Consumer, error) {
	h := HashKey(rawKey)
	var c Consumer
	err := s.Pool.QueryRow(ctx, `SELECT c.id, c.name, c.email, c.status, c.created_at, c.updated_at
		FROM consumers c
		JOIN api_keys k ON k.consumer_id = c.id
		WHERE k.key_hash = $1 AND (k.active OR (k.grace_until IS NOT NULL AND k.grace_until > now()))
		LIMIT 1`, h).
		Scan(&c.ID, &c.Name, &c.Email, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	return c, mapErr(err)
}

func (s *Store) ListAllActiveAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, consumer_id, app_id, name, key_prefix, active, is_secondary, grace_until, rotated_from_id, created_at
		FROM api_keys WHERE active=true OR (grace_until IS NOT NULL AND grace_until > now())`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.ConsumerID, &k.AppID, &k.Name, &k.KeyPrefix, &k.Active, &k.IsSecondary, &k.GraceUntil, &k.RotatedFromID, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

const subSelect = `SELECT s.id, s.consumer_id, c.name, s.api_id, a.name, s.plan_id, COALESCE(p.name,''), s.status, s.active, s.created_at, a.tenant_id
	FROM subscriptions s
	JOIN consumers c ON c.id = s.consumer_id
	JOIN apis a ON a.id = s.api_id
	LEFT JOIN plans p ON p.id = s.plan_id`

func scanSub(row pgx.Row) (Subscription, error) {
	var x Subscription
	err := row.Scan(&x.ID, &x.ConsumerID, &x.ConsumerName, &x.APIID, &x.APIName, &x.PlanID, &x.PlanName, &x.Status, &x.Active, &x.CreatedAt, &x.TenantID)
	return x, mapErr(err)
}

// ErrCrossTenant is returned when a subscription would link a consumer, API or
// plan that belong to different tenants.
var ErrCrossTenant = fmt.Errorf("%w: consumer, API and plan must belong to the same tenant", ErrConflict)

// GetSubscription returns one subscription.
func (s *Store) GetSubscription(ctx context.Context, id string) (Subscription, error) {
	return scanSub(s.Pool.QueryRow(ctx, subSelect+` WHERE s.id=$1`, id))
}

func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.Pool.Query(ctx, subSelect+` ORDER BY c.name, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		x, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) ListPendingSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.Pool.Query(ctx, subSelect+` WHERE s.status='pending' ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		x, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) CreateSubscription(ctx context.Context, consumerID, apiID string, planID *string) (Subscription, error) {
	return s.CreateSubscriptionWithStatus(ctx, consumerID, apiID, planID, "approved", true)
}

func (s *Store) CreateSubscriptionWithStatus(ctx context.Context, consumerID, apiID string, planID *string, status string, active bool) (Subscription, error) {
	var id string
	if status == "" {
		status = "approved"
	}
	// The insert only happens when consumer, API and (optional) plan share a tenant.
	err := s.Pool.QueryRow(ctx, `INSERT INTO subscriptions (consumer_id, api_id, plan_id, status, active)
		SELECT $1, $2, $3, $4, $5
		WHERE (SELECT tenant_id FROM consumers WHERE id=$1) = (SELECT tenant_id FROM apis WHERE id=$2)
		  AND ($3::uuid IS NULL OR (SELECT tenant_id FROM plans WHERE id=$3::uuid) = (SELECT tenant_id FROM apis WHERE id=$2))
		RETURNING id`,
		consumerID, apiID, planID, status, active).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrCrossTenant
	}
	if err != nil {
		return Subscription{}, mapErr(err)
	}
	return scanSub(s.Pool.QueryRow(ctx, subSelect+` WHERE s.id=$1`, id))
}

func (s *Store) ApproveSubscription(ctx context.Context, id string) (Subscription, error) {
	if err := s.execOne(ctx, `UPDATE subscriptions SET status='approved', active=true WHERE id=$1`, id); err != nil {
		return Subscription{}, err
	}
	return scanSub(s.Pool.QueryRow(ctx, subSelect+` WHERE s.id=$1`, id))
}

func (s *Store) RejectSubscription(ctx context.Context, id string) (Subscription, error) {
	if err := s.execOne(ctx, `UPDATE subscriptions SET status='rejected', active=false WHERE id=$1`, id); err != nil {
		return Subscription{}, err
	}
	return scanSub(s.Pool.QueryRow(ctx, subSelect+` WHERE s.id=$1`, id))
}

func (s *Store) UpdateSubscription(ctx context.Context, id string, planID *string, active bool) (Subscription, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE subscriptions SET plan_id=$2, active=$3 WHERE id=$1
		AND ($2::uuid IS NULL OR (SELECT tenant_id FROM plans WHERE id=$2::uuid) = (SELECT tenant_id FROM apis WHERE id=subscriptions.api_id))`,
		id, planID, active)
	if err != nil {
		return Subscription{}, mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.GetSubscription(ctx, id); gerr != nil {
			return Subscription{}, gerr
		}
		return Subscription{}, ErrCrossTenant
	}
	return scanSub(s.Pool.QueryRow(ctx, subSelect+` WHERE s.id=$1`, id))
}

func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM subscriptions WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// Audit logs
// ---------------------------------------------------------------------------

func (s *Store) CreateAuditLog(ctx context.Context, actor, action, resType, resID string, details map[string]any, clientIP string) error {
	return s.CreateRichAuditLog(ctx, AuditLog{
		Actor:        actor,
		Action:       action,
		ResourceType: resType,
		ResourceID:   resID,
		Details:      details,
		ClientIP:     clientIP,
	})
}

func (s *Store) CreateRichAuditLog(ctx context.Context, a AuditLog) error {
	detJSON, _ := json.Marshal(a.Details)
	beforeJSON, _ := json.Marshal(a.BeforeState)
	afterJSON, _ := json.Marshal(a.AfterState)
	diffJSON, _ := json.Marshal(a.StateDiff)

	actor := a.Actor
	if actor == "" {
		actor = a.ActorEmail
	}
	if actor == "" {
		actor = "system"
	}

	_, err := s.Pool.Exec(ctx, `INSERT INTO audit_logs 
		(actor, actor_id, actor_email, actor_role, action, resource_type, resource_id, resource_name, before_state, after_state, state_diff, details, client_ip, user_agent, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		actor, a.ActorID, a.ActorEmail, a.ActorRole, a.Action, a.ResourceType, a.ResourceID, a.ResourceName,
		beforeJSON, afterJSON, diffJSON, detJSON, a.ClientIP, a.UserAgent, nullableUUID(a.TenantID))
	return err
}

func (s *Store) ListAuditLogs(ctx context.Context, limit int) ([]AuditLog, error) {
	return s.ListAuditLogsScoped(ctx, limit, nil)
}

// ListAuditLogsScoped lists audit entries; tenantIDs (when non-nil) restricts
// the result to entries recorded for those tenants.
func (s *Store) ListAuditLogsScoped(ctx context.Context, limit int, tenantIDs []string) ([]AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	args := []any{limit}
	if tenantIDs != nil {
		where = "WHERE tenant_id::text = ANY($2)"
		args = append(args, tenantIDs)
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, ts, actor, COALESCE(actor_id,''), COALESCE(actor_email,''), COALESCE(actor_role,''),
		action, resource_type, resource_id, COALESCE(resource_name,''), before_state, after_state, state_diff, details, client_ip, COALESCE(user_agent,''),
		COALESCE(tenant_id::text,'')
		FROM audit_logs `+where+` ORDER BY ts DESC LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditLog{}
	for rows.Next() {
		var a AuditLog
		var det, before, after, diff []byte
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.ActorID, &a.ActorEmail, &a.ActorRole,
			&a.Action, &a.ResourceType, &a.ResourceID, &a.ResourceName, &before, &after, &diff, &det, &a.ClientIP, &a.UserAgent, &a.TenantID); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(det, &a.Details)
		_ = json.Unmarshal(before, &a.BeforeState)
		_ = json.Unmarshal(after, &a.AfterState)
		_ = json.Unmarshal(diff, &a.StateDiff)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Admin Users & RBAC
// ---------------------------------------------------------------------------

const adminUserCols = `id, email, name, role, password_hash, token_hash, sso_provider, sso_sub, team, active, last_login_at, created_at, updated_at`

func scanAdminUser(row pgx.Row) (AdminUser, error) {
	var u AdminUser
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.TokenHash, &u.SSOProvider, &u.SSOSub, &u.Team, &u.Active, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	return u, mapErr(err)
}

func (s *Store) ListAdminUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+adminUserCols+` FROM admin_users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminUser
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) GetAdminUser(ctx context.Context, id string) (AdminUser, error) {
	return scanAdminUser(s.Pool.QueryRow(ctx, `SELECT `+adminUserCols+` FROM admin_users WHERE id=$1`, id))
}

func (s *Store) GetAdminUserByEmail(ctx context.Context, email string) (AdminUser, error) {
	return scanAdminUser(s.Pool.QueryRow(ctx, `SELECT `+adminUserCols+` FROM admin_users WHERE LOWER(email)=LOWER($1)`, email))
}

func (s *Store) CreateAdminUser(ctx context.Context, u AdminUser) (AdminUser, error) {
	var id string
	if u.Role == "" {
		u.Role = "operator"
	}
	if u.Team == "" {
		u.Team = "Engineering"
	}
	// Accounts must not have a bearer credential derived from their public email.
	var tokenEntropy [32]byte
	if _, err := rand.Read(tokenEntropy[:]); err != nil {
		return AdminUser{}, err
	}
	tokHash := HashKey(hex.EncodeToString(tokenEntropy[:]))
	// No default password: an account without one cannot sign in with a password
	// until one is set (SSO and cluster-token sign-in are unaffected).
	u.Active = true
	err := s.Pool.QueryRow(ctx, `INSERT INTO admin_users (email, name, role, password_hash, token_hash, sso_provider, sso_sub, team, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		u.Email, u.Name, u.Role, u.PasswordHash, tokHash, u.SSOProvider, u.SSOSub, u.Team, u.Active).Scan(&id)
	if err != nil {
		return AdminUser{}, mapErr(err)
	}
	return s.GetAdminUser(ctx, id)
}

func (s *Store) UpdateAdminUser(ctx context.Context, u AdminUser) (AdminUser, error) {
	// A non-empty PasswordHash replaces the stored password; empty keeps it.
	if err := s.execOne(ctx, `UPDATE admin_users SET name=$2, role=$3, team=$4, active=$5,
		password_hash = CASE WHEN $6 = '' THEN password_hash ELSE $6 END, updated_at=now() WHERE id=$1`,
		u.ID, u.Name, u.Role, u.Team, u.Active, u.PasswordHash); err != nil {
		return AdminUser{}, err
	}
	return s.GetAdminUser(ctx, u.ID)
}

// SetAdminUserSSO records the identity provider binding and IdP-derived
// platform role of an account after a verified SSO sign-in.
func (s *Store) SetAdminUserSSO(ctx context.Context, id, provider, sub, name, role string) (AdminUser, error) {
	if err := s.execOne(ctx, `UPDATE admin_users SET sso_provider=$2, sso_sub=$3,
		name = CASE WHEN $4 = '' THEN name ELSE $4 END, role=$5, updated_at=now() WHERE id=$1`, id, provider, sub, name, role); err != nil {
		return AdminUser{}, err
	}
	return s.GetAdminUser(ctx, id)
}

func (s *Store) DeleteAdminUser(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM admin_users WHERE id=$1`, id)
}

func (s *Store) UpdateAdminUserLogin(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE admin_users SET last_login_at=now() WHERE id=$1`, id)
	return err
}

// ---------------------------------------------------------------------------
// Developer Applications & Multi-App Self-Service
// ---------------------------------------------------------------------------

const devAppCols = `id, consumer_id, name, description, environment, created_at, updated_at`

func scanDevApp(row pgx.Row) (DeveloperApp, error) {
	var a DeveloperApp
	err := row.Scan(&a.ID, &a.ConsumerID, &a.Name, &a.Description, &a.Environment, &a.CreatedAt, &a.UpdatedAt)
	return a, mapErr(err)
}

func (s *Store) CreateDeveloperApp(ctx context.Context, a DeveloperApp) (DeveloperApp, error) {
	var id string
	if a.Environment == "" {
		a.Environment = "production"
	}
	err := s.Pool.QueryRow(ctx, `INSERT INTO developer_apps (consumer_id, name, description, environment)
		VALUES ($1,$2,$3,$4) RETURNING id`, a.ConsumerID, a.Name, a.Description, a.Environment).Scan(&id)
	if err != nil {
		return DeveloperApp{}, mapErr(err)
	}
	return s.GetDeveloperApp(ctx, id)
}

func (s *Store) ListDeveloperApps(ctx context.Context, consumerID string) ([]DeveloperApp, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+devAppCols+` FROM developer_apps WHERE consumer_id=$1 ORDER BY created_at DESC`, consumerID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []DeveloperApp
	for rows.Next() {
		a, err := scanDevApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListAllDeveloperApps(ctx context.Context) ([]DeveloperApp, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+devAppCols+` FROM developer_apps ORDER BY name`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []DeveloperApp
	for rows.Next() {
		a, err := scanDevApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetDeveloperApp(ctx context.Context, id string) (DeveloperApp, error) {
	return scanDevApp(s.Pool.QueryRow(ctx, `SELECT `+devAppCols+` FROM developer_apps WHERE id=$1`, id))
}

func (s *Store) DeleteDeveloperApp(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM developer_apps WHERE id=$1`, id)
}

// ---------------------------------------------------------------------------
// Key Rotation with Zero Downtime
// ---------------------------------------------------------------------------

func (s *Store) RotateAPIKey(ctx context.Context, oldKeyID string, gracePeriod time.Duration) (newKey APIKey, oldKey APIKey, err error) {
	var consumerID, name, keyPrefix string
	var appID *string
	err = s.Pool.QueryRow(ctx, `SELECT consumer_id, app_id, name, key_prefix FROM api_keys WHERE id=$1`, oldKeyID).
		Scan(&consumerID, &appID, &name, &keyPrefix)
	if err != nil {
		return APIKey{}, APIKey{}, mapErr(err)
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return APIKey{}, APIKey{}, err
	}
	raw := "rk_" + hex.EncodeToString(buf)
	newKeyName := name + "-rot"
	if len(newKeyName) > 40 {
		newKeyName = newKeyName[:40]
	}

	graceUntil := time.Now().Add(gracePeriod)
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Demote old key to secondary with grace period
		_, err := tx.Exec(ctx, `UPDATE api_keys SET is_secondary=true, grace_until=$2 WHERE id=$1`, oldKeyID, graceUntil)
		if err != nil {
			return err
		}

		// Insert new primary key
		newKey = APIKey{Key: raw, ConsumerID: consumerID, AppID: appID, Name: newKeyName, KeyPrefix: raw[:10], Active: true}
		return tx.QueryRow(ctx, `INSERT INTO api_keys (consumer_id, app_id, name, key_prefix, key_hash, active, is_secondary, rotated_from_id)
			VALUES ($1,$2,$3,$4,$5,true,false,$6)
			RETURNING id, created_at`, consumerID, appID, newKeyName, raw[:10], HashKey(raw), oldKeyID).
			Scan(&newKey.ID, &newKey.CreatedAt)
	})
	if err != nil {
		return APIKey{}, APIKey{}, err
	}
	oldKey = APIKey{ID: oldKeyID, ConsumerID: consumerID, AppID: appID, Name: name, KeyPrefix: keyPrefix, IsSecondary: true, GraceUntil: &graceUntil}
	return newKey, oldKey, nil
}

func (s *Store) RetireSecondaryKey(ctx context.Context, keyID string) error {
	return s.execOne(ctx, `UPDATE api_keys SET active=false, grace_until=now() WHERE id=$1`, keyID)
}

// ---------------------------------------------------------------------------
// Config Revisions & Fleet Configuration Proof
// ---------------------------------------------------------------------------

func (s *Store) SaveConfigRevision(ctx context.Context, createdBy, description string, snapshot any) (int64, error) {
	return s.SaveConfigRevisionWithTarget(ctx, createdBy, description, "all", "active", snapshot)
}

func (s *Store) SaveConfigRevisionWithTarget(ctx context.Context, createdBy, description, targetGroup, status string, snapshot any) (int64, error) {
	raw, _ := json.Marshal(snapshot)
	if targetGroup == "" {
		targetGroup = "all"
	}
	if status == "" {
		status = "active"
	}
	var rev int64
	err := s.Pool.QueryRow(ctx, `INSERT INTO config_revisions (created_by, description, target_group, status, snapshot_data)
		VALUES ($1, $2, $3, $4, $5) RETURNING revision`, createdBy, description, targetGroup, status, raw).Scan(&rev)
	return rev, err
}

func (s *Store) SetRevisionStatusAndTarget(ctx context.Context, rev int64, status, targetGroup string) error {
	return s.execOne(ctx, `UPDATE config_revisions SET status=$2, target_group=$3 WHERE revision=$1`, rev, status, targetGroup)
}

func (s *Store) NotifyRevision(ctx context.Context, rev int64) error {
	_, err := s.Pool.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'UPDATE', 'revision', $1::bigint, 'at', now())::text)`, rev)
	return err
}

func (s *Store) AtomicPublishConfig(ctx context.Context, actor, description, targetGroup, status string, mutateFn func(tx pgx.Tx) error) (int64, error) {
	return s.publishRevision(ctx, actor, description, targetGroup, status, CanarySplit{}, mutateFn)
}

// AtomicPublishCanary applies mutateFn and publishes the result directly as the
// in-flight canary revision with the given traffic split, so the change never
// reaches the whole fleet before it has been evaluated.
func (s *Store) AtomicPublishCanary(ctx context.Context, actor, description string, split CanarySplit, mutateFn func(tx pgx.Tx) error) (int64, error) {
	return s.publishRevision(ctx, actor, description, "canary", "canary", split, mutateFn)
}

func (s *Store) publishRevision(ctx context.Context, actor, description, targetGroup, status string, split CanarySplit, mutateFn func(tx pgx.Tx) error) (int64, error) {
	if actor == "" {
		actor = "superadmin"
	}
	if targetGroup == "" {
		targetGroup = "all"
	}
	if status == "" {
		status = "active"
	}

	var newRev int64
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if err := assertNoCanary(ctx, tx); err != nil {
			return err
		}
		if mutateFn != nil {
			if err := mutateFn(tx); err != nil {
				return err
			}
		}

		raw, err := buildConfigSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `INSERT INTO config_revisions (created_by, description, target_group, status, snapshot_data,
			traffic_percent, canary_header, canary_header_value)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING revision`, actor, description, targetGroup, status, raw,
			split.TrafficPercent, split.Header, split.HeaderValue).Scan(&newRev); err != nil {
			return fmt.Errorf("insert config revision: %w", err)
		}

		_, err = tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'UPDATE', 'revision', $1::bigint, 'at', now())::text)`, newRev)
		return err
	})
	return newRev, err
}

// buildConfigSnapshotTx captures the full configuration (APIs, plans,
// subscriptions, active keys) as stored in tx. Revisions store exactly this.
func buildConfigSnapshotTx(ctx context.Context, tx pgx.Tx) ([]byte, error) {
	rows, err := tx.Query(ctx, `SELECT `+apiCols+` FROM apis ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var apis []API
	for rows.Next() {
		a, err := scanAPI(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		apis = append(apis, a)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT `+planCols+` FROM plans ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var plans []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		plans = append(plans, p)
	}
	rows.Close()

	rows, err = tx.Query(ctx, subSelect+` ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	var subs []Subscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		subs = append(subs, sub)
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT id, consumer_id, app_id, name, key_prefix, active, is_secondary, grace_until, rotated_from_id, created_at
		FROM api_keys WHERE active=true OR (grace_until IS NOT NULL AND grace_until > now()) ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.ConsumerID, &k.AppID, &k.Name, &k.KeyPrefix, &k.Active, &k.IsSecondary, &k.GraceUntil, &k.RotatedFromID, &k.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()

	snap := map[string]any{
		"apis":          apis,
		"plans":         plans,
		"subscriptions": subs,
		"keys":          keys,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	return raw, nil
}

func (s *Store) CreateAPIAtomic(ctx context.Context, a API, actor, description string) (API, int64, error) {
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}
	if a.Visibility == "" {
		a.Visibility = "public"
	}
	if a.QuotaFailurePolicy == "" {
		a.QuotaFailurePolicy = "fail_open"
	}

	var created API
	rev, err := s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		var err error
		created, err = scanAPI(tx.QueryRow(ctx, `INSERT INTO apis
			(name, description, base_path, upstream_url, strip_path, auth_type, jwt_secret,
			 jwks_url, oidc_issuer, oidc_audience, rate_limit_per_minute, quota_per_day, quota_per_month,
			 timeout_ms, cors_enabled, request_headers, is_ai, openapi_spec, visibility, require_approval,
			 is_draft, quota_failure_policy, enabled, traffic_policy, tenant_id,
			 protocol, graphql_schema, graphql_policy, grpc_descriptor_set, grpc_policy, mcp_policy)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31) RETURNING `+apiCols,
			a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType, a.JWTSecret,
			a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay, a.QuotaPerMonth,
			a.TimeoutMS, a.CORSEnabled, a.RequestHeaders, a.IsAI, a.OpenAPISpec, a.Visibility, a.RequireApproval,
			a.IsDraft, a.QuotaFailurePolicy, a.Enabled, a.TrafficPolicy, TenantOrDefault(a.TenantID),
			protocolOrDefault(a.Protocol), a.GraphQLSchema, a.GraphQLPolicy, a.GRPCDescriptorSet, a.GRPCPolicy, a.MCPPolicy))
		return err
	})
	return created, rev, err
}

func (s *Store) UpdateAPIAtomic(ctx context.Context, a API, actor, description string) (API, int64, error) {
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}
	if a.Visibility == "" {
		a.Visibility = "public"
	}
	if a.QuotaFailurePolicy == "" {
		a.QuotaFailurePolicy = "fail_open"
	}

	var updated API
	rev, err := s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		var err error
		updated, err = scanAPI(tx.QueryRow(ctx, `UPDATE apis SET
			name=$2, description=$3, base_path=$4, upstream_url=$5, strip_path=$6, auth_type=$7, jwt_secret=$8,
			jwks_url=$9, oidc_issuer=$10, oidc_audience=$11, rate_limit_per_minute=$12, quota_per_day=$13, quota_per_month=$14,
			timeout_ms=$15, cors_enabled=$16, request_headers=$17, is_ai=$18, openapi_spec=$19, visibility=$20,
			require_approval=$21, is_draft=$22, quota_failure_policy=$23, enabled=$24, traffic_policy=$25,
			protocol=$26, graphql_schema=$27, graphql_policy=$28, grpc_descriptor_set=$29, grpc_policy=$30, mcp_policy=$31,
			updated_at=now() WHERE id=$1 RETURNING `+apiCols,
			a.ID, a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType,
			a.JWTSecret, a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay,
			a.QuotaPerMonth, a.TimeoutMS, a.CORSEnabled, a.RequestHeaders, a.IsAI, a.OpenAPISpec,
			a.Visibility, a.RequireApproval, a.IsDraft, a.QuotaFailurePolicy, a.Enabled, a.TrafficPolicy,
			protocolOrDefault(a.Protocol), a.GraphQLSchema, a.GraphQLPolicy, a.GRPCDescriptorSet, a.GRPCPolicy, a.MCPPolicy))
		return err
	})
	return updated, rev, err
}

func (s *Store) DeleteAPIAtomic(ctx context.Context, id, actor, description string) (int64, error) {
	return s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `DELETE FROM apis WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) PublishAPIAtomic(ctx context.Context, id, actor, description string) (API, int64, error) {
	var published API
	rev, err := s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		var err error
		published, err = scanAPI(tx.QueryRow(ctx, `UPDATE apis SET is_draft=false, enabled=true, updated_at=now() WHERE id=$1 RETURNING `+apiCols, id))
		return err
	})
	return published, rev, err
}

func (s *Store) CreatePlanAtomic(ctx context.Context, p Plan, actor, description string) (Plan, int64, error) {
	var created Plan
	rev, err := s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		var err error
		tier := p.Tier
		if tier == "" {
			tier = "free"
		}
		created, err = scanPlan(tx.QueryRow(ctx,
			`INSERT INTO plans (name, description, rate_limit_per_minute, quota_per_day, quota_per_month, price_monthly_usd, tier, tenant_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+planCols,
			p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier, TenantOrDefault(p.TenantID)))
		return err
	})
	return created, rev, err
}

func (s *Store) UpdatePlanAtomic(ctx context.Context, p Plan, actor, description string) (Plan, int64, error) {
	var updated Plan
	rev, err := s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		var err error
		tier := p.Tier
		if tier == "" {
			tier = "free"
		}
		updated, err = scanPlan(tx.QueryRow(ctx,
			`UPDATE plans SET name=$2, description=$3, rate_limit_per_minute=$4, quota_per_day=$5, quota_per_month=$6, price_monthly_usd=$7, tier=$8, updated_at=now() WHERE id=$1 RETURNING `+planCols,
			p.ID, p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier))
		return err
	})
	return updated, rev, err
}

func (s *Store) DeletePlanAtomic(ctx context.Context, id, actor, description string) (int64, error) {
	return s.AtomicPublishConfig(ctx, actor, description, "all", "active", func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `DELETE FROM plans WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Store) EnsureBaselineRevisionSnapshot(ctx context.Context) {
	apis, err := s.ListAPIs(ctx)
	if err != nil {
		return
	}
	plans, _ := s.ListPlans(ctx)
	subs, _ := s.ListSubscriptions(ctx)
	keys, _ := s.ListAllActiveAPIKeys(ctx)
	snap := map[string]any{
		"apis":          apis,
		"plans":         plans,
		"subscriptions": subs,
		"keys":          keys,
	}
	raw, _ := json.Marshal(snap)
	_, _ = s.Pool.Exec(ctx, `UPDATE config_revisions SET snapshot_data = $1 WHERE snapshot_data IS NULL OR snapshot_data::text = '{}' OR snapshot_data::text = 'null'`, raw)
}

// GetConfigRevisionMeta returns a revision without its snapshot. Use it on
// any periodic or hot path: the snapshot holds the whole fleet configuration
// (tens to hundreds of KB), and reading it every few seconds is what made an
// idle node pull gigabytes a day from a hosted database.
func (s *Store) GetConfigRevisionMeta(ctx context.Context, revision int64) (ConfigRevision, error) {
	var c ConfigRevision
	err := s.Pool.QueryRow(ctx, `SELECT revision, created_at, created_by, description, status, target_group, parent_revision, rollback_of
		FROM config_revisions WHERE revision=$1`, revision).
		Scan(&c.Revision, &c.CreatedAt, &c.CreatedBy, &c.Description, &c.Status, &c.TargetGroup, &c.ParentRevision, &c.RollbackOf)
	return c, mapErr(err)
}

func (s *Store) GetConfigRevision(ctx context.Context, revision int64) (ConfigRevision, error) {
	var c ConfigRevision
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT revision, created_at, created_by, description, status, target_group, parent_revision, rollback_of, snapshot_data
		FROM config_revisions WHERE revision=$1`, revision).
		Scan(&c.Revision, &c.CreatedAt, &c.CreatedBy, &c.Description, &c.Status, &c.TargetGroup, &c.ParentRevision, &c.RollbackOf, &raw)
	if err != nil {
		return ConfigRevision{}, mapErr(err)
	}
	_ = json.Unmarshal(raw, &c.SnapshotData)
	return c, nil
}

func (s *Store) ListConfigRevisions(ctx context.Context, limit int) ([]ConfigRevision, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT revision, created_at, created_by, description, status, target_group, parent_revision, rollback_of
		FROM config_revisions ORDER BY revision DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRevision
	for rows.Next() {
		var c ConfigRevision
		if err := rows.Scan(&c.Revision, &c.CreatedAt, &c.CreatedBy, &c.Description, &c.Status, &c.TargetGroup, &c.ParentRevision, &c.RollbackOf); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) RollbackConfigRevision(ctx context.Context, targetRev int64, actor string) (int64, error) {
	target, err := s.GetConfigRevision(ctx, targetRev)
	if err != nil {
		return 0, err
	}
	if len(target.SnapshotData) == 0 {
		return 0, pgx.ErrNoRows
	}

	rawSnap, _ := json.Marshal(target.SnapshotData)
	var newRev int64
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// 0. A rollback withdraws any in-flight canary: its changes are being reverted.
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET status='rolled_back', traffic_percent=0 WHERE status='canary'`); err != nil {
			return err
		}

		// 1. Insert new revision pointing to rollback target
		desc := "Rollback to revision " + strconv.FormatInt(targetRev, 10)
		err := tx.QueryRow(ctx, `INSERT INTO config_revisions (created_by, description, status, target_group, rollback_of, snapshot_data)
			VALUES ($1, $2, 'active', 'all', $3, $4) RETURNING revision`,
			actor, desc, targetRev, rawSnap).Scan(&newRev)
		if err != nil {
			return err
		}

		// 2. Restore all APIs from snapshot (UPSERT so deleted APIs are resurrected)
		if rawAPIs, ok := target.SnapshotData["apis"]; ok {
			apisJSON, _ := json.Marshal(rawAPIs)
			var apis []API
			if json.Unmarshal(apisJSON, &apis) == nil && len(apis) > 0 {
				var restoredIDs []string
				for _, a := range apis {
					restoredIDs = append(restoredIDs, a.ID)
					createdAt := a.CreatedAt
					if createdAt.IsZero() {
						createdAt = time.Now()
					}
					if _, err := tx.Exec(ctx, `INSERT INTO apis (
						id, name, description, base_path, upstream_url, strip_path, auth_type,
						jwt_secret, jwks_url, oidc_issuer, oidc_audience, rate_limit_per_minute, quota_per_day,
						quota_per_month, timeout_ms, cors_enabled, request_headers, is_ai, openapi_spec, visibility,
						require_approval, quota_failure_policy, enabled, is_draft, traffic_policy, created_at, updated_at, tenant_id
					) VALUES (
						$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
						$14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, now(), $27
					) ON CONFLICT (id) DO UPDATE SET
						name=$2, description=$3, base_path=$4, upstream_url=$5, strip_path=$6, auth_type=$7,
						jwt_secret=$8, jwks_url=$9, oidc_issuer=$10, oidc_audience=$11, rate_limit_per_minute=$12,
						quota_per_day=$13, quota_per_month=$14, timeout_ms=$15, cors_enabled=$16,
						request_headers=$17, is_ai=$18, openapi_spec=$19, visibility=$20, require_approval=$21,
						quota_failure_policy=$22, enabled=$23, is_draft=$24, traffic_policy=$25, updated_at=now()`,
						a.ID, a.Name, a.Description, a.BasePath, a.UpstreamURL, a.StripPath, a.AuthType,
						a.JWTSecret, a.JWKSURL, a.OIDCIssuer, a.OIDCAudience, a.RateLimitPerMinute, a.QuotaPerDay,
						a.QuotaPerMonth, a.TimeoutMS, a.CORSEnabled, nonNilHeaders(a.RequestHeaders), a.IsAI,
						nonNilSpec(a.OpenAPISpec), visibilityOrDefault(a.Visibility), a.RequireApproval,
						quotaPolicyOrDefault(a.QuotaFailurePolicy), a.Enabled, a.IsDraft, a.TrafficPolicy, createdAt, TenantOrDefault(a.TenantID)); err != nil {
						return fmt.Errorf("restore api %s: %w", a.ID, err)
					}
				}
				// Disable any APIs that were created after target revision and not in the snapshot
				if len(restoredIDs) > 0 {
					placeholders := make([]string, len(restoredIDs))
					args := make([]any, len(restoredIDs))
					for i, rid := range restoredIDs {
						placeholders[i] = fmt.Sprintf("$%d", i+1)
						args[i] = rid
					}
					q := fmt.Sprintf("UPDATE apis SET enabled=false, is_draft=true WHERE id NOT IN (%s)", strings.Join(placeholders, ","))
					if _, err := tx.Exec(ctx, q, args...); err != nil {
						return fmt.Errorf("disable newer apis: %w", err)
					}
				}
			}
		}

		// 3. Restore all Plans from snapshot
		if rawPlans, ok := target.SnapshotData["plans"]; ok {
			plansJSON, _ := json.Marshal(rawPlans)
			var plans []Plan
			if json.Unmarshal(plansJSON, &plans) == nil && len(plans) > 0 {
				for _, p := range plans {
					pCreatedAt := p.CreatedAt
					if pCreatedAt.IsZero() {
						pCreatedAt = time.Now()
					}
					tier := p.Tier
					if tier == "" {
						tier = "free"
					}
					if _, err := tx.Exec(ctx, `INSERT INTO plans (
						id, name, description, rate_limit_per_minute, quota_per_day, quota_per_month, price_monthly_usd, tier, created_at, updated_at, tenant_id
					) VALUES (
						$1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10
					) ON CONFLICT (id) DO UPDATE SET
						name=$2, description=$3, rate_limit_per_minute=$4, quota_per_day=$5, quota_per_month=$6, price_monthly_usd=$7, tier=$8, updated_at=now()`,
						p.ID, p.Name, p.Description, p.RateLimitPerMinute, p.QuotaPerDay, p.QuotaPerMonth, p.PriceMonthlyUSD, tier, pCreatedAt, TenantOrDefault(p.TenantID)); err != nil {
						return fmt.Errorf("restore plan %s: %w", p.ID, err)
					}
				}
			}
		}

		// 4. Restore Subscriptions state from snapshot
		if rawSubs, ok := target.SnapshotData["subscriptions"]; ok {
			subsJSON, _ := json.Marshal(rawSubs)
			var subs []Subscription
			if json.Unmarshal(subsJSON, &subs) == nil && len(subs) > 0 {
				var restoredSubIDs []string
				for _, sub := range subs {
					restoredSubIDs = append(restoredSubIDs, sub.ID)
					subCreatedAt := sub.CreatedAt
					if subCreatedAt.IsZero() {
						subCreatedAt = time.Now()
					}
					if _, err := tx.Exec(ctx, `INSERT INTO subscriptions (
						id, consumer_id, api_id, plan_id, status, active, created_at
					) VALUES (
						$1, $2, $3, $4, $5, $6, $7
					) ON CONFLICT (id) DO UPDATE SET 
						plan_id=$4, status=$5, active=$6`,
						sub.ID, sub.ConsumerID, sub.APIID, sub.PlanID, sub.Status, sub.Active, subCreatedAt); err != nil {
						return fmt.Errorf("restore subscription %s: %w", sub.ID, err)
					}
				}
				if len(restoredSubIDs) > 0 {
					placeholders := make([]string, len(restoredSubIDs))
					args := make([]any, len(restoredSubIDs))
					for i, sid := range restoredSubIDs {
						placeholders[i] = fmt.Sprintf("$%d", i+1)
						args[i] = sid
					}
					q := fmt.Sprintf("UPDATE subscriptions SET active=false WHERE id NOT IN (%s)", strings.Join(placeholders, ","))
					if _, err := tx.Exec(ctx, q, args...); err != nil {
						return fmt.Errorf("deactivate newer subscriptions: %w", err)
					}
				}
			}
		}

		// 5. Restore API Keys active state from snapshot
		if rawKeys, ok := target.SnapshotData["keys"]; ok {
			keysJSON, _ := json.Marshal(rawKeys)
			var keys []APIKey
			if json.Unmarshal(keysJSON, &keys) == nil && len(keys) > 0 {
				for _, k := range keys {
					if _, err := tx.Exec(ctx, `UPDATE api_keys SET active=$2, is_secondary=$3, grace_until=$4 WHERE id=$1`,
						k.ID, k.Active, k.IsSecondary, k.GraceUntil); err != nil {
						return fmt.Errorf("restore api key %s: %w", k.ID, err)
					}
				}
			}
		}

		// 6. Record what the tables now contain (newer APIs are kept but disabled),
		//    so the new revision exactly matches the restored configuration.
		restored, err := buildConfigSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET snapshot_data=$1 WHERE revision=$2`, restored, newRev); err != nil {
			return err
		}

		// 7. Trigger cluster LISTEN/NOTIFY
		_, err = tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'ROLLBACK', 'revision', $1::bigint, 'at', now())::text)`, newRev)
		return err
	})
	return newRev, err
}

func (s *Store) GetLatestRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(MAX(revision), 1) FROM config_revisions`).Scan(&rev)
	return rev, err
}

func (s *Store) AcknowledgeRevisionWithGroup(ctx context.Context, nodeID string, revision int64, routes, keys int, tookMS float64, nodeGroup string, isCanary bool) error {
	return s.AcknowledgeRevisionWithCanary(ctx, nodeID, revision, 0, routes, keys, tookMS, nodeGroup, isCanary)
}

// AcknowledgeRevisionWithCanary records that a node applied a stable revision and,
// when it serves a traffic-split canary, which canary revision it holds.
func (s *Store) AcknowledgeRevisionWithCanary(ctx context.Context, nodeID string, revision, canaryRevision int64, routes, keys int, tookMS float64, nodeGroup string, isCanary bool) error {
	if nodeGroup == "" {
		nodeGroup = "default"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO node_acknowledgements (node_id, revision, canary_revision, applied_at, routes_count, keys_count, took_ms, node_group, is_canary)
		VALUES ($1,$2,$3,now(),$4,$5,$6,$7,$8)
		ON CONFLICT (node_id, revision) DO UPDATE SET canary_revision=$3, applied_at=now(), routes_count=$4, keys_count=$5, took_ms=$6, node_group=$7, is_canary=$8`,
		nodeID, revision, canaryRevision, routes, keys, tookMS, nodeGroup, isCanary)
	return err
}

func (s *Store) AcknowledgeRevision(ctx context.Context, nodeID string, revision int64, routes, keys int, tookMS float64) error {
	return s.AcknowledgeRevisionWithGroup(ctx, nodeID, revision, routes, keys, tookMS, "default", false)
}

// NodeLivenessWindow is how recently a node must have acknowledged to count
// in fleet status. Nodes re-acknowledge every 30s (gateway.HeartbeatInterval),
// so this tolerates three missed heartbeats; a stopped or replaced node drops
// out within two minutes instead of holding the fleet "not converged".
var NodeLivenessWindow = 2 * time.Minute

func (s *Store) GetFleetStatus(ctx context.Context) (FleetStatus, error) {
	targetRev, canaryRev, err := s.GetRolloutState(ctx)
	if err != nil {
		return FleetStatus{}, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ON (node_id) node_id, revision, applied_at, routes_count, keys_count, took_ms, node_group, is_canary, canary_revision
		FROM node_acknowledgements
		WHERE applied_at > now() - make_interval(secs => $1)
		ORDER BY node_id, applied_at DESC`, NodeLivenessWindow.Seconds())
	if err != nil {
		return FleetStatus{}, err
	}
	defer rows.Close()
	var nodes []NodeAcknowledgement
	converged := true
	for rows.Next() {
		var n NodeAcknowledgement
		if err := rows.Scan(&n.NodeID, &n.Revision, &n.AppliedAt, &n.RoutesCount, &n.KeysCount, &n.TookMS, &n.NodeGroup, &n.IsCanary, &n.CanaryRev); err != nil {
			return FleetStatus{}, err
		}
		// A node is converged when it serves the stable revision, or (for a dedicated
		// canary node) when it serves the in-flight canary revision.
		onTarget := n.Revision == targetRev || (canaryRev > 0 && n.Revision == canaryRev && n.IsCanary)
		if canaryRev > 0 && !n.IsCanary && n.CanaryRev != 0 && n.CanaryRev != canaryRev {
			onTarget = false
		}
		if !onTarget {
			converged = false
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		converged = false
	}
	return FleetStatus{TargetRevision: targetRev, CanaryRevision: canaryRev, Nodes: nodes, Converged: converged}, rows.Err()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Store) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Enterprise Persistent Sessions (Database-Backed Identity Lifecycle)
// ---------------------------------------------------------------------------

type AdminSession struct {
	ID           string    `json:"id"`
	TokenHash    string    `json:"token_hash"`
	UserID       string    `json:"user_id"`
	IPAddress    string    `json:"ip_address"`
	UserAgent    string    `json:"user_agent"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
}

func (s *Store) CreateAdminSession(ctx context.Context, userID, tokenHash, ip, userAgent string, ttl time.Duration) (AdminSession, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	expiresAt := time.Now().Add(ttl)
	var sess AdminSession
	err := s.Pool.QueryRow(ctx, `INSERT INTO admin_sessions (user_id, token_hash, ip_address, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, user_id, token_hash, ip_address, user_agent, expires_at, created_at, last_active_at`,
		userID, tokenHash, ip, userAgent, expiresAt).
		Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.IPAddress, &sess.UserAgent, &sess.ExpiresAt, &sess.CreatedAt, &sess.LastActiveAt)
	return sess, err
}

func (s *Store) GetAdminSessionAndUser(ctx context.Context, tokenHash string) (AdminSession, AdminUser, error) {
	var sess AdminSession
	var u AdminUser
	var team, passHash, ssoProv, ssoSub *string
	var lastLogin *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT 
			s.id, s.user_id, s.token_hash, s.ip_address, s.user_agent, s.expires_at, s.created_at, s.last_active_at,
			u.id, u.name, u.email, u.role, u.active, u.token_hash, u.team, u.password_hash, u.sso_provider, u.sso_sub, u.last_login_at, u.created_at, u.updated_at
		FROM admin_sessions s
		JOIN admin_users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, tokenHash).
		Scan(
			&sess.ID, &sess.UserID, &sess.TokenHash, &sess.IPAddress, &sess.UserAgent, &sess.ExpiresAt, &sess.CreatedAt, &sess.LastActiveAt,
			&u.ID, &u.Name, &u.Email, &u.Role, &u.Active, &u.TokenHash, &team, &passHash, &ssoProv, &ssoSub, &lastLogin, &u.CreatedAt, &u.UpdatedAt,
		)
	if err != nil {
		return AdminSession{}, AdminUser{}, err
	}
	if team != nil {
		u.Team = *team
	}
	if passHash != nil {
		u.PasswordHash = *passHash
	}
	if ssoProv != nil {
		u.SSOProvider = *ssoProv
	}
	if ssoSub != nil {
		u.SSOSub = *ssoSub
	}
	u.LastLoginAt = lastLogin
	return sess, u, nil
}

func (s *Store) TouchAdminSession(ctx context.Context, tokenHash string, extendTTL time.Duration) error {
	newExpiry := time.Now().Add(extendTTL)
	_, err := s.Pool.Exec(ctx, `UPDATE admin_sessions SET last_active_at = now(), expires_at = $2 WHERE token_hash = $1`, tokenHash, newExpiry)
	return err
}

func (s *Store) DeleteAdminSession(ctx context.Context, tokenHash string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// ExpireAdminSessionsForUser ends every session of a user immediately while
// keeping the rows, so a later request can report why it was refused.
func (s *Store) ExpireAdminSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE admin_sessions SET expires_at = LEAST(expires_at, now() - interval '1 second') WHERE user_id = $1`, userID)
	return err
}

func (s *Store) DeleteAdminSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id = $1`, userID)
	return err
}

func (s *Store) CleanExpiredAdminSessions(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// AI Management Models
// ---------------------------------------------------------------------------

type AIProviderConnection struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenant_id"`
	Name            string         `json:"name"`
	ProviderType    string         `json:"provider_type"` // openai, anthropic, ollama, azure_openai, custom
	BaseURL         string         `json:"base_url"`
	APIKeySecretRef string         `json:"api_key_secret_ref"`
	Capabilities    map[string]any `json:"capabilities"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type AIModelDeployment struct {
	ID                    string    `json:"id"`
	TenantID              string    `json:"tenant_id"`
	ConnectionID          string    `json:"connection_id"`
	ModelName             string    `json:"model_name"`
	DeploymentName        string    `json:"deployment_name"`
	ContextWindowTokens   int       `json:"context_window_tokens"`
	MaxOutputTokens       int       `json:"max_output_tokens"`
	InputPricePerMillion  float64   `json:"input_price_per_million"`
	OutputPricePerMillion float64   `json:"output_price_per_million"`
	Enabled               bool      `json:"enabled"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type AIService struct {
	ID                       string    `json:"id"`
	TenantID                 string    `json:"tenant_id"`
	APIID                    *string   `json:"api_id,omitempty"`
	Name                     string    `json:"name"`
	Alias                    string    `json:"alias"`
	PrimaryModelDeploymentID *string   `json:"primary_model_deployment_id,omitempty"`
	AllowedModels            []string  `json:"allowed_models"`
	RoutingPolicy            string    `json:"routing_policy"` // single, fallback, balanced
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type AIBudgetAccount struct {
	ID                 string    `json:"id"`
	TenantID           string    `json:"tenant_id"`
	ConsumerID         *string   `json:"consumer_id,omitempty"`
	Currency           string    `json:"currency"`
	MonthlyBudgetCents int64     `json:"monthly_budget_cents"`
	CurrentSpendCents  int64     `json:"current_spend_cents"`
	ReservedSpendCents int64     `json:"reserved_spend_cents"`
	StrictEnforcement  bool      `json:"strict_enforcement"`
	ResetDayOfMonth    int       `json:"reset_day_of_month"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type AIBudgetReservation struct {
	ID            string    `json:"id"`
	AccountID     string    `json:"account_id"`
	RequestID     string    `json:"request_id"`
	ReservedCents int64     `json:"reserved_cents"`
	SettledCents  int64     `json:"settled_cents"`
	Status        string    `json:"status"` // pending, settled, released, expired
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type AIEvalSuite struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenant_id"`
	ServiceID *string        `json:"service_id,omitempty"`
	Name      string         `json:"name"`
	Rubric    map[string]any `json:"rubric"`
	TestCases []any          `json:"test_cases"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type AIReleaseManifest struct {
	ID                  string    `json:"id"`
	TenantID            string    `json:"tenant_id"`
	ServiceID           string    `json:"service_id"`
	Version             int       `json:"version"`
	ModelDeploymentID   string    `json:"model_deployment_id"`
	SystemPrompt        string    `json:"system_prompt"`
	EvalRunID           *string   `json:"eval_run_id,omitempty"`
	QualificationStatus string    `json:"qualification_status"` // draft, qualified, rejected, active
	EvidenceDigest      string    `json:"evidence_digest"`
	CreatedAt           time.Time `json:"created_at"`
}

type AIAgentGrant struct {
	ID                   string    `json:"id"`
	TenantID             string    `json:"tenant_id"`
	ConsumerID           *string   `json:"consumer_id,omitempty"`
	AgentName            string    `json:"agent_name"`
	AllowedTools         []string  `json:"allowed_tools"`
	MaxToolCallsPerRun   int       `json:"max_tool_calls_per_run"`
	RequireHumanApproval bool      `json:"require_human_approval"`
	CreatedAt            time.Time `json:"created_at"`
}
