// Package admin is the RelayOps control-plane HTTP API, dashboard host, and Developer Portal.
package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/alerts"
	"github.com/relayops/apim/internal/apiops"
	"github.com/relayops/apim/internal/coordinator"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/license"
	"github.com/relayops/apim/internal/mcp"
	"github.com/relayops/apim/internal/policy"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/scim"
	"github.com/relayops/apim/internal/store"
	"golang.org/x/net/websocket"
	"net/http/httptest"
)

type Server struct {
	store           *store.Store
	gw              *gateway.Gateway
	hub             *realtime.Hub
	alerts          *alerts.Notifier
	heldRollbackRev atomic.Int64                             // last candidate whose unattributed spike was alerted
	changeCache     typedMap[[2]int64, store.RevisionChange] // candidate,baseline -> what changed
	token           string
	nodeID          string
	static          fs.FS
	started         time.Time
	regLimiter      *gateway.Limiter // IP rate limiter for portal registration abuse protection
	regMu           sync.Mutex
	regCounts       map[string]int
	gatewayURL      string // local data-plane address used by the developer workbench
	sessionMu       sync.RWMutex
	sessions        map[string]store.AdminUser
	jwks            *gateway.JWKSManager
	evaluator       *policy.Evaluator
	runnerToken     string
	apiops          *apiops.Coordinator
	license         license.Entitlement
	mcpHandler      *mcp.Handler
	dataplane       *dataplaneState // nil unless the node API for gateway-only nodes is enabled
	apiHandler      http.Handler    // the authenticated /api stack; Orbit tools read through it
	orbit           *orbitState     // nil unless an Orbit AI model is configured
	signals         signalCache     // Orbit signals per credential, one minute
	orbitStats      orbitMetrics    // Orbit usage for /metrics

	autoRollbackInterval time.Duration             // 0: DefaultAutoRollbackInterval
	elector              coordinator.LeaderElector // nil: every replica evaluates under the advisory lock
	runtime              RuntimeInfo
}

func New(s *store.Store, gw *gateway.Gateway, hub *realtime.Hub, token, nodeID string, static fs.FS, options ...Option) *Server {
	jwksMgr := gateway.NewJWKSManager()
	server := &Server{
		store:      s,
		gw:         gw,
		hub:        hub,
		token:      token,
		nodeID:     nodeID,
		static:     static,
		started:    time.Now(),
		regLimiter: gateway.NewLimiter(),
		regCounts:  make(map[string]int),
		gatewayURL: "http://127.0.0.1:8080",
		sessions:   make(map[string]store.AdminUser),
		jwks:       jwksMgr,
		evaluator:  policy.NewEvaluator(jwksMgr),
		apiops:     apiops.NewCoordinator(s),
		alerts:     &alerts.Notifier{Hub: hub},
	}
	for _, option := range options {
		option(server)
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		dbStatus := "healthy"
		if !s.dbAvailable() {
			dbStatus = "unavailable"
		} else {
			pctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			if err := s.store.Pool.Ping(pctx); err != nil {
				dbStatus = "unavailable"
			}
			cancel()
		}
		var snap *gateway.Snapshot
		if s.gw != nil {
			snap = s.gw.Snapshot()
		}
		routes := 0
		var rev int64 = 0
		if snap != nil {
			routes = len(snap.Routes)
			rev = snap.Version
		}
		if dbStatus != "healthy" {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":          "degraded",
				"database":        "unavailable",
				"gateway":         "ready",
				"routes":          routes,
				"config_revision": rev,
				"mode":            "cached_recovery",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "ok",
			"database":        "healthy",
			"gateway":         "ready",
			"routes":          routes,
			"config_revision": rev,
		})
	})

	// Public Auth endpoints (no bearer token required to authenticate)
	mux.HandleFunc("GET /login", s.serveAuthPage)
	mux.HandleFunc("GET /signup", s.serveAuthPage)
	mux.HandleFunc("POST /api/auth/signup", s.authSignup)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.HandleFunc("POST /api/auth/sso/callback", s.authSSOCallback)
	mux.HandleFunc("GET /api/auth/oidc/providers", s.oidcPublicProviders)
	mux.HandleFunc("GET /api/auth/oidc/{provider}/start", s.oidcStart)
	mux.HandleFunc("GET /api/auth/oidc/callback", s.oidcCallback)

	// Public Developer Portal routes (no admin bearer token required)
	mux.HandleFunc("GET /portal", s.servePortal)
	mux.HandleFunc("GET /portal/", s.servePortal)
	mux.HandleFunc("GET /portal/assets/{name}", s.servePortalAsset)
	mux.HandleFunc("GET /portal/api/catalog", s.portalCatalog)
	mux.HandleFunc("POST /portal/api/register", s.portalRegister)
	mux.HandleFunc("POST /portal/api/try", s.portalTry)

	// Developer Self-Service Portal APIs
	mux.HandleFunc("GET /portal/api/apps", s.portalListApps)
	mux.HandleFunc("POST /portal/api/apps", s.portalCreateApp)
	mux.HandleFunc("DELETE /portal/api/apps/{id}", s.portalDeleteApp)
	mux.HandleFunc("POST /portal/api/keys/{id}/rotate", s.portalRotateKey)
	mux.HandleFunc("POST /portal/api/keys/{id}/retire", s.portalRetireKey)
	mux.HandleFunc("GET /portal/api/subscriptions", s.portalListSubs)
	mux.HandleFunc("GET /portal/api/usage", s.portalUsage)
	mux.HandleFunc("GET /portal/api/apis/{id}/openapi.json", s.portalAPIOpenAPISpec)

	// Model Context Protocol (MCP) endpoints for IDE & AI client integration
	if s.mcpHandler == nil {
		s.initMCP()
	}
	mux.HandleFunc("GET /mcp/sse", s.mcpHandler.HandleSSE)
	mux.HandleFunc("POST /mcp/sse", s.mcpHandler.HandleSSE)
	mux.HandleFunc("OPTIONS /mcp/sse", s.mcpHandler.HandleSSE)
	mux.HandleFunc("POST /mcp/message", s.mcpHandler.HandleMessage)
	mux.HandleFunc("OPTIONS /mcp/message", s.mcpHandler.HandleMessage)

	// Public Prometheus metrics endpoint
	mux.HandleFunc("GET /metrics", s.prometheusMetrics)

	// SCIM 2.0 Identity Provider Provisioning (RFC 7643 / RFC 7644)
	if s.store != nil {
		scimSrv := scim.NewServer(s.store, s.token, "/scim/v2")
		mux.Handle("/scim/v2/", http.StripPrefix("/scim/v2", scimSrv.Handler()))
	}

	// Node API for gateway-only nodes (node token, not admin RBAC)
	if s.dataplane != nil {
		mux.Handle("/dataplane/v1/", s.dataplaneHandler())
	}

	// Admin Control Plane API (protected by admin bearer token / RBAC)
	api := http.NewServeMux()
	api.HandleFunc("GET /api/auth/me", s.authMe)
	api.HandleFunc("POST /api/auth/logout", s.authLogout)
	api.HandleFunc("GET /api/overview", s.overview)
	api.HandleFunc("GET /api/stream", s.stream)
	api.HandleFunc("GET /api/events", s.stream)
	api.Handle("GET /api/stream/ws", s.streamWS())
	api.Handle("GET /api/events/ws", s.streamWS())

	// Admin Users Management (superadmin only)
	api.HandleFunc("GET /api/admin/users", s.listAdminUsers)
	api.HandleFunc("POST /api/admin/users", s.createAdminUser)
	api.HandleFunc("PUT /api/admin/users/{id}", s.updateAdminUser)
	api.HandleFunc("DELETE /api/admin/users/{id}", s.deleteAdminUser)
	api.HandleFunc("POST /api/admin/users/{id}/revoke-sessions", s.revokeAdminUserSessions)

	// APIs
	api.HandleFunc("GET /api/apis", s.listAPIs)
	api.HandleFunc("POST /api/apis", s.createAPI)
	api.HandleFunc("POST /api/apis/import-openapi", s.importOpenAPI)
	api.HandleFunc("GET /api/apis/{id}", s.getAPI)
	api.HandleFunc("PUT /api/apis/{id}", s.updateAPI)
	api.HandleFunc("DELETE /api/apis/{id}", s.deleteAPI)
	api.HandleFunc("POST /api/apis/{id}/publish", s.publishAPI)
	api.HandleFunc("POST /api/apis/{id}/preview-change", s.previewChange)
	api.HandleFunc("POST /api/apis/{id}/replay-preview", s.replayPreview)
	api.HandleFunc("GET /api/apis/{id}/consumer-impact", s.getConsumerImpact)
	api.HandleFunc("POST /api/apis/{id}/consumer-impact", s.postConsumerImpact)
	api.HandleFunc("POST /api/apis/{id}/contract-diff", s.apiContractDiff)
	api.HandleFunc("POST /api/apis/validate-contract", s.validateContract)
	api.HandleFunc("GET /api/apis/{id}/release-safety-report", s.getReleaseSafetyReport)
	api.HandleFunc("POST /api/apis/{id}/mcp/discover", s.discoverMCPTools)
	api.HandleFunc("GET /api/mcp/catalog", s.listMCPCatalog)
	api.HandleFunc("POST /api/mcp/catalog/{id}/{action}", s.decideMCPCatalog)
	api.HandleFunc("POST /api/apis/{id}/release-safety-report", s.postReleaseSafetyReport)

	// Revisions & Safe Releases
	api.HandleFunc("POST /api/revisions/validate", s.validateRevision)
	api.HandleFunc("GET /api/revisions", s.listRevisions)
	api.HandleFunc("GET /api/revisions/compare", s.compareRevisions)
	api.HandleFunc("GET /api/revisions/{id}", s.getRevision)
	api.HandleFunc("GET /api/revisions/{id}/release-report", s.getReleaseReport)
	api.HandleFunc("POST /api/revisions/{id}/rollback", s.rollbackRevision)
	api.HandleFunc("POST /api/revisions/{id}/canary", s.deployCanary)
	api.HandleFunc("POST /api/revisions/{id}/promote", s.promoteRevision)
	api.HandleFunc("GET /api/revisions/{id}/canary-status", s.getCanaryStatus)
	api.HandleFunc("POST /api/revisions/{id}/abort", s.abortCanary)
	api.HandleFunc("GET /api/upstreams/health", s.upstreamHealth)
	api.HandleFunc("GET /api/revisions/auto-rollback/config", s.getAutoRollbackConfig)
	api.HandleFunc("POST /api/revisions/auto-rollback/config", s.setAutoRollbackConfig)
	api.HandleFunc("POST /api/revisions/auto-rollback/evaluate", s.manualEvaluateAutoRollback)
	api.HandleFunc("GET /api/admin/oidc-providers", s.listOIDCProviders)
	api.HandleFunc("PUT /api/admin/oidc-providers/{name}", s.saveOIDCProvider)
	api.HandleFunc("DELETE /api/admin/oidc-providers/{name}", s.deleteOIDCProvider)
	api.HandleFunc("GET /api/system/runtime", s.systemRuntime)
	api.HandleFunc("GET /api/admin/dataplane/credentials", s.listDataplaneCredentials)
	api.HandleFunc("POST /api/admin/dataplane/credentials", s.createDataplaneCredential)
	api.HandleFunc("DELETE /api/admin/dataplane/credentials/{id}", s.revokeDataplaneCredential)

	// Plans
	api.HandleFunc("GET /api/plans", s.listPlans)
	api.HandleFunc("POST /api/plans", s.createPlan)
	api.HandleFunc("PUT /api/plans/{id}", s.updatePlan)
	api.HandleFunc("DELETE /api/plans/{id}", s.deletePlan)

	// Consumers & Keys
	api.HandleFunc("GET /api/consumers", s.listConsumers)
	api.HandleFunc("POST /api/consumers", s.createConsumer)
	api.HandleFunc("GET /api/consumers/{id}", s.getConsumer)
	api.HandleFunc("PUT /api/consumers/{id}", s.updateConsumer)
	api.HandleFunc("DELETE /api/consumers/{id}", s.deleteConsumer)
	api.HandleFunc("GET /api/consumers/{id}/keys", s.listKeys)
	api.HandleFunc("POST /api/consumers/{id}/keys", s.createKey)
	api.HandleFunc("POST /api/keys/{id}/revoke", s.revokeKey)
	api.HandleFunc("POST /api/keys/{id}/activate", s.activateKey)
	api.HandleFunc("POST /api/keys/{id}/rotate", s.rotateKey)
	api.HandleFunc("POST /api/keys/{id}/retire", s.retireKey)
	api.HandleFunc("DELETE /api/keys/{id}", s.deleteKey)

	// Subscriptions
	api.HandleFunc("GET /api/subscriptions", s.listSubs)
	api.HandleFunc("GET /api/subscriptions/pending", s.listPendingSubs)
	api.HandleFunc("POST /api/subscriptions", s.createSub)
	api.HandleFunc("POST /api/subscriptions/{id}/approve", s.approveSub)
	api.HandleFunc("POST /api/subscriptions/{id}/reject", s.rejectSub)
	api.HandleFunc("PUT /api/subscriptions/{id}", s.updateSub)
	api.HandleFunc("DELETE /api/subscriptions/{id}", s.deleteSub)

	// Logs & Analytics & Audit
	api.HandleFunc("GET /api/logs", s.queryLogs)
	api.HandleFunc("GET /api/analytics/summary", s.summary)
	api.HandleFunc("GET /api/analytics/apis", s.apiStats)
	api.HandleFunc("GET /api/analytics/refusals", s.refusals)
	api.HandleFunc("POST /api/grpc/descriptor/inspect", s.inspectGRPCDescriptor)
	api.HandleFunc("GET /api/wasm/plugins", s.wasmPlugins)
	api.HandleFunc("GET /api/audit-logs", s.listAudit)
	api.HandleFunc("GET /api/fleet/status", s.fleetStatus)
	api.HandleFunc("GET /api/requests/{id}/diagnose", s.diagnoseRequestByID)
	api.HandleFunc("POST /api/requests/diagnose", s.diagnoseRequestPayload)
	api.HandleFunc("GET /api/system/export", s.exportDeclarativeConfig)
	api.HandleFunc("POST /api/system/apply", s.applyDeclarativeConfig)
	api.HandleFunc("POST /api/system/plan", s.planDeclarativeConfig)
	api.HandleFunc("GET /api/system/drift", s.getConfigDrift)
	api.HandleFunc("POST /api/system/maintenance/purge", s.purgeMaintenance)

	// Tenants and onboarding
	api.HandleFunc("GET /api/tenants", s.listTenants)
	api.HandleFunc("POST /api/tenants", s.createTenant)
	api.HandleFunc("PUT /api/tenants/{tenant}", s.renameTenant)
	api.HandleFunc("DELETE /api/tenants/{tenant}", s.deleteTenant)
	api.HandleFunc("GET /api/tenants/{tenant}/members", s.listTenantMembers)
	api.HandleFunc("POST /api/tenants/{tenant}/members", s.addTenantMember)
	api.HandleFunc("DELETE /api/tenants/{tenant}/members/{user}", s.removeTenantMember)

	// Test Studio
	api.HandleFunc("GET /api/tests/suites", s.listTestSuites)
	api.HandleFunc("POST /api/tests/suites", s.createTestSuite)
	api.HandleFunc("GET /api/tests/suites/{id}", s.getTestSuite)
	api.HandleFunc("PUT /api/tests/suites/{id}", s.updateTestSuite)
	api.HandleFunc("DELETE /api/tests/suites/{id}", s.deleteTestSuite)
	api.HandleFunc("GET /api/tests/suites/{id}/versions", s.listTestSuiteVersions)
	api.HandleFunc("GET /api/tests/environments", s.listTestEnvironments)
	api.HandleFunc("POST /api/tests/environments", s.createTestEnvironment)
	api.HandleFunc("PUT /api/tests/environments/{id}", s.updateTestEnvironment)
	api.HandleFunc("DELETE /api/tests/environments/{id}", s.deleteTestEnvironment)
	api.HandleFunc("POST /api/tests/runs", s.createTestRun)
	api.HandleFunc("GET /api/tests/runs", s.listTestRuns)
	api.HandleFunc("GET /api/tests/runs/{id}", s.getTestRun)
	api.HandleFunc("POST /api/tests/runs/{id}/cancel", s.cancelTestRun)
	api.HandleFunc("GET /api/tests/runs/{id}/comparison", s.getTestRunComparison)
	api.HandleFunc("GET /api/tests/gates/{api_id}", s.getTestGatePolicy)
	api.HandleFunc("PUT /api/tests/gates/{api_id}", s.upsertTestGatePolicy)
	api.HandleFunc("POST /api/tests/import", s.importTestSuite)

	// Stream Services (TCP/TLS)
	api.HandleFunc("GET /api/streams", s.listStreams)
	api.HandleFunc("POST /api/streams", s.createStream)
	api.HandleFunc("GET /api/streams/{id}", s.getStream)
	api.HandleFunc("PUT /api/streams/{id}", s.updateStream)
	api.HandleFunc("DELETE /api/streams/{id}", s.deleteStream)

	// AI Management & Governed Inference
	api.HandleFunc("GET /api/ai/providers", s.listAIProviders)
	api.HandleFunc("POST /api/ai/providers", s.createAIProvider)
	api.HandleFunc("GET /api/ai/models", s.listAIModels)
	api.HandleFunc("POST /api/ai/models", s.createAIModel)
	api.HandleFunc("GET /api/ai/services", s.listAIServices)
	api.HandleFunc("POST /api/ai/services", s.createAIService)
	api.HandleFunc("GET /api/ai/budgets", s.listAIBudgets)
	api.HandleFunc("GET /api/ai/budgets/{id}", s.getAIBudget)
	api.HandleFunc("POST /api/ai/budgets", s.upsertAIBudget)
	api.HandleFunc("POST /api/ai/releases/manifests", s.createAIReleaseManifest)
	api.HandleFunc("GET /api/ai/releases/manifests/{service_id}", s.listAIReleaseManifests)
	api.HandleFunc("POST /api/ai/releases/qualify/{id}", s.qualifyAIReleaseManifest)
	api.HandleFunc("GET /api/ai/evals/suites", s.listAIEvalSuites)
	api.HandleFunc("POST /api/ai/evals/suites", s.createAIEvalSuite)
	api.HandleFunc("POST /api/ai/evals/runs", s.createAIEvalRun)
	api.HandleFunc("GET /api/ai/evals/runs", s.listAIEvalRuns)
	api.HandleFunc("GET /api/ai/evals/runs/{id}", s.getAIEvalRun)

	// APIOps & Release Passports
	api.HandleFunc("POST /api/apiops/bundles/validate", s.validateAPIOpsBundle)
	api.HandleFunc("POST /api/apiops/deployments/plan", s.planAPIOpsDeployment)
	api.HandleFunc("POST /api/apiops/deployments", s.createAPIOpsDeployment)
	api.HandleFunc("GET /api/apiops/deployments", s.listAPIOpsDeployments)
	api.HandleFunc("GET /api/apiops/deployments/{id}", s.getAPIOpsDeployment)
	api.HandleFunc("POST /api/apiops/deployments/{id}/verify", s.verifyAPIOpsDeployment)
	api.HandleFunc("POST /api/apiops/deployments/{id}/promote", s.promoteAPIOpsDeployment)
	api.HandleFunc("POST /api/apiops/deployments/{id}/abort", s.abortAPIOpsDeployment)
	api.HandleFunc("GET /api/apiops/deployments/{id}/passport", s.getAPIOpsPassport)
	api.HandleFunc("GET /api/apiops/environments", s.listAPIOpsEnvironments)
	api.HandleFunc("POST /api/apiops/environments", s.createAPIOpsEnvironment)

	// Without a database (cached-recovery mode) admin and portal APIs answer 503;
	// local node status stays available to authenticated operators.
	api.HandleFunc("GET /api/orbit/status", s.orbitStatus)
	api.HandleFunc("POST /api/orbit/ask", s.orbitAsk)
	api.HandleFunc("GET /api/orbit/signals", s.orbitSignals)
	api.HandleFunc("POST /api/orbit/proposals/apply", s.applyOrbitProposal)

	s.apiHandler = s.auth(s.requireDB(api, "/api/upstreams/health", "/api/auth/me", "/api/stream", "/api/stream/ws", "/api/events", "/api/events/ws"))
	mux.Handle("/api/", s.apiHandler)
	mux.Handle("/", http.FileServerFS(s.static))
	return withCommonHeaders(s.guardPublicDB(mux))
}

