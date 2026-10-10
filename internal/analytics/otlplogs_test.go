package analytics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/realtime"
)

// Every request is exported (sampling only limits PostgreSQL), as OTLP log
// records with severity by status and the standard HTTP attributes.
func TestOTLPLogExportSendsEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var records []map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResourceLogs []struct {
				Resource struct {
					Attributes []map[string]any `json:"attributes"`
				} `json:"resource"`
				ScopeLogs []struct {
					LogRecords []map[string]any `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if r.URL.Path != "/v1/logs" || json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		auth = r.Header.Get("Authorization")
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				records = append(records, sl.LogRecords...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer otel-token")
	exp := NewLogExporterFromEnv("n1")
	if exp == nil {
		t.Fatal("exporter not built from OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	w := &captureWriter{}
	c := NewCollectorWithWriter(w, realtime.NewHub(), "n1")
	c.SetSampleRate(0.1)
	c.SetLogExporter(exp)
	c.flush(context.Background(), sampleBatch(300, 7))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exp.Shutdown(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(records) != 307 {
		t.Fatalf("exported %d records, want all 307 (sampling must not apply to export)", len(records))
	}
	if len(w.logs) >= 307 {
		t.Fatalf("PostgreSQL got %d rows; sampling should still apply there", len(w.logs))
	}
	if auth != "Bearer otel-token" {
		t.Fatalf("OTEL_EXPORTER_OTLP_HEADERS not sent: %q", auth)
	}
	errors := 0
	for _, r := range records {
		if r["severityText"] == "ERROR" {
			errors++
		}
	}
	if errors != 7 {
		t.Fatalf("%d ERROR records, want 7 (the 502s)", errors)
	}
	if exp.exported.Load() != 307 || exp.failed.Load() != 0 {
		t.Fatalf("counters exported=%d failed=%d", exp.exported.Load(), exp.failed.Load())
	}
}

func TestOTLPLogExporterOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	if NewLogExporterFromEnv("n1") != nil {
		t.Fatal("exporter built without an endpoint")
	}
}
