package apiops

import (
	"github.com/relayops/apim/internal/store"
)

// DeclarativeFormatVersion is the only accepted format_version.
const DeclarativeFormatVersion = "1.0"

// DeclarativeConfig is the GitOps document: the desired state of APIs and plans.
// Unknown fields are rejected. Secrets are never exported; jwt_secret may use
// ${secret:NAME} references resolved from the control plane's environment.
type DeclarativeConfig struct {
	FormatVersion string     `json:"format_version"`
	ExportedAt    string     `json:"exported_at,omitempty"`
	APIs          []DeclAPI  `json:"apis"`
	Plans         []DeclPlan `json:"plans"`
}

type DeclAPI struct {
	Name               string               `json:"name"`
	Description        string               `json:"description,omitempty"`
	BasePath           string               `json:"base_path"`
	UpstreamURL        string               `json:"upstream_url"`
	StripPath          *bool                `json:"strip_path,omitempty"` // default true
	AuthType           string               `json:"auth_type,omitempty"`  // default none
	JWTSecret          string               `json:"jwt_secret,omitempty"`
	JWKSURL            string               `json:"jwks_url,omitempty"`
	OIDCIssuer         string               `json:"oidc_issuer,omitempty"`
	OIDCAudience       string               `json:"oidc_audience,omitempty"`
	RateLimitPerMinute int                  `json:"rate_limit_per_minute,omitempty"`
	QuotaPerDay        int                  `json:"quota_per_day,omitempty"`
	QuotaPerMonth      int                  `json:"quota_per_month,omitempty"`
	TimeoutMS          int                  `json:"timeout_ms,omitempty"` // default 30000
	CORSEnabled        bool                 `json:"cors_enabled,omitempty"`
	RequestHeaders     map[string]string    `json:"request_headers,omitempty"`
	IsAI               bool                 `json:"is_ai,omitempty"`
	OpenAPISpec        map[string]any       `json:"openapi_spec,omitempty"`
	Visibility         string               `json:"visibility,omitempty"` // default public
	RequireApproval    bool                 `json:"require_approval,omitempty"`
	IsDraft            bool                 `json:"is_draft,omitempty"`
	QuotaFailurePolicy string               `json:"quota_failure_policy,omitempty"` // default fail_open
	TrafficPolicy      *store.TrafficPolicy `json:"traffic_policy,omitempty"`
	Protocol           string               `json:"protocol,omitempty"` // http, graphql, grpc, mcp
	GraphQLSchema      string               `json:"graphql_schema,omitempty"`
	GRPCDescriptorSet  []byte               `json:"grpc_descriptor_set,omitempty"` // base64 FileDescriptorSet
	GraphQLPolicy      *store.GraphQLPolicy `json:"graphql_policy,omitempty"`
	GRPCPolicy         *store.GRPCPolicy    `json:"grpc_policy,omitempty"`
	MCPPolicy          *store.MCPPolicy     `json:"mcp_policy,omitempty"`
	Enabled            *bool                `json:"enabled,omitempty"` // default true
}

type DeclPlan struct {
	Name               string  `json:"name"`
	Description        string  `json:"description,omitempty"`
	RateLimitPerMinute int     `json:"rate_limit_per_minute"`
	QuotaPerDay        int     `json:"quota_per_day,omitempty"`
	QuotaPerMonth      int     `json:"quota_per_month,omitempty"`
	PriceMonthlyUSD    float64 `json:"price_monthly_usd,omitempty"`
	Tier               string  `json:"tier,omitempty"`
}
