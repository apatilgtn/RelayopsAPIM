// Command relayops runs the RelayOps APIM: data-plane gateway + control-plane
// admin API/dashboard, backed by Postgres with real-time config propagation.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/license"
	"github.com/relayops/apim/internal/orbit"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/stream"
	"github.com/relayops/apim/internal/tracing"
	"github.com/relayops/apim/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) > 1 && os.Args[1] == "dataplane-keygen" {
		if err := dataplaneKeygen(); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Gateway-only nodes hold no database credentials and run no control plane.
	if cfg.Role == config.RoleGateway {
		return runGateway(ctx, cfg)
	}
	if cfg.Role == config.RoleRelay {
		return runRelay(ctx, cfg)
	}

	// The database may be down at startup (outage, or the node was replaced
	// during one). The node then serves its last-known-good config and spools
	// request logs, keeps a lazily connecting pool, and recovers by itself:
	// migrations run as soon as the database is reachable, and reloads, change
	// notifications and spool replay resume without a restart.
	var db *store.Store
	migrated := make(chan struct{})
	reachable := false
	if dbConn, err := store.Open(ctx, cfg.DatabaseURL); err == nil {
		db, reachable = dbConn, true
	} else if lazy, lerr := store.OpenLazy(cfg.DatabaseURL); lerr == nil {
		slog.Warn("database unreachable at startup; serving cached configuration until it returns", "err", err)
		db = lazy
	} else {
		slog.Error("database URL is invalid; running in cached recovery mode without a database", "err", lerr)
	}
	if db == nil && os.Getenv("RELAYOPS_VAULT_ENABLED") == "true" {
		if token := os.Getenv("RELAYOPS_ADMIN_TOKEN"); token == "" || token == "relayops-admin" {
			return errors.New("Vault unavailable and no secure administrator fallback is configured")
		}
	}
	if db != nil {
		defer db.Close()
		if os.Getenv("RELAYOPS_VAULT_ENABLED") == "true" {
			vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			count, err := config.LoadHostedEnvironment(vctx, db.Pool)
			cancel()
			if err != nil {
				if token := os.Getenv("RELAYOPS_ADMIN_TOKEN"); token == "" || token == "relayops-admin" {
					return errors.New("Vault unavailable and no secure administrator fallback is configured")
				}
				slog.Warn("Vault unavailable; using supplied environment fallback")
			} else {
				cfg = config.Load()
				slog.Info("hosted configuration loaded", "values", count)
			}
		}
		migrate := func() error {
			if err := db.Migrate(ctx); err != nil {
				return err
			}
			slog.Info("database ready")
			close(migrated)
			return nil
		}
		var err error = errors.New("database unreachable")
		if reachable {
			err = migrate() // as before: a reachable database is migrated before serving
			if err != nil {
				slog.Warn("database migration failed at startup; retrying in the background", "err", err)
			}
		}
		if err != nil {
			go func() {
				for delay := time.Second; ; {
					select {
					case <-ctx.Done():
						return
					case <-time.After(delay):
					}
					err := migrate()
					if err == nil {
						return
					}
					if !store.IsUnavailable(err) {
						slog.Error("database migration failed; retrying", "err", err)
					}
					if delay < 30*time.Second {
						delay *= 2
					}
				}
			}()
		}
	}

	hub := realtime.NewHub()
	collector := analytics.NewCollector(db, hub, cfg.NodeID)
	enableSpool(collector, cfg)
	redisLimiter := gateway.NewRedisLimiter(cfg.RedisURL, gateway.NewLimiter())
	gw := gateway.New(collector, redisLimiter, cfg.NodeID)
	gw.SetNodeMetadata(cfg.NodeGroup, cfg.IsCanary)
	gw.SetRunnerSecret(cfg.RunnerSecret)
	tracer := tracing.NewFromEnv("relayops-gateway", cfg.NodeID)
	gw.SetTracer(tracer)
	if err := loadWasm(ctx, cfg, gw); err != nil {
		return err
	}
	if db != nil {
		gw.SetAIBudgetLedger(db)
	}
	src := gateway.NewDBSource(db)
	if src != nil && cfg.Role == config.RoleControlPlane {
		// A control-plane node keeps a snapshot for admin features but serves no
		// traffic, so it must not count as a gateway in fleet status.
		src = silentAcks{src}
	}
	watcher := gateway.NewWatcherWithSource(gw, src, hub, cfg.ResyncInterval)
	if err := watcher.Reload(ctx, "startup"); err != nil {
		return err
	}

	bg, cancelBG := context.WithCancel(context.Background())
	defer cancelBG()
	collectorDone := make(chan struct{})
	go func() { collector.Run(bg); close(collectorDone) }()
	go collector.RunRetention(bg, cfg.LogRetention)
	go watcher.Run(bg)
	serveProxy := cfg.Role != config.RoleControlPlane
	if serveProxy {
		go gw.RunHealthChecks(bg)
	} else if cfg.GatewayURL == "" {
		slog.Warn("control-plane role without RELAYOPS_GATEWAY_URL: the portal workbench, MCP invoke_api and Test Studio will call " + cfg.ProxyAddr + ", which this node does not serve")
	}

	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		return err
	}
	if cfg.AdminToken == "" {
		slog.Warn("RELAYOPS_ADMIN_TOKEN is empty: admin API is UNAUTHENTICATED")
	}
	leaderOpt, err := leaderElectionOption(cfg)
	if err != nil {
		return err
	}
	ent := license.Load()
	slog.Info("license", "edition", ent.Public()["edition"], "enforced", ent.Enforced, "preview_ai", ent.PreviewAI, "preview_apiops", ent.PreviewAPIOps)
	adminCore := admin.New(db, gw, hub, cfg.AdminToken, cfg.NodeID, static,
		admin.WithGatewayAddress(cfg.ProxyAddr),
		admin.WithRunnerSecret(cfg.RunnerSecret),
		admin.WithLicense(ent),
		admin.WithGatewayURL(cfg.GatewayURL),
		nodeAPIOption(cfg),
		admin.WithAutoRollbackInterval(cfg.AutoRollbackInterval),
		leaderOpt,
		admin.WithRuntimeInfo(admin.RuntimeInfo{Role: cfg.Role, LogSampleRate: cfg.LogSampleRate}),
		admin.WithAlertWebhook(cfg.AlertWebhookURL),
		admin.WithOrbit(orbit.Config{BaseURL: cfg.OrbitBaseURL, Model: cfg.OrbitModel, APIKey: cfg.OrbitAPIKey, FallbackModel: cfg.OrbitFallbackModel}),
	)
	if db != nil {
		gw.SetMCPReporter(adminCore.MCPReporter(cfg.NodeID))
	}
	go adminCore.StartAutoRollbackSupervisor(bg)
	go adminCore.StartDataplaneNotifier(bg)
	streamMgr := stream.NewManager()
	if db != nil {
		go adminCore.StartTestRunner(bg)
		go adminCore.StartTestRetentionCleaner(bg, 1*time.Hour)
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-bg.Done():
					return
				case <-ticker.C:
					services, err := db.ListStreamServices(bg, "")
					if err == nil {
						_ = streamMgr.Sync(services)
					}
				}
			}
		}()
	}
	if db != nil && cfg.AdoptSQL {
		go func() {
			select {
			case <-migrated: // adopt drift only against an up-to-date schema
				adminCore.StartDriftAdopter(bg, 5*time.Minute)
			case <-bg.Done():
			}
		}()
	}
	adminSrv := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           adminCore.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.AdminClientCAFile != "" {
		tlsCfg, err := clientCATLS(cfg.AdminClientCAFile)
		if err != nil {
			return err
		}
		adminSrv.TLSConfig = tlsCfg
	}

	proxySrv := newProxyServer(cfg, gw)
	if err := configureGatewayTLS(cfg, gw, proxySrv); err != nil {
		return err
	}

	errc := make(chan error, 2)
	if serveProxy {
		go listen(proxySrv, "proxy", cfg.TLSCertFile, cfg.TLSKeyFile, errc)
	}
	go listen(adminSrv, "admin", cfg.AdminTLSCertFile, cfg.AdminTLSKeyFile, errc)

	slog.Info("RelayOps APIM started", "role", cfg.Role, "proxy", cfg.ProxyAddr, "proxy_served", serveProxy, "admin", cfg.AdminAddr, "node", cfg.NodeID)

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errc:
		cancelBG()
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(shutCtx)
	_ = adminSrv.Shutdown(shutCtx)
	streamMgr.StopAll()
	cancelBG()
	<-collectorDone // final log flush
	if e := collector.LogExporter(); e != nil {
		e.Shutdown(shutCtx) // after the collector's final flush
	}
	tracer.Shutdown(shutCtx)
	return nil
}
