// Command soak runs a sustained-load test against a RelayOps fleet while the
// configuration keeps changing, and reports whether traffic stayed healthy.
//
// During the run it sends a steady request rate (round-robin across gateway
// nodes, many distinct callers) and, every churn interval, cycles through:
// publish a new revision -> deploy it as a 20% canary -> promote it. At the end
// it checks invariants and writes a JSON + Markdown report:
//
//   - no request failed (transport error or non-2xx) unless -max-error-rate allows it
//
//   - every node converged on the final revision
//
//   - latency percentiles over the whole run
//
//     go run ./cmd/soak -gateways http://localhost:8080 -admin http://localhost:9090 \
//     -token relayops-admin -upstream http://localhost:7070 -d 10m -rps 200 -out soak
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cfg struct {
	gateways     []string
	admin, token string
	upstream     string
	duration     time.Duration
	rps          int
	churn        time.Duration
	callers      int
	maxErrorRate float64
	out          string
	cleanup      bool
}

type result struct {
	Started          time.Time          `json:"started"`
	DurationSeconds  float64            `json:"duration_seconds"`
	TargetRPS        int                `json:"target_rps"`
	Gateways         []string           `json:"gateways"`
	Requests         int64              `json:"requests"`
	AchievedRPS      float64            `json:"achieved_rps"`
	Status           map[string]int     `json:"status"`
	TransportErrors  int64              `json:"transport_errors"`
	ErrorRatePct     float64            `json:"error_rate_pct"`
	LatencyMS        map[string]float64 `json:"latency_ms"`
	UpstreamDirectMS map[string]float64 `json:"upstream_direct_latency_ms"`
	PerGateway       map[string]int64   `json:"requests_per_gateway"`
	APIName          string             `json:"api_name"`
	APIBasePath      string             `json:"api_base_path"`
	Publishes        int                `json:"publishes"`
	Canaries         int                `json:"canaries"`
	Promotions       int                `json:"promotions"`
	ChurnFailures    []string           `json:"churn_failures"`
	CanaryServed     int64              `json:"requests_served_by_canary"`
	FinalRevision    int64              `json:"final_revision"`
	FleetConverged   bool               `json:"fleet_converged"`
	SampleErrors     []string           `json:"sample_errors"`
	Pass             bool               `json:"pass"`
	FailedChecks     []string           `json:"failed_checks"`
}

func main() {
	var c cfg
	var gws string
	flag.StringVar(&gws, "gateways", "http://localhost:8080", "comma-separated gateway base URLs")
	flag.StringVar(&c.admin, "admin", "http://localhost:9090", "control-plane base URL")
	flag.StringVar(&c.token, "token", os.Getenv("RELAYOPS_TOKEN"), "admin bearer token")
	flag.StringVar(&c.upstream, "upstream", "http://localhost:7070", "mock upstream base URL")
	flag.DurationVar(&c.duration, "d", 5*time.Minute, "test duration")
	flag.IntVar(&c.rps, "rps", 100, "request rate")
	flag.DurationVar(&c.churn, "churn", 20*time.Second, "interval between configuration changes")
	flag.IntVar(&c.callers, "callers", 500, "distinct callers (X-API-Key values)")
	flag.Float64Var(&c.maxErrorRate, "max-error-rate", 0, "allowed failed-request percentage")
	flag.StringVar(&c.out, "out", "soak-report", "report file prefix (.json and .md)")
	flag.BoolVar(&c.cleanup, "cleanup", false, "delete the soak API afterwards (keep it to audit its request logs)")
	flag.Parse()
	c.gateways = strings.Split(gws, ",")
	res, err := run(c)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeReports(c, res); err != nil {
		log.Fatal(err)
	}
	if !res.Pass {
		log.Printf("FAIL: %s", strings.Join(res.FailedChecks, "; "))
		os.Exit(1)
	}
	log.Printf("PASS: %d requests, %.2f%% errors, p99 %.1f ms", res.Requests, res.ErrorRatePct, res.LatencyMS["p99"])
}

// ---------------------------------------------------------------------------

type admin struct {
	base, token string
	client      *http.Client
}

