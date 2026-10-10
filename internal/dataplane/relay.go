package dataplane

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Relay serves the node API to the gateways in one region (RELAYOPS_ROLE=relay).
//
//   - Configuration: one long-poll per node group to the control plane, shared
//     by every gateway the relay serves, which long-poll the relay instead.
//     Envelopes are passed through byte for byte, so the control plane's
//     signature still holds and gateways with verify keys still check it: the
//     relay cannot alter configuration.
//   - Everything else (acknowledgements, request logs, ticks, AI budget) is
//     proxied with the gateway's own credentials, so the control plane
//     authenticates and attributes every gateway itself.
//   - Gateways are authenticated against the control plane (whoami), cached
//     for RelayAuthTTL. While the control plane is unreachable, a gateway
//     verified within RelayAuthStaleTTL keeps receiving the cached
//     configuration, so a region keeps working through a partition.
type Relay struct {
	upstream *Client
	proxy    *httputil.ReverseProxy

	mu      sync.Mutex
	entries map[string]*relayEntry

	authMu sync.Mutex
	auth   map[[32]byte]relayAuth

	ctx context.Context

	upstreamSyncs  atomic.Int64
	upstreamErrors atomic.Int64
	served         atomic.Int64

	authTTL atomic.Int64 // how long a verified credential is trusted, in ns
}

// SetAuthTTL changes how long a verified gateway credential is trusted
// without asking the control plane again (default RelayAuthTTL). Safe to call
// while the relay serves requests.
func (rl *Relay) SetAuthTTL(d time.Duration) { rl.authTTL.Store(int64(d)) }

var (
	RelayAuthTTL      = 60 * time.Second
	RelayAuthStaleTTL = 24 * time.Hour
)

type relayAuth struct {
	nodeID   string
	verified time.Time
}

type relayEntry struct {
	mu      sync.Mutex
	raw     []byte // envelope as received from the control plane
	gz      []byte
	etag    string
	changed chan struct{}
	ready   chan struct{}
}

// NewRelay builds a relay on upstream, the relay's own client to the control plane.
func NewRelay(ctx context.Context, upstream *Client) (*Relay, error) {
	target, err := url.Parse(upstream.base)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = upstream.http.Transport
	rl := &Relay{upstream: upstream, proxy: proxy, entries: map[string]*relayEntry{},
		auth: map[[32]byte]relayAuth{}, ctx: ctx}
	rl.SetAuthTTL(RelayAuthTTL)
	return rl, nil
}

func (rl *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathConfig, rl.serveConfig)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusOK, map[string]any{"status": "ok", "role": "relay", "groups": rl.groups()})
	})
	mux.HandleFunc("GET /metrics", rl.metrics)
	// Everything else under the node API goes to the control plane as is.
	mux.Handle("/dataplane/v1/", rl.proxy)
	return mux
}

func (rl *Relay) groups() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.entries)
}

// authenticate checks the gateway's credentials with the control plane.
func (rl *Relay) authenticate(r *http.Request) (string, int) {
	tok := r.Header.Get("Authorization")
	node := r.Header.Get(HeaderNodeID)
	if !strings.HasPrefix(tok, "Bearer ") || node == "" {
		return "", http.StatusUnauthorized
	}
	key := sha256.Sum256([]byte(tok + "\x00" + node))
	rl.authMu.Lock()
	cached, ok := rl.auth[key]
	rl.authMu.Unlock()
	if ok && time.Since(cached.verified) < time.Duration(rl.authTTL.Load()) {
		return cached.nodeID, 0
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rl.upstream.base+PathWhoami, nil)
	for _, h := range []string{"Authorization", HeaderNodeID, HeaderNodeGroup, HeaderCanary} {
		req.Header.Set(h, r.Header.Get(h))
	}
	resp, err := rl.upstream.http.Do(req)
	if err != nil || resp.StatusCode >= 500 {
		if resp != nil {
			resp.Body.Close()
		}
		// Control plane unreachable: keep serving recently verified gateways.
		if ok && time.Since(cached.verified) < RelayAuthStaleTTL {
			return cached.nodeID, 0
		}
		return "", http.StatusServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rl.authMu.Lock()
		delete(rl.auth, key)
		rl.authMu.Unlock()
		return "", resp.StatusCode
	}
	var who struct {
		NodeID string `json:"node_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&who)
	rl.authMu.Lock()
	rl.auth[key] = relayAuth{nodeID: who.NodeID, verified: time.Now()}
	rl.authMu.Unlock()
	return who.NodeID, 0
}

func (rl *Relay) serveConfig(w http.ResponseWriter, r *http.Request) {
	if _, status := rl.authenticate(r); status != 0 {
		code := "unauthorized"
		if status == http.StatusServiceUnavailable {
			code = "control_plane_unavailable"
		}
		writeStatus(w, status, map[string]string{"error": code})
		return
	}
	group := r.Header.Get(HeaderNodeGroup)
	if group == "" {
		group = "default"
	}
	canary, _ := strconv.ParseBool(r.Header.Get(HeaderCanary))
	e := rl.entry(group, canary)
	select {
	case <-e.ready:
	case <-time.After(10 * time.Second):
		writeStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "configuration_not_yet_available"})
		return
	case <-r.Context().Done():
		return
	}
	var wait time.Duration
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v > 0 {
		wait = min(time.Duration(v)*time.Second, MaxWait)
	}
	inm := strings.TrimPrefix(r.Header.Get("If-None-Match"), "W/")
	if inm != "" && !strings.HasPrefix(inm, `"`) {
		inm = `"` + inm + `"`
	}
	deadline := time.Now().Add(wait)
	for {
		e.mu.Lock()
		raw, gz, etag, changed := e.raw, e.gz, e.etag, e.changed
		e.mu.Unlock()
		if inm == "" || inm != etag {
			rl.served.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "no-store")
			body := raw
			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				w.Header().Set("Content-Encoding", "gzip")
				body = gz
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-changed:
			timer.Stop()
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		}
	}
}

