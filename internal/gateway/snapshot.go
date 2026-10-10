package gateway

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/ast"

	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/store"
)

// Snapshot is an immutable, fully-resolved view of gateway config. It is
// swapped atomically on every reload so requests never see partial state.
//
// A snapshot may carry a Canary snapshot built from a newer, traffic-split
// canary revision. Requests are assigned to the canary cohort by routing header
// or by a sticky hash of the caller's credential (or client IP).
type Snapshot struct {
	Version      int64
	LoadedAt     time.Time
	PolicySource string
	Routes       []*Route                   // sorted longest base path first
	Keys         map[string]store.KeyRecord // key hash -> key (live access state)
	Subs         map[string]store.SubRecord // consumerID|apiID -> subscription (limits resolved per revision)

	Canary     *Snapshot
	CanaryRule CanaryRule
	IsCanary   bool
}

// CanaryRule decides which requests a traffic-split canary receives.
type CanaryRule struct {
	TrafficPercent int
	Header         string
	HeaderValue    string
}

type Route struct {
	API      store.API
	Upstream *url.URL // primary target, used for path joining and display
	Pool     *upstreamPool
	// grpcMethods is the gRPC API's method catalog from its descriptor set
	// (nil: no descriptor, any method is forwarded).
	grpcMethods map[string]grpc.MethodInfo
	// graphqlSchema validates requests when the GraphQL API sets
	// validate_against_schema.
	graphqlSchema *ast.Schema
	// mcp is the MCP API's catalog state from the control plane.
	mcp *mcpRouteState
}

func buildSnapshot(d store.SnapshotData, version int64, reg *upstreamRegistry) (*Snapshot, []error) {
	keys := make(map[string]store.KeyRecord, len(d.Keys))
	for _, k := range d.Keys {
		keys[k.KeyHash] = k
	}
	s, errs := buildRevisionSnapshot(d.APIs, d.Subs, keys, version, reg)
	s.PolicySource = d.PolicySource
	attachMCPCatalog(s, d.MCPCatalog)
	if d.Canary != nil && d.Canary.Revision > version {
		c, cerrs := buildRevisionSnapshot(d.Canary.APIs, d.Canary.Subs, keys, d.Canary.Revision, reg)
		c.PolicySource = "revision"
		c.IsCanary = true
		for _, e := range cerrs {
			errs = append(errs, fmt.Errorf("canary rev %d: %w", d.Canary.Revision, e))
		}
		attachMCPCatalog(c, d.MCPCatalog)
		s.Canary = c
		s.CanaryRule = CanaryRule{
			TrafficPercent: clampPercent(d.Canary.TrafficPercent),
			Header:         strings.TrimSpace(d.Canary.Header),
			HeaderValue:    d.Canary.HeaderValue,
		}
	}
	return s, errs
}

func buildRevisionSnapshot(apis []store.API, subs []store.SubRecord, keys map[string]store.KeyRecord, version int64, reg *upstreamRegistry) (*Snapshot, []error) {
	s := &Snapshot{
		Version:  version,
		LoadedAt: time.Now(),
		Keys:     keys,
		Subs:     make(map[string]store.SubRecord, len(subs)),
	}
	var errs []error
	for _, a := range apis {
		a.TrafficPolicy.Normalize()
		if err := a.TrafficPolicy.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("api %q: %v", a.Name, err))
			continue
		}
		u, err := url.Parse(a.UpstreamURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("api %q: invalid upstream %q", a.Name, a.UpstreamURL))
			continue
		}
		pool, err := newUpstreamPool(a, reg)
		if err != nil {
			errs = append(errs, fmt.Errorf("api %q: %v", a.Name, err))
			continue
		}
		a.BasePath = normalizeBasePath(a.BasePath)
		route := &Route{API: a, Upstream: u, Pool: pool}
		if a.Protocol == "grpc" {
			a.GRPCPolicy.Normalize()
			route.API.GRPCPolicy = a.GRPCPolicy
			if len(a.GRPCDescriptorSet) > 0 {
				methods, err := grpc.ParseDescriptorSet(a.GRPCDescriptorSet)
				if err != nil {
					errs = append(errs, fmt.Errorf("api %q: %v", a.Name, err))
					continue
				}
				route.grpcMethods = methods
			}
		}
		if a.Protocol == "graphql" && a.GraphQLPolicy.ValidateAgainstSchema {
			schema, err := graphql.LoadSchema(a.GraphQLSchema)
			if err != nil {
				errs = append(errs, fmt.Errorf("api %q: %v", a.Name, err))
				continue
			}
			route.graphqlSchema = schema
		}
		s.Routes = append(s.Routes, route)
	}
	sort.Slice(s.Routes, func(i, j int) bool { return len(s.Routes[i].API.BasePath) > len(s.Routes[j].API.BasePath) })
	for _, x := range subs {
		s.Subs[x.ConsumerID+"|"+x.APIID] = x
	}
	return s, errs
}

func clampPercent(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func normalizeBasePath(p string) string {
	if p == "" {
		return "/"
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// Match returns the route with the longest base path matching the request path.
func (s *Snapshot) Match(path string) *Route {
	for _, r := range s.Routes {
		bp := r.API.BasePath
		if bp == "/" || path == bp || strings.HasPrefix(path, bp+"/") {
			return r
		}
	}
	return nil
}

// ForRequest returns the snapshot (stable or canary) that should serve r.
func (s *Snapshot) ForRequest(r *http.Request, clientIP string) *Snapshot {
	if s == nil || s.Canary == nil {
		return s
	}
	// Deterministic Test Studio runner selection (internal authenticated only)
	if targetRevStr := r.Header.Get("X-RelayOps-Target-Revision"); targetRevStr != "" {
		clean := strings.TrimPrefix(targetRevStr, "rev_")
		if reqRev, err := strconv.ParseInt(clean, 10, 64); err == nil && reqRev > 0 {
			if s.Canary != nil && s.Canary.Version == reqRev {
				return s.Canary
			}
			if s.Version == reqRev {
				return s
			}
		}
	}
	if cohort := r.Header.Get("X-RelayOps-Target-Cohort"); cohort == "canary" {
		return s.Canary
	} else if cohort == "stable" || cohort == "baseline" {
		return s
	}

	rule := s.CanaryRule
	if rule.Header != "" {
		if v := r.Header.Get(rule.Header); v != "" && (rule.HeaderValue == "" || v == rule.HeaderValue) {
			return s.Canary
		}
	}
	if rule.TrafficPercent >= 100 {
		return s.Canary
	}
	if rule.TrafficPercent > 0 && CohortBucket(stickyKey(r, clientIP)) < rule.TrafficPercent {
		return s.Canary
	}
	return s
}

// CohortBucket maps a caller identity to a stable bucket in [0, 100).
func CohortBucket(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}

// stickyKey identifies the caller so the same client consistently lands in the
// same cohort for the lifetime of a canary.
func stickyKey(r *http.Request, clientIP string) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return "k:" + k
	}
	if k := r.URL.Query().Get("apikey"); k != "" {
		return "k:" + k
	}
	if a := r.Header.Get("Authorization"); a != "" {
		return "a:" + a
	}
	return "ip:" + clientIP
}
