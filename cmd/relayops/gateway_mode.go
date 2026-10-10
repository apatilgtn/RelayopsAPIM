package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/coordinator"
	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/stream"
	"github.com/relayops/apim/internal/tracing"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// runGateway runs a gateway-only node: the proxy and a status listener, with
// configuration, acknowledgements, request logs and AI budgets exchanged with
// the control plane's node API. It never opens a database connection.
func runGateway(ctx context.Context, cfg config.Config) error {
	tlsCfg, err := dataplane.LoadClientTLS(cfg.DataplaneCAFile, cfg.DataplaneClientCertFile, cfg.DataplaneClientKeyFile)
	if err != nil {
		return err
	}
	var verifier *dataplane.Verifier
	if cfg.DataplaneVerifyKeys != "" {
		if verifier, err = dataplane.ParseVerifyKeys(cfg.DataplaneVerifyKeys); err != nil {
			return fmt.Errorf("RELAYOPS_DATAPLANE_VERIFY_KEYS: %w", err)
		}
	} else {
		slog.Warn("RELAYOPS_DATAPLANE_VERIFY_KEYS is not set: configuration signatures are not verified")
	}
	client, err := dataplane.NewClient(dataplane.ClientConfig{
		TLS:       tlsCfg,
		BaseURL:   cfg.ControlPlaneURL,
		Token:     cfg.DataplaneToken,
		NodeID:    cfg.NodeID,
		NodeGroup: cfg.NodeGroup,
		IsCanary:  cfg.IsCanary,
		AllowHTTP: cfg.DataplaneAllowHTTP,
	})
	if err != nil {
		return err
	}

	hub := realtime.NewHub()
	collector := analytics.NewCollectorWithWriter(dataplane.NewLogWriter(client), hub, cfg.NodeID)
	enableSpool(collector, cfg)
	gw := gateway.New(collector, gateway.NewRedisLimiter(cfg.RedisURL, gateway.NewLimiter()), cfg.NodeID)
	gw.SetNodeMetadata(cfg.NodeGroup, cfg.IsCanary)
	gw.SetRunnerSecret(cfg.RunnerSecret)
	tracer := tracing.NewFromEnv("relayops-gateway", cfg.NodeID)
	gw.SetTracer(tracer)
	if err := loadWasm(ctx, cfg, gw); err != nil {
		return err
	}
	gw.SetAIBudgetLedger(dataplane.NewLedger(client))
	gw.SetMCPReporter(dataplane.NewMCPReporter(client))

	streamMgr := stream.NewManager()
	src := dataplane.NewRemoteSource(client)
	if verifier != nil {
		src.RequireSignatures(verifier)
	}
	src.OnStreams(func(services []store.StreamService) {
		if err := streamMgr.Sync(services); err != nil {
			slog.Warn("stream service sync", "err", err)
		}
	})
	watcher := gateway.NewWatcherWithSource(gw, src, hub, cfg.ResyncInterval)
	if err := watcher.Reload(ctx, "startup"); err != nil {
		return err
	}

	bg, cancelBG := context.WithCancel(context.Background())
	defer cancelBG()
	// Live traffic for the console, which runs on the control plane.
	ticks := dataplane.NewTickForwarder(client)
	collector.OnTick(ticks.Forward)
	go ticks.Run(bg)
	collectorDone := make(chan struct{})
	go func() { collector.Run(bg); close(collectorDone) }()
	go watcher.Run(bg)
	go gw.RunHealthChecks(bg)

	proxySrv := newProxyServer(cfg, gw)
	if err := configureGatewayTLS(cfg, gw, proxySrv); err != nil {
		return err
	}
	statusSrv := &http.Server{
		Addr:              cfg.StatusAddr,
		Handler:           dataplane.StatusHandler(gw, src),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 2)
	go listen(proxySrv, "proxy", cfg.TLSCertFile, cfg.TLSKeyFile, errc)
	go listen(statusSrv, "status", "", "", errc)
	slog.Info("RelayOps gateway node started", "role", cfg.Role, "proxy", cfg.ProxyAddr, "status", cfg.StatusAddr,
		"control_plane", cfg.ControlPlaneURL, "node", cfg.NodeID, "group", cfg.NodeGroup, "canary", cfg.IsCanary)

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
	_ = statusSrv.Shutdown(shutCtx)
	streamMgr.StopAll()
	cancelBG()
	<-collectorDone // final log flush (to the control plane, or the spool)
	if e := collector.LogExporter(); e != nil {
		e.Shutdown(shutCtx) // after the collector's final flush
	}
	tracer.Shutdown(shutCtx)
	return nil
}

