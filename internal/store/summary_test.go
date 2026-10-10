package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIntegrationSummarySeriesAndTopStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	api, _, err := s.CreateAPIAtomic(ctx, API{Name: "sum", BasePath: "/sum", UpstreamURL: "http://x", AuthType: "none", TimeoutMS: 1000, Enabled: true}, "t", "sum")
	if err != nil {
		t.Fatal(err)
	}
	var logs []RequestLog
	for i := 0; i < 10; i++ {
		status := 200
		if i < 3 {
			status = 503
		}
		id := api.ID
		logs = append(logs, RequestLog{TS: time.Now().Add(-time.Duration(i) * time.Minute), NodeID: "n", RequestID: fmt.Sprint(i),
			LogID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), APIID: &id, APIName: "sum", Method: "GET", Path: "/", Status: status, LatencyMS: 10})
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summarize(ctx, time.Hour, "1h")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Series) != 60 {
		t.Fatalf("series has %d points", len(sum.Series))
	}
	var total, errs int64
	for _, p := range sum.Series {
		total, errs = total+p.Count, errs+p.Errors
	}
	if total != 10 || errs != 3 {
		t.Fatalf("series totals %d requests, %d errors", total, errs)
	}
	if len(sum.TopAPIs) != 1 || sum.TopAPIs[0].Errors != 3 || sum.TopAPIs[0].AvgLatency != 10 {
		t.Fatalf("top APIs %+v", sum.TopAPIs)
	}
}
