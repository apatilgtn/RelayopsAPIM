package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/store"
)

// ---------------------------------------------------------------------------
// Node API (/dataplane/v1/*): served to gateway-only nodes, which hold no
// database credentials. Authenticated with the shared node token, separately
// from admin users and RBAC.
// ---------------------------------------------------------------------------

const (
	// dpCacheMaxAge bounds how long a computed snapshot is reused without a
	// change notification. Key rotation grace periods expire without a NOTIFY,
	// so snapshots are recomputed at least this often while gateways poll.
	dpCacheMaxAge = 15 * time.Second
	// dpDebounce lets a burst of notifications (a bulk import) settle before
	// waiting gateways recompute.
	dpDebounce = 100 * time.Millisecond

	dpMaxLogBody      = 32 << 20 // compressed request body
	dpMaxLogDecoded   = 128 << 20
	dpMaxSmallRequest = 64 << 10
)

// DataplaneOptions configures the node API for gateway-only nodes.
type DataplaneOptions struct {
	// SharedToken is accepted from any node. Empty: per-node credentials only.
	SharedToken string
	// Signer signs configuration responses; nil leaves them unsigned.
	Signer *dataplane.Signer
	// RequireClientCert refuses node API calls without a verified TLS client
	// certificate (the admin listener must be configured with a client CA).
	RequireClientCert bool
}

// WithDataplane enables the node API.
func WithDataplane(o DataplaneOptions) Option {
	return func(s *Server) { s.dataplane = newDataplaneState(o) }
}

// WithDataplaneToken enables the node API with a shared node token (and
// per-node credentials). An empty token leaves the node API off.
func WithDataplaneToken(token string) Option {
	return func(s *Server) {
		if token != "" {
			s.dataplane = newDataplaneState(DataplaneOptions{SharedToken: token})
		}
	}
}

type dataplaneState struct {
	token             []byte // shared token; empty when only per-node credentials are accepted
	signer            *dataplane.Signer
	requireClientCert bool

	mu      sync.Mutex
	gen     uint64
	changed chan struct{} // closed and replaced on every config change

	computeMu sync.Mutex // one snapshot computation at a time
	cacheMu   sync.Mutex
	cache     map[string]*dpEntry
}

type dpEntry struct {
	gen  uint64
	at   time.Time
	etag string
	raw  []byte // JSON ConfigResponse
	gz   []byte // gzip of raw
}

func newDataplaneState(o DataplaneOptions) *dataplaneState {
	return &dataplaneState{token: []byte(o.SharedToken), signer: o.Signer, requireClientCert: o.RequireClientCert,
		changed: make(chan struct{}), cache: map[string]*dpEntry{}}
}

// dpIdentity is the authenticated caller of the node API. NodeID is empty for
// the shared token, which may act for any node.
type dpIdentity struct {
	NodeID       string
	CredentialID string
}

type dpIdentityKey struct{}

func dpCaller(r *http.Request) dpIdentity {
	id, _ := r.Context().Value(dpIdentityKey{}).(dpIdentity)
	return id
}

// mayActFor reports whether the caller may report as nodeID.
func (id dpIdentity) mayActFor(nodeID string) bool { return id.NodeID == "" || id.NodeID == nodeID }

func (d *dataplaneState) current() (uint64, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.gen, d.changed
}

func (d *dataplaneState) bump() {
	d.mu.Lock()
	d.gen++
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
}

