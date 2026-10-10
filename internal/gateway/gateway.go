// Package gateway is the RelayOps data plane: an HTTP reverse proxy that
// routes, authenticates, rate-limits and observes every request using an
// atomically swapped config snapshot sourced from Postgres.
package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	mrand "math/rand"
	rand2 "math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/ai"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/policy"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/tracing"
)

// maxRetryBody is the largest request body buffered so it can be replayed on retry.
// Larger bodies are streamed and the request is not retried.
const maxRetryBody = 1 << 20

type Gateway struct {
	snap         atomic.Pointer[Snapshot]
	limiter      *Limiter
	redisLimiter *RedisLimiter
	jwks         *JWKSManager
	evaluator    *policy.Evaluator
	collector    *analytics.Collector
	proxy        *httputil.ReverseProxy
	nodeID       string
	nodeGroup    string
	isCanary     bool
	upstreams    *upstreamRegistry
	tracer       *tracing.Tracer
	runnerSecret string
	aiBudgetLedger AIBudgetLedger
	wasm           *policy.WasmManager // nil: APIs that list WASM plugins are refused
	// mcpDrift remembers MCP tools whose definition no longer matches its pin
	// (key: api|tool|pin), so calls to them are refused until re-approved.
	mcpDrift       sync.Map
	mcpDriftEvents atomic.Uint64
	mcpObs         chan store.MCPObservation // definitions to report for review (nil: not reporting)
	apq            sync.Map                  // GraphQL API id -> *graphql.PersistedCache
	trustedProxies []*net.IPNet        // proxies whose forwarded client-certificate headers are accepted

	tmu        sync.Mutex
	transports map[transportKey]*http.Transport
}

type AIBudgetLedger interface {
	GetAIBudgetAccountByConsumer(ctx context.Context, tenantID, consumerID string) (*store.AIBudgetAccount, error)
	ReserveAIBudget(ctx context.Context, accountID, requestID string, reserveCents int64) (*store.AIBudgetReservation, error)
	// SettleAIBudgetUsage prices metered usage (the tenant's model prices, or
	// the estimate) and settles the reservation, returning the cents charged.
	SettleAIBudgetUsage(ctx context.Context, requestID string, usage store.AIUsage) (int64, error)
	ReleaseAIBudget(ctx context.Context, requestID string) error
}

func (g *Gateway) SetAIBudgetLedger(ledger AIBudgetLedger) {
	g.aiBudgetLedger = ledger
}

func (g *Gateway) SetRunnerSecret(secret string) {
	g.runnerSecret = secret
}

type ctxKey struct{}

// reqState carries per-request data from the handler into the proxy callbacks.
type reqState struct {
	route              *Route
	mcp                *mcpExchange // set for MCP JSON-RPC requests
	grpc               *grpcExchange // set for gRPC calls
	gqlWS              *gqlWSCheck   // set for GraphQL over WebSocket
	requestID          string
	consumerID         string
	consumerName       string
	err                string
	model              string
	tokensPrompt       int
	tokensCompletion   int
	tokensTotal        int
	decisionPolicy     string
	decisionReason     string
	authStatus         string
	subscriptionStatus string
	rateLimitStatus    string
	configRevision     int64
	matchedRoute       string
	policyEvaluations  map[string]any
	upstreamDurationMS float64
	upstreamStart      time.Time
	inURL              *url.URL // outbound path after strip_path, before target joining
	span               *tracing.Span
	traceID            string
	cohort             string
	upstreamAttempts   []map[string]any
	isGRPC             bool
	isGraphQL          bool
	aiProvenance       string
	aiBudgetReserved   bool
}

func New(c *analytics.Collector, rl *RedisLimiter, nodeID string) *Gateway {
	memLimiter := NewLimiter()
	if rl == nil {
		rl = &RedisLimiter{fallback: memLimiter}
	}
	jwksMgr := NewJWKSManager()
	g := &Gateway{
		limiter:      memLimiter,
		redisLimiter: rl,
		jwks:         jwksMgr,
		evaluator:    policy.NewEvaluator(jwksMgr),
		collector:    c,
		nodeID:       nodeID,
		transports:   map[transportKey]*http.Transport{},
		upstreams:    newUpstreamRegistry(),
		tracer:       &tracing.Tracer{},
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:        g.rewrite,
		Transport:      routeTransport{g},
		FlushInterval:  -1, // flush immediately: SSE / streaming / long-poll friendly
		ErrorHandler:   g.proxyError,
		ModifyResponse: g.modifyResponse,
		// Reuse copy buffers: without a pool every response allocates 32 KiB.
		BufferPool: proxyBuffers{},
	}
	return g
}

func (s *Snapshot) LookupKey(hash string) (store.KeyRecord, bool) {
	if s == nil || s.Keys == nil {
		return store.KeyRecord{}, false
	}
	k, ok := s.Keys[hash]
	return k, ok
}

func (s *Snapshot) LookupSub(consumerID, apiID string) (store.SubRecord, bool) {
	if s == nil || s.Subs == nil {
		return store.SubRecord{}, false
	}
	sub, ok := s.Subs[consumerID+"|"+apiID]
	return sub, ok
}

type LimiterAdapter struct {
	gw *Gateway
}

func (a *LimiterAdapter) CheckRateLimit(ctx context.Context, key string, limitPerMinute int) (bool, int, time.Duration, bool) {
	d := a.gw.redisLimiter.Allow(ctx, key, limitPerMinute)
	degraded := !a.gw.redisLimiter.IsHealthy()
	return d.Allowed, d.Remaining, d.RetryAfter, degraded
}

func (a *LimiterAdapter) CheckDailyQuota(ctx context.Context, key string, quotaDay int) (bool, int, bool) {
	d := a.gw.redisLimiter.CheckQuotaWithPolicy(ctx, key, quotaDay, 0, "block")
	return d.Allowed, d.RemainingDay, d.Degraded
}