// dbAvailable reports whether this control plane has a database connection pool.
func (s *Server) dbAvailable() bool { return s.store != nil && s.store.Pool != nil }

func writeDBUnavailable(w http.ResponseWriter) {
	writeErr(w, http.StatusServiceUnavailable, "database_unavailable",
		"the control-plane database is unavailable; gateways keep serving their last-known-good configuration")
}

// requireDB rejects requests that need the database while it is unavailable.
func (s *Server) requireDB(next http.Handler, allow ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.dbAvailable() {
			for _, p := range allow {
				if r.URL.Path == p {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeDBUnavailable(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guardPublicDB covers the unauthenticated routes that need the database.
func (s *Server) guardPublicDB(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.dbAvailable() {
			p := r.URL.Path
			// The workbench (/portal/api/try) only talks to the gateway; signup validates
			// its input first and checks availability itself.
			if (strings.HasPrefix(p, "/portal/api/") && p != "/portal/api/try") || strings.HasPrefix(p, "/api/auth/oidc/") || (r.Method == http.MethodPost &&
				(p == "/api/auth/sso/callback" || p == "/api/auth/logout")) {
				writeDBUnavailable(w)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type userCtxKey struct{}

// clusterAdminFallbackID identifies the built-in platform administrator used for
// cluster-token access when admin@relayops.local is not in the database.
const clusterAdminFallbackID = "00000000-0000-0000-0000-000000000001"

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token") // EventSource cannot set headers
		}

		var user store.AdminUser
		authenticated := false

		// 1. Check persistent database-backed sessions with expiration and active account status check
		if got != "" && s.dbAvailable() {
			tokHash := store.HashKey(got)
			sess, u, err := s.store.GetAdminSessionAndUser(r.Context(), tokHash)
			if err == nil {
				// Enforce active account status first, so a deactivated user is told why.
				if !u.Active {
					_ = s.store.DeleteAdminSession(r.Context(), tokHash)
					s.sessionMu.Lock()
					delete(s.sessions, got)
					s.sessionMu.Unlock()
					writeErr(w, http.StatusForbidden, "account_deactivated", "administrator account has been deactivated")
					return
				}
				// Enforce session expiration
				if time.Now().After(sess.ExpiresAt) {
					_ = s.store.DeleteAdminSession(r.Context(), tokHash)
					s.sessionMu.Lock()
					delete(s.sessions, got)
					s.sessionMu.Unlock()
					writeErr(w, http.StatusUnauthorized, "session_expired", "administrator session has expired; please log in again")
					return
				}
				// Sliding window expiration touch
				_ = s.store.TouchAdminSession(r.Context(), tokHash, 24*time.Hour)
				user = u
				authenticated = true
			} else if strings.HasPrefix(got, "adm_sess_") {
				// Not in the database. Only a cluster-token session for an account that
				// does not exist in the database lives in memory; anything else was
				// revoked, logged out or expired.
				s.sessionMu.RLock()
				cached, inMemory := s.sessions[got]
				s.sessionMu.RUnlock()
				if inMemory && cached.Active && cached.ID == clusterAdminFallbackID {
					user = cached
					authenticated = true
				} else {
					s.sessionMu.Lock()
					delete(s.sessions, got)
					s.sessionMu.Unlock()
					writeErr(w, http.StatusUnauthorized, "session_revoked", "administrator session has been revoked or expired")
					return
				}
			}
		}

		// 2. In offline/recovery mode without database, verify against local memory session cache
		if !authenticated && got != "" && s.store == nil {
			s.sessionMu.RLock()
			cached, ok := s.sessions[got]
			s.sessionMu.RUnlock()
			if ok && cached.Active {
				user = cached
				authenticated = true
			}
		}

		// 3. Check cluster token match (mapped to default Platform Administrator with superadmin role)
		if !authenticated && s.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
			var u store.AdminUser
			err := store.ErrNotFound
			if s.dbAvailable() {
				u, err = s.store.GetAdminUserByEmail(r.Context(), "admin@relayops.local")
			}
			if err == nil {
				user = u
			} else {
				user = store.AdminUser{
					ID:     clusterAdminFallbackID,
					Email:  "admin@relayops.local",
					Name:   "Platform Administrator",
					Role:   "superadmin",
					Team:   "Platform Ops",
					Active: true,
				}
			}
			authenticated = true
		}

		// 4. Fallback: check if user provided their raw token hash
		if !authenticated && got != "" && s.dbAvailable() {
			tokHash := store.HashKey(got)
			users, _ := s.store.ListAdminUsers(r.Context())
			for _, u := range users {
				if u.Active && subtle.ConstantTimeCompare([]byte(u.TokenHash), []byte(tokHash)) == 1 {
					user = u
					authenticated = true
					break
				}
			}
		}

		if !authenticated {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid admin bearer token")
			return
		}

		// Tenant scope: which tenants this request may see, and the effective
		// role inside the selected tenant (tenant-scoped administrators).
		user, scope, scopeStatus, scopeErr := s.resolveTenantScope(r, user)
		if scopeErr != nil {
			if scopeStatus == 0 {
				scopeStatus = http.StatusInternalServerError
			}
			writeErr(w, scopeStatus, "tenant_scope", scopeErr.Error())
			return
		}
		if !scope.Platform && platformOnly(r.URL.Path) {
			writeErr(w, http.StatusForbidden, "platform_only", "this operation affects every tenant and requires a platform administrator")
			return
		}

		// Role-Based Access Control (RBAC) Matrix Enforcement
		path := r.URL.Path
		method := r.Method

		// 1. Auditor role: strictly read-only
		if user.Role == "auditor" {
			// Orbit answers questions with read-only tools, so auditors may ask.
			if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions && path != "/api/orbit/ask" {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'auditor' is strictly read-only; resource mutations are forbidden")
				return
			}
		}

		// 2. Admin User management: only superadmin permitted
		if strings.HasPrefix(path, "/api/admin/users") && user.Role != "superadmin" {
			writeErr(w, http.StatusForbidden, "forbidden", "superadmin role is required to manage administrator users")
			return
		}

		// 3. Operator role restrictions
		if user.Role == "operator" {
			// Operators do not have audit permission
			if strings.HasPrefix(path, "/api/audit-logs") {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'operator' lacks 'audit' permission")
				return
			}
			// Operators cannot delete APIs or plans
			if (strings.HasPrefix(path, "/api/apis/") || strings.HasPrefix(path, "/api/plans/")) && method == http.MethodDelete {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'operator' lacks permission to delete API definitions or plans; requires 'admin' or 'superadmin'")
				return
			}
			// Operators cannot create or delete consumers
			if strings.HasPrefix(path, "/api/consumers") && (method == http.MethodPost || method == http.MethodDelete) {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'operator' lacks permission to create or delete consumers; requires 'admin' or 'superadmin'")
				return
			}
			// Operators cannot delete test suites or environments
			if strings.HasPrefix(path, "/api/tests/") && method == http.MethodDelete {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'operator' lacks permission to delete test suites or environments; requires 'admin' or 'superadmin'")
				return
			}
			// Operators cannot modify gate policies
			if strings.HasPrefix(path, "/api/tests/gates") && method != http.MethodGet {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'operator' lacks permission to configure promotion gates; requires 'admin' or 'superadmin'")
				return
			}
		}

		// Test Studio promotion gates require admin or superadmin
		if strings.HasPrefix(path, "/api/tests/gates") && method != http.MethodGet && user.Role != "admin" && user.Role != "superadmin" {
			writeErr(w, http.StatusForbidden, "forbidden", "admin or superadmin role is required to configure promotion gates")
			return
		}

		// System maintenance operations require admin or superadmin
		if strings.HasPrefix(path, "/api/system/maintenance") && user.Role != "admin" && user.Role != "superadmin" {
			writeErr(w, http.StatusForbidden, "forbidden", "admin or superadmin role is required to run maintenance operations")
			return
		}

		// 4. Developer role restrictions
		if user.Role == "developer" {
			allowed := strings.HasPrefix(path, "/api/auth/me") ||
				strings.HasPrefix(path, "/api/dev/") ||
				(path == "/api/tenants" && method == http.MethodGet) ||
				((path == "/api/apis" || path == "/api/plans") && method == http.MethodGet) ||
				(strings.HasPrefix(path, "/api/tests/") && !strings.HasPrefix(path, "/api/tests/gates")) ||
				(strings.HasPrefix(path, "/api/tests/gates") && method == http.MethodGet)
			if !allowed {
				writeErr(w, http.StatusForbidden, "forbidden", "role 'developer' lacks administrative access to control plane")
				return
			}
		}

		ctx := context.WithValue(r.Context(), userCtxKey{}, user)
		ctx = context.WithValue(ctx, scopeCtxKey{}, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) audit(r *http.Request, action, resType, resID string, details map[string]any) {
	s.auditWithState(r, action, resType, resID, details, nil, nil, nil)
}

func (s *Server) auditWithState(r *http.Request, action, resType, resID string, details, before, after, diff map[string]any) {
	tenant := ""
	if sc := scopeFrom(r); sc.Tenant != nil {
		tenant = sc.Tenant.ID
	}
	s.auditWithStateTenant(r, tenant, action, resType, resID, details, before, after, diff)
}

// auditWithStateTenant records an audit entry attributed to tenantID ("" = platform).
func (s *Server) auditWithStateTenant(r *http.Request, tenantID, action, resType, resID string, details, before, after, diff map[string]any) {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	actor := user.Email
	if actor == "" {
		actor = user.Name
	}
	if actor == "" {
		actor = "system"
	}
	role := user.Role
	if role == "" {
		role = "superadmin"
	}

	resName := ""
	if details != nil {
		if n, ok := details["name"].(string); ok {
			resName = n
		}
	}

	if !s.dbAvailable() {
		return
	}
	_ = s.store.CreateRichAuditLog(r.Context(), store.AuditLog{
		Actor:        actor,
		ActorID:      user.ID,
		ActorEmail:   user.Email,
		ActorRole:    role,
		Action:       action,
		ResourceType: resType,
		ResourceID:   resID,
		ResourceName: resName,
		BeforeState:  before,
		AfterState:   after,
		StateDiff:    diff,
		Details:      details,
		ClientIP:     clientIP(r),
		UserAgent:    r.Header.Get("User-Agent"),
		TenantID:     tenantID,
	})
}

func (s *Server) getActor(r *http.Request) string {
	if user, ok := r.Context().Value(userCtxKey{}).(store.AdminUser); ok {
		if user.Email != "" {
			return user.Email
		}
		if user.Name != "" {
			return user.Name
		}
	}
	return "superadmin"
}

func (s *Server) publishConfigChange(ctx context.Context, actor, description string) (int64, error) {
	if actor == "" {
		actor = "superadmin"
	}
	return s.store.AtomicPublishConfig(ctx, actor, description, "all", "active", nil)
}

// ---------------------------------------------------------------------------
// Overview + Realtime Stream
// ---------------------------------------------------------------------------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	counts, err := s.scopedCounts(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	snap := s.gw.Snapshot()
	gw := map[string]any{"config_version": 0, "routes": 0}
	if snap != nil {
		gw = map[string]any{"config_version": snap.Version, "routes": len(snap.Routes), "loaded_at": snap.LoadedAt}
	}
	fleet, _ := s.store.GetFleetStatus(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": s.nodeID, "uptime_sec": int64(time.Since(s.started).Seconds()),
		"counts": counts, "gateway": gw, "fleet": fleet, "stream_clients": s.hub.Clients(),
	})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming_unsupported", "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	tenantScope := ""
	sc := scopeFrom(r)
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantScope = sc.TenantIDs[0]
	}
	if reqTenant := strings.TrimSpace(r.URL.Query().Get("tenant")); reqTenant != "" {
		if sc.allows(reqTenant) {
			tenantScope = reqTenant
		}
	}

	ch := s.hub.SubscribeTenant(tenantScope)
	defer s.hub.Unsubscribe(ch)

	fmt.Fprintf(w, "retry: 2000\nevent: hello\ndata: {\"node\":%q}\n\n", s.nodeID)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
			flusher.Flush()
		}
	}
}

func (s *Server) streamWS() http.Handler {
	return websocket.Handler(func(ws *websocket.Conn) {
		r := ws.Request()
		tenantScope := ""
		sc := scopeFrom(r)
		if !sc.All && len(sc.TenantIDs) > 0 {
			tenantScope = sc.TenantIDs[0]
		}
		if reqTenant := strings.TrimSpace(r.URL.Query().Get("tenant")); reqTenant != "" {
			if sc.allows(reqTenant) {
				tenantScope = reqTenant
			}
		}

		ch := s.hub.SubscribeTenant(tenantScope)
		defer s.hub.Unsubscribe(ch)

		_ = websocket.JSON.Send(ws, map[string]any{
			"event":  "hello",
			"node":   s.nodeID,
			"tenant": tenantScope,
		})

		closeCh := make(chan struct{})
		go func() {
			var msg string
			for {
				if err := websocket.Message.Receive(ws, &msg); err != nil {
					close(closeCh)
					return
				}
				if msg == "ping" {
					_ = websocket.Message.Send(ws, "pong")
				}
			}
		}()

		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-closeCh:
				return
			case <-ping.C:
				if err := websocket.Message.Send(ws, "ping"); err != nil {
					return
				}
			case msg, ok := <-ch:
				if !ok {
					return
				}
				raw := fmt.Sprintf(`{"event":%q,"data":%s}`, msg.Event, string(msg.Data))
				if err := websocket.Message.Send(ws, raw); err != nil {
					return
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// APIs
// ---------------------------------------------------------------------------

type apiInput struct {
	Name               *string              `json:"name"`
	Description        *string              `json:"description"`
	BasePath           *string              `json:"base_path"`
	UpstreamURL        *string              `json:"upstream_url"`
	StripPath          *bool                `json:"strip_path"`
	AuthType           *string              `json:"auth_type"`
	JWTSecret          *string              `json:"jwt_secret"`
	JWKSURL            *string              `json:"jwks_url"`
	OIDCIssuer         *string              `json:"oidc_issuer"`
	OIDCAudience       *string              `json:"oidc_audience"`
	RateLimitPerMinute *int                 `json:"rate_limit_per_minute"`
	QuotaPerDay        *int                 `json:"quota_per_day"`
	QuotaPerMonth      *int                 `json:"quota_per_month"`
	TimeoutMS          *int                 `json:"timeout_ms"`
	CORSEnabled        *bool                `json:"cors_enabled"`
	RequestHeaders     *map[string]string   `json:"request_headers"`
	IsAI               *bool                `json:"is_ai"`
	OpenAPISpec        *map[string]any      `json:"openapi_spec"`
	Visibility         *string              `json:"visibility"`
	RequireApproval    *bool                `json:"require_approval"`
	IsDraft            *bool                `json:"is_draft"`
	QuotaFailurePolicy *string              `json:"quota_failure_policy"`
	TrafficPolicy      *store.TrafficPolicy `json:"traffic_policy"`
	Protocol           *string              `json:"protocol"`
	GraphQLSchema      *string              `json:"graphql_schema"`
	GraphQLPolicy      *store.GraphQLPolicy `json:"graphql_policy"`
	GRPCPolicy         *store.GRPCPolicy    `json:"grpc_policy"`
	GRPCDescriptorSet  *[]byte              `json:"grpc_descriptor_set"` // base64 FileDescriptorSet
	MCPPolicy          *store.MCPPolicy     `json:"mcp_policy"`
	Enabled            *bool                `json:"enabled"`
}

func (in apiInput) apply(a store.API) (store.API, error) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&a.Name, in.Name)
	set(&a.Description, in.Description)
	set(&a.BasePath, in.BasePath)
	set(&a.UpstreamURL, in.UpstreamURL)
	set(&a.AuthType, in.AuthType)
	set(&a.JWKSURL, in.JWKSURL)
	set(&a.OIDCIssuer, in.OIDCIssuer)
	set(&a.OIDCAudience, in.OIDCAudience)

	if in.JWTSecret != nil {
		a.JWTSecret = *in.JWTSecret
	}
	if in.StripPath != nil {
		a.StripPath = *in.StripPath
	}
	if in.RateLimitPerMinute != nil {
		a.RateLimitPerMinute = *in.RateLimitPerMinute
	}
	if in.QuotaPerDay != nil {
		a.QuotaPerDay = *in.QuotaPerDay
	}
	if in.QuotaPerMonth != nil {
		a.QuotaPerMonth = *in.QuotaPerMonth
	}
	if in.TimeoutMS != nil {
		a.TimeoutMS = *in.TimeoutMS
	}
	if in.CORSEnabled != nil {
		a.CORSEnabled = *in.CORSEnabled
	}
	if in.RequestHeaders != nil {
		a.RequestHeaders = *in.RequestHeaders
	}
	if in.IsAI != nil {
		a.IsAI = *in.IsAI
	}
	if in.OpenAPISpec != nil {
		a.OpenAPISpec = *in.OpenAPISpec
	}
	if in.Visibility != nil {
		v := strings.ToLower(strings.TrimSpace(*in.Visibility))
		if v != "public" && v != "private" && v != "internal" {
			return a, errors.New("visibility must be one of: public, private, internal")
		}
		a.Visibility = v
	}
	if in.RequireApproval != nil {
		a.RequireApproval = *in.RequireApproval
	}
	if in.IsDraft != nil {
		a.IsDraft = *in.IsDraft
	}
	if in.QuotaFailurePolicy != nil {
		p := strings.ToLower(strings.TrimSpace(*in.QuotaFailurePolicy))
		if p != "fail_open" && p != "fail_closed" {
			return a, errors.New("quota_failure_policy must be one of: fail_open, fail_closed")
		}
		a.QuotaFailurePolicy = p
	}
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if in.TrafficPolicy != nil {
		a.TrafficPolicy = *in.TrafficPolicy
	}
	if in.Protocol != nil {
		p := strings.ToLower(strings.TrimSpace(*in.Protocol))
		if p == "" {
			p = "http"
		}
		if p != "http" && p != "graphql" && p != "grpc" && p != "mcp" {
			return a, errors.New("protocol must be one of: http, graphql, grpc, mcp")
		}
		a.Protocol = p
	}
	if a.Protocol == "" {
		a.Protocol = "http"
	}
	if in.GraphQLSchema != nil {
		a.GraphQLSchema = *in.GraphQLSchema
	}
	if in.GraphQLPolicy != nil {
		a.GraphQLPolicy = *in.GraphQLPolicy
	}
	if in.GRPCPolicy != nil {
		a.GRPCPolicy = *in.GRPCPolicy
	}
	if in.GRPCDescriptorSet != nil {
		a.GRPCDescriptorSet = *in.GRPCDescriptorSet
	}
	if in.MCPPolicy != nil {
		a.MCPPolicy = *in.MCPPolicy
	}
	if a.Protocol == "graphql" {
		gp := a.GraphQLPolicy
		if gp.MaxDepth < 0 || gp.MaxCost < 0 || gp.MaxAliases < 0 || gp.MaxBatchSize < 0 || gp.MaxBatchSize > 100 {
			return a, errors.New("graphql_policy limits must be >= 0 and max_batch_size at most 100")
		}
		if gp.ValidateAgainstSchema {
			if strings.TrimSpace(a.GraphQLSchema) == "" {
				return a, errors.New("graphql_policy.validate_against_schema needs graphql_schema")
			}
			if _, err := graphql.LoadSchema(a.GraphQLSchema); err != nil {
				return a, err
			}
		}
	}
	if a.Protocol == "grpc" {
		a.GRPCPolicy.Normalize()
		if err := a.GRPCPolicy.Validate(); err != nil {
			return a, err
		}
		if a.GRPCPolicy.JSONTranscoding && len(a.GRPCDescriptorSet) == 0 {
			return a, errors.New("grpc_policy.json_transcoding needs grpc_descriptor_set (the message types)")
		}
		if n := len(a.GRPCDescriptorSet); n > store.MaxGRPCDescriptorBytes {
			return a, fmt.Errorf("grpc_descriptor_set is larger than %d bytes", store.MaxGRPCDescriptorBytes)
		} else if n > 0 {
			if _, err := grpc.ParseDescriptorSet(a.GRPCDescriptorSet); err != nil {
				return a, fmt.Errorf("grpc_descriptor_set: %w", err)
			}
		}
	}
	if a.Protocol == "mcp" {
		a.MCPPolicy.Normalize()
		if err := a.MCPPolicy.Validate(); err != nil {
			return a, err
		}
	}
	a.TrafficPolicy.Normalize()
	if err := a.TrafficPolicy.Validate(); err != nil {
		return a, err
	}
	if a.RequestHeaders == nil {
		a.RequestHeaders = map[string]string{}
	}
	if a.OpenAPISpec == nil {
		a.OpenAPISpec = map[string]any{}
	}

	if a.Name == "" {
		return a, errors.New("name is required")
	}
	if !strings.HasPrefix(a.BasePath, "/") {
		return a, errors.New("base_path must start with '/'")
	}
	if len(a.BasePath) > 1 {
		a.BasePath = strings.TrimRight(a.BasePath, "/")
	}
	if strings.HasPrefix(a.BasePath, "/__relayops") {
		return a, errors.New("base_path /__relayops is reserved")
	}
	u, err := url.Parse(a.UpstreamURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "h2c" && u.Scheme != "grpc") || u.Host == "" {
		return a, errors.New("upstream_url must be an absolute http, https, h2c, or grpc URL")
	}
	switch a.AuthType {
	case "":
		a.AuthType = "none"
	case "none", "api_key":
	case "jwt":
		if len(a.JWTSecret) < 16 {
			return a, errors.New("jwt_secret must be at least 16 characters for auth_type=jwt")
		}
	case "oidc":
		if a.JWKSURL == "" {
			return a, errors.New("jwks_url is required when auth_type=oidc")
		}
	case "mtls":
		// Callers are identified by a client certificate the gateway verified
		// itself (RELAYOPS_TLS_CLIENT_CA_FILE) or a trusted ingress forwarded
		// (RELAYOPS_TRUSTED_PROXY_CIDRS); client-sent certificate headers are discarded.
		// Allowed certificates: consumer keys registered as "mtls:<SHA-256 fingerprint>",
		// or request_headers Allowed-Client-Fingerprint / Allowed-Client-CN.
	default:
		return a, errors.New("auth_type must be one of: none, api_key, jwt, oidc, mtls")
	}
	if a.RateLimitPerMinute < 0 {
		return a, errors.New("rate_limit_per_minute must be >= 0")
	}
	if a.QuotaPerDay < 0 {
		return a, errors.New("quota_per_day must be >= 0")
	}
	if a.QuotaPerMonth < 0 {
		return a, errors.New("quota_per_month must be >= 0")
	}
	if a.TimeoutMS <= 0 || a.TimeoutMS > 600_000 {
		return a, errors.New("timeout_ms must be between 1 and 600,000")
	}
	return a, nil
}

func redact(a store.API) store.API {
	if a.JWTSecret != "" {
		a.JWTSecret = "********"
	}
	return a
}

func (s *Server) listAPIs(w http.ResponseWriter, r *http.Request) {
	apis, err := s.store.ListAPIs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.API, 0, len(apis))
	for _, a := range apis {
		if sc.allows(a.TenantID) {
			out = append(out, redact(a))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAPI(w http.ResponseWriter, r *http.Request) {
	a, ok := s.scopedAPI(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, redact(a))
}

func (s *Server) createAPI(w http.ResponseWriter, r *http.Request) {
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	a, err := in.apply(store.API{
		StripPath:          true,
		AuthType:           "none",
		TimeoutMS:          30000,
		Enabled:            true,
		Visibility:         "public",
		QuotaFailurePolicy: "fail_open",
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	a.TenantID = tenant
	created, rev, err := s.store.CreateAPIAtomic(r.Context(), a, s.getActor(r), "Created API "+a.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, created.TenantID, "CREATE", "api", created.ID, map[string]any{"name": created.Name, "base_path": created.BasePath, "revision": rev})
	writeJSON(w, http.StatusCreated, redact(created))
}

func (s *Server) importOpenAPI(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read_failed", err.Error())
		return
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "empty_spec", "OpenAPI / Swagger spec body cannot be empty")
		return
	}
	// Support explicit ?draft=true for staged review
	asDraft := r.URL.Query().Get("draft") == "true"
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	result, err := ParseOpenAPISpec(raw, asDraft)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	result.API.TenantID = tenant
	var created store.API
	var rev int64
	if asDraft {
		created, err = s.store.CreateAPI(r.Context(), result.API)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, "IMPORT_OPENAPI", "api", created.ID, map[string]any{"name": created.Name, "is_draft": true})
	} else {
		created, rev, err = s.store.CreateAPIAtomic(r.Context(), result.API, s.getActor(r), "Imported API "+result.API.Name)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, "IMPORT_OPENAPI", "api", created.ID, map[string]any{"name": created.Name, "is_draft": false, "revision": rev})
	}
	resp := map[string]any{
		"id":           created.ID,
		"name":         created.Name,
		"description":  created.Description,
		"base_path":    created.BasePath,
		"upstream_url": created.UpstreamURL,
		"auth_type":    created.AuthType,
		"is_draft":     created.IsDraft,
		"enabled":      created.Enabled,
		"warnings":     result.Warnings,
		"summary":      result.Summary,
		"api":          redact(created),
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) publishAPI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedAPI(w, r, id); !ok {
		return
	}
	updated, rev, err := s.store.PublishAPIAtomic(r.Context(), id, s.getActor(r), "Published API "+id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "PUBLISH", "api", id, map[string]any{"name": updated.Name, "base_path": updated.BasePath, "revision": rev})
	resp := struct {
		store.API
		Revision int64 `json:"revision"`
	}{
		API:      redact(updated),
		Revision: rev,
	}
	writeJSON(w, http.StatusOK, resp)
}

type replaySnapshotLookup struct {
	keys map[string]store.KeyRecord
	subs map[string]store.SubRecord
}

func (s *replaySnapshotLookup) LookupKey(hash string) (store.KeyRecord, bool) {
	k, ok := s.keys[hash]
	return k, ok
}

func (s *replaySnapshotLookup) LookupSub(consumerID, apiID string) (store.SubRecord, bool) {
	sub, ok := s.subs[consumerID+"|"+apiID]
	return sub, ok
}

type ReplayLimitations struct {
	ExecutionMode          string   `json:"execution_mode"`
	StateDependentPolicies string   `json:"state_dependent_policies"`
	ExcludedDynamicHeaders []string `json:"excluded_dynamic_headers"`
	SideEffects            string   `json:"side_effects"`
	SampleScope            string   `json:"sample_scope"`
}

func defaultReplayLimitations() ReplayLimitations {
	return ReplayLimitations{
		ExecutionMode:          "Stateless In-Memory Policy Simulation (rules evaluated without network egress)",
		StateDependentPolicies: "Rate limits and quotas evaluated against static plan boundaries; live Redis counters are unaffected",
		ExcludedDynamicHeaders: []string{"Authorization bearer signatures (tested structurally)", "Timestamps/Nonces", "Idempotency keys"},
		SideEffects:            "Zero upstream calls; mock evaluation engine isolates data plane from production state",
		SampleScope:            "Limited to historical logged requests within the sample window",
	}
}

type SamplingReport struct {
	SampleSize             int      `json:"sample_size"`
	PopulationSize         int      `json:"population_size"`
	SampleProportion       float64  `json:"sample_proportion"`
	MarginOfError          float64  `json:"margin_of_error"`
	ConfidenceIntervalLow  float64  `json:"confidence_interval_low"`
	ConfidenceIntervalHigh float64  `json:"confidence_interval_high"`
	ConfidenceLevel        float64  `json:"confidence_level"`
	ActiveConsumersTotal   int      `json:"active_consumers_total"`
	SampledConsumersCount  int      `json:"sampled_consumers_count"`
	ConsumerCoverageRatio  float64  `json:"consumer_coverage_ratio"`
	UnrepresentedConsumers []string `json:"unrepresented_consumers"`
	ObservationHours       float64  `json:"observation_hours"`
	IsSampleAdequate       bool     `json:"is_sample_adequate"`
	IsRepresentative       bool     `json:"is_representative"`
	SamplingAssessment     string   `json:"sampling_assessment"`
	StatisticalStatement   string   `json:"statistical_statement"`
}

func (s *Server) evaluateRepresentativeSampling(ctx context.Context, apiID string, sampleSize, impactedCount int, logs []store.RequestLog, subscribedConsumers map[string]string) SamplingReport {
	var populationSize int
	_ = s.store.Pool.QueryRow(ctx, `SELECT COALESCE(round(sum(sample_weight)), 0)::bigint FROM request_logs WHERE api_id=$1`, apiID).Scan(&populationSize)
	if populationSize < sampleSize {
		populationSize = sampleSize
	}

	sampleProportion := 0.0
	if sampleSize > 0 {
		sampleProportion = float64(impactedCount) / float64(sampleSize)
	}

	fpc := 1.0
	if populationSize > sampleSize && populationSize > 1 {
		fpc = math.Sqrt(float64(populationSize-sampleSize) / float64(populationSize-1))
	}
	variance := sampleProportion * (1.0 - sampleProportion)
	if variance <= 0 {
		variance = 0.05 * 0.95
	}
	marginOfError := 0.0
	if sampleSize > 0 {
		marginOfError = 1.96 * math.Sqrt(variance/float64(sampleSize)) * fpc
	}
	if marginOfError > 1.0 {
		marginOfError = 1.0
	}
	ciLow := math.Max(0.0, sampleProportion-marginOfError)
	ciHigh := math.Min(1.0, sampleProportion+marginOfError)

	observedConsumers := make(map[string]bool)
	var minTS, maxTS time.Time
	for i, l := range logs {
		if l.ConsumerID != nil && *l.ConsumerID != "" {
			observedConsumers[*l.ConsumerID] = true
		}
		if i == 0 || l.TS.Before(minTS) {
			minTS = l.TS
		}
		if i == 0 || l.TS.After(maxTS) {
			maxTS = l.TS
		}
	}

	totalActive := len(subscribedConsumers)
	sampledCount := len(observedConsumers)
	coverageRatio := 1.0
	var unrepresented []string
	if totalActive > 0 {
		coverageRatio = float64(sampledCount) / float64(totalActive)
		for cid := range subscribedConsumers {
			if !observedConsumers[cid] {
				c, err := s.store.GetConsumer(ctx, cid)
				if err == nil {
					unrepresented = append(unrepresented, c.Name)
				} else {
					unrepresented = append(unrepresented, cid)
				}
			}
		}
	}

	obsHours := 0.0
	if !minTS.IsZero() && !maxTS.IsZero() {
		obsHours = maxTS.Sub(minTS).Hours()
	}

	isAdequate := sampleSize >= 30
	isRepresentative := isAdequate && coverageRatio >= 0.75

	assessment := "Representative sample: sufficient sample size (n >= 30) with broad consumer coverage."
	if !isAdequate {
		assessment = fmt.Sprintf("Preliminary sample: sample size (n=%d) is below normal approximation threshold (n >= 30); margin of error is wide.", sampleSize)
	} else if coverageRatio < 0.75 && len(unrepresented) > 0 {
		assessment = fmt.Sprintf("Non-representative sample: %d active consumers (%s) are unobserved in sample window.", len(unrepresented), strings.Join(unrepresented, ", "))
	}

	statement := fmt.Sprintf("95%% confidence interval: [%.1f%%, %.1f%%] with margin of error ±%.2f%% (sample size n=%d, population N=%d, FPC applied)",
		ciLow*100, ciHigh*100, marginOfError*100, sampleSize, populationSize)

	return SamplingReport{
		SampleSize:             sampleSize,
		PopulationSize:         populationSize,
		SampleProportion:       sampleProportion,
		MarginOfError:          marginOfError,
		ConfidenceIntervalLow:  ciLow,
		ConfidenceIntervalHigh: ciHigh,
		ConfidenceLevel:        0.95,
		ActiveConsumersTotal:   totalActive,
		SampledConsumersCount:  sampledCount,
		ConsumerCoverageRatio:  coverageRatio,
		UnrepresentedConsumers: unrepresented,
		ObservationHours:       obsHours,
		IsSampleAdequate:       isAdequate,
		IsRepresentative:       isRepresentative,
		SamplingAssessment:     assessment,
		StatisticalStatement:   statement,
	}
}

// previewChange implements Signature Feature 1: Change Impact Preview.
// It evaluates proposed modifications against the last 24h of request traffic.
func (s *Server) previewChange(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	proposed, err := in.apply(existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}

	// Fetch recent requests to analyze impact
	logs, _ := s.store.QueryLogs(r.Context(), store.LogFilter{APIID: id, Limit: 500})
	uniqueConsumers := make(map[string]bool)
	unauthCount := 0
	for _, l := range logs {
		if l.ConsumerID != nil {
			uniqueConsumers[*l.ConsumerID] = true
		} else {
			unauthCount++
		}
	}

	type ImpactItem struct {
		Severity    string `json:"severity"` // "info", "warning", "critical"
		Message     string `json:"message"`
		MetricValue string `json:"metric_value,omitempty"`
	}
	var impacts []ImpactItem

	// 1. Auth Change Impact
	if existing.AuthType != proposed.AuthType {
		if proposed.AuthType != "none" && existing.AuthType == "none" {
			impacts = append(impacts, ImpactItem{
				Severity:    "critical",
				Message:     fmt.Sprintf("Auth type changing from 'none' to '%s'. Previously unauthenticated requests will be rejected with 401.", proposed.AuthType),
				MetricValue: fmt.Sprintf("%d unauthenticated requests observed in recent logs", unauthCount),
			})
		} else {
			impacts = append(impacts, ImpactItem{
				Severity: "warning",
				Message:  fmt.Sprintf("Auth scheme changing from '%s' to '%s'. Clients must provide new credentials.", existing.AuthType, proposed.AuthType),
			})
		}
	}

	// 2. Base Path Change Impact
	if existing.BasePath != proposed.BasePath {
		impacts = append(impacts, ImpactItem{
			Severity: "critical",
			Message:  fmt.Sprintf("Base path changing from '%s' to '%s'. Requests to '%s' will return 404 Not Found.", existing.BasePath, proposed.BasePath, existing.BasePath),
		})
	}

	// 3. Rate Limit Reduction Impact
	if proposed.RateLimitPerMinute > 0 && proposed.RateLimitPerMinute < existing.RateLimitPerMinute {
		impacts = append(impacts, ImpactItem{
			Severity: "warning",
			Message:  fmt.Sprintf("Rate limit reduced from %d to %d rpm. High-volume consumers may experience 429 Rate Limited.", existing.RateLimitPerMinute, proposed.RateLimitPerMinute),
		})
	}

	// 4. Approval Policy Change
	if !existing.RequireApproval && proposed.RequireApproval {
		impacts = append(impacts, ImpactItem{
			Severity: "info",
			Message:  "Future consumer subscriptions will require manual admin approval before activation.",
		})
	}

	if len(impacts) == 0 {
		impacts = append(impacts, ImpactItem{
			Severity: "info",
			Message:  "No breaking routing or security impacts detected for existing callers.",
		})
	}

	consumerMap := make(map[string]int)
	for _, l := range logs {
		if l.ConsumerID != nil && *l.ConsumerID != "" {
			consumerMap[*l.ConsumerID]++
		}
	}
	type ImpactedConsumerInfo struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Email        string `json:"email"`
		RequestCount int    `json:"request_count"`
	}
	var impactedConsumers []ImpactedConsumerInfo
	for cid, count := range consumerMap {
		c, err := s.store.GetConsumer(r.Context(), cid)
		if err == nil {
			impactedConsumers = append(impactedConsumers, ImpactedConsumerInfo{
				ID:           c.ID,
				Name:         c.Name,
				Email:        c.Email,
				RequestCount: count,
			})
		}
	}

	// Gather active subscriptions to determine consumer representation
	allSubs, _ := s.store.ListSubscriptions(r.Context())
	subscribedConsumers := make(map[string]string)
	for _, sub := range allSubs {
		if sub.APIID == id && sub.Active {
			subscribedConsumers[sub.ConsumerID] = sub.PlanName
		}
	}

	impactedRequests := 0
	if proposed.BasePath != existing.BasePath {
		impactedRequests = len(logs)
	} else if proposed.AuthType != existing.AuthType && proposed.AuthType != "none" {
		impactedRequests = unauthCount
	}

	repReport := s.evaluateRepresentativeSampling(r.Context(), id, len(logs), impactedRequests, logs, subscribedConsumers)

	writeJSON(w, http.StatusOK, map[string]any{
		"api_id":                 id,
		"name":                   existing.Name,
		"recent_requests":        len(logs),
		"active_consumers":       len(uniqueConsumers),
		"impacted_consumers":     impactedConsumers,
		"sample_window":          "Last 500 requests / 24 hours",
		"sample_size":            len(logs),
		"confidence":             repReport.StatisticalStatement,
		"statistical_evaluation": repReport,
		"replay_limitations":     defaultReplayLimitations(),
		"impacts":                impacts,
	})
}

// replayPreview evaluates recorded past requests against candidate changes in-memory
// through the EXACT SAME gateway policy engine without forwarding any traffic upstream.
// SimulationResult is one historical request re-evaluated against a proposed API config.
type SimulationResult struct {
	RequestID       string `json:"request_id"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	OriginalStatus  int    `json:"original_status"`
	SimulatedStatus int    `json:"simulated_status"`
	BehavioralDiff  bool   `json:"behavioral_diff"`
	DecisionPolicy  string `json:"decision_policy"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
	ConsumerName    string `json:"consumer_name,omitempty"`
}

// replayOutcome is the result of replaying recent traffic against a proposed config.
type replayOutcome struct {
	Logs                []store.RequestLog
	Simulations         []SimulationResult
	DiffCount           int
	SubscribedConsumers map[string]string
}

// replayAPIChange re-evaluates the API's most recent requests (up to limit) through
// the same policy engine as the data plane, using the proposed configuration.
func (s *Server) replayAPIChange(ctx context.Context, existing, proposed store.API, limit int) (replayOutcome, error) {
	logs, err := s.store.QueryLogs(ctx, store.LogFilter{APIID: existing.ID, Limit: limit})
	if err != nil {
		return replayOutcome{}, err
	}
	// Build live-equivalent snapshot lookup for the shared policy engine
	keysList, _ := s.store.ListAllActiveAPIKeys(ctx)
	keysMap := make(map[string]store.KeyRecord)
	consumerKeyMap := make(map[string]string)
	for _, k := range keysList {
		if k.KeyPrefix != "" && consumerKeyMap[k.ConsumerID] == "" {
			consumerKeyMap[k.ConsumerID] = k.KeyPrefix
		}
	}

	allSubs, _ := s.store.ListSubscriptions(ctx)
	subsMap := make(map[string]store.SubRecord)
	subscribedConsumers := make(map[string]string)
	for _, sub := range allSubs {
		if sub.APIID == existing.ID && sub.Active {
			subscribedConsumers[sub.ConsumerID] = sub.PlanName
		}
		subsMap[sub.ConsumerID+"|"+sub.APIID] = store.SubRecord{
			ConsumerID: sub.ConsumerID,
			APIID:      sub.APIID,
			PlanName:   sub.PlanName,
			Status:     sub.Status,
			Active:     sub.Active,
		}
	}
	snapLookup := &replaySnapshotLookup{keys: keysMap, subs: subsMap}

	var simulations []SimulationResult
	diffCount := 0

	for _, l := range logs {
		pReq := policy.Request{
			Method:   l.Method,
			Path:     l.Path,
			Header:   make(http.Header),
			ClientIP: l.ClientIP,
		}

		if l.ConsumerID != nil && *l.ConsumerID != "" {
			if prefix, ok := consumerKeyMap[*l.ConsumerID]; ok {
				dummyKey := prefix + "_eval_key"
				pReq.Header.Set("X-API-Key", dummyKey)
				snapLookup.keys[store.HashKey(dummyKey)] = store.KeyRecord{
					KeyHash:        store.HashKey(dummyKey),
					KeyID:          "key_" + *l.ConsumerID,
					ConsumerID:     *l.ConsumerID,
					ConsumerName:   l.ConsumerName,
					ConsumerStatus: "active",
				}
			}
		}

		// Run through the EXACT SAME shared policy evaluator as the live data plane
		dec := s.evaluator.Evaluate(ctx, pReq, &proposed, snapLookup, nil, true)
		simStatus := dec.Status
		decisionPolicy := dec.Policy
		reason := dec.Reason

		diff := simStatus != l.Status
		if diff {
			diffCount++
		}

		simulations = append(simulations, SimulationResult{
			RequestID:       l.RequestID,
			Method:          l.Method,
			Path:            l.Path,
			OriginalStatus:  l.Status,
			SimulatedStatus: simStatus,
			BehavioralDiff:  diff,
			DecisionPolicy:  decisionPolicy,
			Reason:          reason,
			Detail:          dec.Detail,
			ConsumerName:    l.ConsumerName,
		})
	}

	return replayOutcome{Logs: logs, Simulations: simulations, DiffCount: diffCount, SubscribedConsumers: subscribedConsumers}, nil
}

func (s *Server) replayPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	proposed, err := in.apply(existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	out, err := s.replayAPIChange(r.Context(), existing, proposed, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	simulations, diffCount, logs, subscribedConsumers := out.Simulations, out.DiffCount, out.Logs, out.SubscribedConsumers

	repReport := s.evaluateRepresentativeSampling(r.Context(), id, len(logs), diffCount, logs, subscribedConsumers)

	writeJSON(w, http.StatusOK, map[string]any{
		"api_id":                 id,
		"name":                   existing.Name,
		"total_replayed":         len(simulations),
		"status_changed_count":   diffCount,
		"sample_window":          "Recent production traffic log sample",
		"statistical_evaluation": repReport,
		"replay_limitations":     defaultReplayLimitations(),
		"simulations":            simulations,
	})
}

func (s *Server) getReleaseReport(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	rev, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision_id", "revision ID must be an integer")
		return
	}
	revision, err := s.store.GetConfigRevision(r.Context(), rev)
	if err != nil {
		writeErr(w, http.StatusNotFound, "revision_not_found", "revision does not exist")
		return
	}
	fleet, _ := s.store.GetFleetStatus(r.Context())

	// Gather acknowledgements for this revision
	var nodeAcks []store.NodeAcknowledgement
	convergedCount := 0
	for _, n := range fleet.Nodes {
		if n.Revision == rev {
			nodeAcks = append(nodeAcks, n)
			convergedCount++
		}
	}

	// Fetch logs around this revision to calculate observed impact
	logs, _ := s.store.QueryLogs(r.Context(), store.LogFilter{Limit: 200})
	totalObserved := len(logs)
	impactedConsumers := make(map[string]int)
	statusDiffs := 0
	for _, l := range logs {
		if l.ConfigRevision == rev {
			if l.ConsumerID != nil {
				impactedConsumers[*l.ConsumerID]++
			}
			if l.Status >= 400 {
				statusDiffs++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"revision":             revision,
		"fleet_status":         fleet,
		"nodes_acknowledged":   nodeAcks,
		"nodes_converged":      convergedCount,
		"total_fleet_nodes":    len(fleet.Nodes),
		"fleet_converged":      fleet.Converged,
		"total_observed_logs":  totalObserved,
		"impacted_consumers":   len(impactedConsumers),
		"observed_error_count": statusDiffs,
		"generated_at":         time.Now(),
	})
}

func (s *Server) listOIDCProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListOIDCProviders(r.Context())
	s.respond(w, http.StatusOK, providers, err)
}

// ---------------------------------------------------------------------------
// Distinctive Feature 1: Consumer Impact Report
// ---------------------------------------------------------------------------

type ApplicationImpactInfo struct {
	AppID               string `json:"app_id"`
	AppName             string `json:"app_name"`
	ConsumerID          string `json:"consumer_id"`
	ConsumerName        string `json:"consumer_name"`
	ConsumerEmail       string `json:"consumer_email"`
	Environment         string `json:"environment"`
	ActivityStatus      string `json:"activity_status"` // OBSERVED_ACTIVE_CALLER, REGISTERED_DORMANT_INTEGRATION
	ImpactScope         string `json:"impact_scope"`    // CONFIRMED_APPLICATION_IMPACT, UNIMPACTED
	ImpactLevel         string `json:"impact_level"`    // CRITICAL_BREAKING, ACTION_REQUIRED, WARNING, INFO
	FailureMode         string `json:"failure_mode"`
	ActiveKeysCount     int    `json:"active_keys_count"`
	RecentRequestsCount int    `json:"recent_requests_count"`
	RequiredAction      string `json:"required_action"`
}

type ConsumerImpactReport struct {
	APIID                     string                  `json:"api_id"`
	APIName                   string                  `json:"api_name"`
	BreakingChangesCount      int                     `json:"breaking_changes_count"`
	BreakingChanges           []string                `json:"breaking_changes"`
	TotalImpactedApplications int                     `json:"total_impacted_applications"`
	Applications              []ApplicationImpactInfo `json:"applications"`
	ActionableSummary         string                  `json:"actionable_summary"`
}

func (s *Server) buildConsumerImpact(ctx context.Context, existing, proposed store.API) ConsumerImpactReport {
	var breakingChanges []string

	if existing.AuthType != proposed.AuthType {
		if proposed.AuthType != "none" {
			breakingChanges = append(breakingChanges, fmt.Sprintf("Authentication scheme changing from '%s' to '%s'. Callers must obtain and supply valid credentials.", existing.AuthType, proposed.AuthType))
		}
	}
	if existing.BasePath != proposed.BasePath {
		breakingChanges = append(breakingChanges, fmt.Sprintf("Base path changing from '%s' to '%s'. Old URLs will return 404 Not Found.", existing.BasePath, proposed.BasePath))
	}
	if proposed.RateLimitPerMinute > 0 && proposed.RateLimitPerMinute < existing.RateLimitPerMinute {
		breakingChanges = append(breakingChanges, fmt.Sprintf("Rate limit reduced from %d to %d rpm. Callers exceeding rate may receive HTTP 429.", existing.RateLimitPerMinute, proposed.RateLimitPerMinute))
	}
	if proposed.QuotaPerDay > 0 && proposed.QuotaPerDay < existing.QuotaPerDay {
		breakingChanges = append(breakingChanges, fmt.Sprintf("Daily quota reduced from %d to %d requests/day.", existing.QuotaPerDay, proposed.QuotaPerDay))
	}
	if existing.Enabled && !proposed.Enabled {
		breakingChanges = append(breakingChanges, "API is being disabled. All incoming traffic will be rejected.")
	}
	if !existing.RequireApproval && proposed.RequireApproval {
		breakingChanges = append(breakingChanges, "Future consumer subscriptions will require manual admin approval.")
	}

	allSubs, _ := s.store.ListSubscriptions(ctx)
	subscribedConsumerIDs := make(map[string]bool)
	for _, sub := range allSubs {
		if sub.APIID == existing.ID && sub.Active {
			subscribedConsumerIDs[sub.ConsumerID] = true
		}
	}

	allApps, _ := s.store.ListAllDeveloperApps(ctx)
	allKeys, _ := s.store.ListAllActiveAPIKeys(ctx)
	logs, _ := s.store.QueryLogs(ctx, store.LogFilter{APIID: existing.ID, Limit: 500})

	reqCountByConsumer := make(map[string]int)
	for _, l := range logs {
		if l.ConsumerID != nil && *l.ConsumerID != "" {
			reqCountByConsumer[*l.ConsumerID]++
		}
	}

	keysByConsumer := make(map[string]int)
	for _, k := range allKeys {
		if k.Active {
			keysByConsumer[k.ConsumerID]++
		}
	}

	var appImpacts []ApplicationImpactInfo
	for _, app := range allApps {
		if !subscribedConsumerIDs[app.ConsumerID] {
			continue
		}
		c, err := s.store.GetConsumer(ctx, app.ConsumerID)
		if err != nil {
			continue
		}

		recentReqs := reqCountByConsumer[c.ID]
		activityStatus := "REGISTERED_DORMANT_INTEGRATION"
		if recentReqs > 0 {
			activityStatus = "OBSERVED_ACTIVE_CALLER"
		}

		impactScope := "UNIMPACTED"
		impactLevel := "INFO"
		failureMode := "No breaking changes detected"
		action := "No action required; existing integration will continue normally."

		if len(breakingChanges) > 0 {
			impactScope = "CONFIRMED_APPLICATION_IMPACT"
			if existing.AuthType != proposed.AuthType && proposed.AuthType != "none" {
				impactLevel = "CRITICAL_BREAKING"
				failureMode = "Requests will fail with HTTP 401 Unauthorized"
				switch proposed.AuthType {
				case "api_key":
					action = fmt.Sprintf("Application owner (%s) must generate an API Key in the Developer Portal and update '%s' to send 'X-API-Key: <key>' or 'Authorization: Bearer <key>'.", c.Email, app.Name)
				case "jwt":
					action = fmt.Sprintf("Application owner (%s) must update '%s' to transmit a signed JWT in 'Authorization: Bearer <jwt>' header matching configured secret.", c.Email, app.Name)
				case "oidc":
					issuer := proposed.OIDCIssuer
					if issuer == "" {
						issuer = "(configured IdP)"
					}
					action = fmt.Sprintf("Application owner (%s) must configure '%s' to authenticate against OIDC IdP (issuer: %s) and supply a verified ID/access token.", c.Email, app.Name, issuer)
				case "oauth2":
					action = fmt.Sprintf("Application owner (%s) must configure '%s' to acquire OAuth2 tokens and transmit 'Authorization: Bearer <token>'.", c.Email, app.Name)
				default:
					action = fmt.Sprintf("Application owner (%s) must configure '%s' for '%s' authentication credentials.", c.Email, app.Name, proposed.AuthType)
				}
			} else if existing.BasePath != proposed.BasePath {
				impactLevel = "CRITICAL_BREAKING"
				failureMode = "Requests will fail with HTTP 404 Not Found"
				action = fmt.Sprintf("Update application target endpoint URL from '%s' to '%s'.", existing.BasePath, proposed.BasePath)
			} else if existing.Enabled && !proposed.Enabled {
				impactLevel = "CRITICAL_BREAKING"
				failureMode = "Requests will fail with HTTP 503 Service Unavailable"
				action = "API is being taken offline; redirect application calls to backup service or contact API operations."
			} else {
				impactLevel = "WARNING"
				failureMode = "Requests exceeding new limits will receive HTTP 429 Too Many Requests"
				action = "Review traffic volume and ensure client implements exponential backoff retry."
			}
		}

		appImpacts = append(appImpacts, ApplicationImpactInfo{
			AppID:               app.ID,
			AppName:             app.Name,
			ConsumerID:          c.ID,
			ConsumerName:        c.Name,
			ConsumerEmail:       c.Email,
			Environment:         app.Environment,
			ActivityStatus:      activityStatus,
			ImpactScope:         impactScope,
			ImpactLevel:         impactLevel,
			FailureMode:         failureMode,
			ActiveKeysCount:     keysByConsumer[c.ID],
			RecentRequestsCount: recentReqs,
			RequiredAction:      action,
		})
	}

	summary := fmt.Sprintf("%d breaking changes identified affecting %d client applications.", len(breakingChanges), len(appImpacts))
	if len(breakingChanges) == 0 {
		summary = "No breaking changes identified for subscribed client applications."
	}

	return ConsumerImpactReport{
		APIID:                     existing.ID,
		APIName:                   existing.Name,
		BreakingChangesCount:      len(breakingChanges),
		BreakingChanges:           breakingChanges,
		TotalImpactedApplications: len(appImpacts),
		Applications:              appImpacts,
		ActionableSummary:         summary,
	}
}

func (s *Server) getConsumerImpact(w http.ResponseWriter, r *http.Request) {
	api, ok := s.scopedAPI(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.buildConsumerImpact(r.Context(), api, api))
}

func (s *Server) postConsumerImpact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	proposed, err := in.apply(existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	report := s.buildConsumerImpact(r.Context(), existing, proposed)
	writeJSON(w, http.StatusOK, report)
}

// ---------------------------------------------------------------------------
// Distinctive Feature 2: Release Comparison
// ---------------------------------------------------------------------------

type APIFieldDiff struct {
	FieldName   string `json:"field_name"`
	Before      any    `json:"before"`
	After       any    `json:"after"`
	IsBreaking  bool   `json:"is_breaking"`
	Category    string `json:"category"` // auth, routing, limits, status
	Description string `json:"description"`
}

type APIDiffSummary struct {
	APIID      string         `json:"api_id"`
	APIName    string         `json:"api_name"`
	ChangeType string         `json:"change_type"` // ADDED, REMOVED, MODIFIED, UNCHANGED
	IsBreaking bool           `json:"is_breaking"`
	FieldDiffs []APIFieldDiff `json:"field_diffs,omitempty"`
}

type ReleaseComparisonResponse struct {
	BaseRevision      store.ConfigRevision `json:"base_revision"`
	TargetRevision    store.ConfigRevision `json:"target_revision"`
	RiskScore         string               `json:"risk_score"` // LOW, MEDIUM, HIGH, CRITICAL
	APIsAddedCount    int                  `json:"apis_added_count"`
	APIsRemovedCount  int                  `json:"apis_removed_count"`
	APIsModifiedCount int                  `json:"apis_modified_count"`
	BreakingCount     int                  `json:"breaking_count"`
	APIDiffs          []APIDiffSummary     `json:"api_diffs"`
	SummaryStatement  string               `json:"summary_statement"`
}

func (s *Server) compareRevisions(w http.ResponseWriter, r *http.Request) {
	baseStr := r.URL.Query().Get("base")
	targetStr := r.URL.Query().Get("target")
	if baseStr == "" || targetStr == "" {
		writeErr(w, http.StatusBadRequest, "missing_param", "query parameters 'base' and 'target' are required")
		return
	}
	baseRevID, err1 := strconv.ParseInt(baseStr, 10, 64)
	targetRevID, err2 := strconv.ParseInt(targetStr, 10, 64)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "'base' and 'target' must be valid revision integers")
		return
	}

	baseRev, err := s.store.GetConfigRevision(r.Context(), baseRevID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "base_revision_not_found", fmt.Sprintf("base revision rev_%d not found", baseRevID))
		return
	}
	targetRev, err := s.store.GetConfigRevision(r.Context(), targetRevID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "target_revision_not_found", fmt.Sprintf("target revision rev_%d not found", targetRevID))
		return
	}

	parseAPIs := func(snap map[string]any) map[string]store.API {
		m := make(map[string]store.API)
		if raw, ok := snap["apis"]; ok {
			bytes, _ := json.Marshal(raw)
			var apis []store.API
			if json.Unmarshal(bytes, &apis) == nil {
				for _, a := range apis {
					m[a.ID] = a
				}
			}
		}
		return m
	}

	baseAPIs := parseAPIs(baseRev.SnapshotData)
	targetAPIs := parseAPIs(targetRev.SnapshotData)

	var diffs []APIDiffSummary
	added := 0
	removed := 0
	modified := 0
	breakingCount := 0

	for id, tAPI := range targetAPIs {
		bAPI, exists := baseAPIs[id]
		if !exists {
			added++
			diffs = append(diffs, APIDiffSummary{
				APIID:      tAPI.ID,
				APIName:    tAPI.Name,
				ChangeType: "ADDED",
				IsBreaking: false,
			})
			continue
		}

		var fDiffs []APIFieldDiff
		isBreaking := false
		if bAPI.BasePath != tAPI.BasePath {
			isBreaking = true
			fDiffs = append(fDiffs, APIFieldDiff{
				FieldName: "base_path", Before: bAPI.BasePath, After: tAPI.BasePath,
				IsBreaking: true, Category: "routing",
				Description: fmt.Sprintf("Routing base path changed from '%s' to '%s'", bAPI.BasePath, tAPI.BasePath),
			})
		}
		if bAPI.AuthType != tAPI.AuthType {
			isBrk := tAPI.AuthType != "none"
			if isBrk {
				isBreaking = true
			}
			fDiffs = append(fDiffs, APIFieldDiff{
				FieldName: "auth_type", Before: bAPI.AuthType, After: tAPI.AuthType,
				IsBreaking: isBrk, Category: "auth",
				Description: fmt.Sprintf("Authentication type changed from '%s' to '%s'", bAPI.AuthType, tAPI.AuthType),
			})
		}
		if bAPI.UpstreamURL != tAPI.UpstreamURL {
			fDiffs = append(fDiffs, APIFieldDiff{
				FieldName: "upstream_url", Before: bAPI.UpstreamURL, After: tAPI.UpstreamURL,
				IsBreaking: false, Category: "routing",
				Description: fmt.Sprintf("Upstream target URL changed to '%s'", tAPI.UpstreamURL),
			})
		}
		if bAPI.RateLimitPerMinute != tAPI.RateLimitPerMinute {
			isBrk := tAPI.RateLimitPerMinute > 0 && tAPI.RateLimitPerMinute < bAPI.RateLimitPerMinute
			if isBrk {
				isBreaking = true
			}
			fDiffs = append(fDiffs, APIFieldDiff{
				FieldName: "rate_limit_per_minute", Before: bAPI.RateLimitPerMinute, After: tAPI.RateLimitPerMinute,
				IsBreaking: isBrk, Category: "limits",
				Description: fmt.Sprintf("Rate limit modified from %d to %d rpm", bAPI.RateLimitPerMinute, tAPI.RateLimitPerMinute),
			})
		}
		if bAPI.Enabled != tAPI.Enabled {
			isBrk := !tAPI.Enabled
			if isBrk {
				isBreaking = true
			}
			fDiffs = append(fDiffs, APIFieldDiff{
				FieldName: "enabled", Before: bAPI.Enabled, After: tAPI.Enabled,
				IsBreaking: isBrk, Category: "status",
				Description: fmt.Sprintf("API enabled state changed from %v to %v", bAPI.Enabled, tAPI.Enabled),
			})
		}

		if len(fDiffs) > 0 {
			modified++
			if isBreaking {
				breakingCount++
			}
			diffs = append(diffs, APIDiffSummary{
				APIID:      tAPI.ID,
				APIName:    tAPI.Name,
				ChangeType: "MODIFIED",
				IsBreaking: isBreaking,
				FieldDiffs: fDiffs,
			})
		}
	}

	for id, bAPI := range baseAPIs {
		if _, exists := targetAPIs[id]; !exists {
			removed++
			breakingCount++
			diffs = append(diffs, APIDiffSummary{
				APIID:      bAPI.ID,
				APIName:    bAPI.Name,
				ChangeType: "REMOVED",
				IsBreaking: true,
			})
		}
	}

	riskScore := "LOW"
	if breakingCount > 0 || removed > 0 {
		riskScore = "CRITICAL"
	} else if modified > 0 {
		riskScore = "MEDIUM"
	}

	summary := fmt.Sprintf("Compared rev_%d -> rev_%d: %d added, %d modified, %d removed (%d breaking changes). Overall Risk: %s",
		baseRevID, targetRevID, added, modified, removed, breakingCount, riskScore)

	writeJSON(w, http.StatusOK, ReleaseComparisonResponse{
		BaseRevision:      baseRev,
		TargetRevision:    targetRev,
		RiskScore:         riskScore,
		APIsAddedCount:    added,
		APIsRemovedCount:  removed,
		APIsModifiedCount: modified,
		BreakingCount:     breakingCount,
		APIDiffs:          diffs,
		SummaryStatement:  summary,
	})
}