// loadWasm loads policy plugins from RELAYOPS_WASM_PLUGINS_DIR into gw. A
// directory that cannot be loaded stops startup: APIs listing its plugins
// would otherwise be refused at request time.
func loadWasm(ctx context.Context, cfg config.Config, gw *gateway.Gateway) error {
	if cfg.WasmPluginsDir == "" {
		return nil
	}
	m, names, err := gateway.LoadWasmPlugins(ctx, cfg.WasmPluginsDir)
	if err != nil {
		return fmt.Errorf("RELAYOPS_WASM_PLUGINS_DIR: %w", err)
	}
	gw.SetWasmManager(m)
	slog.Info("WASM policy plugins loaded", "dir", cfg.WasmPluginsDir, "plugins", names)
	return nil
}

// runRelay serves the node API to a region's gateways on RELAYOPS_ADMIN_ADDR
// (TLS with the admin certificate files), backed by the control plane at
// RELAYOPS_CONTROL_PLANE_URL. Gateways point their RELAYOPS_CONTROL_PLANE_URL
// at the relay.
func runRelay(ctx context.Context, cfg config.Config) error {
	tlsCfg, err := dataplane.LoadClientTLS(cfg.DataplaneCAFile, cfg.DataplaneClientCertFile, cfg.DataplaneClientKeyFile)
	if err != nil {
		return err
	}
	client, err := dataplane.NewClient(dataplane.ClientConfig{
		BaseURL: cfg.ControlPlaneURL, Token: cfg.DataplaneToken, NodeID: cfg.NodeID,
		NodeGroup: cfg.NodeGroup, AllowHTTP: cfg.DataplaneAllowHTTP, TLS: tlsCfg,
	})
	if err != nil {
		return err
	}
	relay, err := dataplane.NewRelay(ctx, client)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.AdminAddr, Handler: relay.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go listen(srv, "relay", cfg.AdminTLSCertFile, cfg.AdminTLSKeyFile, errc)
	slog.Info("RelayOps relay started", "addr", cfg.AdminAddr, "control_plane", cfg.ControlPlaneURL, "node", cfg.NodeID)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// leaderElectionOption selects how control-plane replicas pick the one that
// runs the auto-rollback supervisor.
func leaderElectionOption(cfg config.Config) (admin.Option, error) {
	if cfg.LeaderElection != "kubernetes" {
		return func(*admin.Server) {}, nil
	}
	e, err := coordinator.NewKubernetesLeaseElector(cfg.NodeID, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("RELAYOPS_LEADER_ELECTION=kubernetes: %w", err)
	}
	slog.Info("auto-rollback supervisor uses a Kubernetes Lease for leader election", "node", cfg.NodeID)
	return admin.WithLeaderElector(e), nil
}

// dataplaneKeygen prints a new configuration signing key pair
// (`relayops dataplane-keygen`).
func dataplaneKeygen() error {
	seed, pub, err := dataplane.GenerateSigningKey()
	if err != nil {
		return err
	}
	fmt.Printf("# Control plane (keep secret):\nRELAYOPS_DATAPLANE_SIGNING_KEY=%s\n\n# Gateway-only nodes:\nRELAYOPS_DATAPLANE_VERIFY_KEYS=%s\n", seed, pub)
	return nil
}

// nodeAPIOption configures the control plane's node API from cfg.
func nodeAPIOption(cfg config.Config) admin.Option {
	if !cfg.NodeAPIEnabled() {
		return func(*admin.Server) {}
	}
	o := admin.DataplaneOptions{SharedToken: cfg.DataplaneToken, RequireClientCert: cfg.DataplaneRequireClientCert}
	if cfg.DataplaneSigningKey != "" {
		signer, err := dataplane.ParseSigningKey(cfg.DataplaneSigningKey)
		if err != nil {
			slog.Error("RELAYOPS_DATAPLANE_SIGNING_KEY is invalid; configuration is served unsigned", "err", err)
		} else {
			o.Signer = signer
			slog.Info("node API signs configuration", "key_id", signer.KeyID(), "public_key", signer.PublicKey())
		}
	}
	if o.SharedToken == "" {
		slog.Info("node API accepts per-node credentials only (no RELAYOPS_DATAPLANE_TOKEN)")
	}
	return admin.WithDataplane(o)
}

// clientCATLS makes the admin listener request client certificates and verify
// any it receives against caFile. Browsers without one still connect; the node
// API can then require a verified certificate.
func clientCATLS(caFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read RELAYOPS_ADMIN_CLIENT_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven}, nil
}

// silentAcks drops acknowledgements, keeping a node out of fleet status.
type silentAcks struct{ gateway.SnapshotSource }