func (a *LimiterAdapter) CheckMonthlyQuota(ctx context.Context, key string, quotaMonth int) (bool, int, bool) {
	d := a.gw.redisLimiter.CheckQuotaWithPolicy(ctx, key, 0, quotaMonth, "block")
	return d.Allowed, d.RemainingMonth, d.Degraded
}

func (g *Gateway) NodeID() string {
	if g.nodeID == "" {
		return "relayops-gw-1"
	}
	return g.nodeID
}

func (g *Gateway) NodeGroup() string {
	if g.nodeGroup == "" {
		return "default"
	}
	return g.nodeGroup
}

func (g *Gateway) IsCanary() bool {
	return g.isCanary
}

func (g *Gateway) SetNodeMetadata(group string, isCanary bool) {
	if group == "" {
		group = "default"
	}
	g.nodeGroup = group
	g.isCanary = isCanary
}

// SetTracer installs the OpenTelemetry tracer used for request spans.
func (g *Gateway) SetTracer(t *tracing.Tracer) {
	if t != nil {
		g.tracer = t
	}
}

func (g *Gateway) Collector() *analytics.Collector {
	return g.collector
}

func (g *Gateway) Snapshot() *Snapshot { return g.snap.Load() }

func (g *Gateway) swap(s *Snapshot) {
	g.snap.Store(s)
	keep := map[string]bool{}
	for _, snap := range []*Snapshot{s, s.Canary} {
		if snap == nil {
			continue
		}
		for _, r := range snap.Routes {
			for _, t := range r.Pool.targets {
				keep[t.key] = true
			}
		}
	}
	g.upstreams.retain(keep)
}