// StartDataplaneNotifier follows config changes so waiting gateways are
// answered as soon as configuration changes. It blocks until ctx is done.
func (s *Server) StartDataplaneNotifier(ctx context.Context) {
	if s.dataplane == nil || !s.dbAvailable() {
		return
	}
	src := gateway.NewDBSource(s.store)
	backoff := time.Second
	for ctx.Err() == nil {
		err := src.Watch(ctx, func(string) { s.dataplane.bump() })
		if ctx.Err() != nil {
			return
		}
		slog.Warn("node API change listener disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Server) dataplaneHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+dataplane.PathConfig, s.dpConfig)
	mux.HandleFunc("POST "+dataplane.PathAck, s.dpAck)
	mux.HandleFunc("POST "+dataplane.PathLogs, s.dpLogs)
	mux.HandleFunc("POST "+dataplane.PathBudgetLookup, s.dpBudgetLookup)
	mux.HandleFunc("POST "+dataplane.PathBudgetReserve, s.dpBudgetReserve)
	mux.HandleFunc("POST "+dataplane.PathBudgetSettle, s.dpBudgetSettle)
	mux.HandleFunc("POST "+dataplane.PathBudgetRelease, s.dpBudgetRelease)
	mux.HandleFunc("POST "+dataplane.PathTicks, s.dpTicks)
	mux.HandleFunc("POST "+dataplane.PathMCPObservations, s.dpMCPObservations)
	mux.HandleFunc("GET "+dataplane.PathWhoami, s.dpWhoami)
	return s.requireDataplaneToken(s.requireDB(mux))
}

// requireDataplaneToken authenticates the node: the shared token (any node),
// or a per-node credential, which only acts as the node it was issued to.
// Optionally a verified TLS client certificate is required as well.
func (s *Server) requireDataplaneToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dataplane.requireClientCert && (r.TLS == nil || len(r.TLS.VerifiedChains) == 0) {
			writeErr(w, http.StatusUnauthorized, "client_certificate_required", "a verified TLS client certificate is required")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid node token is required")
			return
		}
		var id dpIdentity
		switch {
		case len(s.dataplane.token) > 0 && subtle.ConstantTimeCompare([]byte(tok), s.dataplane.token) == 1:
			// shared token: may act for any node
		case strings.HasPrefix(tok, store.DataplaneTokenPrefix) && s.dbAvailable():
			cred, err := s.store.AuthenticateDataplaneToken(r.Context(), tok)
			if err != nil {
				if store.IsUnavailable(err) {
					writeDBUnavailable(w)
					return
				}
				writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid node token is required")
				return
			}
			if claimed := r.Header.Get(dataplane.HeaderNodeID); claimed != cred.NodeID {
				slog.Warn("node API: credential used for another node", "credential_node", cred.NodeID, "claimed_node", claimed, "remote_ip", clientIP(r))
				writeErr(w, http.StatusForbidden, "node_mismatch", "this credential was issued to a different node")
				return
			}
			_ = s.store.TouchDataplaneCredential(r.Context(), cred.ID, clientIP(r))
			id = dpIdentity{NodeID: cred.NodeID, CredentialID: cred.ID}
		default:
			writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid node token is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), dpIdentityKey{}, id)))
	})
}