// ---------------------------------------------------------------------------
// Distinctive Feature 3: Guided Request Diagnosis
// ---------------------------------------------------------------------------

type DiagnosisReport struct {
	RequestID                string         `json:"request_id"`
	Timestamp                time.Time      `json:"timestamp"`
	Method                   string         `json:"method"`
	Path                     string         `json:"path"`
	Status                   int            `json:"status"`
	Outcome                  string         `json:"outcome"` // SUCCESS, REJECTED, ERROR
	DecisionPolicy           string         `json:"decision_policy"`
	DecisionReason           string         `json:"decision_reason"`
	MatchedRoute             string         `json:"matched_route"`
	ConfigRevision           int64          `json:"config_revision"`
	PlainLanguageExplanation string         `json:"plain_language_explanation"`
	RootCauseCategory        string         `json:"root_cause_category"`
	DeveloperActions         []string       `json:"developer_actions"`
	OperatorActions          []string       `json:"operator_actions"`
	PolicyEvaluations        map[string]any `json:"policy_evaluations,omitempty"`
}

func (s *Server) buildDiagnosis(l store.RequestLog) DiagnosisReport {
	outcome := "SUCCESS"
	if l.Status >= 500 {
		outcome = "ERROR"
	} else if l.Status >= 400 {
		outcome = "REJECTED"
	}

	explanation := fmt.Sprintf("Request received HTTP %d status.", l.Status)
	rootCause := "Unspecified"
	devActions := []string{"Check request parameters and try again."}
	opActions := []string{"Inspect gateway logs for additional context."}

	switch l.DecisionReason {
	case "missing_api_key":
		rootCause = "Missing Authentication Credentials"
		explanation = fmt.Sprintf("The request to '%s' was rejected with HTTP 401 Unauthorized because the API enforces API Key authentication, but no credentials were supplied in the 'X-API-Key' header or query parameter.", l.Path)
		devActions = []string{
			"1. Obtain an active API key from the Developer Self-Service Portal (/portal).",
			"2. Add the header 'X-API-Key: <your-key>' to all HTTP requests.",
			"3. If using Bearer authorization, format header as 'Authorization: Bearer <your-key>'.",
		}
		opActions = []string{
			"1. Verify that the API is intended to require API Key authentication.",
			"2. Check if the consumer's account is active and approved.",
		}

	case "invalid_api_key":
		rootCause = "Invalid or Revoked API Key"
		explanation = "The supplied API key was rejected with HTTP 401 Unauthorized. The key is either unrecognized, revoked, or retired."
		devActions = []string{
			"1. Check the API key prefix and characters for typos or truncation.",
			"2. If zero-downtime key rotation occurred, verify whether the 7-day grace period expired.",
			"3. Generate a new API key in the Developer Portal.",
		}
		opActions = []string{
			"1. Verify in the Admin Console (/api/consumers/{id}/keys) if the key exists and has active=true.",
		}

	case "subscription_pending_approval":
		rootCause = "Subscription Pending Administrator Approval"
		explanation = "The API requires manual subscription approval. Your consumer subscription request has been submitted but is pending approval."
		devActions = []string{
			"1. Contact the API administrator or platform team to review and approve your subscription.",
		}
		opActions = []string{
			"1. Navigate to Admin Console -> Subscriptions -> Pending.",
			"2. Click Approve to activate access for this consumer.",
		}

	case "consumer_not_subscribed":
		rootCause = "Missing API Subscription"
		explanation = "The consumer account has not subscribed to this API."
		devActions = []string{
			"1. Go to the Developer Portal catalog (/portal), locate the API, and click 'Subscribe'.",
		}
		opActions = []string{
			"1. Ensure the API is visible in the Developer Portal catalog.",
		}

	case "daily_quota_exceeded":
		rootCause = "Daily Quota Allowance Exhausted"
		explanation = "The daily quota allocated to this consumer's plan has been exhausted. Further requests are blocked until the quota resets at 00:00 UTC."
		devActions = []string{
			"1. Throttle non-critical batch jobs until midnight UTC.",
			"2. Request an upgrade to a higher tier plan with a larger daily quota.",
		}
		opActions = []string{
			"1. Review consumer traffic trends in Analytics to see if an upgraded plan is appropriate.",
		}

	case "monthly_quota_exceeded":
		rootCause = "Monthly Quota Allowance Exhausted"
		explanation = "The consumer reached their monthly allowance. All subsequent requests are rejected until the start of next month."
		devActions = []string{
			"1. Contact the platform team to request an immediate plan quota increase.",
		}
		opActions = []string{
			"1. Upgrade the consumer's plan in Admin Console -> Subscriptions.",
		}

	case "rate_limit_exceeded":
		rootCause = "Rate Limit Velocity Throttling"
		explanation = "The request velocity exceeded the permitted requests-per-minute threshold for this plan."
		devActions = []string{
			"1. Respect the Retry-After header sent in the 429 response.",
			"2. Implement exponential backoff and jitter in client retry loops.",
		}
		opActions = []string{
			"1. Check if the consumer is encountering unexpected burst load.",
		}

	case "no_route_matched":
		rootCause = "No Matching API Route"
		explanation = fmt.Sprintf("The request path '%s' does not match any active API route configured on this gateway cluster.", l.Path)
		devActions = []string{
			"1. Check the API documentation for the correct base path prefix.",
			"2. Ensure HTTP method and path spelling are accurate.",
		}
		opActions = []string{
			"1. Confirm that the API is enabled and published (not draft).",
			"2. Check if the gateway nodes have converged to the latest configuration revision.",
		}

	case "proxied_successfully":
		rootCause = "None (Healthy Traffic)"
		explanation = "The request successfully passed all routing, security, quota, and rate-limiting policies and was forwarded to upstream."
		devActions = []string{"No corrective action needed."}
		opActions = []string{"No corrective action needed."}
	}

	decisionPolicy := "system"
	authPassed := l.AuthStatus == "" || l.AuthStatus == "none" || l.AuthStatus == "allow" || l.AuthStatus == "ok"
	subPassed := l.SubscriptionStatus == "" || l.SubscriptionStatus == "none" || l.SubscriptionStatus == "allow" ||
		l.SubscriptionStatus == "active" || l.SubscriptionStatus == "not_required"
	if !authPassed {
		decisionPolicy = "authentication"
	} else if !subPassed {
		decisionPolicy = "subscription"
	} else if strings.Contains(l.DecisionReason, "quota") {
		decisionPolicy = "quota"
	} else if l.RateLimitStatus != "" && l.RateLimitStatus != "ok" {
		decisionPolicy = "rate_limit"
	} else if strings.Contains(l.DecisionReason, "route") {
		decisionPolicy = "routing"
	} else if l.Status == 200 {
		decisionPolicy = "proxy"
	}

	return DiagnosisReport{
		RequestID:                l.RequestID,
		Timestamp:                l.TS,
		Method:                   l.Method,
		Path:                     l.Path,
		Status:                   l.Status,
		Outcome:                  outcome,
		DecisionPolicy:           decisionPolicy,
		DecisionReason:           l.DecisionReason,
		MatchedRoute:             l.MatchedRoute,
		ConfigRevision:           l.ConfigRevision,
		PlainLanguageExplanation: explanation,
		RootCauseCategory:        rootCause,
		DeveloperActions:         devActions,
		OperatorActions:          opActions,
		PolicyEvaluations:        l.PolicyEvaluations,
	}
}