// ---------------------------------------------------------------------------
// Request pipeline
// ---------------------------------------------------------------------------

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	st := &reqState{
		requestID:          r.Header.Get("X-Request-ID"),
		authStatus:         "none",
		subscriptionStatus: "none",
		rateLimitStatus:    "ok",
		decisionReason:     "processing",
	}
	if st.requestID == "" || len(st.requestID) > 128 {
		st.requestID = newRequestID()
	}
	rec := &recorder{ResponseWriter: w, st: st, aiMeter: ai.NewStreamMeter()}
	rec.Header().Set("X-Request-ID", st.requestID)

	if r.URL.Path == "/__relayops/health" {
		snap := g.snap.Load()
		health := map[string]any{
			"status":         "ok",
			"config_version": snapVersion(snap),
			"node_id":        g.NodeID(),
			"node_group":     g.NodeGroup(),
			"is_canary":      g.IsCanary(),
		}
		if snap != nil && snap.Canary != nil {
			health["canary_revision"] = snap.Canary.Version
			health["canary_traffic_percent"] = snap.CanaryRule.TrafficPercent
			health["canary_header"] = snap.CanaryRule.Header
		}
		writeJSON(rec, http.StatusOK, health)
		return
	}

	st.span = g.tracer.StartServer(r.Header, r.Method+" gateway")
	if st.span != nil {
		st.traceID = st.span.Context.TraceIDHex()
	} else {
		st.traceID = tracing.TraceIDFromHeader(r.Header)
	}
	if st.traceID != "" {
		rec.Header().Set("X-RelayOps-Trace-Id", st.traceID)
	}

	defer func() {
		status := rec.status
		if !rec.wroteHeader {
			status = http.StatusOK
			if st.err == "" && r.Header.Get("Upgrade") != "" {
				status = http.StatusSwitchingProtocols
			}
		}
		// A gRPC call's outcome is its grpc-status (HTTP is 200 even on
		// failure); log the equivalent HTTP status so analytics and
		// auto-rollback count failed calls.
		if st.isGRPC && (status == http.StatusOK || st.grpc != nil && st.grpc.finalStatus != nil) {
			code, ok := grpcStatusOf(rec.Header())
			if st.grpc != nil && st.grpc.finalStatus != nil {
				code, ok = *st.grpc.finalStatus, true
			}
			if ok {
				if ev, ok := st.policyEvaluations["grpc"].(map[string]any); ok {
					ev["status"] = code
				}
				status = grpc.HTTPStatus(code)
				if code != grpc.StatusOK && st.err == "" {
					st.err = "grpc-status " + strconv.Itoa(code) + " " + grpc.StatusText(code)
					if st.decisionReason == "" || st.decisionReason == "processing" {
						st.decisionPolicy, st.decisionReason = "upstream", "grpc_"+strings.ToLower(grpc.StatusText(code))
					}
				}
			}
		}
		if status >= 200 && status < 400 && (st.decisionReason == "processing" || st.decisionReason == "") {
			st.decisionReason = "proxied_successfully"
			st.decisionPolicy = "proxy"
		}
		if st.route != nil && st.route.API.IsAI && rec.aiMeter != nil {
			rec.aiMeter.Finish()
			if rec.aiMeter.Model != "" {
				st.model = rec.aiMeter.Model
			}
			if rec.aiMeter.PromptTokens > 0 {
				st.tokensPrompt = rec.aiMeter.PromptTokens
			}
			if rec.aiMeter.CompletionTokens > 0 {
				st.tokensCompletion = rec.aiMeter.CompletionTokens
			}
			if rec.aiMeter.TotalTokens > 0 {
				st.tokensTotal = rec.aiMeter.TotalTokens
			}
			if rec.aiMeter.Provenance != "" {
				st.aiProvenance = string(rec.aiMeter.Provenance)
			}
		}
		if st.aiBudgetReserved && g.aiBudgetLedger != nil {
			if status >= 200 && status < 400 {
				// Default estimate, used only when the tenant has no priced
				// deployment for this model.
				estimate := int64(1)
				if st.tokensTotal > 0 {
					promptCents := float64(st.tokensPrompt) * 0.00015
					compCents := float64(st.tokensCompletion) * 0.00025
					calc := int64(math.Ceil(promptCents + compCents))
					if calc > estimate {
						estimate = calc
					}
				}
				usage := store.AIUsage{Model: st.model, PromptTokens: int64(st.tokensPrompt),
					CompletionTokens: int64(st.tokensCompletion), EstimateCents: estimate}
				if cents, serr := g.aiBudgetLedger.SettleAIBudgetUsage(context.Background(), st.requestID, usage); serr != nil {
					slog.Warn("AI budget settlement failed", "request_id", st.requestID, "model", st.model, "error", serr)
				} else {
					slog.Debug("AI budget settled", "request_id", st.requestID, "model", st.model, "cents", cents)
				}
			} else {
				if rerr := g.aiBudgetLedger.ReleaseAIBudget(context.Background(), st.requestID); rerr != nil {
					slog.Warn("AI budget reservation release failed", "request_id", st.requestID, "error", rerr)
				}
			}
		}
		l := store.RequestLog{
			TS: start, NodeID: g.NodeID(), RequestID: st.requestID, Method: r.Method, Path: r.URL.Path,
			Status: status, LatencyMS: float64(time.Since(start).Microseconds()) / 1000.0,
			BytesOut: rec.bytes, ClientIP: clientIP(r), Error: st.err, ConsumerName: st.consumerName,
			Model: st.model, TokensPrompt: st.tokensPrompt, TokensCompletion: st.tokensCompletion, TokensTotal: st.tokensTotal,
			DecisionReason: st.decisionReason, AuthStatus: st.authStatus,
			SubscriptionStatus: st.subscriptionStatus, RateLimitStatus: st.rateLimitStatus,
			UpstreamDurationMS: st.upstreamDurationMS,
			ConfigRevision:     st.configRevision,
			MatchedRoute:       st.matchedRoute,
			PolicyEvaluations:  st.policyEvaluations,
			TraceID:            st.traceID,
		}
		if len(st.upstreamAttempts) > 0 {
			if l.PolicyEvaluations == nil {
				l.PolicyEvaluations = map[string]any{}
			}
			l.PolicyEvaluations["upstream"] = map[string]any{"attempts": st.upstreamAttempts}
		}
		if st.cohort != "" {
			if l.PolicyEvaluations == nil {
				l.PolicyEvaluations = map[string]any{}
			}
			l.PolicyEvaluations["release"] = map[string]any{"cohort": st.cohort, "revision": st.configRevision}
		}
		if st.route != nil && st.route.API.IsAI {
			if l.PolicyEvaluations == nil {
				l.PolicyEvaluations = map[string]any{}
			}
			l.PolicyEvaluations["ai_usage"] = map[string]any{
				"model":             st.model,
				"prompt_tokens":     st.tokensPrompt,
				"completion_tokens": st.tokensCompletion,
				"total_tokens":      st.tokensTotal,
				"provenance":        st.aiProvenance,
			}
		}
		if sp := st.span; sp != nil {
			sp.SetAttr("http.request.method", r.Method)
			sp.SetAttr("url.path", r.URL.Path)
			sp.SetAttr("http.response.status_code", status)
			sp.SetAttr("relayops.request_id", st.requestID)
			sp.SetAttr("relayops.decision.policy", st.decisionPolicy)
			sp.SetAttr("relayops.decision.reason", st.decisionReason)
			sp.SetAttr("relayops.config_revision", st.configRevision)
			sp.SetAttr("relayops.node_id", g.NodeID())
			if st.cohort != "" {
				sp.SetAttr("relayops.release.cohort", st.cohort)
			}
			if st.route != nil {
				sp.Name = r.Method + " " + st.route.API.BasePath
				sp.SetAttr("http.route", st.route.API.BasePath)
				sp.SetAttr("relayops.api.name", st.route.API.Name)
			}
			if st.consumerName != "" {
				sp.SetAttr("relayops.consumer.name", st.consumerName)
			}
			if status >= 500 {
				sp.SetStatus(tracing.StatusError, st.decisionReason)
			}
			sp.End()
		}
		if st.route != nil {
			id := st.route.API.ID
			l.APIID, l.APIName = &id, st.route.API.Name
			l.TenantID = st.route.API.TenantID
		}
		if st.consumerID != "" {
			cid := st.consumerID
			l.ConsumerID = &cid
		}
		g.collector.Record(l)
	}()

	// Ingress security: protect internal deterministic test cohort and revision overrides.
	// Only honor if:
	// 1. Direct TCP peer (RemoteAddr, NOT spoofable XFF) is loopback or local private network, AND
	// 2. Caller presents a valid internal runner token matching runnerSecret (or loopback fallback).
	targetCohort := r.Header.Get("X-RelayOps-Target-Cohort")
	targetRev := r.Header.Get("X-RelayOps-Target-Revision")
	runnerTok := r.Header.Get("X-RelayOps-Runner-Token")

	authorizedRunner := false
	peerIP := directPeerIP(r)
	if isLoopbackOrLocal(peerIP) {
		if g.runnerSecret != "" && subtle.ConstantTimeCompare([]byte(runnerTok), []byte(g.runnerSecret)) == 1 {
			authorizedRunner = true
		} else if g.runnerSecret == "" && isLoopbackOnly(peerIP) {
			authorizedRunner = true
		}
	}

	if !authorizedRunner {
		if targetCohort != "" {
			r.Header.Del("X-RelayOps-Target-Cohort")
		}
		if targetRev != "" {
			r.Header.Del("X-RelayOps-Target-Revision")
		}
	}
	r.Header.Del("X-RelayOps-Runner-Token") // never leak runner token upstream

	snap := g.snap.Load()
	if snap != nil && snap.Canary != nil {
		snap = snap.ForRequest(r, clientIP(r))
		st.cohort = "stable"
		if snap.IsCanary {
			st.cohort = "canary"
		}
		rec.Header().Set("X-RelayOps-Cohort", st.cohort)
	}
	if snap == nil {
		st.decisionPolicy = "system"
		st.decisionReason = "gateway_not_ready"
		g.reject(rec, st, http.StatusServiceUnavailable, "not_ready", "gateway configuration not loaded yet")
		return
	}
	// Internal targeting is consumed here, never forwarded to an API upstream.
	r.Header.Del("X-RelayOps-Target-Cohort")
	r.Header.Del("X-RelayOps-Target-Revision")
	st.configRevision = snapVersion(snap)
	rec.Header().Set("X-RelayOps-Revision", fmt.Sprintf("rev_%010d", st.configRevision))

	route := snap.Match(r.URL.Path)
	if route == nil {
		st.decisionPolicy = "routing"
		st.decisionReason = "no_route_matched"
		g.reject(rec, st, http.StatusNotFound, "no_route", "no API matches path "+r.URL.Path)
		return
	}
	st.route = route
	st.matchedRoute = route.API.BasePath
	st.policyEvaluations = map[string]any{
		"routing": map[string]any{"matched": true, "base_path": route.API.BasePath, "upstream": route.Upstream.String()},
	}
	api := &route.API

	st.isGRPC = grpc.IsGRPCRequest(r) || api.Protocol == "grpc"
	st.isGraphQL = api.Protocol == "graphql"
	if api.Protocol == "mcp" && r.Method == http.MethodPost {
		if !g.parseMCP(rec, r, st, api) {
			return
		}
	}

	// gRPC Inspection & Policy Enforcement
	if st.isGRPC {
		st.grpc = &grpcExchange{}
		if mode := grpcWebMode(r.Header.Get("Content-Type")); mode != "" {
			st.grpc.setupWeb(r, mode)
		} else if api.GRPCPolicy.JSONTranscoding && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			st.grpc.wantJSON = true
		}
		service, method, err := grpc.ParseMethod(r.URL.Path)
		ev := map[string]any{"service": service, "method": method}
		if st.grpc.web != "" {
			ev["transport"] = "grpc-web-" + st.grpc.web
		} else if st.grpc.wantJSON {
			ev["transport"] = "json"
		}
		if err == nil && service != "" {
			st.matchedRoute = fmt.Sprintf("%s/%s", service, method)
			st.policyEvaluations["grpc"] = ev
		}
		reflection := strings.HasPrefix(service, "grpc.reflection")
		if route.grpcMethods != nil && !reflection {
			info, known := route.grpcMethods[service+"/"+method]
			if !known {
				st.decisionPolicy, st.decisionReason = "grpc", "unknown_method"
				g.reject(rec, st, http.StatusNotImplemented, "unknown_method",
					fmt.Sprintf("method %s/%s is not in this API's descriptor set", service, method))
				return
			}
			ev["kind"] = info.Kind()
		}
		if !api.GRPCPolicy.AllowReflection && reflection {
			st.decisionPolicy = "grpc"
			st.decisionReason = "reflection_disabled"
			g.reject(rec, st, http.StatusForbidden, "reflection_disabled", "gRPC server reflection is disabled for this API")
			return
		}
		// Enforce the message limit per message, on streams too, as frames
		// pass through.
		limit := api.GRPCPolicy.MaxMessageSizeBytes
		if limit <= 0 {
			limit = defaultGRPCMaxMessage
		}
		if st.grpc.wantJSON {
			info, ok := route.grpcMethods[service+"/"+method]
			if !ok || info.Desc == nil {
				st.decisionPolicy, st.decisionReason = "grpc", "transcoding_unavailable"
				g.reject(rec, st, http.StatusNotImplemented, "transcoding_unavailable", "JSON transcoding needs the method in the API's grpc_descriptor_set")
				return
			}
			if err := st.grpc.setupJSON(r, info.Desc, limit); err != nil {
				st.decisionPolicy, st.decisionReason = "grpc", "invalid_json_request"
				g.reject(rec, st, http.StatusBadRequest, "invalid_json_request", err.Error())
				return
			}
		} else if r.Body != nil && r.Body != http.NoBody {
			st.grpc.req = grpc.NewMessageReader(r.Body, limit)
			r.Body = st.grpc.req
		}
	}

	// GraphQL: parse each operation, validate it against the schema when
	// configured, and apply depth, cost, alias, introspection, mutation and
	// allowlist policy before anything reaches the upstream.
	if st.isGraphQL && isWebSocketUpgrade(r) {
		// Operations arrive inside the WebSocket; each is checked as the
		// client starts it (see graphql_ws.go). Compression is not offered,
		// so messages can be read.
		r.Header.Del("Sec-WebSocket-Extensions")
		st.gqlWS = &gqlWSCheck{g: g, api: api, route: route, st: st}
		st.policyEvaluations["graphql"] = map[string]any{"transport": "websocket"}
	} else if st.isGraphQL {
		pol := api.GraphQLPolicy
		refuse := func(reason, msg string) {
			st.decisionPolicy, st.decisionReason = "graphql", reason
			g.reject(rec, st, http.StatusBadRequest, reason, msg)
		}
		reqs, batch, err := graphql.ExtractRequests(r, pol.MaxBatchSize)
		if err != nil {
			refuse("invalid_graphql_request", err.Error())
			return
		}
		var evals []map[string]any
		for i := range reqs {
			gqlReq := &reqs[i]
			// Persisted queries: verify the hash, or use the verified text.
			if err := graphql.ResolvePersisted(gqlReq, g.persistedCache(api.ID)); err != nil {
				st.policyEvaluations["graphql"] = map[string]any{"persisted_query": err.Error()}
				if errors.Is(err, graphql.ErrPersistedQueryNotFound) {
					st.decisionPolicy, st.decisionReason = "graphql", "persisted_query_not_found"
					writeJSON(rec, http.StatusOK, graphql.PersistedQueryNotFoundResponse)
					return
				}
				refuse("persisted_query_mismatch", err.Error())
				return
			}
			analysis, err := graphql.AnalyzeOperation(gqlReq.Query, gqlReq.OperationName, gqlReq.Variables, pol.ListSizeArguments)
			ev := map[string]any{
				"operation_name": analysis.OperationName,
				"operation_type": analysis.OperationType,
				"depth":          analysis.Depth,
				"cost":           analysis.Cost,
				"aliases":        analysis.Aliases,
				"introspection":  analysis.IsIntrospection,
				"query_hash":     analysis.Hash,
			}
			evals = append(evals, ev)
			if batch {
				st.policyEvaluations["graphql"] = map[string]any{"batch": evals}
			} else {
				st.policyEvaluations["graphql"] = ev
			}
			if err != nil {
				refuse("invalid_graphql_request", err.Error())
				return
			}
			if !batch && analysis.OperationName != "" {
				st.matchedRoute = fmt.Sprintf("%s (%s %s)", api.BasePath, analysis.OperationType, analysis.OperationName)
			}
			// Mutations over GET would be open to cross-site request forgery.
			if r.Method == http.MethodGet && analysis.OperationType != "query" {
				st.decisionPolicy, st.decisionReason = "graphql", "mutation_over_get"
				g.reject(rec, st, http.StatusMethodNotAllowed, "mutation_over_get", "GraphQL "+analysis.OperationType+" operations must use POST")
				return
			}
			if err := graphql.ValidatePolicy(gqlReq, analysis, pol); err != nil {
				refuse("graphql_policy_violation", err.Error())
				return
			}
			if route.graphqlSchema != nil {
				if errs := graphql.ValidateAgainstSchema(route.graphqlSchema, gqlReq.Query); len(errs) > 0 {
					ev["validation_errors"] = errs
					refuse("graphql_validation_failed", strings.Join(errs, "; "))
					return
				}
			}
			g.persistedCache(api.ID).Remember(gqlReq)
		}
	}

	// If AI API, safely sample request body to preserve original stream without truncation
	if api.IsAI && r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 5<<20)) // 5MB max sample
		if err == nil && len(bodyBytes) > 0 {
			// Lossless stream reconstruction: multi-reader prepends the sample and streams remainder seamlessly
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bodyBytes), r.Body))
			var aiReq struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(bodyBytes, &aiReq) == nil && aiReq.Model != "" {
				st.model = aiReq.Model
			}
			// Approximate prompt tokens if not reported: ~4 chars per token
			st.tokensPrompt = len(bodyBytes) / 4
			st.aiProvenance = string(ai.ProvenanceEstimated)
		}
	}

	// --- CORS ---------------------------------------------------------------
	if api.CORSEnabled {
		if origin := r.Header.Get("Origin"); origin != "" {
			h := rec.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Credentials", "true")
			expose := "X-Request-ID, X-RateLimit-Limit, X-RateLimit-Remaining, X-Quota-Day-Limit, X-Quota-Day-Remaining, X-Quota-Month-Limit, X-Quota-Month-Remaining, X-RelayOps-Degraded, Retry-After"
			if st.isGRPC {
				expose += ", Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin" // gRPC-Web clients read these
			}
			h.Set("Access-Control-Expose-Headers", expose)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				if reqH := r.Header.Get("Access-Control-Request-Headers"); reqH != "" {
					h.Set("Access-Control-Allow-Headers", reqH)
				}
				h.Set("Access-Control-Max-Age", "600")
				rec.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}

	// --- Unified Policy Pipeline Evaluation ---
	g.bindClientCertHeaders(r)

	pReq := policy.Request{
		Method:      r.Method,
		Path:        r.URL.Path,
		Header:      r.Header,
		QueryParams: r.URL.Query(),
		ClientIP:    clientIP(r),
	}
	limiter := &LimiterAdapter{gw: g}
	dec := g.evaluator.Evaluate(r.Context(), pReq, api, snap, limiter, false)

	st.decisionPolicy = dec.Policy
	st.decisionReason = dec.Reason
	// Merge: protocol inspection (GraphQL, gRPC, MCP) recorded its findings
	// before authentication, and they belong in the same decision trail.
	for k, v := range dec.Evaluations {
		st.policyEvaluations[k] = v
	}
	if dec.ConsumerID != "" {
		st.consumerID = dec.ConsumerID
	}
	if dec.ConsumerName != "" {
		st.consumerName = dec.ConsumerName // API-key consumer name, or the verified JWT/OIDC subject
	}
	st.authStatus, st.subscriptionStatus, st.rateLimitStatus = decisionStatuses(dec)

	if !dec.Allowed {
		if dec.RateLimitLimit > 0 {
			rec.Header().Set("X-RateLimit-Limit", strconv.Itoa(dec.RateLimitLimit))
			rec.Header().Set("X-RateLimit-Remaining", strconv.Itoa(dec.RateLimitRemaining))
		}
		if dec.QuotaDayLimit > 0 {
			rec.Header().Set("X-Quota-Day-Limit", strconv.Itoa(dec.QuotaDayLimit))
			rec.Header().Set("X-Quota-Day-Remaining", strconv.Itoa(dec.QuotaDayRemaining))
		}
		if dec.Policy == "rate_limit" {
			rec.Header().Set("Retry-After", "60")
		}
		g.reject(rec, st, dec.Status, dec.Reason, dec.Detail)
		return
	}

	if dec.RateLimitLimit > 0 {
		rec.Header().Set("X-RateLimit-Limit", strconv.Itoa(dec.RateLimitLimit))
		rec.Header().Set("X-RateLimit-Remaining", strconv.Itoa(dec.RateLimitRemaining))
	}
	if dec.QuotaDayLimit > 0 {
		rec.Header().Set("X-Quota-Day-Limit", strconv.Itoa(dec.QuotaDayLimit))
		rec.Header().Set("X-Quota-Day-Remaining", strconv.Itoa(dec.QuotaDayRemaining))
	}
	if dec.QuotaMonthLimit > 0 {
		rec.Header().Set("X-Quota-Month-Limit", strconv.Itoa(dec.QuotaMonthLimit))
		rec.Header().Set("X-Quota-Month-Remaining", strconv.Itoa(dec.QuotaMonthRemaining))
	}
	if dec.Degraded {
		rec.Header().Set("X-RelayOps-Degraded", "true")
	}

	// gRPC method rules (who may call which method).
	if st.isGRPC && len(api.GRPCPolicy.Rules) > 0 || st.isGRPC && api.GRPCPolicy.DefaultAction == "deny" {
		if service, method, err := grpc.ParseMethod(r.URL.Path); err == nil {
			if ok, rule := api.GRPCPolicy.Allows(service+"/"+method, st.consumerID, dec.PlanName); !ok {
				st.decisionPolicy, st.decisionReason = "grpc", "method_not_allowed"
				if ev, ok := st.policyEvaluations["grpc"].(map[string]any); ok {
					ev["refused_by"] = rule
				}
				g.reject(rec, st, http.StatusForbidden, "method_not_allowed",
					fmt.Sprintf("method %s/%s is not allowed for this caller", service, method))
				return
			}
		}
	}

	// MCP tool authorisation, pinning and per-tool limits.
	if st.mcp != nil {
		if !g.authorizeMCP(rec, r, st, api, dec.PlanName) {
			return
		}
	}

	// WASM policy plugins (traffic_policy.wasm_plugins), before any budget is reserved.
	if len(api.TrafficPolicy.WasmPlugins) > 0 {
		if !g.runWasmPlugins(rec, r, st, api.TrafficPolicy, pReq) {
			return
		}
	}

	// AI Budget Reservation & Admission Control
	if api.IsAI && g.aiBudgetLedger != nil && st.consumerID != "" {
		acc, err := g.aiBudgetLedger.GetAIBudgetAccountByConsumer(r.Context(), api.TenantID, st.consumerID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				st.decisionPolicy = "ai_budget"
				st.decisionReason = "ledger_unavailable"
				g.reject(rec, st, http.StatusBadGateway, "ai_budget_unavailable", "AI budget ledger unavailable")
				return
			}
		} else if acc != nil {
			const initialEstimateCents = 2
			_, rerr := g.aiBudgetLedger.ReserveAIBudget(r.Context(), acc.ID, st.requestID, initialEstimateCents)
			if rerr != nil {
				if errors.Is(rerr, store.ErrBudgetExceeded) || acc.StrictEnforcement {
					st.decisionPolicy = "ai_budget"
					st.decisionReason = "budget_exceeded"
					g.reject(rec, st, http.StatusTooManyRequests, "ai_budget_exceeded", "monthly AI budget limit reached for consumer")
					return
				}
			} else {
				st.aiBudgetReserved = true
				if st.policyEvaluations == nil {
					st.policyEvaluations = make(map[string]any)
				}
				st.policyEvaluations["ai_budget"] = map[string]any{
					"account_id": acc.ID,
					"reserved":   true,
				}
			}
		}
	}

	// --- Proxy ----------------------------------------------------------------
	st.decisionPolicy = "proxy"
	st.decisionReason = "proxied_successfully"
	rec.Header().Set("X-RelayOps-Decision-Policy", "proxy")
	rec.Header().Set("X-RelayOps-Decision-Reason", "proxied_successfully")

	out := r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))
	if api.StripPath && api.BasePath != "/" {
		u := *r.URL
		p := strings.TrimPrefix(r.URL.Path, api.BasePath)
		// The exact base path maps to the upstream URL itself (no trailing
		// slash added); "/base/" still maps to "<upstream>/".
		if p != "" && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u.Path, u.RawPath = p, ""
		out.URL = &u
	}
	st.inURL = out.URL
	st.upstreamStart = time.Now()
	if st.grpc != nil {
		g.serveGRPC(rec, out, st)
	} else {
		g.proxy.ServeHTTP(rec, out)
	}
}

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	st := pr.In.Context().Value(ctxKey{}).(*reqState)
	api := &st.route.API
	pr.SetURL(st.route.Upstream)
	pr.SetXForwarded()
	h := pr.Out.Header
	h.Set("X-Request-ID", st.requestID)
	h.Del("X-API-Key") // never leak credentials upstream
	if api.TrafficPolicy.WasmResponseBodyLimitBytes > 0 && len(g.responsePlugins(api.TrafficPolicy)) > 0 {
		// Response plugins read the body: let the transport negotiate (and
		// transparently decode) compression instead of the client.
		h.Del("Accept-Encoding")
	}
	if q := pr.Out.URL.Query(); q.Has("apikey") {
		q.Del("apikey")
		pr.Out.URL.RawQuery = q.Encode()
	}
	if st.consumerID != "" {
		h.Set("X-Consumer-ID", st.consumerID)
	}
	if st.consumerName != "" {
		h.Set("X-Consumer-Name", st.consumerName)
	}
	for k, v := range api.RequestHeaders {
		// Dynamically resolve secrets like ${secret:NVIDIA_API_KEY}
		resolved := config.ResolveSecrets(v)
		h.Set(k, resolved)
	}
}

