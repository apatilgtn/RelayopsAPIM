package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/relayops/apim/internal/certs"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/store"
)

// reloadSignals fans SIGHUP out to every certificate watcher, so an operator
// (or cert-manager's post-renewal hook) can reload certificates at once.
type reloadSignals struct {
	mu   sync.Mutex
	subs []chan struct{}
}

func newReloadSignals(ctx context.Context) *reloadSignals {
	r := &reloadSignals{}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				slog.Info("SIGHUP: reloading certificates")
				r.mu.Lock()
				for _, c := range r.subs {
					select {
					case c <- struct{}{}:
					default: // a reload is already pending
					}
				}
				r.mu.Unlock()
			}
		}
	}()
	return r
}

func (r *reloadSignals) subscribe() <-chan struct{} {
	c := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs = append(r.subs, c)
	r.mu.Unlock()
	return c
}

// reloadOnSignal re-reads a client key pair whenever a reload is signalled.
func reloadOnSignal(ctx context.Context, k *certs.KeyPairFile, reload <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload:
			if err := k.Reload(); err != nil {
				slog.Error("client certificate reload failed; keeping the current certificate", "err", err)
			}
		}
	}
}

// listenerTLS returns a reloading certificate manager for a listener, or nil
// when the listener has no certificates (it then serves plain HTTP).
func listenerTLS(ctx context.Context, name string, files gateway.TLSFiles, requireClient bool, acme *certs.ACME,
	inv *certs.Inventory, interval time.Duration, hup *reloadSignals) (*gateway.DynamicCertManager, error) {
	if files.CertFile == "" && files.Dir == "" && acme == nil {
		return nil, nil
	}
	m := gateway.NewDynamicCertManager(nil)
	m.SetName(name)
	m.SetInventory(inv)
	m.RequireClientCertificates(requireClient)
	if acme != nil {
		m.SetACME(acme)
	}
	if err := m.LoadFiles(files); err != nil {
		return nil, fmt.Errorf("%s TLS: %w", name, err)
	}
	go m.Watch(ctx, interval, hup.subscribe())
	return m, nil
}

// configureGatewayTLS applies trusted proxies and, when gateway certificates
// or ACME are configured, SNI certificate selection, client-certificate
// checking and certificate reloading to the proxy listener.
func configureGatewayTLS(ctx context.Context, cfg config.Config, gw *gateway.Gateway, srv *http.Server, acme *certs.ACME, hup *reloadSignals) error {
	if cfg.TrustedProxyCIDRs != "" {
		nets, err := gateway.ParseCIDRs(cfg.TrustedProxyCIDRs)
		if err != nil {
			return fmt.Errorf("RELAYOPS_TRUSTED_PROXY_CIDRS: %w", err)
		}
		gw.SetTrustedProxies(nets)
	}
	files := gateway.TLSFiles{
		CertFile: cfg.TLSCertFile, KeyFile: cfg.TLSKeyFile, Dir: cfg.TLSCertsDir,
		ClientCAFile: cfg.TLSClientCAFile, CRLFile: cfg.TLSClientCRLFile,
	}
	m, err := listenerTLS(ctx, "gateway", files, cfg.TLSClientAuth == "require", acme, gw.CertificateInventory(), cfg.TLSReloadInterval, hup)
	if err != nil || m == nil {
		return err
	}
	srv.TLSConfig = m.TLSConfig()
	slog.Info("gateway TLS configured", "certificates", len(m.Statuses()), "acme", acme != nil,
		"client_ca", cfg.TLSClientCAFile != "", "client_auth", cfg.TLSClientAuth, "crl", cfg.TLSClientCRLFile != "",
		"reload_interval", cfg.TLSReloadInterval)
	return nil
}