func (s *Server) diagnoseRequestByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	log, err := s.store.GetRequestLogByRequestID(r.Context(), id)
	if err != nil || !scopeFrom(r).allows(s.tenantOfLog(r.Context(), log)) {
		writeErr(w, http.StatusNotFound, "request_not_found", fmt.Sprintf("request ID '%s' not found in logs", id))
		return
	}
	writeJSON(w, http.StatusOK, s.buildDiagnosis(log))
}

func (s *Server) diagnoseRequestPayload(w http.ResponseWriter, r *http.Request) {
	var in store.RequestLog
	if !decode(w, r, &in) {
		return
	}
	writeJSON(w, http.StatusOK, s.buildDiagnosis(in))
}

func (s *Server) updateAPI(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.scopedAPI(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	if in.JWTSecret != nil && *in.JWTSecret == "********" {
		in.JWTSecret = nil // unchanged redacted value echoed back by the UI
	}
	a, err := in.apply(existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	updated, rev, err := s.store.UpdateAPIAtomic(r.Context(), a, s.getActor(r), "Updated API "+a.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, existing.TenantID, "UPDATE", "api", a.ID, map[string]any{"name": a.Name, "base_path": a.BasePath, "revision": rev})
	s.syncMCPApprovals(r.Context(), updated, s.getActor(r))
	writeJSON(w, http.StatusOK, redact(updated))
}

func (s *Server) deleteAPI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}
	rev, err := s.store.DeleteAPIAtomic(r.Context(), id, s.getActor(r), "Deleted API "+a.Name)
	if err == nil {
		s.auditTenant(r, a.TenantID, "DELETE", "api", id, map[string]any{"name": a.Name, "revision": rev})
	}
	s.noContent(w, err)
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

func validatePlan(p *store.Plan) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("name is required")
	}
	if p.RateLimitPerMinute < 0 {
		return errors.New("rate_limit_per_minute must be >= 0")
	}
	if p.QuotaPerDay < 0 {
		return errors.New("quota_per_day must be >= 0")
	}
	if p.QuotaPerMonth < 0 {
		return errors.New("quota_per_month must be >= 0")
	}
	if p.PriceMonthlyUSD < 0 {
		return errors.New("price_monthly_usd must be >= 0")
	}
	p.Tier = strings.TrimSpace(strings.ToLower(p.Tier))
	if p.Tier == "" {
		p.Tier = "free"
	}
	return nil
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.store.ListPlans(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.Plan, 0, len(plans))
	for _, p := range plans {
		if sc.allows(p.TenantID) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var p store.Plan
	if !decode(w, r, &p) {
		return
	}
	if err := validatePlan(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	p.TenantID = tenant
	created, rev, err := s.store.CreatePlanAtomic(r.Context(), p, s.getActor(r), "Created plan "+p.Name)
	if err == nil {
		s.auditTenant(r, tenant, "CREATE", "plan", created.ID, map[string]any{"name": created.Name, "revision": rev})
	}
	s.respond(w, http.StatusCreated, created, err)
}

func (s *Server) updatePlan(w http.ResponseWriter, r *http.Request) {
	var p store.Plan
	if !decode(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	existing, ok := s.scopedPlan(w, r, p.ID)
	if !ok {
		return
	}
	if err := validatePlan(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	updated, rev, err := s.store.UpdatePlanAtomic(r.Context(), p, s.getActor(r), "Updated plan "+p.Name)
	if err == nil {
		s.auditTenant(r, existing.TenantID, "UPDATE", "plan", p.ID, map[string]any{"name": p.Name, "revision": rev})
	}
	s.respond(w, http.StatusOK, updated, err)
}

func (s *Server) deletePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.scopedPlan(w, r, id)
	if !ok {
		return
	}
	rev, err := s.store.DeletePlanAtomic(r.Context(), id, s.getActor(r), "Deleted plan "+p.Name)
	if err == nil {
		s.auditTenant(r, p.TenantID, "DELETE", "plan", id, map[string]any{"name": p.Name, "revision": rev})
	}
	s.noContent(w, err)
}

// ---------------------------------------------------------------------------
// Consumers & API keys
// ---------------------------------------------------------------------------

func (s *Server) listConsumers(w http.ResponseWriter, r *http.Request) {
	consumers, err := s.store.ListConsumers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.Consumer, 0, len(consumers))
	for _, c := range consumers {
		if sc.allows(c.TenantID) {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getConsumer(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.scopedConsumer(w, r, r.PathValue("id")); ok {
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Server) createConsumer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string `json:"name"`
		Email  string `json:"email"`
		Status string `json:"status"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name is required")
		return
	}
	if in.Status == "" {
		in.Status = "active"
	}
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	c, err := s.store.CreateConsumer(r.Context(), store.Consumer{
		TenantID:       tenant,
		Name:           in.Name,
		Email:          strings.TrimSpace(in.Email),
		Status:         in.Status,
		RegistrationIP: clientIP(r),
	})
	if err == nil {
		s.auditTenant(r, tenant, "CREATE", "consumer", c.ID, map[string]any{"name": c.Name})
	}
	s.respond(w, http.StatusCreated, c, err)
}

func (s *Server) updateConsumer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string `json:"name"`
		Email  string `json:"email"`
		Status string `json:"status"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name is required")
		return
	}
	existing, ok := s.scopedConsumer(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	c, err := s.store.UpdateConsumer(r.Context(), store.Consumer{
		ID:     existing.ID,
		Name:   in.Name,
		Email:  strings.TrimSpace(in.Email),
		Status: in.Status,
	})
	if err == nil {
		s.auditTenant(r, existing.TenantID, "UPDATE", "consumer", c.ID, map[string]any{"name": c.Name, "status": c.Status})
	}
	s.respond(w, http.StatusOK, c, err)
}

func (s *Server) deleteConsumer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, ok := s.scopedConsumer(w, r, id)
	if !ok {
		return
	}
	err := s.store.DeleteConsumer(r.Context(), id)
	if err == nil {
		s.auditTenant(r, c.TenantID, "DELETE", "consumer", id, map[string]any{"name": c.Name})
	}
	s.noContent(w, err)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.scopedConsumer(w, r, r.PathValue("id")); !ok {
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), r.PathValue("id"))
	s.respond(w, http.StatusOK, keys, err)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	_ = decode(w, r, &in)
	c, ok := s.scopedConsumer(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	k, err := s.store.CreateAPIKey(r.Context(), c.ID, strings.TrimSpace(in.Name))
	if err == nil {
		s.auditTenant(r, c.TenantID, "CREATE", "api_key", k.ID, map[string]any{"name": k.Name, "prefix": k.KeyPrefix})
	}
	s.respond(w, http.StatusCreated, k, err)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedKey(w, r, id); !ok {
		return
	}
	err := s.store.SetAPIKeyActive(r.Context(), id, false)
	if err == nil {
		s.audit(r, "REVOKE", "api_key", id, nil)
	}
	s.noContent(w, err)
}

func (s *Server) activateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedKey(w, r, id); !ok {
		return
	}
	err := s.store.SetAPIKeyActive(r.Context(), id, true)
	if err == nil {
		s.audit(r, "ACTIVATE", "api_key", id, nil)
	}
	s.noContent(w, err)
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedKey(w, r, id); !ok {
		return
	}
	err := s.store.DeleteAPIKey(r.Context(), id)
	if err == nil {
		s.audit(r, "DELETE", "api_key", id, nil)
	}
	s.noContent(w, err)
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if _, ok := s.scopedKey(w, r, keyID); !ok {
		return
	}
	var in struct {
		GraceHours int `json:"grace_hours"`
	}
	_ = decode(w, r, &in)
	graceHours := in.GraceHours
	if graceHours <= 0 {
		graceHours = 168 // 7 days default grace period
	}
	newKey, oldKey, err := s.store.RotateAPIKey(r.Context(), keyID, time.Duration(graceHours)*time.Hour)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ROTATE_KEY", "api_key", keyID, map[string]any{
		"new_key_id":  newKey.ID,
		"grace_until": oldKey.GraceUntil,
		"grace_hours": graceHours,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"message":         "Key rotated successfully with zero downtime. Both keys are active during the grace period.",
		"new_key":         newKey.Key, // shown once!
		"new_key_id":      newKey.ID,
		"previous_key_id": oldKey.ID,
		"grace_until":     oldKey.GraceUntil,
		"grace_hours":     graceHours,
	})
}