func (g *Gateway) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	st, _ := r.Context().Value(ctxKey{}).(*reqState)
	var to *trailersOnly
	if errors.As(err, &to) {
		writeTrailersOnly(w, to)
		return
	}
	// A request message over the limit aborted the upstream call: report
	// that, not an upstream failure.
	if st != nil && st.grpc != nil {
		if lerr := st.grpc.limitErr(); lerr != nil {
			st.decisionPolicy, st.decisionReason, st.err = "grpc", "message_too_large", lerr.Error()
			g.reject(w, st, http.StatusRequestEntityTooLarge, "message_too_large", lerr.Error())
			return
		}
	}
	status, code := http.StatusBadGateway, "upstream_unavailable"
	var ne net.Error
	switch {
	case errors.Is(r.Context().Err(), context.Canceled):
		status, code = 499, "client_closed_request"
	case errors.Is(err, ErrCircuitOpen):
		status, code = http.StatusServiceUnavailable, "upstream_circuit_open"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		status, code = http.StatusGatewayTimeout, "upstream_timeout"
	}
	if st != nil {
		st.err = err.Error()
		st.decisionPolicy = "upstream"
		st.decisionReason = code
		w.Header().Set("X-RelayOps-Decision-Policy", "upstream")
		w.Header().Set("X-RelayOps-Decision-Reason", code)
		if !st.upstreamStart.IsZero() {
			st.upstreamDurationMS = float64(time.Since(st.upstreamStart).Microseconds()) / 1000
		}
		if st.isGRPC && (st.grpc == nil || !st.grpc.wantJSON) {
			if st.grpc != nil && st.grpc.web != "" {
				writeGRPCWebError(w, st.grpc, grpcStatusFromHTTP(status), "upstream request failed: "+code)
				return
			}
			grpc.WriteGRPCError(w, grpcStatusFromHTTP(status), "upstream request failed: "+code)
			return
		}
	}
	writeError(w, status, code, "upstream request failed", st)
}

