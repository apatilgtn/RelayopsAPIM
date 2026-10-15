// Package config loads runtime settings from environment variables and .env files.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Environment      string
	DataDir          string
	PersistentData   bool
	DatabaseURL      string        // RELAYOPS_DATABASE_URL
	RedisURL         string        // RELAYOPS_REDIS_URL    - Redis connection for cluster rate limiting & quotas
	ProxyAddr        string        // RELAYOPS_PROXY_ADDR   - data plane listener
	AdminAddr        string        // RELAYOPS_ADMIN_ADDR   - admin API + dashboard
	AdminToken       string        // RELAYOPS_ADMIN_TOKEN  - bearer token for admin API ("" disables auth)
	NodeID           string        // RELAYOPS_NODE_ID      - identifies this gateway node in logs
	NodeGroup        string        // RELAYOPS_NODE_GROUP   - cluster node group ("default", "canary", etc.)
	IsCanary         bool          // RELAYOPS_CANARY       - true if this node serves canary traffic
	LogRetention     time.Duration // RELAYOPS_LOG_RETENTION_HOURS
	ResyncInterval   time.Duration // RELAYOPS_RESYNC_SECONDS - safety-net full reload
	AdoptSQL         bool          // RELAYOPS_ADOPT_SQL_CHANGES - publish direct SQL edits to apis/plans as revisions
	LogSpoolMB       int           // RELAYOPS_LOG_SPOOL_MB - on-disk spool for request logs during DB outages (0 disables)
	RunnerSecret     string        // RELAYOPS_RUNNER_SECRET - internal secret for authenticating test studio runner
	TLSCertFile      string        // RELAYOPS_TLS_CERT_FILE - data plane server certificate
	TLSKeyFile       string        // RELAYOPS_TLS_KEY_FILE - data plane server private key
	H2CEnabled       bool          // RELAYOPS_H2C_ENABLED - enable cleartext HTTP/2 (h2c) for internal proxy paths
	AdminTLSCertFile string        // RELAYOPS_ADMIN_TLS_CERT_FILE - control plane server certificate
	AdminTLSKeyFile  string        // RELAYOPS_ADMIN_TLS_KEY_FILE - control plane server private key

	Role               string // RELAYOPS_ROLE - all (default), control-plane or gateway
	ControlPlaneURL    string // RELAYOPS_CONTROL_PLANE_URL - control plane base URL gateway-only nodes pull config from
	DataplaneToken     string // RELAYOPS_DATAPLANE_TOKEN - shared secret gateway-only nodes present to the control plane node API
	StatusAddr         string // RELAYOPS_STATUS_ADDR - gateway-only nodes: /metrics, /healthz and /readyz listener
	DataplaneAllowHTTP bool   // RELAYOPS_DATAPLANE_ALLOW_HTTP - allow a plain-HTTP control plane URL (local development only)
	PublicGatewayURL   string // RELAYOPS_PUBLIC_GATEWAY_URL - gateway address API consumers use (published API references, portal examples), e.g. https://api.example.com
	GatewayURL         string // RELAYOPS_GATEWAY_URL - gateway base URL the control plane calls (workbench, MCP, Test Studio); defaults to the local proxy listener

	// Node API trust (see docs/DATAPLANE_SEPARATION_DESIGN_2026-10-09.md).
	DataplaneAPI               bool   // RELAYOPS_DATAPLANE_API - serve the node API (per-node credentials) without a shared token
	DataplaneSigningKey        string // RELAYOPS_DATAPLANE_SIGNING_KEY - control plane: base64 Ed25519 seed that signs configuration
	DataplaneVerifyKeys        string // RELAYOPS_DATAPLANE_VERIFY_KEYS - gateway: comma-separated base64 public keys; requires signed configuration
	DataplaneCAFile            string // RELAYOPS_DATAPLANE_CA_FILE - gateway: CA bundle for the control plane's certificate
	DataplaneClientCertFile    string // RELAYOPS_DATAPLANE_CLIENT_CERT_FILE - gateway: client certificate for mTLS
	DataplaneClientKeyFile     string // RELAYOPS_DATAPLANE_CLIENT_KEY_FILE - gateway: client certificate key for mTLS
	AdminClientCAFile          string // RELAYOPS_ADMIN_CLIENT_CA_FILE - control plane: CA that client certificates must chain to
	DataplaneRequireClientCert bool   // RELAYOPS_DATAPLANE_REQUIRE_CLIENT_CERT - control plane: node API requires a verified client certificate

	LogSampleRate float64 // RELAYOPS_LOG_SAMPLE_RATE - fraction (0-1] of successful requests persisted to request_logs; errors are always kept

	TLSCertsDir       string // RELAYOPS_TLS_CERTS_DIR - per-hostname gateway certificates: <host>.crt/<host>.key (_wildcard.example.com for *.example.com)
	TLSClientCAFile   string // RELAYOPS_TLS_CLIENT_CA_FILE - CA that gateway client certificates (mtls APIs) must chain to
	TLSClientAuth     string // RELAYOPS_TLS_CLIENT_AUTH - optional (default with a client CA) or require
	TrustedProxyCIDRs string // RELAYOPS_TRUSTED_PROXY_CIDRS - reverse proxies whose forwarded client-certificate headers are accepted
	TLSClientCRLFile  string // RELAYOPS_TLS_CLIENT_CRL_FILE - revocation lists (PEM or DER) client certificates are checked against

	// TLSReloadInterval is how often certificate, key, CA and CRL files are
	// re-read (RELAYOPS_TLS_RELOAD_SECONDS, default 30; 0 disables polling,
	// SIGHUP still reloads).
	TLSReloadInterval time.Duration

	// ACME certificate issuance for the gateway listener (see docs/CERTIFICATE_MANAGEMENT.md).
	ACMEDomains      []string // RELAYOPS_ACME_DOMAINS - comma-separated hostnames; the only names certificates are requested for
	ACMEEmail        string   // RELAYOPS_ACME_EMAIL - account contact
	ACMEAcceptTOS    bool     // RELAYOPS_ACME_ACCEPT_TOS - the operator accepts the CA's terms of service
	ACMEDirectoryURL string   // RELAYOPS_ACME_DIRECTORY_URL - default Let's Encrypt production
	ACMECacheDir     string   // RELAYOPS_ACME_CACHE_DIR - default <data dir>/acme
	ACMECacheKey     string   // RELAYOPS_ACME_CACHE_KEY - base64 32-byte key: share the cache through Postgres, encrypted
	ACMEHTTPAddr     string   // RELAYOPS_ACME_HTTP_ADDR - optional HTTP-01 listener such as :80

	AlertWebhookURL string // RELAYOPS_ALERT_WEBHOOK_URL - POST alerts (e.g. MCP definitions held for review) to this webhook (Slack/Teams compatible)

	// Orbit AI, the console's operations assistant. Any OpenAI-compatible
	// chat completions endpoint; a RelayOps AI route also works, which puts
	// Orbit's own model usage under the gateway's budgets and audit.
	OrbitBaseURL       string // RELAYOPS_ORBIT_BASE_URL - e.g. https://integrate.api.nvidia.com/v1
	OrbitModel         string // RELAYOPS_ORBIT_MODEL - e.g. meta/llama-3.3-70b-instruct
	OrbitAPIKey        string // RELAYOPS_ORBIT_API_KEY - sent as a bearer token
	OrbitFallbackModel string // RELAYOPS_ORBIT_FALLBACK_MODEL - answers when the model is retired, overloaded or unreachable
	WasmPluginsDir     string // RELAYOPS_WASM_PLUGINS_DIR - directory of *.wasm policy plugins APIs can list in traffic_policy.wasm_plugins

	LogExport string // RELAYOPS_LOG_EXPORT - "otlp" sends every request log to the OTEL collector (OTEL_EXPORTER_OTLP_[LOGS_]ENDPOINT)

	LeaderElection string // RELAYOPS_LEADER_ELECTION - postgres (default: every replica evaluates under an advisory lock) or kubernetes (one Lease holder evaluates)

	AutoRollbackInterval time.Duration // RELAYOPS_AUTO_ROLLBACK_INTERVAL_SECONDS - supervisor evaluation interval (default 3s; 15s suits metered hosted databases)
}

