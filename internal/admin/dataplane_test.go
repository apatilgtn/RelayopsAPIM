package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

const testNodeToken = "node-token-0123456789-0123456789-abcdef"

func TestDataplaneFingerprintIgnoresRowOrder(t *testing.T) {
	mk := func(keys []store.KeyRecord, subs []store.SubRecord, streams []store.StreamService) string {
		resp, err := buildConfigResponse(store.SnapshotData{Revision: 7, Keys: keys, Subs: subs}, streams, nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Fingerprint
	}
	a := mk(
		[]store.KeyRecord{{KeyHash: "a"}, {KeyHash: "b"}},
		[]store.SubRecord{{ConsumerID: "c1", APIID: "x"}, {ConsumerID: "c1", APIID: "a"}, {ConsumerID: "c0", APIID: "z"}},
		[]store.StreamService{{ID: "s1"}, {ID: "s2"}},
	)
	b := mk(
		[]store.KeyRecord{{KeyHash: "b"}, {KeyHash: "a"}},
		[]store.SubRecord{{ConsumerID: "c0", APIID: "z"}, {ConsumerID: "c1", APIID: "a"}, {ConsumerID: "c1", APIID: "x"}},
		[]store.StreamService{{ID: "s2"}, {ID: "s1"}},
	)
	if a != b {
		t.Fatalf("fingerprint depends on row order: %s vs %s", a, b)
	}
	if c := mk([]store.KeyRecord{{KeyHash: "a"}}, nil, nil); c == a {
		t.Fatal("revoking a key must change the fingerprint")
	}
}

func newDataplaneTestServer() *Server {
	return New(nil, nil, realtime.NewHub(), "admin-token", "cp-1", fstest.MapFS{}, WithDataplaneToken(testNodeToken))
}

func TestDataplaneTokenRequired(t *testing.T) {
	s := newDataplaneTestServer()
	h := s.Handler()
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "Bearer nope", http.StatusUnauthorized},
		{"admin token is not a node token", "Bearer admin-token", http.StatusUnauthorized},
		// Correct token reaches the handler, which needs the database.
		{"correct", "Bearer " + testNodeToken, http.StatusServiceUnavailable},
	} {
		req := httptest.NewRequest(http.MethodGet, dataplane.PathConfig, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

func TestDataplaneAPINotMountedWithoutToken(t *testing.T) {
	s := New(nil, nil, realtime.NewHub(), "admin-token", "cp-1", fstest.MapFS{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, dataplane.PathConfig, nil)
	req.Header.Set("Authorization", "Bearer "+testNodeToken)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("node API answered %d without a configured token", rec.Code)
	}
}

// seed puts a computed snapshot in the cache so the long-poll can be tested
// without a database.
func seed(s *Server, gen uint64, etag string) {
	s.dataplane.cacheMu.Lock() // the long-poll reads the cache concurrently
	defer s.dataplane.cacheMu.Unlock()
	s.dataplane.cache["default|false"] = &dpEntry{gen: gen, at: time.Now(), etag: etag, raw: []byte(`{"fingerprint":"x"}`)}
}

func TestDataplaneLongPollNotModified(t *testing.T) {
	s := newDataplaneTestServer()
	seed(s, 0, `"v1"`)
	req := httptest.NewRequest(http.MethodGet, dataplane.PathConfig+"?wait=1", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	rec := httptest.NewRecorder()
	start := time.Now()
	s.dpConfig(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", rec.Code)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("returned after %v; the long-poll should hold for the wait", d)
	}
}

// The gateway sends the bare fingerprint from the response body; it must match
// the quoted ETag, or every poll returns 200 and the gateway reloads in a loop.
func TestDataplaneLongPollAcceptsBareFingerprint(t *testing.T) {
	s := newDataplaneTestServer()
	seed(s, 0, `"abc123"`)
	req := httptest.NewRequest(http.MethodGet, dataplane.PathConfig+"?wait=1", nil)
	req.Header.Set("If-None-Match", "abc123")
	rec := httptest.NewRecorder()
	s.dpConfig(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("bare fingerprint: status %d, want 304", rec.Code)
	}
}

func TestDataplaneLongPollWakesOnChange(t *testing.T) {
	s := newDataplaneTestServer()
	seed(s, 0, `"v1"`)
	go func() {
		time.Sleep(100 * time.Millisecond)
		seed(s, 1, `"v2"`) // what the next computation would produce
		s.dataplane.bump()
	}()
	req := httptest.NewRequest(http.MethodGet, dataplane.PathConfig+"?wait=10", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	rec := httptest.NewRecorder()
	start := time.Now()
	s.dpConfig(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"v2"` {
		t.Fatalf("status %d etag %q, want 200 \"v2\"", rec.Code, rec.Header().Get("ETag"))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("change took %v to reach the waiting gateway", d)
	}
}