func (g *Gateway) reject(w http.ResponseWriter, st *reqState, status int, code, msg string) {
	if st != nil {
		st.err = code + ": " + msg
		if st.decisionReason == "" || st.decisionReason == "processing" {
			st.decisionReason = code
		}
		if st.decisionPolicy != "" {
			w.Header().Set("X-RelayOps-Decision-Policy", st.decisionPolicy)
		}
		if st.decisionReason != "" {
			w.Header().Set("X-RelayOps-Decision-Reason", st.decisionReason)
		}
		if st.configRevision > 0 {
			w.Header().Set("X-RelayOps-Revision", fmt.Sprintf("rev_%010d", st.configRevision))
		}
		if st.isGRPC && (st.grpc == nil || !st.grpc.wantJSON) {
			if st.grpc != nil && st.grpc.web != "" {
				writeGRPCWebError(w, st.grpc, grpcStatusFromHTTP(status), msg)
				return
			}
			grpc.WriteGRPCError(w, grpcStatusFromHTTP(status), msg)
			return
		}
		if st.isGraphQL {
			writeJSON(w, status, graphql.FormatGraphQLError(msg))
			return
		}
		if st.mcp != nil {
			writeMCPError(w, status, st.mcp, code, msg)
			return
		}
	}
	writeError(w, status, code, msg, st)
}