// Node roles. RoleAll runs gateway and control plane in one process, as before.
const (
	RoleAll          = "all"
	RoleControlPlane = "control-plane"
	RoleGateway      = "gateway"
	// RoleRelay serves the node API to one region's gateways from a cache of
	// the control plane's (signed) configuration, forwarding everything else.
	RoleRelay = "relay"
)

// MinDataplaneTokenLength is the shortest node API token accepted.
const MinDataplaneTokenLength = 32

// Validate reports configuration that cannot work for the selected role.
func (c Config) Validate() error {
	if err := c.ValidateProduction(); err != nil {
		return err
	}
	switch c.Role {
	case RoleAll, RoleControlPlane:
	case RoleGateway, RoleRelay:
		if c.ControlPlaneURL == "" {
			return fmt.Errorf("RELAYOPS_ROLE=%s requires RELAYOPS_CONTROL_PLANE_URL", c.Role)
		}
		if c.DataplaneToken == "" {
			return fmt.Errorf("RELAYOPS_ROLE=%s requires RELAYOPS_DATAPLANE_TOKEN", c.Role)
		}
	default:
		return fmt.Errorf("RELAYOPS_ROLE must be %q, %q, %q or %q, got %q", RoleAll, RoleControlPlane, RoleGateway, RoleRelay, c.Role)
	}
	if c.DataplaneToken != "" && len(c.DataplaneToken) < MinDataplaneTokenLength {
		return fmt.Errorf("RELAYOPS_DATAPLANE_TOKEN must be at least %d characters", MinDataplaneTokenLength)
	}
	if c.LeaderElection != "" && c.LeaderElection != "postgres" && c.LeaderElection != "kubernetes" {
		return fmt.Errorf("RELAYOPS_LEADER_ELECTION must be postgres or kubernetes, got %q", c.LeaderElection)
	}
	if c.TLSClientAuth != "" && c.TLSClientAuth != "optional" && c.TLSClientAuth != "require" {
		return fmt.Errorf("RELAYOPS_TLS_CLIENT_AUTH must be optional or require, got %q", c.TLSClientAuth)
	}
	if c.TLSClientCAFile != "" && c.TLSCertFile == "" && c.TLSCertsDir == "" && len(c.ACMEDomains) == 0 {
		return errors.New("RELAYOPS_TLS_CLIENT_CA_FILE needs a gateway TLS certificate (RELAYOPS_TLS_CERT_FILE, RELAYOPS_TLS_CERTS_DIR or RELAYOPS_ACME_DOMAINS)")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("RELAYOPS_TLS_CERT_FILE and RELAYOPS_TLS_KEY_FILE must be set together")
	}
	if c.TLSClientCRLFile != "" && c.TLSClientCAFile == "" && c.TLSCertsDir == "" {
		return errors.New("RELAYOPS_TLS_CLIENT_CRL_FILE needs client CAs (RELAYOPS_TLS_CLIENT_CA_FILE or <host>.client-ca.pem in RELAYOPS_TLS_CERTS_DIR)")
	}
	if c.TLSReloadInterval < 0 {
		return errors.New("RELAYOPS_TLS_RELOAD_SECONDS must be 0 (no polling) or more")
	}
	if len(c.ACMEDomains) > 0 {
		if c.Role == RoleControlPlane || c.Role == RoleRelay {
			return fmt.Errorf("RELAYOPS_ACME_DOMAINS issues gateway certificates; RELAYOPS_ROLE=%s serves no gateway listener", c.Role)
		}
		if !c.ACMEAcceptTOS {
			return errors.New("RELAYOPS_ACME_DOMAINS needs RELAYOPS_ACME_ACCEPT_TOS=true: accepting the certificate authority's terms of service is the operator's decision")
		}
	}
	if c.ACMECacheKey != "" {
		if _, err := c.ACMECacheKeyBytes(); err != nil {
			return err
		}
	}
	if c.LogSampleRate < 0 || c.LogSampleRate > 1 { // 0: unset, keep everything
		return fmt.Errorf("RELAYOPS_LOG_SAMPLE_RATE must be greater than 0 and at most 1, got %v", c.LogSampleRate)
	}
	if c.DataplaneRequireClientCert && (c.AdminClientCAFile == "" || c.AdminTLSCertFile == "" || c.AdminTLSKeyFile == "") {
		return errors.New("RELAYOPS_DATAPLANE_REQUIRE_CLIENT_CERT needs RELAYOPS_ADMIN_CLIENT_CA_FILE and the admin TLS certificate and key")
	}
	return nil
}