func (silentAcks) Ack(context.Context, gateway.NodeAck) error { return nil }

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// enableSpool applies request-log sampling and keeps request logs that cannot
// be delivered in a bounded on-disk spool.
func enableSpool(c *analytics.Collector, cfg config.Config) {
	c.SetSampleRate(cfg.LogSampleRate)
	if cfg.LogExport == "otlp" {
		if e := analytics.NewLogExporterFromEnv(cfg.NodeID); e != nil {
			c.SetLogExporter(e)
			slog.Info("request logs exported over OTLP (all requests, before sampling)")
		} else {
			slog.Warn("RELAYOPS_LOG_EXPORT=otlp but no OTEL_EXPORTER_OTLP_LOGS_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT; log export disabled")
		}
	}
	if cfg.LogSampleRate > 0 && cfg.LogSampleRate < 1 {
		slog.Info("request-log sampling enabled; errors are always kept", "success_sample_rate", cfg.LogSampleRate)
	}
	if cfg.LogSpoolMB <= 0 {
		return
	}
	spoolPath := "data/request_log_spool." + unsafeFileChars.ReplaceAllString(cfg.NodeID, "_") + ".jsonl"
	if err := c.EnableSpool(spoolPath, int64(cfg.LogSpoolMB)<<20); err != nil {
		slog.Warn("request log spool disabled; logs that cannot be delivered will be lost", "err", err)
	}
}

func newProxyServer(cfg config.Config, gw *gateway.Gateway) *http.Server {
	var handler http.Handler = gw
	if cfg.H2CEnabled {
		handler = h2c.NewHandler(gw, &http2.Server{})
		slog.Info("cleartext HTTP/2 (h2c) enabled on proxy listener", "addr", cfg.ProxyAddr)
	}
	return &http.Server{
		Addr:              cfg.ProxyAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// configureGatewayTLS applies trusted proxies and, when gateway certificates
// are configured, SNI certificate selection and client-certificate checking
// to the proxy listener.
func configureGatewayTLS(cfg config.Config, gw *gateway.Gateway, srv *http.Server) error {
	if cfg.TrustedProxyCIDRs != "" {
		nets, err := gateway.ParseCIDRs(cfg.TrustedProxyCIDRs)
		if err != nil {
			return fmt.Errorf("RELAYOPS_TRUSTED_PROXY_CIDRS: %w", err)
		}
		gw.SetTrustedProxies(nets)
	}
	if cfg.TLSCertFile == "" && cfg.TLSCertsDir == "" {
		return nil
	}
	var def *tls.Certificate
	if cfg.TLSCertFile != "" {
		c, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("gateway TLS certificate: %w", err)
		}
		def = &c
	}
	m := gateway.NewDynamicCertManager(def, nil)
	var hosts []string
	if cfg.TLSCertsDir != "" {
		files, _ := filepath.Glob(filepath.Join(cfg.TLSCertsDir, "*.crt"))
		for _, crt := range files {
			base := strings.TrimSuffix(filepath.Base(crt), ".crt")
			certPEM, err := os.ReadFile(crt)
			if err != nil {
				return err
			}
			keyPEM, err := os.ReadFile(filepath.Join(cfg.TLSCertsDir, base+".key"))
			if err != nil {
				return fmt.Errorf("certificate %s has no matching key: %w", crt, err)
			}
			host := strings.Replace(base, "_wildcard", "*", 1)
			if err := m.RegisterPEM(host, "", certPEM, keyPEM); err != nil {
				return fmt.Errorf("certificate %s: %w", crt, err)
			}
			hosts = append(hosts, host)
		}
	}
	if cfg.TLSClientCAFile != "" {
		caPEM, err := os.ReadFile(cfg.TLSClientCAFile)
		if err != nil {
			return fmt.Errorf("RELAYOPS_TLS_CLIENT_CA_FILE: %w", err)
		}
		if err := m.SetClientCAs(caPEM); err != nil {
			return fmt.Errorf("RELAYOPS_TLS_CLIENT_CA_FILE: %w", err)
		}
		mode := tls.VerifyClientCertIfGiven
		if cfg.TLSClientAuth == "require" {
			mode = tls.RequireAndVerifyClientCert
		}
		m.SetClientAuth(mode)
	}
	srv.TLSConfig = m.TLSConfig()
	slog.Info("gateway TLS configured", "default_certificate", def != nil, "sni_hosts", hosts,
		"client_ca", cfg.TLSClientCAFile != "", "client_auth", cfg.TLSClientAuth)
	return nil
}

// listen serves srv, with TLS when it has a TLS configuration or both
// certificate files are set, and reports any failure other than a graceful
// shutdown on errc.
func listen(srv *http.Server, name, certFile, keyFile string, errc chan<- error) {
	var err error
	if srv.TLSConfig != nil && srv.TLSConfig.GetCertificate != nil {
		slog.Info("starting "+name+" listener with TLS (SNI)", "addr", srv.Addr)
		err = srv.ListenAndServeTLS("", "")
	} else if certFile != "" && keyFile != "" {
		slog.Info("starting "+name+" listener with TLS", "addr", srv.Addr)
		err = srv.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		errc <- err
	}
}