func snapVersion(s *Snapshot) int64 {
	if s == nil {
		return 0
	}
	return s.Version
}

// ---------------------------------------------------------------------------
// Transports & Connection Pooling
// ---------------------------------------------------------------------------

type routeTransport struct{ g *Gateway }

// RoundTrip sends the request to a target chosen by the route's upstream pool,
// retrying on another target according to the API's retry policy and feeding
// outcomes into the circuit breaker. Every attempt gets its own CLIENT span.
func (t routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g := t.g
	st, _ := req.Context().Value(ctxKey{}).(*reqState)
	if st == nil || st.route == nil || st.route.Pool == nil {
		return g.transportFor(30000).RoundTrip(req)
	}
	pool := st.route.Pool
	base := g.transportFor(st.route.API.TimeoutMS)

	attempts := 1
	rp := pool.retryPolicy()
	// gRPC calls go out once: a stream cannot be replayed, and gRPC errors
	// arrive in trailers, not as retryable HTTP statuses.
	if rp != nil && rp.Attempts > 1 && !st.isGRPC {
		attempts = rp.Attempts
		if (!isIdempotent(req.Method) && !rp.RetryNonIdempotent) || req.Header.Get("Upgrade") != "" {
			attempts = 1
		}
	}

	var body []byte
	if attempts > 1 && req.Body != nil && req.Body != http.NoBody {
		buf, err := io.ReadAll(io.LimitReader(req.Body, maxRetryBody+1))
		if err != nil {
			return nil, err
		}
		if len(buf) > maxRetryBody {
			// Too large to replay: stream it through once without retries.
			req.Body = readCloser{io.MultiReader(bytes.NewReader(buf), req.Body), req.Body}
			attempts = 1
		} else {
			_ = req.Body.Close()
			body = buf
		}
	}

	tried := map[*targetState]bool{}
	defer func() {
		if !st.upstreamStart.IsZero() {
			st.upstreamDurationMS = float64(time.Since(st.upstreamStart).Microseconds()) / 1000
		}
	}()

	for i := 0; i < attempts; i++ {
		tgt, err := pool.pick(tried)
		if err != nil {
			st.upstreamAttempts = append(st.upstreamAttempts, map[string]any{"attempt": i + 1, "error": err.Error()})
			return nil, err
		}
		tried[tgt] = true
		if i > 0 {
			tgt.retriesTotal.Add(1)
		}

		out := req.Clone(req.Context())
		if st.inURL != nil {
			out.URL = targetURL(tgt.url, st.inURL, req.URL.RawQuery)
		}
		out.Host = ""
		rt := http.RoundTripper(base)
		switch out.URL.Scheme {
		case "h2c", "grpc":
			// Cleartext HTTP/2 with prior knowledge, as gRPC servers expect.
			out.URL.Scheme = "http"
			rt = g.h2cTransportFor(st.route.API.TimeoutMS)
		case "grpcs":
			out.URL.Scheme = "https"
		case "http":
			if st.isGRPC {
				rt = g.h2cTransportFor(st.route.API.TimeoutMS)
			}
		}
		if body != nil {
			out.Body = io.NopCloser(bytes.NewReader(body))
			out.ContentLength = int64(len(body))
			out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}

		span := g.tracer.StartChild(st.span, req.Method+" upstream", tracing.KindClient)
		if span != nil {
			out.Header.Set("traceparent", span.Context.Traceparent())
			span.SetAttr("http.request.method", req.Method)
			span.SetAttr("url.full", out.URL.String())
			span.SetAttr("server.address", out.URL.Host)
			span.SetAttr("http.request.resend_count", i)
		}

		start := time.Now()
		tgt.inflight.Add(1)
		resp, rerr := rt.RoundTrip(out)
		tgt.inflight.Add(-1)
		elapsed := float64(time.Since(start).Microseconds()) / 1000

		clientGone := rerr != nil && req.Context().Err() != nil
		failed := rerr != nil || isBreakerFailure(resp.StatusCode)
		errMsg := ""
		if rerr != nil {
			errMsg = rerr.Error()
		} else if failed {
			errMsg = fmt.Sprintf("upstream returned %d", resp.StatusCode)
		}
		if clientGone {
			pool.release(tgt)
		} else {
			pool.report(tgt, !failed, errMsg)
		}

		attempt := map[string]any{"attempt": i + 1, "target": tgt.rawURL, "duration_ms": elapsed}
		if rerr != nil {
			attempt["error"] = rerr.Error()
		} else {
			attempt["status"] = resp.StatusCode
		}
		st.upstreamAttempts = append(st.upstreamAttempts, attempt)

		if span != nil {
			if rerr != nil {
				span.SetStatus(tracing.StatusError, rerr.Error())
			} else {
				span.SetAttr("http.response.status_code", resp.StatusCode)
				if resp.StatusCode >= 500 {
					span.SetStatus(tracing.StatusError, http.StatusText(resp.StatusCode))
				}
			}
			span.End()
		}

		retryable := i < attempts-1 && !clientGone && rp != nil &&
			(rerr != nil || statusIn(resp.StatusCode, rp.RetryOnStatus))
		if !retryable {
			return resp, rerr
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
		backoff := time.Duration(rp.BackoffMS) * time.Millisecond << i
		if backoff > 0 {
			backoff += time.Duration(mrand.Int63n(int64(backoff)/2 + 1))
			timer := time.NewTimer(backoff)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
		}
	}
	return nil, errors.New("upstream retries exhausted")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// targetURL builds the outbound URL for target: the target's base path joined
// with the (stripped) request path, keeping the already-sanitised query string.
func targetURL(target, in *url.URL, rawQuery string) *url.URL {
	out := *in
	out.Scheme = target.Scheme
	out.Host = target.Host
	out.User = nil
	out.Path, out.RawPath = joinURLPath(target, in)
	out.RawQuery = rawQuery
	return &out
}

// transportKey selects a shared upstream transport.
type transportKey struct {
	timeoutMS int
	h2c       bool // cleartext HTTP/2 with prior knowledge (gRPC over plain TCP)
}

func (g *Gateway) transportFor(timeoutMS int) *http.Transport {
	return g.transport(transportKey{timeoutMS: timeoutMS})
}

func (g *Gateway) h2cTransportFor(timeoutMS int) *http.Transport {
	return g.transport(transportKey{timeoutMS: timeoutMS, h2c: true})
}

func (g *Gateway) transport(key transportKey) *http.Transport {
	timeoutMS := key.timeoutMS
	g.tmu.Lock()
	defer g.tmu.Unlock()
	if t, ok := g.transports[key]; ok {
		return t
	}
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: time.Duration(timeoutMS) * time.Millisecond,
	}
	if key.h2c {
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		t.Protocols = p
	}
	g.transports[key] = t
	return t
}

// ---------------------------------------------------------------------------
// Recorder with Accurate AI Streaming Usage Inspection
// ---------------------------------------------------------------------------

type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
	st          *reqState
	aiMeter     *ai.StreamMeter
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)

	// If this is an AI route, incrementally feed the stateful stream meter
	if r.st != nil && r.st.route != nil && r.st.route.API.IsAI && r.aiMeter != nil {
		r.aiMeter.Feed(b)
	}

	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeError(w http.ResponseWriter, status int, code, msg string, st *reqState) {
	body := map[string]any{"error": code, "message": msg}
	if st != nil {
		body["request_id"] = st.requestID
		body["decision"] = st.decisionReason
		if st.decisionPolicy != "" {
			body["policy"] = st.decisionPolicy
		}
		if st.configRevision > 0 {
			body["revision"] = st.configRevision
		}
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopbackOrLocal(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr == "localhost"
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func isLoopbackOnly(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr == "localhost"
	}
	return ip.IsLoopback()
}

func directPeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newRequestID uses the runtime's ChaCha8 generator: request IDs correlate
// logs and are not secrets, and crypto/rand costs a syscall per request.
func newRequestID() string {
	var b [12]byte
	binary.LittleEndian.PutUint64(b[:8], rand2.Uint64())
	binary.LittleEndian.PutUint32(b[8:], rand2.Uint32())
	return hex.EncodeToString(b[:])
}

// proxyBuffers pools the reverse proxy's response copy buffers.
type proxyBuffers struct{}

var proxyBufferPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

func (proxyBuffers) Get() []byte  { return *(proxyBufferPool.Get().(*[]byte)) }
func (proxyBuffers) Put(b []byte) { proxyBufferPool.Put(&b) }