// NodeAPIEnabled reports whether this control plane serves the node API.
func (c Config) NodeAPIEnabled() bool {
	return c.Role != RoleGateway && (c.Role == RoleControlPlane || c.DataplaneToken != "" || c.DataplaneAPI)
}

func init() {
	loadDotEnv(".env")
}

func Load() Config {
	host, _ := os.Hostname()
	redisURL := env("RELAYOPS_REDIS_URL", "redis://localhost:6379/0")
	if v := strings.ToLower(redisURL); v == "disabled" || v == "none" || v == "off" {
		redisURL = "" // in-memory rate limiting and quotas only
	}
	return Config{
		Environment:      strings.ToLower(env("RELAYOPS_ENV", "development")),
		DataDir:          DataDirectory(),
		PersistentData:   envBool("RELAYOPS_PERSISTENT_DATA", false),
		DatabaseURL:      env("RELAYOPS_DATABASE_URL", "postgres://postgres@localhost:5432/relayops?sslmode=disable"),
		RedisURL:         redisURL,
		ProxyAddr:        env("RELAYOPS_PROXY_ADDR", ":8080"),
		AdminAddr:        env("RELAYOPS_ADMIN_ADDR", ":9090"),
		AdminToken:       env("RELAYOPS_ADMIN_TOKEN", "relayops-admin"),
		NodeID:           env("RELAYOPS_NODE_ID", host),
		NodeGroup:        env("RELAYOPS_NODE_GROUP", "default"),
		IsCanary:         envBool("RELAYOPS_CANARY", false),
		LogRetention:     time.Duration(envInt("RELAYOPS_LOG_RETENTION_HOURS", 24)) * time.Hour,
		ResyncInterval:   time.Duration(envInt("RELAYOPS_RESYNC_SECONDS", 300)) * time.Second,
		AdoptSQL:         envBool("RELAYOPS_ADOPT_SQL_CHANGES", true),
		LogSpoolMB:       envInt("RELAYOPS_LOG_SPOOL_MB", 256),
		RunnerSecret:     loadRunnerSecret(host),
		TLSCertFile:      env("RELAYOPS_TLS_CERT_FILE", ""),
		TLSKeyFile:       env("RELAYOPS_TLS_KEY_FILE", ""),
		H2CEnabled:       envBool("RELAYOPS_H2C_ENABLED", false),
		AdminTLSCertFile: env("RELAYOPS_ADMIN_TLS_CERT_FILE", ""),
		AdminTLSKeyFile:  env("RELAYOPS_ADMIN_TLS_KEY_FILE", ""),

		Role:               strings.ToLower(strings.TrimSpace(env("RELAYOPS_ROLE", RoleAll))),
		ControlPlaneURL:    strings.TrimRight(env("RELAYOPS_CONTROL_PLANE_URL", ""), "/"),
		DataplaneToken:     strings.TrimSpace(env("RELAYOPS_DATAPLANE_TOKEN", "")),
		StatusAddr:         env("RELAYOPS_STATUS_ADDR", ":9091"),
		DataplaneAllowHTTP: envBool("RELAYOPS_DATAPLANE_ALLOW_HTTP", false),
		GatewayURL:         strings.TrimRight(env("RELAYOPS_GATEWAY_URL", ""), "/"),
		PublicGatewayURL:   strings.TrimRight(env("RELAYOPS_PUBLIC_GATEWAY_URL", ""), "/"),

		DataplaneAPI:               envBool("RELAYOPS_DATAPLANE_API", false),
		DataplaneSigningKey:        strings.TrimSpace(env("RELAYOPS_DATAPLANE_SIGNING_KEY", "")),
		DataplaneVerifyKeys:        strings.TrimSpace(env("RELAYOPS_DATAPLANE_VERIFY_KEYS", "")),
		DataplaneCAFile:            env("RELAYOPS_DATAPLANE_CA_FILE", ""),
		DataplaneClientCertFile:    env("RELAYOPS_DATAPLANE_CLIENT_CERT_FILE", ""),
		DataplaneClientKeyFile:     env("RELAYOPS_DATAPLANE_CLIENT_KEY_FILE", ""),
		AdminClientCAFile:          env("RELAYOPS_ADMIN_CLIENT_CA_FILE", ""),
		DataplaneRequireClientCert: envBool("RELAYOPS_DATAPLANE_REQUIRE_CLIENT_CERT", false),

		LogSampleRate: envFloat("RELAYOPS_LOG_SAMPLE_RATE", 1),

		AutoRollbackInterval: time.Duration(envInt("RELAYOPS_AUTO_ROLLBACK_INTERVAL_SECONDS", 3)) * time.Second,
		WasmPluginsDir:       env("RELAYOPS_WASM_PLUGINS_DIR", ""),
		AlertWebhookURL:      strings.TrimSpace(env("RELAYOPS_ALERT_WEBHOOK_URL", "")),
		OrbitBaseURL:         strings.TrimSpace(env("RELAYOPS_ORBIT_BASE_URL", "")),
		OrbitModel:           strings.TrimSpace(env("RELAYOPS_ORBIT_MODEL", "")),
		OrbitAPIKey:          strings.TrimSpace(env("RELAYOPS_ORBIT_API_KEY", "")),
		OrbitFallbackModel:   strings.TrimSpace(env("RELAYOPS_ORBIT_FALLBACK_MODEL", "")),
		LeaderElection:       strings.ToLower(env("RELAYOPS_LEADER_ELECTION", "postgres")),
		LogExport:            strings.ToLower(env("RELAYOPS_LOG_EXPORT", "")),
		TLSCertsDir:          env("RELAYOPS_TLS_CERTS_DIR", ""),
		TLSClientCAFile:      env("RELAYOPS_TLS_CLIENT_CA_FILE", ""),
		TLSClientAuth:        strings.ToLower(env("RELAYOPS_TLS_CLIENT_AUTH", "optional")),
		TrustedProxyCIDRs:    env("RELAYOPS_TRUSTED_PROXY_CIDRS", ""),
		TLSClientCRLFile:     env("RELAYOPS_TLS_CLIENT_CRL_FILE", ""),
		TLSReloadInterval:    time.Duration(envInt("RELAYOPS_TLS_RELOAD_SECONDS", 30)) * time.Second,
		ACMEDomains:          splitList(env("RELAYOPS_ACME_DOMAINS", "")),
		ACMEEmail:            strings.TrimSpace(env("RELAYOPS_ACME_EMAIL", "")),
		ACMEAcceptTOS:        envBool("RELAYOPS_ACME_ACCEPT_TOS", false),
		ACMEDirectoryURL:     strings.TrimSpace(env("RELAYOPS_ACME_DIRECTORY_URL", "")),
		ACMECacheDir:         strings.TrimSpace(env("RELAYOPS_ACME_CACHE_DIR", "")),
		ACMECacheKey:         strings.TrimSpace(env("RELAYOPS_ACME_CACHE_KEY", "")),
		ACMEHTTPAddr:         strings.TrimSpace(env("RELAYOPS_ACME_HTTP_ADDR", "")),
	}
}