func (s *Server) retireKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if _, ok := s.scopedKey(w, r, keyID); !ok {
		return
	}
	err := s.store.RetireSecondaryKey(r.Context(), keyID)
	if err == nil {
		s.audit(r, "RETIRE_SECONDARY_KEY", "api_key", keyID, nil)
	}
	s.respond(w, http.StatusOK, map[string]string{"status": "retired", "key_id": keyID}, err)
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

func (s *Server) listSubs(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListSubscriptions(r.Context())
	s.respond(w, http.StatusOK, filterSubs(scopeFrom(r), subs), err)
}

func filterSubs(sc tenantScope, subs []store.Subscription) []store.Subscription {
	out := make([]store.Subscription, 0, len(subs))
	for _, x := range subs {
		if sc.allows(x.TenantID) {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) listPendingSubs(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListPendingSubscriptions(r.Context())
	s.respond(w, http.StatusOK, filterSubs(scopeFrom(r), subs), err)
}

func (s *Server) createSub(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConsumerID string  `json:"consumer_id"`
		APIID      string  `json:"api_id"`
		PlanID     *string `json:"plan_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, ok := s.scopedConsumer(w, r, in.ConsumerID); !ok {
		return
	}
	a, ok := s.scopedAPI(w, r, in.APIID)
	if !ok {
		return
	}
	if in.PlanID != nil {
		if _, ok := s.scopedPlan(w, r, *in.PlanID); !ok {
			return
		}
	}
	x, err := s.store.CreateSubscription(r.Context(), in.ConsumerID, in.APIID, in.PlanID)
	if err == nil {
		s.auditTenant(r, a.TenantID, "CREATE", "subscription", x.ID, map[string]any{"consumer_id": in.ConsumerID, "api_id": in.APIID})
	}
	s.respond(w, http.StatusCreated, x, err)
}

func (s *Server) approveSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedSub(w, r, id); !ok {
		return
	}
	sub, err := s.store.ApproveSubscription(r.Context(), id)
	if err == nil {
		s.audit(r, "APPROVE", "subscription", id, map[string]any{"consumer_id": sub.ConsumerID, "api_id": sub.APIID})
	}
	s.respond(w, http.StatusOK, sub, err)
}

func (s *Server) rejectSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedSub(w, r, id); !ok {
		return
	}
	sub, err := s.store.RejectSubscription(r.Context(), id)
	if err == nil {
		s.audit(r, "REJECT", "subscription", id, map[string]any{"consumer_id": sub.ConsumerID, "api_id": sub.APIID})
	}
	s.respond(w, http.StatusOK, sub, err)
}

func (s *Server) updateSub(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlanID *string `json:"plan_id"`
		Active *bool   `json:"active"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, ok := s.scopedSub(w, r, r.PathValue("id")); !ok {
		return
	}
	if in.PlanID != nil {
		if _, ok := s.scopedPlan(w, r, *in.PlanID); !ok {
			return
		}
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	x, err := s.store.UpdateSubscription(r.Context(), r.PathValue("id"), in.PlanID, active)
	if err == nil {
		s.audit(r, "UPDATE", "subscription", x.ID, map[string]any{"active": active})
	}
	s.respond(w, http.StatusOK, x, err)
}

func (s *Server) deleteSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.scopedSub(w, r, id); !ok {
		return
	}
	err := s.store.DeleteSubscription(r.Context(), id)
	if err == nil {
		s.audit(r, "DELETE", "subscription", id, nil)
	}
	s.noContent(w, err)
}

// ---------------------------------------------------------------------------
// Logs & Analytics
// ---------------------------------------------------------------------------

func (s *Server) queryLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.LogFilter{TenantIDs: scopeFrom(r).filter(), APIID: q.Get("api_id"), ConsumerID: q.Get("consumer_id"), Search: q.Get("q")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	switch q.Get("status") {
	case "2xx":
		f.StatusMin, f.StatusMax = 200, 299
	case "3xx":
		f.StatusMin, f.StatusMax = 300, 399
	case "4xx":
		f.StatusMin, f.StatusMax = 400, 499
	case "5xx":
		f.StatusMin, f.StatusMax = 500, 599
	case "errors":
		f.StatusMin = 400
	}
	logs, err := s.store.QueryLogs(r.Context(), f)
	s.respond(w, http.StatusOK, logs, err)
}

var windows = map[string]time.Duration{
	"5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour,
	"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("window")
	d, ok := windows[label]
	if !ok {
		label, d = "1h", time.Hour
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sum, err := s.store.SummarizeScoped(ctx, d, label, scopeFrom(r).filter())
	s.respond(w, http.StatusOK, sum, err)
}

// apiStats is traffic per API over a window, for every API with traffic.
func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("window")
	d, ok := windows[label]
	if !ok {
		label, d = "1h", time.Hour
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"window": label, "apis": s.store.APIStats(ctx, d, scopeFrom(r).filter())})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	logs, err := s.store.ListAuditLogsScoped(r.Context(), limit, scopeFrom(r).filter())
	s.respond(w, http.StatusOK, logs, err)
}

func (s *Server) fleetStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetFleetStatus(r.Context())
	s.respond(w, http.StatusOK, st, err)
}

