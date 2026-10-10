package analytics

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func TestCollector_PrometheusMetricsAndHistograms(t *testing.T) {
	c := NewCollector(nil, nil, "node-test-1")

	// Record requests with various statuses and latencies
	c.Record(store.RequestLog{
		Status:             200,
		LatencyMS:          3.5, // 0.0035s <= 0.005s bucket
		UpstreamDurationMS: 2.1,
	})
	c.Record(store.RequestLog{
		Status:             200,
		LatencyMS:          15.0, // 0.015s <= 0.025s bucket
		UpstreamDurationMS: 12.0,
	})
	c.Record(store.RequestLog{
		Status:          429,
		LatencyMS:       1.2,
		RateLimitStatus: "rate_limited",
	})
	c.Record(store.RequestLog{
		Status:    500,
		LatencyMS: 120.0,
		Error:     "upstream failure",
	})

	var buf bytes.Buffer
	c.WritePrometheusMetrics(&buf)
	metricsOut := buf.String()

	t.Logf("Generated Prometheus Metrics:\n%s", metricsOut)

	// Verify monotonic counters
	if !strings.Contains(metricsOut, `relayops_requests_total{status="200"} 2`) {
		t.Errorf("expected 200 status counter = 2")
	}
	if !strings.Contains(metricsOut, `relayops_requests_total{status="429"} 1`) {
		t.Errorf("expected 429 status counter = 1")
	}
	if !strings.Contains(metricsOut, `relayops_requests_total{status="500"} 1`) {
		t.Errorf("expected 500 status counter = 1")
	}

	// Verify rate limit counter
	if !strings.Contains(metricsOut, `relayops_rate_limit_exceeded_total 1`) {
		t.Errorf("expected rate limit exceeded counter = 1")
	}

	// Verify latency histogram buckets
	if !strings.Contains(metricsOut, `relayops_request_duration_seconds_bucket{le="0.005"} 2`) { // 3.5ms and 1.2ms
		t.Errorf("expected 0.005s bucket = 2")
	}
	if !strings.Contains(metricsOut, `relayops_request_duration_seconds_bucket{le="+Inf"} 4`) {
		t.Errorf("expected +Inf bucket = 4")
	}
	if !strings.Contains(metricsOut, `relayops_request_duration_seconds_count 4`) {
		t.Errorf("expected duration count = 4")
	}

	// Verify upstream summary
	if !strings.Contains(metricsOut, `relayops_upstream_duration_seconds_count 2`) {
		t.Errorf("expected upstream duration count = 2")
	}
}

func init() {
	_ = time.Second
}