// adminTLS returns the admin (or relay) listener's TLS configuration, or nil
// for plain HTTP. With RELAYOPS_ADMIN_CLIENT_CA_FILE it requests client
// certificates and verifies any it receives; browsers without one still
// connect, and the node API can then require a verified certificate.
func adminTLS(ctx context.Context, name string, cfg config.Config, inv *certs.Inventory, hup *reloadSignals) (*tls.Config, error) {
	if cfg.AdminTLSCertFile == "" && cfg.AdminTLSKeyFile == "" {
		return nil, nil
	}
	if cfg.AdminTLSCertFile == "" || cfg.AdminTLSKeyFile == "" {
		return nil, errors.New("RELAYOPS_ADMIN_TLS_CERT_FILE and RELAYOPS_ADMIN_TLS_KEY_FILE must be set together")
	}
	files := gateway.TLSFiles{CertFile: cfg.AdminTLSCertFile, KeyFile: cfg.AdminTLSKeyFile, ClientCAFile: cfg.AdminClientCAFile}
	m, err := listenerTLS(ctx, name, files, false, nil, inv, cfg.TLSReloadInterval, hup)
	if err != nil || m == nil {
		return nil, err
	}
	return m.TLSConfig(), nil
}

// newACME configures ACME issuance, or returns nil when no hostnames are
// listed. Nodes with a database and RELAYOPS_ACME_CACHE_KEY share an
// encrypted cache in Postgres; others cache on disk.
func newACME(cfg config.Config, db *store.Store) (*certs.ACME, error) {
	var orderLock func(context.Context, string) (bool, error)
	if len(cfg.ACMEDomains) == 0 {
		return nil, nil
	}
	var cache interface {
		Get(context.Context, string) ([]byte, error)
		Put(context.Context, string, []byte) error
		Delete(context.Context, string) error
	}
	switch {
	case db != nil && cfg.ACMECacheKey != "":
		key, err := cfg.ACMECacheKeyBytes()
		if err != nil {
			return nil, err
		}
		c, err := certs.NewEncryptedCache(db.ACMECache(), key)
		if err != nil {
			return nil, err
		}
		cache = c
		orderLock = func(ctx context.Context, host string) (bool, error) {
			return db.TryACMEOrderLease(ctx, host, cfg.NodeID, 10*time.Minute)
		}
		slog.Info("ACME cache: shared through the database, encrypted")
	default:
		c, err := certs.DirCache(cfg.ACMECacheDirectory())
		if err != nil {
			return nil, err
		}
		cache = c
		slog.Info("ACME cache: on disk; use durable storage so certificates survive restarts. With several gateway nodes, set RELAYOPS_ACME_CACHE_KEY on nodes with database access so they share one cache", "dir", cfg.ACMECacheDirectory())
	}
	a, err := certs.NewACME(certs.ACMEConfig{
		Domains: cfg.ACMEDomains, Email: cfg.ACMEEmail, AcceptTOS: cfg.ACMEAcceptTOS,
		DirectoryURL: cfg.ACMEDirectoryURL, Cache: cache, OrderLock: orderLock,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("ACME enabled", "domains", cfg.ACMEDomains, "directory", cfg.ACMEDirectoryURL, "http_01", cfg.ACMEHTTPAddr != "")
	return a, nil
}

// startACME warms ACME certificates and serves HTTP-01 validations when
// RELAYOPS_ACME_HTTP_ADDR is set. It returns the HTTP-01 server, or nil.
func startACME(ctx context.Context, cfg config.Config, a *certs.ACME, errc chan<- error) *http.Server {
	if a == nil {
		return nil
	}
	var srv *http.Server
	if cfg.ACMEHTTPAddr != "" {
		srv = &http.Server{Addr: cfg.ACMEHTTPAddr, Handler: a.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
		go listen(srv, "acme-http", errc)
	}
	go func() {
		// Give the listeners a moment to start: TLS-ALPN-01 validation
		// connects back to the gateway listener.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		a.Warm(ctx, func(host string, err error) {
			switch {
			case errors.Is(err, certs.ErrOrderInProgress):
				slog.Info("ACME certificate is being obtained by another node", "host", host)
				return
			case err != nil:
				slog.Error("ACME certificate not obtained; the gateway will retry on the next request for it", "host", host, "err", err)
				return
			}
			slog.Info("ACME certificate ready", "host", host)
		})
	}()
	return srv
}

// listen serves srv, with TLS when it has a TLS configuration, and reports
// any failure other than a graceful shutdown on errc.
func listen(srv *http.Server, name string, errc chan<- error) {
	var err error
	if srv.TLSConfig != nil {
		slog.Info("starting "+name+" listener with TLS", "addr", srv.Addr)
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		errc <- fmt.Errorf("%s listener: %w", name, err)
	}
}
