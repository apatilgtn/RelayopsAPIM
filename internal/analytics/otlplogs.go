package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/store"
)

// LogExporter ships every request log to an OpenTelemetry collector as OTLP
// log records (OTLP/HTTP JSON), so analytics can live outside PostgreSQL
// while PostgreSQL keeps only what RELAYOPS_LOG_SAMPLE_RATE persists.
//
// Configuration (standard OTEL variables):
//
//	OTEL_EXPORTER_OTLP_LOGS_ENDPOINT  full URL, e.g. http://otel-collector:4318/v1/logs
//	OTEL_EXPORTER_OTLP_ENDPOINT       base URL; "/v1/logs" is appended
//	OTEL_EXPORTER_OTLP_HEADERS        comma-separated key=value pairs (auth headers)
//	OTEL_SERVICE_NAME                 service.name (default relayops-gateway)
type LogExporter struct {
	endpoint string
	headers  map[string]string
	service  string
	nodeID   string
	client   *http.Client
	ch       chan store.RequestLog
	done     chan struct{}

	exported atomic.Int64
	dropped  atomic.Int64 // buffer full
	failed   atomic.Int64 // records in batches the collector did not accept
}

const (
	logExportBuffer = 20_000
	logExportBatch  = 1_000
)

// NewLogExporterFromEnv builds an exporter from OTEL_* variables, or returns
// nil when no endpoint is configured.
func NewLogExporterFromEnv(nodeID string) *LogExporter {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	if endpoint == "" {
		if base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			endpoint = strings.TrimRight(base, "/") + "/v1/logs"
		}
	}
	if endpoint == "" {
		return nil
	}
	service := os.Getenv("OTEL_SERVICE_NAME")
	if service == "" {
		service = "relayops-gateway"
	}
	return NewLogExporter(endpoint, parseOTELHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")), service, nodeID)
}

func NewLogExporter(endpoint string, headers map[string]string, service, nodeID string) *LogExporter {
	e := &LogExporter{
		endpoint: endpoint, headers: headers, service: service, nodeID: nodeID,
		client: &http.Client{Timeout: 10 * time.Second},
		ch:     make(chan store.RequestLog, logExportBuffer),
		done:   make(chan struct{}),
	}
	go e.loop()
	return e
}

func parseOTELHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// Enqueue hands logs to the exporter without blocking; when its buffer is
// full the logs are dropped and counted.
func (e *LogExporter) Enqueue(logs []store.RequestLog) {
	for _, l := range logs {
		select {
		case e.ch <- l:
		default:
			e.dropped.Add(1)
		}
	}
}