// dpConfig returns the configuration for the calling node. With If-None-Match
// matching the current fingerprint and ?wait=N it long-polls up to N seconds.
func (s *Server) dpConfig(w http.ResponseWriter, r *http.Request) {
	group := r.Header.Get(dataplane.HeaderNodeGroup)
	if group == "" {
		group = "default"
	}
	canary, _ := strconv.ParseBool(r.Header.Get(dataplane.HeaderCanary))
	var wait time.Duration
	if v, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && v > 0 {
		wait = min(time.Duration(v)*time.Second, dataplane.MaxWait)
	}
	// Accept the fingerprint with or without the quotes an HTTP ETag carries.
	inm := strings.TrimPrefix(r.Header.Get("If-None-Match"), "W/")
	if inm != "" && !strings.HasPrefix(inm, `"`) {
		inm = `"` + inm + `"`
	}
	deadline := time.Now().Add(wait)
	for {
		gen, changed := s.dataplane.current()
		entry, err := s.dpSnapshot(r.Context(), group, canary, gen)
		if err != nil {
			s.fail(w, err)
			return
		}
		if inm == "" || inm != entry.etag {
			writeDPEntry(w, r, entry)
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			w.Header().Set("ETag", entry.etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-changed:
			timer.Stop()
			time.Sleep(dpDebounce)
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		}
	}
}

func writeDPEntry(w http.ResponseWriter, r *http.Request, e *dpEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", e.etag)
	w.Header().Set("Cache-Control", "no-store")
	body := e.raw
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		body = e.gz
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// dpSnapshot returns the node's configuration, recomputed when the change
// generation moved or the cached copy is older than dpCacheMaxAge.
func (s *Server) dpSnapshot(ctx context.Context, group string, canary bool, gen uint64) (*dpEntry, error) {
	key := group + "|" + strconv.FormatBool(canary)
	fresh := func() *dpEntry {
		s.dataplane.cacheMu.Lock()
		defer s.dataplane.cacheMu.Unlock()
		if e := s.dataplane.cache[key]; e != nil && e.gen == gen && time.Since(e.at) < dpCacheMaxAge {
			return e
		}
		return nil
	}
	if e := fresh(); e != nil {
		return e, nil
	}
	s.dataplane.computeMu.Lock()
	defer s.dataplane.computeMu.Unlock()
	if e := fresh(); e != nil {
		return e, nil // another request computed it while we waited
	}
	data, err := s.store.LoadSnapshotDataForNode(ctx, group, canary)
	if err != nil {
		return nil, err
	}
	streams, err := s.store.ListStreamServices(ctx, "")
	if err != nil {
		return nil, err
	}
	resp, err := buildConfigResponse(data, streams, s.dataplane.signer)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	e := &dpEntry{gen: gen, at: time.Now(), etag: `"` + resp.Fingerprint + `"`, raw: raw, gz: gz.Bytes()}
	s.dataplane.cacheMu.Lock()
	s.dataplane.cache[key] = e
	s.dataplane.cacheMu.Unlock()
	return e, nil
}

// buildConfigResponse puts the snapshot in a canonical order (the access-state
// queries have no ORDER BY) and fingerprints the payload bytes, so an unchanged
// configuration always has the same fingerprint. With a signer it signs them.
func buildConfigResponse(data store.SnapshotData, streams []store.StreamService, signer *dataplane.Signer) (*dataplane.ConfigResponse, error) {
	sortKeys(data.Keys)
	sortSubs(data.Subs)
	if data.Canary != nil {
		sortSubs(data.Canary.Subs)
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].ID < streams[j].ID })
	if streams == nil {
		streams = []store.StreamService{}
	}
	payload, err := json.Marshal(dataplane.ConfigPayload{Data: data, Streams: streams})
	if err != nil {
		return nil, err
	}
	resp := &dataplane.ConfigResponse{
		Fingerprint: dataplane.Fingerprint(payload),
		IssuedAt:    time.Now().UTC(),
		Payload:     payload,
	}
	if signer != nil {
		signer.Sign(resp)
	}
	return resp, nil
}

func sortKeys(keys []store.KeyRecord) {
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyHash < keys[j].KeyHash })
}

func sortSubs(subs []store.SubRecord) {
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].ConsumerID != subs[j].ConsumerID {
			return subs[i].ConsumerID < subs[j].ConsumerID
		}
		return subs[i].APIID < subs[j].APIID
	})
}

func (s *Server) dpAck(w http.ResponseWriter, r *http.Request) {
	var ack gateway.NodeAck
	if !decodeDP(w, r, dpMaxSmallRequest, &ack) {
		return
	}
	if ack.NodeID == "" || ack.Revision <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "node_id and a positive revision are required")
		return
	}
	if !dpCaller(r).mayActFor(ack.NodeID) {
		writeErr(w, http.StatusForbidden, "node_mismatch", "a node credential can only acknowledge for its own node")
		return
	}
	if ack.NodeGroup == "" {
		ack.NodeGroup = "default"
	}
	err := s.store.AcknowledgeRevisionWithCanary(r.Context(), ack.NodeID, ack.Revision, ack.CanaryRevision,
		ack.Routes, ack.Keys, ack.TookMS, ack.NodeGroup, ack.IsCanary)
	s.noContent(w, err)
}