// entry returns the cache for one node group, starting its upstream
// long-poll the first time the group is asked for.
func (rl *Relay) entry(group string, canary bool) *relayEntry {
	key := group + "|" + strconv.FormatBool(canary)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if e, ok := rl.entries[key]; ok {
		return e
	}
	e := &relayEntry{changed: make(chan struct{}), ready: make(chan struct{})}
	rl.entries[key] = e
	go rl.follow(rl.upstream.forGroup(group, canary), e, key)
	return e
}

// follow keeps e current: an initial fetch, then long-polls on its
// fingerprint, with backoff while the control plane is unreachable. The
// last configuration keeps being served throughout.
func (rl *Relay) follow(c *Client, e *relayEntry, key string) {
	backoff := time.Second
	etag := ""
	for rl.ctx.Err() == nil {
		wait := pollWait
		if etag == "" {
			wait = 0
		}
		f, err := c.fetchConfig(rl.ctx, etag, wait)
		if err != nil {
			if rl.ctx.Err() != nil {
				return
			}
			rl.upstreamErrors.Add(1)
			slog.Warn("relay: control plane fetch failed; serving cached configuration", "group", key, "err", err, "retry_in", backoff)
			select {
			case <-rl.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		rl.upstreamSyncs.Add(1)
		if f == nil {
			continue // unchanged
		}
		if Fingerprint(f.Payload) != f.Fingerprint {
			rl.upstreamErrors.Add(1)
			slog.Error("relay: control plane sent a configuration whose payload does not match its fingerprint; ignored", "group", key)
			continue
		}
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(f.raw)
		_ = zw.Close()
		etag = f.Fingerprint
		e.mu.Lock()
		first := e.raw == nil
		e.raw, e.gz, e.etag = f.raw, gz.Bytes(), `"`+f.Fingerprint+`"`
		close(e.changed)
		e.changed = make(chan struct{})
		e.mu.Unlock()
		if first {
			close(e.ready)
		}
	}
}

func (rl *Relay) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP relayops_relay_groups Node groups whose configuration this relay follows\n# TYPE relayops_relay_groups gauge\nrelayops_relay_groups %d\n\n", rl.groups())
	fmt.Fprintf(w, "# HELP relayops_relay_upstream_syncs_total Successful exchanges with the control plane\n# TYPE relayops_relay_upstream_syncs_total counter\nrelayops_relay_upstream_syncs_total %d\n\n", rl.upstreamSyncs.Load())
	fmt.Fprintf(w, "# HELP relayops_relay_upstream_errors_total Failed exchanges with the control plane\n# TYPE relayops_relay_upstream_errors_total counter\nrelayops_relay_upstream_errors_total %d\n\n", rl.upstreamErrors.Load())
	fmt.Fprintf(w, "# HELP relayops_relay_configs_served_total Configurations served to gateways\n# TYPE relayops_relay_configs_served_total counter\nrelayops_relay_configs_served_total %d\n\n", rl.served.Load())
}
