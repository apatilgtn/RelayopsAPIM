package dataplane

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/store"
)

const token = "node-token-0123456789-0123456789-abcdef"

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, Token: token, NodeID: "gw-1", NodeGroup: "edge", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRequiresHTTPS(t *testing.T) {
	if _, err := NewClient(ClientConfig{BaseURL: "http://cp:9090", Token: token}); err == nil {
		t.Fatal("plain HTTP accepted without AllowHTTP")
	}
	if _, err := NewClient(ClientConfig{BaseURL: "https://cp:9090", Token: token}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(ClientConfig{BaseURL: "https://cp:9090"}); err == nil {
		t.Fatal("missing token accepted")
	}
}

// testEnvelope builds the configuration response a control plane would send
// for revision rev, signed when signer is set.
func testEnvelope(t *testing.T, rev int64, signer *Signer) *ConfigResponse {
	t.Helper()
	payload, err := json.Marshal(ConfigPayload{Data: store.SnapshotData{Revision: rev}, Streams: []store.StreamService{{ID: "tcp-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	env := &ConfigResponse{Fingerprint: Fingerprint(payload), IssuedAt: time.Now().UTC(), Payload: payload}
	if signer != nil {
		signer.Sign(env)
	}
	return env
}

func TestSignedEnvelopeVerification(t *testing.T) {
	seed, pub, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ParseSigningKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ParseVerifyKeys(pub)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := testEnvelope(t, 3, signer).Open(verifier); err != nil || p.Data.Revision != 3 {
		t.Fatalf("valid signed envelope: %v", err)
	}
	if _, err := testEnvelope(t, 3, nil).Open(verifier); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("unsigned envelope accepted when signatures are required: %v", err)
	}
	tampered := testEnvelope(t, 3, signer)
	tampered.Payload = []byte(`{"data":{"Revision":4},"streams":[]}`)
	if _, err := tampered.Open(verifier); err == nil {
		t.Fatal("payload changed after signing was accepted")
	}
	forged := testEnvelope(t, 3, signer)
	forged.Payload = []byte(`{"data":{"Revision":4},"streams":[]}`)
	forged.Fingerprint = Fingerprint(forged.Payload) // attacker recomputes the fingerprint but cannot re-sign
	if _, err := forged.Open(verifier); err == nil {
		t.Fatal("re-fingerprinted payload accepted without a valid signature")
	}
	_, otherPub, _ := GenerateSigningKey()
	other, _ := ParseVerifyKeys(otherPub)
	if _, err := testEnvelope(t, 3, signer).Open(other); err == nil {
		t.Fatal("envelope signed by an untrusted key accepted")
	}
	// Key rotation: both old and new keys trusted.
	both, err := ParseVerifyKeys(otherPub + "," + pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testEnvelope(t, 3, signer).Open(both); err != nil {
		t.Fatalf("rotation key list: %v", err)
	}
}

func TestSignedCacheAndReplay(t *testing.T) {
	seed, pub, _ := GenerateSigningKey()
	signer, _ := ParseSigningKey(seed)
	verifier, _ := ParseVerifyKeys(pub)
	src := NewRemoteSource(nil)
	src.RequireSignatures(verifier)

	env := testEnvelope(t, 5, signer)
	raw, _ := json.Marshal(env)
	data, err := src.DecodeCache(raw)
	if err != nil || data.Revision != 5 {
		t.Fatalf("signed cache: rev %d err %v", data.Revision, err)
	}
	if cached, err := src.EncodeCache(data); err != nil || string(cached) != string(raw) {
		t.Fatalf("cache must be the signed envelope: %v", err)
	}
	// A plain snapshot (or a tampered file) is refused for a cold start.
	if _, err := src.DecodeCache([]byte(`{"Revision":9}`)); err == nil {
		t.Fatal("unsigned cache accepted when signatures are required")
	}
	// An envelope issued well before the applied one is refused as a replay.
	old := &ConfigResponse{Fingerprint: env.Fingerprint, IssuedAt: env.IssuedAt.Add(-time.Hour), Payload: env.Payload}
	signer.Sign(old)
	oldRaw, _ := json.Marshal(old)
	if _, err := src.DecodeCache(oldRaw); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("replayed old envelope: %v", err)
	}
}

func TestRemoteSourceLoadWatchAck(t *testing.T) {
	var version atomic.Int64
	version.Store(1)
	var acks atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathConfig, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get(HeaderNodeGroup) != "edge" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		env := testEnvelope(t, version.Load(), nil)
		if r.Header.Get("If-None-Match") == env.Fingerprint {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(env)
	})
	mux.HandleFunc("POST "+PathAck, func(w http.ResponseWriter, r *http.Request) {
		var a gateway.NodeAck
		_ = json.NewDecoder(r.Body).Decode(&a)
		if a.NodeID == "gw-1" && a.Revision > 0 {
			acks.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	src := NewRemoteSource(newTestClient(t, mux))
	var streams atomic.Int64
	src.OnStreams(func(s []store.StreamService) { streams.Store(int64(len(s))) })

	data, err := src.Load(context.Background(), "edge", false)
	if err != nil || data.Revision != 1 || streams.Load() != 1 {
		t.Fatalf("load: rev %d streams %d err %v", data.Revision, streams.Load(), err)
	}
	if err := src.Ack(context.Background(), gateway.NodeAck{NodeID: "gw-1", Revision: 1}); err != nil || acks.Load() != 1 {
		t.Fatalf("ack: %v (acks %d)", err, acks.Load())
	}

	old := pollWait
	pollWait = time.Second
	defer func() { pollWait = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notified := make(chan string, 4)
	go func() { _ = src.Watch(ctx, func(r string) { notified <- r }) }()
	select {
	case <-notified:
		t.Fatal("notified without a change")
	case <-time.After(200 * time.Millisecond):
	}
	version.Store(2)
	select {
	case <-notified:
	case <-time.After(3 * time.Second):
		t.Fatal("change not noticed")
	}
	data, err = src.Load(context.Background(), "edge", false)
	if err != nil || data.Revision != 2 {
		t.Fatalf("load after change: rev %d err %v", data.Revision, err)
	}
	if src.LastSync().IsZero() {
		t.Fatal("last sync not recorded")
	}
}

func TestRemoteSourceWatchReturnsOnFailure(t *testing.T) {
	src := NewRemoteSource(newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})))
	err := src.Watch(context.Background(), func(string) {})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Fatalf("got %v, want a 401 StatusError", err)
	}
	if src.SyncErrors() != 1 {
		t.Fatalf("sync errors %d, want 1", src.SyncErrors())
	}
}

func TestLedgerMapsSentinelErrors(t *testing.T) {
	mux := http.NewServeMux()
	reply := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("POST "+PathBudgetLookup, reply(http.StatusNotFound, `{"error":"not_found"}`))
	mux.HandleFunc("POST "+PathBudgetReserve, reply(http.StatusConflict, `{"error":"budget_exceeded"}`))
	mux.HandleFunc("POST "+PathBudgetSettle, reply(http.StatusServiceUnavailable, `{"error":"database_unavailable"}`))
	mux.HandleFunc("POST "+PathBudgetRelease, reply(http.StatusNoContent, ``))
	l := NewLedger(newTestClient(t, mux))
	ctx := context.Background()
	if _, err := l.GetAIBudgetAccountByConsumer(ctx, "t", "c"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lookup: %v, want ErrNotFound", err)
	}
	if _, err := l.ReserveAIBudget(ctx, "a", "r", 2); !errors.Is(err, store.ErrBudgetExceeded) {
		t.Errorf("reserve: %v, want ErrBudgetExceeded", err)
	}
	// Any other failure must surface so the gateway fails closed.
	if _, err := l.SettleAIBudgetUsage(ctx, "r", store.AIUsage{Model: "m", EstimateCents: 3}); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("settle: %v, want an unavailable error", err)
	}
	if err := l.ReleaseAIBudget(ctx, "r"); err != nil {
		t.Errorf("release: %v", err)
	}
}

func TestLogWriterSendsGzipBatch(t *testing.T) {
	var got []store.RequestLog
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathLogs, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		zr, err := gzipReader(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(zr).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	})
	w := NewLogWriter(newTestClient(t, mux))
	if err := w.InsertLogs(context.Background(), []store.RequestLog{{LogID: "l1", NodeID: "gw-1", Status: 200}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LogID != "l1" {
		t.Fatalf("received %+v", got)
	}
}

func gzipReader(r *http.Request) (io.Reader, error) { return gzip.NewReader(r.Body) }