func loadRunnerSecret(host string) string {
	if s := os.Getenv("RELAYOPS_RUNNER_SECRET"); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	// Generate a cryptographically secure deployment-specific secret
	b := make([]byte, 32)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	panic("cannot generate secure Test Studio runner secret")
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		return v == "1" || v == "true" || v == "yes" || v == "on"
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			return f
		}
		return -1 // invalid: rejected by Validate
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, found := strings.Cut(line, "="); found {
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if _, exists := os.LookupEnv(k); !exists {
				_ = os.Setenv(k, v)
			}
		}
	}
}

var secretPattern = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_-]+)\}`)

// ResolveSecrets replaces ${secret:NAME} with the value of RELAYOPS_SECRET_NAME or NAME from environment.
func ResolveSecrets(text string) string {
	return secretPattern.ReplaceAllStringFunc(text, func(m string) string {
		sub := secretPattern.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		name := sub[1]
		if v, ok := os.LookupEnv("RELAYOPS_SECRET_" + name); ok && v != "" {
			return v
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return m
	})
}

// ErrSecretNotFound means no value is configured for a secret.
var ErrSecretNotFound = errors.New("secret is not configured")

// MaxSecretFileBytes bounds a secret read from a file.
const MaxSecretFileBytes = 1 << 20

// ReadFileLimited reads a file of at most max bytes.
func ReadFileLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return b, nil
}

// SecretValue returns the value of secret name, read from the file named by
// RELAYOPS_SECRET_<NAME>_FILE (re-read on every call, so a rotated file is
// picked up), or from RELAYOPS_SECRET_<NAME>, or from <NAME>. ResolveSecrets,
// which runs per request for header values, reads no files. A value from an
// environment variable may spell newlines as "\n", as some secret stores
// require for multi-line values such as PEM. Errors never contain the value.
func SecretValue(name string) (string, error) {
	if path := os.Getenv("RELAYOPS_SECRET_" + name + "_FILE"); path != "" {
		b, err := ReadFileLimited(path, MaxSecretFileBytes)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", name, err)
		}
		return string(b), nil
	}
	for _, key := range []string{"RELAYOPS_SECRET_" + name, name} {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			if !strings.Contains(v, "\n") && strings.Contains(v, `\n`) {
				v = strings.ReplaceAll(v, `\n`, "\n")
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("secret %s: %w (set RELAYOPS_SECRET_%s or RELAYOPS_SECRET_%s_FILE)", name, ErrSecretNotFound, name, name)
}

// splitList splits a comma-separated list, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ACMECacheKeyBytes decodes RELAYOPS_ACME_CACHE_KEY.
func (c Config) ACMECacheKeyBytes() ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(c.ACMECacheKey)
	if err != nil || len(k) != 32 {
		return nil, errors.New("RELAYOPS_ACME_CACHE_KEY must be 32 random bytes, base64-encoded (for example: openssl rand -base64 32)")
	}
	return k, nil
}

// ACMECacheDirectory is where ACME state is cached on disk.
func (c Config) ACMECacheDirectory() string {
	if c.ACMECacheDir != "" {
		return c.ACMECacheDir
	}
	return filepath.Join(DataDirectory(), "acme")
}