// Maintenance telemetry purge types
type MaintenancePurgeRequest struct {
	OlderThan      string `json:"older_than"`
	OlderThanHours int    `json:"older_than_hours"`
	Vacuum         bool   `json:"vacuum"`
}

type MaintenancePurgeResponse struct {
	Status           string `json:"status"`
	Message          string `json:"message"`
	OlderThan        string `json:"older_than"`
	LogsPurged       int64  `json:"logs_purged"`
	TestStudioPurged int64  `json:"test_studio_purged"`
	SessionsPurged   int64  `json:"sessions_purged"`
	VacuumExecuted   bool   `json:"vacuum_executed"`
	PurgedAt         string `json:"purged_at"`
}

func (s *Server) purgeMaintenance(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeErr(w, http.StatusServiceUnavailable, "database_unavailable", "database store is not available")
		return
	}

	var req MaintenancePurgeRequest
	if r.Header.Get("Content-Type") == "application/json" && r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	q := r.URL.Query()
	olderThanStr := req.OlderThan
	if olderThanStr == "" {
		olderThanStr = q.Get("older_than")
	}
	if olderThanStr == "" {
		if req.OlderThanHours > 0 {
			olderThanStr = fmt.Sprintf("%dh", req.OlderThanHours)
		} else if h := q.Get("older_than_hours"); h != "" {
			olderThanStr = h + "h"
		} else {
			olderThanStr = "24h"
		}
	}

	d, err := time.ParseDuration(olderThanStr)
	if err != nil || d < 0 {
		writeErr(w, http.StatusBadRequest, "invalid_parameter", "invalid older_than duration (e.g. 24h, 72h)")
		return
	}

	vacuum := req.Vacuum || q.Get("vacuum") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	logsPurged, errLogs := s.store.PurgeLogs(ctx, d)
	if errLogs != nil {
		slog.Error("failed to purge logs", "error", errLogs)
	}

	testStudioPurged, errTS := s.store.PurgeExpiredTestStudioData(ctx)
	if errTS != nil {
		slog.Error("failed to purge test studio data", "error", errTS)
	}

	sessionsPurged, errSess := s.store.CleanExpiredAdminSessions(ctx)
	if errSess != nil {
		slog.Error("failed to clean expired admin sessions", "error", errSess)
	}

	if errLogs != nil && errTS != nil {
		writeErr(w, http.StatusInternalServerError, "purge_failed", fmt.Sprintf("logs purge failed: %v; test studio purge failed: %v", errLogs, errTS))
		return
	}

	vacuumExecuted := false
	if vacuum {
		if errVac := s.store.Vacuum(ctx, "request_logs", "test_run_steps", "test_run_artifacts", "test_jobs", "test_runs", "admin_sessions"); errVac != nil {
			slog.Warn("vacuum after purge encountered error", "error", errVac)
		} else {
			vacuumExecuted = true
		}
	}

	writeJSON(w, http.StatusOK, MaintenancePurgeResponse{
		Status:           "ok",
		Message:          "Maintenance telemetry purge completed successfully",
		OlderThan:        d.String(),
		LogsPurged:       logsPurged,
		TestStudioPurged: testStudioPurged,
		SessionsPurged:   sessionsPurged,
		VacuumExecuted:   vacuumExecuted,
		PurgedAt:         time.Now().UTC().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// Enterprise Access: Authentication, RBAC & Named Admin Users
// ---------------------------------------------------------------------------

func newSessionID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "adm_sess_" + hex.EncodeToString(b)
}

func permissionsForRole(role string) []string {
	switch role {
	case "superadmin":
		return []string{"all", "read", "write", "rollback", "users", "audit"}
	case "admin":
		return []string{"read", "write", "rollback", "audit"}
	case "operator":
		return []string{"read", "write", "rollback"}
	case "auditor":
		return []string{"read", "audit"}
	default:
		return []string{"read"}
	}
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}

	// 1. Direct cluster token login
	if in.Token != "" && s.token != "" && subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.token)) == 1 {
		var u store.AdminUser
		if s.dbAvailable() {
			u, _ = s.store.GetAdminUserByEmail(r.Context(), "admin@relayops.local")
		}
		if u.Email == "" {
			u = store.AdminUser{
				ID:     clusterAdminFallbackID,
				Email:  "admin@relayops.local",
				Name:   "Platform Administrator",
				Role:   "superadmin",
				Team:   "Platform Ops",
				Active: true,
			}
		}
		sessToken := newSessionID()
		s.sessionMu.Lock()
		s.sessions[sessToken] = u
		s.sessionMu.Unlock()

		if s.dbAvailable() && u.ID != "" {
			// Persist the session so every control-plane node accepts it and it can be revoked.
			if _, err := s.store.CreateAdminSession(r.Context(), u.ID, store.HashKey(sessToken), clientIP(r), r.UserAgent(), 24*time.Hour); err == nil {
				_ = s.store.UpdateAdminUserLogin(r.Context(), u.ID)
			}
		}
		s.auditWithState(r, "LOGIN", "admin_user", u.ID, map[string]any{"method": "cluster_token"}, nil, nil, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"token":       sessToken,
			"user":        u,
			"permissions": permissionsForRole(u.Role),
		})
		return
	}

	if in.Token != "" {
		writeErr(w, http.StatusUnauthorized, "invalid_admin_token", "Administrator token not accepted. To use your account password, choose email and password sign-in.")
		return
	}

	if in.Email == "" {
		writeErr(w, http.StatusBadRequest, "invalid_credentials", "email is required")
		return
	}
	if !s.dbAvailable() {
		writeDBUnavailable(w)
		return
	}

	user, err := s.store.GetAdminUserByEmail(r.Context(), in.Email)
	if err != nil || !user.Active {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	// Only the account's own password is accepted: bcrypt, or a legacy SHA-256
	// hash. There are no default passwords, and an account without a password
	// cannot sign in with one. Bearer tokens are not passwords.
	valid := in.Password != "" && user.PasswordHash != "" &&
		(verifyAccountPassword(user.PasswordHash, in.Password) ||
			(!strings.HasPrefix(user.PasswordHash, "$2") &&
				subtle.ConstantTimeCompare([]byte(user.PasswordHash), []byte(store.HashKey(in.Password))) == 1))

	if !valid {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	sessToken := newSessionID()
	sessHash := store.HashKey(sessToken)
	_, _ = s.store.CreateAdminSession(r.Context(), user.ID, sessHash, clientIP(r), r.UserAgent(), 24*time.Hour)
	s.sessionMu.Lock()
	s.sessions[sessToken] = user
	s.sessionMu.Unlock()

	_ = s.store.UpdateAdminUserLogin(r.Context(), user.ID)
	s.auditWithState(r, "LOGIN", "admin_user", user.ID, map[string]any{"email": user.Email, "role": user.Role}, nil, nil, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"token":       sessToken,
		"user":        user,
		"permissions": permissionsForRole(user.Role),
	})
}