func (a admin) call(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, r)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func run(c cfg) (*result, error) {
	a := admin{base: strings.TrimRight(c.admin, "/"), token: c.token, client: &http.Client{Timeout: 30 * time.Second}}
	suffix := fmt.Sprint(time.Now().Unix())
	basePath := "/soak-" + suffix

	// Baseline: the upstream's own latency, measured directly.
	res0 := directBaseline(c.upstream, 200)

	// Setup: an API with two upstream targets, retries and a breaker.
	var api struct {
		ID string `json:"id"`
	}
	err := a.call("POST", "/api/apis", map[string]any{
		"name": "soak-" + suffix, "base_path": basePath, "upstream_url": c.upstream, "timeout_ms": 5000,
		"traffic_policy": map[string]any{
			"targets":         []map[string]any{{"url": c.upstream}, {"url": c.upstream + "/"}},
			"retries":         map[string]any{"attempts": 2, "backoff_ms": 5},
			"circuit_breaker": map[string]any{"failure_threshold": 20, "open_seconds": 5},
		},
	}, &api)
	if err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	if c.cleanup {
		defer func() { _ = a.call("DELETE", "/api/apis/"+api.ID, nil, nil) }()
	}
	time.Sleep(time.Second) // let every node apply the new route

	res := &result{Started: time.Now(), TargetRPS: c.rps, Gateways: c.gateways, Status: map[string]int{}, LatencyMS: map[string]float64{},
		UpstreamDirectMS: res0, PerGateway: map[string]int64{}, APIName: "soak-" + suffix, APIBasePath: basePath}
	var (
		mu        sync.Mutex
		latencies []float64
		requests  atomic.Int64
		transport atomic.Int64
		canaryHit atomic.Int64
		wg        sync.WaitGroup
	)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 512}}
	stop := make(chan struct{})

	// Configuration churn.
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		tick := time.NewTicker(c.churn)
		defer tick.Stop()
		step := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			var e error
			switch step % 3 {
			case 0:
				e = a.call("PUT", "/api/apis/"+api.ID, map[string]any{"description": fmt.Sprintf("soak change %d", step)}, nil)
				if e == nil {
					res.Publishes++
				}
			case 1:
				var revs []struct {
					Revision int64  `json:"revision"`
					Status   string `json:"status"`
				}
				if e = a.call("GET", "/api/revisions?limit=1", nil, &revs); e == nil && len(revs) > 0 {
					e = a.call("POST", fmt.Sprintf("/api/revisions/%d/canary", revs[0].Revision), map[string]any{"traffic_percent": 20}, nil)
					if e == nil {
						res.Canaries++
					}
				}
			case 2:
				var fleet struct {
					CanaryRevision int64 `json:"canary_revision"`
				}
				if e = a.call("GET", "/api/fleet/status", nil, &fleet); e == nil && fleet.CanaryRevision > 0 {
					e = a.call("POST", fmt.Sprintf("/api/revisions/%d/promote", fleet.CanaryRevision), nil, nil)
					if e == nil {
						res.Promotions++
					}
				}
			}
			if e != nil {
				mu.Lock()
				res.ChurnFailures = append(res.ChurnFailures, fmt.Sprintf("%s: %v", time.Now().Format(time.RFC3339), e))
				mu.Unlock()
			}
			step++
		}
	}()

	// Load: every 5 ms send however many requests are due, so the rate holds
	// even where timers are coarse (Windows ticks every ~15.6 ms).
	tick := time.NewTicker(5 * time.Millisecond)
	deadline := time.After(c.duration)
	loadStart := time.Now()
	i := 0
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-tick.C:
		}
		due := int(time.Since(loadStart).Seconds()*float64(c.rps)) - i
		for ; due > 0; due-- {
			gw := strings.TrimRight(c.gateways[i%len(c.gateways)], "/")
			caller := fmt.Sprintf("soak-caller-%d", rand.Intn(c.callers))
			i++
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("GET", gw+basePath+"/echo", nil)
				req.Header.Set("X-API-Key", caller) // cohort stickiness key; the API itself is public
				start := time.Now()
				resp, err := client.Do(req)
				ms := float64(time.Since(start).Microseconds()) / 1000
				requests.Add(1)
				if err != nil {
					transport.Add(1)
					mu.Lock()
					if len(res.SampleErrors) < 20 {
						res.SampleErrors = append(res.SampleErrors, err.Error())
					}
					mu.Unlock()
					return
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				if resp.Header.Get("X-Relayops-Cohort") == "canary" {
					canaryHit.Add(1)
				}
				mu.Lock()
				res.PerGateway[gw]++
				latencies = append(latencies, ms)
				res.Status[fmt.Sprintf("%d", resp.StatusCode)]++
				if resp.StatusCode >= 300 && len(res.SampleErrors) < 20 {
					res.SampleErrors = append(res.SampleErrors, fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body))))
				}
				mu.Unlock()
			}()
		}
	}
	tick.Stop()
	close(stop)
	<-churnDone
	wg.Wait()
	res.DurationSeconds = time.Since(res.Started).Seconds()

	// Leave the fleet clean: promote any canary still in flight.
	var fleet struct {
		TargetRevision int64 `json:"target_revision"`
		CanaryRevision int64 `json:"canary_revision"`
		Converged      bool  `json:"converged"`
	}
	if a.call("GET", "/api/fleet/status", nil, &fleet) == nil && fleet.CanaryRevision > 0 {
		_ = a.call("POST", fmt.Sprintf("/api/revisions/%d/promote", fleet.CanaryRevision), nil, nil)
	}
	for w := 0; w < 20; w++ {
		time.Sleep(500 * time.Millisecond)
		if a.call("GET", "/api/fleet/status", nil, &fleet) == nil && fleet.Converged {
			break
		}
	}
	res.FinalRevision, res.FleetConverged = fleet.TargetRevision, fleet.Converged

	res.Requests = requests.Load()
	res.TransportErrors = transport.Load()
	res.CanaryServed = canaryHit.Load()
	res.AchievedRPS = float64(res.Requests) / res.DurationSeconds
	failed := res.TransportErrors
	for code, n := range res.Status {
		if !strings.HasPrefix(code, "2") {
			failed += int64(n)
		}
	}
	if res.Requests > 0 {
		res.ErrorRatePct = float64(failed) / float64(res.Requests) * 100
	}
	sort.Float64s(latencies)
	for _, p := range []struct {
		name string
		q    float64
	}{{"p50", .50}, {"p90", .90}, {"p95", .95}, {"p99", .99}, {"p99.9", .999}, {"max", 1}} {
		if len(latencies) > 0 {
			idx := int(float64(len(latencies)-1) * p.q)
			res.LatencyMS[p.name] = latencies[idx]
		}
	}

	if res.ErrorRatePct > c.maxErrorRate {
		res.FailedChecks = append(res.FailedChecks, fmt.Sprintf("error rate %.3f%% above %.3f%%", res.ErrorRatePct, c.maxErrorRate))
	}
	if !res.FleetConverged {
		res.FailedChecks = append(res.FailedChecks, "fleet did not converge on the final revision")
	}
	if len(res.ChurnFailures) > 0 {
		res.FailedChecks = append(res.FailedChecks, fmt.Sprintf("%d configuration changes failed", len(res.ChurnFailures)))
	}
	if res.AchievedRPS < float64(c.rps)*0.9 {
		res.FailedChecks = append(res.FailedChecks, fmt.Sprintf("achieved %.0f rps, below 90%% of target", res.AchievedRPS))
	}
	if res.Canaries > 0 && res.CanaryServed == 0 {
		res.FailedChecks = append(res.FailedChecks, "canaries were deployed but no request was served by one")
	}
	res.Pass = len(res.FailedChecks) == 0
	return res, nil
}