func (s *Server) dpLogs(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, dpMaxLogBody)
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "invalid gzip body")
			return
		}
		defer zr.Close()
		body = io.LimitReader(zr, dpMaxLogDecoded)
	}
	var logs []store.RequestLog
	if err := json.NewDecoder(body).Decode(&logs); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid request log batch")
		return
	}
	caller := dpCaller(r)
	for _, l := range logs {
		// log_id makes replays after a lost acknowledgement idempotent.
		if l.LogID == "" || l.NodeID == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "every log needs log_id and node_id")
			return
		}
		if !caller.mayActFor(l.NodeID) {
			writeErr(w, http.StatusForbidden, "node_mismatch", "a node credential can only submit its own node's logs")
			return
		}
	}
	s.noContent(w, s.store.InsertLogs(r.Context(), logs))
}

// dpWhoami confirms a node's credentials (relays use it to authenticate the
// gateways they serve) and returns the node they identify.
func (s *Server) dpWhoami(w http.ResponseWriter, r *http.Request) {
	id := dpCaller(r)
	node := id.NodeID
	if node == "" {
		node = r.Header.Get(dataplane.HeaderNodeID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"node_id": node, "per_node_credential": id.NodeID != ""})
}

// dpTicks republishes a gateway-only node's live-traffic tick to this control
// plane's console subscribers, filtered per tenant as local ticks are.
func (s *Server) dpTicks(w http.ResponseWriter, r *http.Request) {
	var t analytics.Tick
	if !decodeDP(w, r, 1<<20, &t) {
		return
	}
	if t.Node == "" || !dpCaller(r).mayActFor(t.Node) {
		writeErr(w, http.StatusForbidden, "node_mismatch", "a node credential can only publish its own node's traffic")
		return
	}
	if s.hub != nil {
		analytics.PublishTick(s.hub, t)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) dpBudgetLookup(w http.ResponseWriter, r *http.Request) {
	var in dataplane.BudgetLookupRequest
	if !decodeDP(w, r, dpMaxSmallRequest, &in) {
		return
	}
	acc, err := s.store.GetAIBudgetAccountByConsumer(r.Context(), in.TenantID, in.ConsumerID)
	if err == nil && acc == nil {
		err = store.ErrNotFound
	}
	s.dpLedgerRespond(w, acc, err)
}

func (s *Server) dpBudgetReserve(w http.ResponseWriter, r *http.Request) {
	var in dataplane.BudgetReserveRequest
	if !decodeDP(w, r, dpMaxSmallRequest, &in) {
		return
	}
	res, err := s.store.ReserveAIBudget(r.Context(), in.AccountID, in.RequestID, in.ReserveCents)
	s.dpLedgerRespond(w, res, err)
}

func (s *Server) dpBudgetSettle(w http.ResponseWriter, r *http.Request) {
	var in dataplane.BudgetSettleRequest
	if !decodeDP(w, r, dpMaxSmallRequest, &in) {
		return
	}
	cents, err := s.store.SettleAIBudgetUsage(r.Context(), in.RequestID, in.Usage)
	if err != nil {
		s.dpLedgerRespond(w, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, dataplane.BudgetSettleResponse{Cents: cents})
}

func (s *Server) dpBudgetRelease(w http.ResponseWriter, r *http.Request) {
	var in dataplane.BudgetReleaseRequest
	if !decodeDP(w, r, dpMaxSmallRequest, &in) {
		return
	}
	s.dpLedgerRespond(w, nil, s.store.ReleaseAIBudget(r.Context(), in.RequestID))
}

// dpLedgerRespond maps ledger sentinel errors to the codes the gateway's
// remote ledger turns back into store.ErrNotFound / store.ErrBudgetExceeded.
func (s *Server) dpLedgerRespond(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, store.ErrBudgetExceeded):
		writeErr(w, http.StatusConflict, dataplane.CodeBudgetExceeded, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, dataplane.CodeNotFound, "not found")
	case err != nil:
		s.fail(w, err)
	case v == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func decodeDP(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}