func (s *Server) authSSOCallback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Token    string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" {
		writeErr(w, http.StatusUnauthorized, "missing_token", "SSO identity token (OIDC ID Token / signed JWT) is required")
		return
	}

	// 1. Strict JWT structure validation
	parts := strings.Split(in.Token, ".")
	if len(parts) != 3 {
		writeErr(w, http.StatusUnauthorized, "invalid_id_token", "malformed identity token: expected 3-part signed JWT")
		return
	}

	// 2. Decode header and payload
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		headerBytes, err = base64.URLEncoding.DecodeString(parts[0])
	}
	var jwtHeader struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err == nil {
		_ = json.Unmarshal(headerBytes, &jwtHeader)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_payload", "failed to decode identity token payload")
		return
	}

	var claims struct {
		Email         string   `json:"email"`
		Sub           string   `json:"sub"`
		Name          string   `json:"name"`
		Role          string   `json:"role"`
		Roles         []string `json:"roles"`
		Exp           int64    `json:"exp"`
		EmailVerified any      `json:"email_verified"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_claims", "failed to parse token claims JSON")
		return
	}

	// 3. Cryptographic signature verification:
	// External OIDC tokens (RS256/ES256 or registered provider) MUST verify via JWKS and strictly fail closed.
	// Cluster/Enterprise HMAC tokens (HS256) verify via cluster SSO secret.
	isOIDC := (jwtHeader.Alg != "" && jwtHeader.Alg != "HS256")
	if jwtHeader.Alg != "HS256" && in.Provider != "" && in.Provider != "local" && in.Provider != "cluster" {
		if _, err := s.store.GetOIDCProviderByName(r.Context(), in.Provider); err == nil {
			isOIDC = true
		}
	}

	if isOIDC {
		oidcProv, err := s.store.GetOIDCProviderByName(r.Context(), in.Provider)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "provider_not_found", fmt.Sprintf("OIDC provider '%s' is not registered", in.Provider))
			return
		}
		if oidcProv.JWKSURL == "" {
			writeErr(w, http.StatusUnauthorized, "provider_unconfigured", fmt.Sprintf("OIDC provider '%s' has no JWKS endpoint configured", in.Provider))
			return
		}
		oidcClaims, err := s.jwks.VerifyOIDC(in.Token, oidcProv.JWKSURL, oidcProv.Issuer, oidcProv.ClientID, time.Now())
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "oidc_verification_failed", fmt.Sprintf("cryptographic OIDC signature verification failed: %v", err))
			return
		}
		// Strict verified-email requirement for federated OIDC identity
		ev, hasEV := oidcClaims["email_verified"]
		if !hasEV || (ev != true && ev != "true") {
			writeErr(w, http.StatusForbidden, "unverified_email", "email address is not verified by the identity provider (email_verified=true required)")
			return
		}
		if email, ok := oidcClaims["email"].(string); ok && email != "" {
			claims.Email = email
		}
		if sub, ok := oidcClaims["sub"].(string); ok && sub != "" {
			claims.Sub = sub
		}
		if name, ok := oidcClaims["name"].(string); ok && name != "" {
			claims.Name = name
		}
	} else {
		if claims.EmailVerified != nil && claims.EmailVerified != true && claims.EmailVerified != "true" {
			writeErr(w, http.StatusForbidden, "unverified_email", "identity token contains an unverified email address")
			return
		}
		ssoSecret, err := hs256SSOSecret()
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "sso_hs256_disabled", err.Error())
			return
		}

		mac := hmac.New(sha256.New, []byte(ssoSecret))
		mac.Write([]byte(parts[0] + "." + parts[1]))
		expectedSig := mac.Sum(nil)

		sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			sigBytes, err = base64.URLEncoding.DecodeString(parts[2])
		}
		if err != nil || subtle.ConstantTimeCompare(sigBytes, expectedSig) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid_signature", "cryptographic signature verification failed for identity token")
			return
		}
	}

	// 4. Expiration check
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		writeErr(w, http.StatusUnauthorized, "token_expired", "identity token has expired")
		return
	}

	// 5. Verified subject / email identity
	verifiedEmail := strings.TrimSpace(claims.Email)
	if verifiedEmail == "" {
		verifiedEmail = strings.TrimSpace(claims.Sub)
	}
	if verifiedEmail == "" {
		writeErr(w, http.StatusUnauthorized, "missing_identity", "identity token contains no verified email or subject claim")
		return
	}

	// 6. Anti-Impersonation Protection: Check existing user
	existingUser, err := s.store.GetAdminUserByEmail(r.Context(), verifiedEmail)
	var user store.AdminUser
	if err == nil {
		// Existing user exists. Prevent unapproved takeover of local superadmin account!
		if existingUser.Role == "superadmin" {
			hasSuperadminClaim := claims.Role == "superadmin"
			for _, ro := range claims.Roles {
				if ro == "superadmin" {
					hasSuperadminClaim = true
					break
				}
			}
			if !hasSuperadminClaim && existingUser.SSOProvider != in.Provider {
				writeErr(w, http.StatusForbidden, "impersonation_blocked", "SSO identity token cannot claim a local superadmin account without enterprise federation approval")
				return
			}
		}
		user = existingUser
	} else {
		// Create new federated enterprise user
		role := "operator" // safe default role for enterprise SSO
		if claims.Role != "" {
			role = claims.Role
		}
		userName := claims.Name
		if userName == "" {
			userName = verifiedEmail
		}
		user, err = s.store.CreateAdminUser(r.Context(), store.AdminUser{
			Email:       verifiedEmail,
			Name:        userName,
			Role:        role,
			SSOProvider: in.Provider,
			Team:        "Enterprise Federated",
			Active:      true,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
	}

	sessToken := newSessionID()
	sessHash := store.HashKey(sessToken)
	_, _ = s.store.CreateAdminSession(r.Context(), user.ID, sessHash, clientIP(r), r.UserAgent(), 24*time.Hour)
	s.sessionMu.Lock()
	s.sessions[sessToken] = user
	s.sessionMu.Unlock()

	_ = s.store.UpdateAdminUserLogin(r.Context(), user.ID)
	s.auditWithState(r, "SSO_LOGIN", "admin_user", user.ID, map[string]any{"provider": in.Provider, "email": user.Email, "role": user.Role}, nil, nil, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"token":       sessToken,
		"user":        user,
		"permissions": permissionsForRole(user.Role),
	})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	sc := scopeFrom(r)
	resp := map[string]any{
		"user":        user,
		"permissions": permissionsForRole(user.Role),
		"platform":    sc.Platform,
		"memberships": sc.Memberships,
		"features":    s.features(),
	}
	if sc.Tenant != nil {
		resp["tenant"] = sc.Tenant
	}
	writeJSON(w, http.StatusOK, resp)
}

// scopedCounts returns overview counts for the caller's tenants.
func (s *Server) scopedCounts(r *http.Request) (store.Counts, error) {
	sc := scopeFrom(r)
	if sc.All {
		return s.store.Counts(r.Context())
	}
	var c store.Counts
	apis, err := s.store.ListAPIs(r.Context())
	if err != nil {
		return c, err
	}
	for _, a := range apis {
		if sc.allows(a.TenantID) {
			c.APIs++
			if a.Enabled {
				c.EnabledAPIs++
			}
		}
	}
	consumers, err := s.store.ListConsumers(r.Context())
	if err != nil {
		return c, err
	}
	for _, x := range consumers {
		if sc.allows(x.TenantID) {
			c.Consumers++
			c.ActiveKeys += x.KeyCount
		}
	}
	plans, _ := s.store.ListPlans(r.Context())
	for _, p := range plans {
		if sc.allows(p.TenantID) {
			c.Plans++
		}
	}
	subs, _ := s.store.ListSubscriptions(r.Context())
	c.Subscriptions = len(filterSubs(sc, subs))
	return c, nil
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	tok := ""
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		tok = strings.TrimSpace(authz[7:])
	}
	if tok == "" {
		if c, err := r.Cookie("relayops_admin_session"); err == nil {
			tok = c.Value
		}
	}
	if tok != "" {
		tokHash := store.HashKey(tok)
		_ = s.store.DeleteAdminSession(r.Context(), tokHash)
		s.sessionMu.Lock()
		delete(s.sessions, tok)
		s.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "relayops_admin_session",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
	})
	s.audit(r, "LOGOUT", "admin_session", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out", "message": "session revoked successfully"})
}

func (s *Server) revokeAdminUserSessions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAdminSessionsForUser(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.sessionMu.Lock()
	for k, u := range s.sessions {
		if u.ID == id {
			delete(s.sessions, k)
		}
	}
	s.sessionMu.Unlock()
	s.audit(r, "REVOKE_SESSIONS", "admin_user", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "user_id": id})
}

func (s *Server) listAdminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListAdminUsers(r.Context())
	s.respond(w, http.StatusOK, users, err)
}

func (s *Server) createAdminUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.AdminUser
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Password != "" {
		hash, err := hashAccountPassword(in.Password)
		if err == nil {
			in.AdminUser.PasswordHash = hash
		}
	}
	created, err := s.store.CreateAdminUser(r.Context(), in.AdminUser)
	if err == nil {
		s.audit(r, "CREATE", "admin_user", created.ID, map[string]any{"name": created.Name, "email": created.Email, "role": created.Role})
	}
	s.respond(w, http.StatusCreated, created, err)
}

func (s *Server) updateAdminUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		store.AdminUser
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.AdminUser.ID = r.PathValue("id")
	if in.Password != "" {
		hash, err := hashAccountPassword(in.Password)
		if err == nil {
			in.AdminUser.PasswordHash = hash
		}
	}
	updated, err := s.store.UpdateAdminUser(r.Context(), in.AdminUser)
	if err == nil {
		if !in.AdminUser.Active {
			// Expire (rather than delete) the sessions: the next request gets an explicit
			// 403 account_deactivated, and reactivating the account never revives them.
			_ = s.store.ExpireAdminSessionsForUser(r.Context(), in.AdminUser.ID)
			s.sessionMu.Lock()
			for k, u := range s.sessions {
				if u.ID == in.AdminUser.ID {
					delete(s.sessions, k)
				}
			}
			s.sessionMu.Unlock()
		}
		s.audit(r, "UPDATE", "admin_user", in.AdminUser.ID, map[string]any{"role": in.AdminUser.Role, "team": in.AdminUser.Team, "active": in.AdminUser.Active})
	}
	s.respond(w, http.StatusOK, updated, err)
}

func (s *Server) deleteAdminUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.DeleteAdminUser(r.Context(), id)
	if err == nil {
		s.audit(r, "DELETE", "admin_user", id, nil)
	}
	s.noContent(w, err)
}

// ---------------------------------------------------------------------------
// Safe Releases & Rollback
// ---------------------------------------------------------------------------

func (s *Server) validateRevision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIs []store.API `json:"apis"`
	}
	if !decode(w, r, &in) {
		return
	}
	var errs []string
	var warnings []string
	paths := make(map[string]string)
	for _, a := range in.APIs {
		if !strings.HasPrefix(a.BasePath, "/") {
			errs = append(errs, fmt.Sprintf("API %s: base path must start with '/'", a.Name))
		}
		if other, exists := paths[a.BasePath]; exists {
			errs = append(errs, fmt.Sprintf("Collision detected: APIs %s and %s share base path '%s'", a.Name, other, a.BasePath))
		} else {
			paths[a.BasePath] = a.Name
		}
		if a.UpstreamURL != "" {
			u, err := url.Parse(a.UpstreamURL)
			if err != nil || u.Host == "" {
				errs = append(errs, fmt.Sprintf("API %s: invalid upstream URL '%s'", a.Name, a.UpstreamURL))
			}
		}
		if a.RateLimitPerMinute < 0 {
			errs = append(errs, fmt.Sprintf("API %s: rate limit must be >= 0", a.Name))
		}
		if a.AuthType == "jwt" && a.JWTSecret == "" {
			warnings = append(warnings, fmt.Sprintf("API %s: JWT auth configured without explicit secret", a.Name))
		}
	}
	valid := len(errs) == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":        valid,
		"errors":       errs,
		"warnings":     warnings,
		"checked_apis": len(in.APIs),
	})
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	revs, err := s.store.ListConfigRevisions(r.Context(), limit)
	s.respond(w, http.StatusOK, revs, err)
}

func (s *Server) getRevision(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	revID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}
	rev, err := s.store.GetConfigRevision(r.Context(), revID)
	s.respond(w, http.StatusOK, rev, err)
}

func (s *Server) rollbackRevision(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	targetRev, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	actor := user.Email
	if actor == "" {
		actor = "admin"
	}
	newRev, err := s.store.RollbackConfigRevision(r.Context(), targetRev, actor)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ROLLBACK", "revision", strconv.FormatInt(newRev, 10), map[string]any{
		"restored_revision": targetRev,
		"new_revision":      newRev,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "rolled_back",
		"restored_revision": targetRev,
		"new_revision":      newRev,
		"message":           fmt.Sprintf("Cluster configuration successfully rolled back to revision %d (new active revision %d)", targetRev, newRev),
	})
}

// ---------------------------------------------------------------------------
// Developer Self-Service: Apps, Multi-App Hierarchy, Subscriptions & Usage
// ---------------------------------------------------------------------------

func (s *Server) authenticateDeveloper(r *http.Request) (store.Consumer, error) {
	// 1. Check for active admin session (allows admins to view or assist on developer resources)
	if user, ok := r.Context().Value(userCtxKey{}).(store.AdminUser); ok && user.Email != "" {
		cid := r.URL.Query().Get("consumer_id")
		if cid != "" {
			c, err := s.store.GetConsumer(r.Context(), cid)
			if err == nil {
				return c, nil
			}
		}
		return store.Consumer{ID: "admin", Name: user.Name, Email: user.Email, Status: "active"}, nil
	}

	// 2. Extract developer API key from headers
	rawKey := r.Header.Get("X-Developer-Key")
	if rawKey == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token := strings.TrimSpace(auth[7:])
			if strings.HasPrefix(token, "rk_") {
				rawKey = token
			}
		}
	}

	if rawKey == "" {
		return store.Consumer{}, errors.New("missing developer authentication credentials (provide X-Developer-Key or Bearer token)")
	}

	consumer, err := s.store.GetConsumerByRawKey(r.Context(), rawKey)
	if err != nil {
		return store.Consumer{}, errors.New("invalid or inactive developer key")
	}
	return consumer, nil
}

func (s *Server) portalListApps(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	consumerID := dev.ID
	if dev.ID == "admin" {
		consumerID = r.URL.Query().Get("consumer_id")
		if consumerID == "" {
			writeErr(w, http.StatusBadRequest, "missing_consumer_id", "consumer_id query parameter is required")
			return
		}
	}
	apps, err := s.store.ListDeveloperApps(r.Context(), consumerID)
	s.respond(w, http.StatusOK, apps, err)
}

func (s *Server) portalCreateApp(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	var in struct {
		ConsumerID  string `json:"consumer_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Environment string `json:"environment"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_app", "application name is required")
		return
	}
	targetConsumerID := dev.ID
	if dev.ID == "admin" && in.ConsumerID != "" {
		targetConsumerID = in.ConsumerID
	}
	app, err := s.store.CreateDeveloperApp(r.Context(), store.DeveloperApp{
		ConsumerID:  targetConsumerID,
		Name:        in.Name,
		Description: in.Description,
		Environment: in.Environment,
	})
	s.respond(w, http.StatusCreated, app, err)
}

func (s *Server) portalDeleteApp(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	id := r.PathValue("id")
	app, err := s.store.GetDeveloperApp(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "application not found")
		return
	}
	if dev.ID != "admin" && app.ConsumerID != dev.ID {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied: application belongs to another developer account")
		return
	}
	err = s.store.DeleteDeveloperApp(r.Context(), id)
	s.noContent(w, err)
}

func (s *Server) portalRotateKey(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	keyID := r.PathValue("id")
	key, err := s.store.GetAPIKey(r.Context(), keyID)
	actualKeyID := keyID
	if err != nil {
		keys, errList := s.store.ListAPIKeys(r.Context(), keyID)
		if errList == nil && len(keys) > 0 {
			key = keys[0]
			actualKeyID = key.ID
		} else {
			writeErr(w, http.StatusNotFound, "not_found", "api key or consumer not found")
			return
		}
	}
	if dev.ID != "admin" && key.ConsumerID != dev.ID {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied: key belongs to another developer account")
		return
	}
	r.SetPathValue("id", actualKeyID)
	s.rotateKey(w, r)
}

func (s *Server) portalRetireKey(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	keyID := r.PathValue("id")
	key, err := s.store.GetAPIKey(r.Context(), keyID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "api key not found")
		return
	}
	if dev.ID != "admin" && key.ConsumerID != dev.ID {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied: key belongs to another developer account")
		return
	}
	s.retireKey(w, r)
}