// directBaseline measures the upstream without the gateway, sequentially.
func directBaseline(upstream string, n int) map[string]float64 {
	client := &http.Client{Timeout: 10 * time.Second}
	var lat []float64
	for i := 0; i < n; i++ {
		start := time.Now()
		resp, err := client.Get(strings.TrimRight(upstream, "/") + "/echo")
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lat = append(lat, float64(time.Since(start).Microseconds())/1000)
	}
	out := map[string]float64{}
	sort.Float64s(lat)
	if len(lat) > 0 {
		out["p50"], out["p99"] = lat[len(lat)/2], lat[int(float64(len(lat)-1)*0.99)]
	}
	return out
}

func writeReports(c cfg, r *result) error {
	raw, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(c.out+".json", raw, 0o644); err != nil {
		return err
	}
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL: " + strings.Join(r.FailedChecks, "; ")
	}
	codes := make([]string, 0, len(r.Status))
	for k, v := range r.Status {
		codes = append(codes, fmt.Sprintf("%s: %d", k, v))
	}
	sort.Strings(codes)
	md := fmt.Sprintf(`# RelayOps soak test

**Result: %s**

| | |
|---|---|
| Started | %s |
| Duration | %.0f s |
| Gateway nodes | %d (%s) |
| Target / achieved rate | %d / %.1f requests per second |
| Requests | %d |
| Status codes | %s |
| Transport errors | %d |
| Failed requests | %.3f%% |
| Latency through the gateway p50 / p95 / p99 / p99.9 / max | %.1f / %.1f / %.1f / %.1f / %.1f ms |
| Upstream called directly p50 / p99 (baseline) | %.1f / %.1f ms |
| Revisions published / canaries / promotions | %d / %d / %d |
| Requests served by a canary | %d |
| Final revision, fleet converged | rev_%d, %v |
| Failed configuration changes | %d |

Latency is measured end to end by the load generator on the same machine as the
gateway, the mock upstream and PostgreSQL.
`, verdict, r.Started.Format(time.RFC3339), r.DurationSeconds, len(r.Gateways), strings.Join(r.Gateways, ", "),
		r.TargetRPS, r.AchievedRPS, r.Requests, strings.Join(codes, ", "), r.TransportErrors, r.ErrorRatePct,
		r.LatencyMS["p50"], r.LatencyMS["p95"], r.LatencyMS["p99"], r.LatencyMS["p99.9"], r.LatencyMS["max"],
		r.UpstreamDirectMS["p50"], r.UpstreamDirectMS["p99"],
		r.Publishes, r.Canaries, r.Promotions, r.CanaryServed, r.FinalRevision, r.FleetConverged, len(r.ChurnFailures))
	if len(r.SampleErrors) > 0 {
		md += "\n## Sample errors\n\n```\n" + strings.Join(r.SampleErrors, "\n") + "\n```\n"
	}
	if len(r.ChurnFailures) > 0 {
		md += "\n## Failed configuration changes\n\n```\n" + strings.Join(r.ChurnFailures, "\n") + "\n```\n"
	}
	return os.WriteFile(c.out+".md", []byte(md), 0o644)
}