func (e *LogExporter) loop() {
	defer close(e.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var batch []store.RequestLog
	flush := func() {
		if len(batch) > 0 {
			e.export(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case l, ok := <-e.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, l)
			if len(batch) >= logExportBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// Shutdown flushes buffered logs, waiting at most until ctx is done.
func (e *LogExporter) Shutdown(ctx context.Context) {
	close(e.ch)
	select {
	case <-e.done:
	case <-ctx.Done():
	}
}

func (e *LogExporter) export(batch []store.RequestLog) {
	body, err := EncodeOTLPLogs(batch, e.service, e.nodeID)
	if err != nil {
		e.failed.Add(int64(len(batch)))
		return
	}
	req, err := http.NewRequest(http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		e.failed.Add(int64(len(batch)))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.failed.Add(int64(len(batch)))
		slog.Debug("OTLP log export failed", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e.failed.Add(int64(len(batch)))
		slog.Debug("OTLP log export rejected", "status", resp.StatusCode)
		return
	}
	e.exported.Add(int64(len(batch)))
}

type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 travels as a string in OTLP/JSON
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: otlpValue{StringValue: &v}} }
func intAttr(k string, v int64) otlpAttr {
	s := strconv.FormatInt(v, 10)
	return otlpAttr{Key: k, Value: otlpValue{IntValue: &s}}
}
func dblAttr(k string, v float64) otlpAttr { return otlpAttr{Key: k, Value: otlpValue{DoubleValue: &v}} }

// EncodeOTLPLogs renders request logs as an OTLP ExportLogsServiceRequest.
func EncodeOTLPLogs(logs []store.RequestLog, service, nodeID string) ([]byte, error) {
	records := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		sevNum, sevText := 9, "INFO"
		switch {
		case l.Status >= 500 || l.Status == 0:
			sevNum, sevText = 17, "ERROR"
		case l.Status >= 400:
			sevNum, sevText = 13, "WARN"
		}
		attrs := []otlpAttr{
			strAttr("http.request.method", l.Method),
			strAttr("url.path", l.Path),
			intAttr("http.response.status_code", int64(l.Status)),
			dblAttr("relayops.latency_ms", l.LatencyMS),
			intAttr("relayops.config_revision", l.ConfigRevision),
			strAttr("relayops.request_id", l.RequestID),
			strAttr("relayops.log_id", l.LogID),
		}
		opt := func(k, v string) {
			if v != "" {
				attrs = append(attrs, strAttr(k, v))
			}
		}
		opt("relayops.api.name", l.APIName)
		if l.APIID != nil {
			opt("relayops.api.id", *l.APIID)
		}
		if l.ConsumerID != nil {
			opt("relayops.consumer.id", *l.ConsumerID)
		}
		opt("relayops.consumer.name", l.ConsumerName)
		opt("relayops.tenant.id", l.TenantID)
		opt("client.address", l.ClientIP)
		opt("relayops.decision_reason", l.DecisionReason)
		opt("relayops.auth_status", l.AuthStatus)
		opt("relayops.rate_limit_status", l.RateLimitStatus)
		opt("error.message", l.Error)
		if l.BytesOut > 0 {
			attrs = append(attrs, intAttr("http.response.body.size", l.BytesOut))
		}
		if l.UpstreamDurationMS > 0 {
			attrs = append(attrs, dblAttr("relayops.upstream_duration_ms", l.UpstreamDurationMS))
		}
		if l.Model != "" {
			attrs = append(attrs, strAttr("gen_ai.request.model", l.Model),
				intAttr("gen_ai.usage.input_tokens", int64(l.TokensPrompt)),
				intAttr("gen_ai.usage.output_tokens", int64(l.TokensCompletion)))
		}
		rec := map[string]any{
			"timeUnixNano":   strconv.FormatInt(l.TS.UnixNano(), 10),
			"severityNumber": sevNum,
			"severityText":   sevText,
			"body":           otlpValue{StringValue: ptr(fmt.Sprintf("%s %s %d", l.Method, l.Path, l.Status))},
			"attributes":     attrs,
		}
		if len(l.TraceID) == 32 {
			rec["traceId"] = l.TraceID
		}
		records = append(records, rec)
	}
	return json.Marshal(map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": map[string]any{"attributes": []otlpAttr{strAttr("service.name", service), strAttr("service.instance.id", nodeID)}},
			"scopeLogs": []any{map[string]any{
				"scope":      map[string]any{"name": "relayops.request_log"},
				"logRecords": records,
			}},
		}},
	})
}

func ptr[T any](v T) *T { return &v }

// WriteMetrics writes the exporter's Prometheus counters.
func (e *LogExporter) WriteMetrics(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprintf(w, "# HELP relayops_request_logs_exported_total Request logs accepted by the OTLP log collector\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_exported_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_exported_total %d\n\n", e.exported.Load())
	fmt.Fprintf(w, "# HELP relayops_request_logs_export_failed_total Request logs the OTLP collector did not accept\n")
	fmt.Fprintf(w, "# TYPE relayops_request_logs_export_failed_total counter\n")
	fmt.Fprintf(w, "relayops_request_logs_export_failed_total{reason=\"rejected\"} %d\n", e.failed.Load())
	fmt.Fprintf(w, "relayops_request_logs_export_failed_total{reason=\"buffer_full\"} %d\n\n", e.dropped.Load())
}