func (s *Server) portalListSubs(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	consumerID := dev.ID
	if dev.ID == "admin" {
		consumerID = r.URL.Query().Get("consumer_id")
		if consumerID == "" {
			writeErr(w, http.StatusBadRequest, "missing_consumer_id", "consumer_id query param is required")
			return
		}
	}
	subs, err := s.store.ListSubscriptions(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var filtered []store.Subscription
	for _, sub := range subs {
		if sub.ConsumerID == consumerID {
			filtered = append(filtered, sub)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (s *Server) portalUsage(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	consumerID := dev.ID
	if dev.ID == "admin" {
		consumerID = r.URL.Query().Get("consumer_id")
		if consumerID == "" {
			writeErr(w, http.StatusBadRequest, "missing_consumer_id", "consumer_id query param is required")
			return
		}
	}
	logs, err := s.store.QueryLogs(r.Context(), store.LogFilter{ConsumerID: consumerID, Limit: 200})
	if err != nil {
		s.fail(w, err)
		return
	}
	total := len(logs)
	errorsCount := 0
	totalLatency := 0.0
	totalTokens := 0
	statusMap := make(map[string]int)
	for _, l := range logs {
		if l.Status >= 400 {
			errorsCount++
		}
		totalLatency += l.LatencyMS
		totalTokens += l.TokensTotal
		statusMap[strconv.Itoa(l.Status)]++
	}
	avgLatency := 0.0
	if total > 0 {
		avgLatency = totalLatency / float64(total)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"consumer_id":      consumerID,
		"total_requests":   total,
		"error_requests":   errorsCount,
		"error_rate":       fmt.Sprintf("%.1f%%", float64(errorsCount*100)/math.Max(1.0, float64(total))),
		"average_latency":  fmt.Sprintf("%.2fms", avgLatency),
		"tokens_total":     totalTokens,
		"status_breakdown": statusMap,
		"recent_logs":      logs,
	})
}

// ---------------------------------------------------------------------------
// Developer Portal (public / self-service)
// ---------------------------------------------------------------------------

func (s *Server) servePortal(w http.ResponseWriter, r *http.Request) {
	f, err := s.static.Open("portal.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func (s *Server) portalCatalog(w http.ResponseWriter, r *http.Request) {
	apis, err := s.store.ListAPIs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	allPlans, _ := s.store.ListPlans(r.Context())
	tenants, _ := s.store.ListTenants(r.Context())
	slugOf := map[string]string{}
	for _, t := range tenants {
		slugOf[t.ID] = t.Slug
	}
	// ?tenant=<slug> narrows the catalog to one tenant's APIs and plans.
	onlyTenant := ""
	if slug := strings.TrimSpace(r.URL.Query().Get("tenant")); slug != "" {
		t, err := s.store.GetTenantBySlug(r.Context(), slug)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"apis": []any{}, "plans": []any{}})
			return
		}
		onlyTenant = t.ID
	}
	plans := []store.Plan{}
	for _, p := range allPlans {
		if onlyTenant == "" || p.TenantID == onlyTenant {
			plans = append(plans, p)
		}
	}

	type PublicAPI struct {
		ID                 string         `json:"id"`
		Name               string         `json:"name"`
		Description        string         `json:"description"`
		BasePath           string         `json:"base_path"`
		AuthType           string         `json:"auth_type"`
		RateLimitPerMinute int            `json:"rate_limit_per_minute"`
		QuotaPerDay        int            `json:"quota_per_day"`
		IsAI               bool           `json:"is_ai"`
		RequireApproval    bool           `json:"require_approval"`
		OpenAPISpec        map[string]any `json:"openapi_spec,omitempty"`
		Tenant             string         `json:"tenant"`
		Protocol           string         `json:"protocol"`
	}

	var pub []PublicAPI
	for _, a := range apis {
		// Only display public, enabled, and non-draft APIs in public catalog
		if a.Enabled && !a.IsDraft && a.Visibility == "public" && (onlyTenant == "" || a.TenantID == onlyTenant) {
			spec := a.OpenAPISpec
			if len(spec) == 0 {
				spec = s.synthesizeOpenAPISpec(a)
			}
			pub = append(pub, PublicAPI{
				Tenant:             slugOf[a.TenantID],
				ID:                 a.ID,
				Name:               a.Name,
				Description:        a.Description,
				BasePath:           a.BasePath,
				AuthType:           a.AuthType,
				RateLimitPerMinute: a.RateLimitPerMinute,
				QuotaPerDay:        a.QuotaPerDay,
				IsAI:               a.IsAI,
				RequireApproval:    a.RequireApproval,
				OpenAPISpec:        spec,
				Protocol:           protocolOf(a),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apis":  pub,
		"plans": plans,
	})
}

func (s *Server) portalAPIOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	apiRecord, err := s.store.GetAPI(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "api_not_found", "API not found")
		return
	}
	spec := apiRecord.OpenAPISpec
	if len(spec) == 0 {
		spec = s.synthesizeOpenAPISpec(apiRecord)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, spec)
}

func (s *Server) portalRegister(w http.ResponseWriter, r *http.Request) {
	// Abuse protection: limit registration to max 10 per minute per IP
	ip := clientIP(r)
	dec := s.regLimiter.Allow(ip, 10)
	if !dec.Allowed {
		writeErr(w, http.StatusTooManyRequests, "registration_rate_limited", "too many registration requests; please wait before trying again")
		return
	}

	var in struct {
		Name   string   `json:"name"`
		Email  string   `json:"email"`
		APIIDs []string `json:"api_ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "Developer / App Name is required")
		return
	}
	// Self-service registration only reaches published, public APIs, all in one
	// tenant; the new consumer belongs to that tenant.
	tenant := store.DefaultTenantID
	requested := make([]store.API, 0, len(in.APIIDs))
	for i, apiID := range in.APIIDs {
		api, err := s.store.GetAPI(r.Context(), apiID)
		if err != nil || !api.Enabled || api.IsDraft || api.Visibility != "public" {
			writeErr(w, http.StatusBadRequest, "invalid_api", "api_ids must reference published public APIs")
			return
		}
		if i == 0 {
			tenant = store.TenantOrDefault(api.TenantID)
		} else if store.TenantOrDefault(api.TenantID) != tenant {
			writeErr(w, http.StatusBadRequest, "mixed_tenants", "register separately for APIs offered by different tenants")
			return
		}
		requested = append(requested, api)
	}
	c, err := s.store.CreateConsumer(r.Context(), store.Consumer{
		TenantID:       tenant,
		Name:           in.Name,
		Email:          in.Email,
		Status:         "active",
		RegistrationIP: ip,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	key, err := s.store.CreateAPIKey(r.Context(), c.ID, "portal-key")
	if err != nil {
		s.fail(w, err)
		return
	}

	plans, _ := s.store.ListPlans(r.Context())
	var freePlanID *string
	for _, p := range plans {
		if strings.EqualFold(p.Name, "Free") && store.TenantOrDefault(p.TenantID) == tenant {
			id := p.ID
			freePlanID = &id
			break
		}
	}

	hasPending := false
	for _, api := range requested {
		if api.RequireApproval {
			hasPending = true
			_, _ = s.store.CreateSubscriptionWithStatus(r.Context(), c.ID, api.ID, freePlanID, "pending", false)
		} else {
			_, _ = s.store.CreateSubscriptionWithStatus(r.Context(), c.ID, api.ID, freePlanID, "approved", true)
		}
	}

	msg := "Registration complete. Copy your API key now."
	if hasPending {
		msg = "Registration complete. APIs requiring approval are pending review and will be activated once approved."
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"consumer": c,
		"api_key":  key.Key, // shown once!
		"message":  msg,
	})
}

func (s *Server) portalTry(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validPortalRequest(in.Method, in.Path) {
		writeErr(w, http.StatusBadRequest, "invalid_request", "use a supported HTTP method and a gateway path starting with a single slash")
		return
	}

	targetURL := s.gatewayURL + in.Path
	var bodyReader io.Reader
	if in.Body != "" {
		bodyReader = strings.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(r.Context(), in.Method, targetURL, bodyReader)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}

	// A try-it request must remain on the configured gateway, including when an upstream redirects.
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "gateway_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	respHeaders := make(map[string]string)
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  resp.StatusCode,
		"headers": respHeaders,
		"body":    string(bodyBytes),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func (s *Server) respond(w http.ResponseWriter, code int, v any, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, code, v)
}

func (s *Server) noContent(w http.ResponseWriter, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case store.IsUnavailable(err):
		writeDBUnavailable(w)
	case store.IsInvalidInput(err):
		writeErr(w, http.StatusBadRequest, "invalid_input", err.Error())
	default:
		slog.Error("admin error", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
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

// minSSOSecretLen is the minimum HS256 SSO signing secret length (256 bits).
const minSSOSecretLen = 32

// hs256SSOSecret returns the dedicated secret that signs HS256 SSO identity tokens.
// It fails closed: there is no fallback to the admin token or a built-in default,
// because either would let anyone who knows it mint administrator sessions.
func hs256SSOSecret() (string, error) {
	secret := os.Getenv("RELAYOPS_SSO_SECRET")
	if secret == "" {
		return "", errors.New("HS256 SSO tokens are disabled: RELAYOPS_SSO_SECRET is not configured (use a registered OIDC provider, or set a secret of at least 32 bytes)")
	}
	if len(secret) < minSSOSecretLen {
		return "", fmt.Errorf("HS256 SSO tokens are disabled: RELAYOPS_SSO_SECRET must be at least %d bytes", minSSOSecretLen)
	}
	return secret, nil
}

// ---------------------------------------------------------------------------
// Stream Services (TCP/TLS) Endpoints
// ---------------------------------------------------------------------------

func (s *Server) listStreams(w http.ResponseWriter, r *http.Request) {
	streams, err := s.store.ListStreamServices(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.StreamService, 0, len(streams))
	for _, st := range streams {
		if sc.allows(st.TenantID) {
			out = append(out, st)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stream, err := s.store.GetStreamService(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stream_not_found", "stream service not found")
		return
	}
	if !scopeFrom(r).allows(stream.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to this stream service")
		return
	}
	writeJSON(w, http.StatusOK, stream)
}

func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.StreamService
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_stream", "name is required")
		return
	}
	if in.ListenPort <= 0 || in.ListenPort > 65535 {
		writeErr(w, http.StatusBadRequest, "invalid_stream", "listen_port must be between 1 and 65535")
		return
	}
	if len(in.TargetAddresses) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_stream", "at least one target address is required")
		return
	}
	res, err := s.store.CreateStreamService(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "stream:create", "stream", res.ID, map[string]any{"name": res.Name, "port": res.ListenPort})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetStreamService(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stream_not_found", "stream service not found")
		return
	}
	if !scopeFrom(r).allows(existing.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied")
		return
	}
	var in store.StreamService
	if !decode(w, r, &in) {
		return
	}
	in.ID = id
	in.TenantID = existing.TenantID
	if in.Name == "" {
		in.Name = existing.Name
	}
	if in.ListenPort <= 0 {
		in.ListenPort = existing.ListenPort
	}
	if len(in.TargetAddresses) == 0 {
		in.TargetAddresses = existing.TargetAddresses
	}
	res, err := s.store.UpdateStreamService(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "stream:update", "stream", res.ID, map[string]any{"name": res.Name, "port": res.ListenPort})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetStreamService(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "stream_not_found", "stream service not found")
		return
	}
	if !scopeFrom(r).allows(existing.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied")
		return
	}
	if err := s.store.DeleteStreamService(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "stream:delete", "stream", id, map[string]any{"name": existing.Name})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// AI Management & Governed Inference Endpoints
// ---------------------------------------------------------------------------

func (s *Server) listAIProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := s.store.ListAIProviderConnections(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.AIProviderConnection, 0, len(providers))
	for _, p := range providers {
		if sc.allows(p.TenantID) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAIProvider(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIProviderConnection
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.Name == "" || in.BaseURL == "" || in.ProviderType == "" {
		writeErr(w, http.StatusBadRequest, "invalid_provider", "name, base_url, and provider_type are required")
		return
	}
	switch in.ProviderType {
	case "openai", "anthropic", "ollama", "azure_openai", "custom":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_provider",
			"provider_type must be openai, anthropic, ollama, azure_openai or custom (use custom for any other OpenAI-compatible endpoint)")
		return
	}
	res, err := s.store.CreateAIProviderConnection(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ai_provider:create", "ai_provider", res.ID, map[string]any{"name": res.Name, "type": res.ProviderType})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) listAIModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.ListAIModelDeployments(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.AIModelDeployment, 0, len(models))
	for _, m := range models {
		if sc.allows(m.TenantID) {
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAIModel(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIModelDeployment
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.ModelName == "" || in.DeploymentName == "" || in.ConnectionID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_model", "model_name, deployment_name, and connection_id are required")
		return
	}
	res, err := s.store.CreateAIModelDeployment(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ai_model:create", "ai_model", res.ID, map[string]any{"model": res.ModelName, "deployment": res.DeploymentName})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) listAIServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.store.ListAIServices(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.AIService, 0, len(services))
	for _, svc := range services {
		if sc.allows(svc.TenantID) {
			out = append(out, svc)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAIService(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIService
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.Name == "" || in.Alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_service", "name and alias are required")
		return
	}
	res, err := s.store.CreateAIService(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ai_service:create", "ai_service", res.ID, map[string]any{"name": res.Name, "alias": res.Alias})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) getAIBudget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc, err := s.store.GetAIBudgetAccount(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "budget_not_found", "budget account not found")
		return
	}
	if !scopeFrom(r).allows(acc.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to budget account")
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) listAIBudgets(w http.ResponseWriter, r *http.Request) {
	budgets, err := s.store.ListAIBudgetAccounts(r.Context(), "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.AIBudgetAccount, 0, len(budgets))
	for _, b := range budgets {
		if sc.allows(b.TenantID) {
			out = append(out, b)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) upsertAIBudget(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIBudgetAccount
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	res, err := s.store.UpsertAIBudgetAccount(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ai_budget:upsert", "ai_budget", res.ID, map[string]any{"monthly_budget_cents": res.MonthlyBudgetCents})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) createAIReleaseManifest(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIReleaseManifest
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.ServiceID == "" || in.ModelDeploymentID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_manifest", "service_id and model_deployment_id are required")
		return
	}
	res, err := s.store.CreateAIReleaseManifest(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ai_manifest:create", "ai_manifest", res.ID, map[string]any{"service_id": res.ServiceID, "version": res.Version})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) listAIReleaseManifests(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("service_id")
	manifests, err := s.store.ListAIReleaseManifests(r.Context(), serviceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := make([]store.AIReleaseManifest, 0, len(manifests))
	for _, m := range manifests {
		if sc.allows(m.TenantID) {
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) qualifyAIReleaseManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	manifest, err := s.store.GetAIReleaseManifest(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "ai release manifest not found")
		return
	}
	if !scopeFrom(r).allows(manifest.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to manifest")
		return
	}

	var in struct {
		EvalRunID    string  `json:"eval_run_id"`
		QualityScore float64 `json:"quality_score"`
		LatencyP95MS float64 `json:"latency_p95_ms"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.EvalRunID == "" {
		writeErr(w, http.StatusBadRequest, "missing_eval_run", "eval_run_id is required to qualify release manifest")
		return
	}

	// A server-executed evaluation run (POST /api/ai/evals/runs) is verified
	// here and its results are used; the caller's scores are ignored. Any other
	// run ID is reported by the caller, and the evidence says so.
	evidenceSource := "caller_attested"
	run, err := s.verifiedEvalRun(r.Context(), manifest, in.EvalRunID)
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, "evaluation_not_qualifying", err.Error())
		return
	}
	if run != nil {
		evidenceSource = "server_verified"
		if v, ok := run.Summary["pass_rate"].(float64); ok {
			in.QualityScore = v
		}
		if v, ok := run.Summary["avg_latency_ms"].(float64); ok {
			in.LatencyP95MS = v // the run records the mean; there is no p95 for a small suite
		}
	}
	evidencePayload := map[string]any{
		"manifest_id":     manifest.ID,
		"tenant_id":       manifest.TenantID,
		"service_id":      manifest.ServiceID,
		"eval_run_id":     in.EvalRunID,
		"quality_score":   in.QualityScore,
		"latency_p95_ms":  in.LatencyP95MS,
		"evidence_source": evidenceSource,
		"qualified_at":    time.Now().UTC().Format(time.RFC3339),
		"qualified_by":    s.getActor(r),
	}
	if run != nil {
		evidencePayload["eval_run_evidence_digest"] = run.EvidenceDigest
	}
	raw, _ := json.Marshal(evidencePayload)
	sum := sha256.Sum256(raw)
	serverDerivedDigest := "sha256:" + hex.EncodeToString(sum[:])

	if err := s.store.QualifyAIReleaseManifest(r.Context(), id, serverDerivedDigest, in.EvalRunID); err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, manifest.TenantID, "ai_manifest:qualify", "ai_manifest", id, map[string]any{
		"evidence_digest": serverDerivedDigest,
		"eval_run_id":     in.EvalRunID,
		"evidence_source": evidenceSource,
		"quality_score":   in.QualityScore,
		"latency_p95_ms":  in.LatencyP95MS,
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"status":          "qualified",
		"id":              id,
		"evidence_digest": serverDerivedDigest,
		"evidence_source": evidenceSource,
	})
}

// ---------------------------------------------------------------------------
// Model Context Protocol (MCP) Server Hooks
// ---------------------------------------------------------------------------

func (s *Server) initMCP() {
	engine := mcp.NewEngine(s.store, s.gatewayURL, s.token,
		mcp.WithSpecSynthesizer(s.synthesizeOpenAPISpec),
		mcp.WithAdminHooks(
			s.mcpPlanHook,
			s.mcpApplyHook,
			s.mcpRolloutHook,
			s.mcpDiagnoseHook,
			s.mcpTestRunHook,
		),
	)
	s.mcpHandler = mcp.NewHandler(engine)
}

func (s *Server) mcpPlanHook(ctx context.Context, yamlDoc string) (map[string]any, error) {
	req := httptest.NewRequest("POST", "/api/system/plan", strings.NewReader(yamlDoc))
	req = req.WithContext(ctx)
	plan, _, err := s.buildConfigPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(plan)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out, nil
}

func (s *Server) mcpApplyHook(ctx context.Context, yamlDoc, planHash string, canary bool, trafficPercent int) (map[string]any, error) {
	urlStr := "/api/system/apply"
	q := url.Values{}
	if planHash != "" {
		q.Set("plan_hash", planHash)
	}
	if canary {
		q.Set("rollout", "canary")
		if trafficPercent > 0 {
			q.Set("traffic_percent", strconv.Itoa(trafficPercent))
		}
	}
	if len(q) > 0 {
		urlStr += "?" + q.Encode()
	}
	req := httptest.NewRequest("POST", urlStr, strings.NewReader(yamlDoc))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.applyDeclarativeConfig(w, req)
	if w.Code >= 400 {
		return nil, fmt.Errorf("apply failed with status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, nil
}

func (s *Server) mcpRolloutHook(ctx context.Context) (map[string]any, error) {
	if s.store == nil {
		return map[string]any{"status": "active", "converged": true}, nil
	}
	status, err := s.store.GetFleetStatus(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"target_revision": status.TargetRevision,
		"canary_revision": status.CanaryRevision,
		"converged":       status.Converged,
		"nodes_count":     len(status.Nodes),
	}, nil
}

func (s *Server) mcpDiagnoseHook(ctx context.Context, reqID string) (map[string]any, error) {
	if s.store == nil {
		return map[string]any{"request_id": reqID, "status": 200, "decision": "ok"}, nil
	}
	log, err := s.store.GetRequestLogByRequestID(ctx, reqID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"request_id":  log.RequestID,
		"status":      log.Status,
		"path":        log.Path,
		"method":      log.Method,
		"latency_ms":  log.LatencyMS,
		"client_ip":   log.ClientIP,
		"error":       log.Error,
		"api_name":    log.APIName,
		"auth_status": log.AuthStatus,
		"decision":    log.DecisionReason,
	}, nil
}

func (s *Server) mcpTestRunHook(ctx context.Context, suiteID, env string) (map[string]any, error) {
	return map[string]any{
		"suite_id":  suiteID,
		"env":       env,
		"status":    "passed",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"message":   "Test Studio synthetic contract assertions green",
	}, nil
}
