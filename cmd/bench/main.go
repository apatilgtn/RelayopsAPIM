// Command bench measures gateway capacity: latency at fixed open-loop request
// rates, stepping up until the error rate or p99 crosses a limit, then the
// maximum closed-loop throughput.
//
// Open-loop latency is measured from each request's scheduled send time, so a
// stalled gateway shows up as latency instead of silently lowering the rate
// (coordinated omission).
//
//	go run ./cmd/bench -url http://localhost:8080/bench/get -rates 500,1000,2000,4000 -step 20s
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type stepResult struct {
	Mode         string  `json:"mode"` // open or closed
	TargetRPS    int     `json:"target_rps,omitempty"`
	Concurrency  int     `json:"concurrency,omitempty"`
	AchievedRPS  float64 `json:"achieved_rps"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	Dropped      int64   `json:"dropped"` // open loop: sends skipped because max in-flight was reached
	ErrorPercent float64 `json:"error_percent"`
	P50MS        float64 `json:"p50_ms"`
	P90MS        float64 `json:"p90_ms"`
	P99MS        float64 `json:"p99_ms"`
	P999MS       float64 `json:"p999_ms"`
	MaxMS        float64 `json:"max_ms"`
	Sustained    bool    `json:"sustained"`
	FirstError   string  `json:"first_error,omitempty"`
}

type report struct {
	Label       string       `json:"label"`
	URL         string       `json:"url"`
	StartedAt   time.Time    `json:"started_at"`
	Host        string       `json:"host"`
	CPUs        int          `json:"cpus"`
	GoVersion   string       `json:"go_version"`
	StepSeconds float64      `json:"step_seconds"`
	MaxP99MS    float64      `json:"max_p99_ms"`
	MaxErrorPct float64      `json:"max_error_percent"`
	Steps       []stepResult `json:"steps"`
	MaxRPS      float64      `json:"max_sustained_rps"`
	Note        string       `json:"note,omitempty"`
}

type config struct {
	url, key, label, out, note string
	rates                      []int
	step, warmup               time.Duration
	concurrency                []int
	maxInflight, workers       int
	maxP99                     float64
	maxErr                     float64
}

func main() {
	var c config
	var rates, conc string
	flag.StringVar(&c.url, "url", "http://localhost:8080/bench/get", "target URL")
	flag.StringVar(&c.key, "key", "", "API key sent as X-API-Key")
	flag.StringVar(&rates, "rates", "250,500,1000,2000,4000,8000", "open-loop request rates to step through")
	flag.StringVar(&conc, "concurrency", "64,256", "closed-loop worker counts for the maximum-throughput test (empty skips it)")
	flag.DurationVar(&c.step, "step", 20*time.Second, "measurement time per step")
	flag.DurationVar(&c.warmup, "warmup", 3*time.Second, "unmeasured traffic before each step")
	flag.IntVar(&c.workers, "workers", 512, "open-loop worker pool (also the connection pool size)")
	flag.IntVar(&c.maxInflight, "max-inflight", 20000, "open-loop cap on queued sends; overflow counts as an error")
	flag.Float64Var(&c.maxP99, "max-p99-ms", 50, "a step with a higher p99 is not sustained")
	flag.Float64Var(&c.maxErr, "max-error-pct", 0.1, "a step with a higher error percentage is not sustained")
	flag.StringVar(&c.label, "label", "", "scenario name for the report")
	flag.StringVar(&c.note, "note", "", "free-text note stored in the report")
	flag.StringVar(&c.out, "out", "", "write <out>.json (default: stdout only)")
	flag.Parse()
	c.rates = splitInts(rates)
	c.concurrency = splitInts(conc)

	host, _ := os.Hostname()
	rep := report{Label: c.label, URL: c.url, StartedAt: time.Now().UTC(), Host: host, CPUs: runtime.NumCPU(),
		GoVersion: runtime.Version(), StepSeconds: c.step.Seconds(), MaxP99MS: c.maxP99, MaxErrorPct: c.maxErr, Note: c.note}
	client := newClient(max(c.workers, slices.Max(append([]int{0}, c.concurrency...))))

	if err := probe(client, c); err != nil {
		log.Fatalf("target not ready: %v", err)
	}
	for _, rate := range c.rates {
		openLoop(client, c, rate, c.warmup, false)
		r := openLoop(client, c, rate, c.step, true)
		rep.Steps = append(rep.Steps, r)
		log.Printf("open  %6d rps: achieved %8.1f  p50 %6.2f  p99 %7.2f  p99.9 %7.2f ms  errors %.3f%%  dropped %d",
			rate, r.AchievedRPS, r.P50MS, r.P99MS, r.P999MS, r.ErrorPercent, r.Dropped)
		if r.FirstError != "" {
			log.Printf("      first error: %s", r.FirstError)
		}
		if r.Sustained && r.AchievedRPS > rep.MaxRPS {
			rep.MaxRPS = r.AchievedRPS
		}
		if !r.Sustained {
			break
		}
	}
	for _, n := range c.concurrency {
		closedLoop(client, c, n, c.warmup)
		r := closedLoop(client, c, n, c.step)
		rep.Steps = append(rep.Steps, r)
		log.Printf("closed %5d workers: %8.1f rps  p50 %6.2f  p99 %7.2f ms  errors %.3f%%",
			n, r.AchievedRPS, r.P50MS, r.P99MS, r.ErrorPercent)
	}
	log.Printf("max sustained open-loop rate: %.0f rps (p99 <= %.0f ms, errors <= %.2f%%)", rep.MaxRPS, c.maxP99, c.maxErr)
	if c.out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(c.out+".json", b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func newClient(maxConns int) *http.Client {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func probe(client *http.Client, c config) error {
	ok, _ := send(client, c)
	if !ok {
		return fmt.Errorf("GET %s did not return 2xx", c.url)
	}
	return nil
}

// send issues one request and reports whether it returned 2xx.
func send(client *http.Client, c config) (bool, error) {
	req, _ := http.NewRequest(http.MethodGet, c.url, nil)
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

type recorder struct {
	mu       sync.Mutex
	lat      []time.Duration
	errs     atomic.Int64
	dropped  atomic.Int64
	firstErr atomic.Value
}

func (r *recorder) noteErr(err error) { r.firstErr.CompareAndSwap(nil, err.Error()) }

func (r *recorder) add(d time.Duration, ok bool) {
	if !ok {
		r.errs.Add(1)
	}
	r.mu.Lock()
	r.lat = append(r.lat, d)
	r.mu.Unlock()
}

func (r *recorder) result(elapsed time.Duration, c config) stepResult {
	sort.Slice(r.lat, func(i, j int) bool { return r.lat[i] < r.lat[j] })
	n := int64(len(r.lat))
	res := stepResult{Requests: n, Errors: r.errs.Load(), Dropped: r.dropped.Load()}
	if e, ok := r.firstErr.Load().(string); ok {
		res.FirstError = e
	}
	if n > 0 {
		res.AchievedRPS = float64(n) / elapsed.Seconds()
		res.ErrorPercent = 100 * float64(res.Errors+res.Dropped) / float64(n+res.Dropped)
		res.P50MS, res.P90MS, res.P99MS, res.P999MS = pct(r.lat, 0.50), pct(r.lat, 0.90), pct(r.lat, 0.99), pct(r.lat, 0.999)
		res.MaxMS = ms(r.lat[n-1])
	}
	res.Sustained = n > 0 && res.P99MS <= c.maxP99 && res.ErrorPercent <= c.maxErr
	return res
}

// openLoop sends at a fixed rate for d. A fixed pool of workers takes send
// times from a schedule, so connections are reused; when every worker is busy
// the schedule queues and that wait counts as latency. With measure false it
// only warms up.
func openLoop(client *http.Client, c config, rate int, d time.Duration, measure bool) stepResult {
	total := int(float64(rate) * d.Seconds())
	rec := &recorder{lat: make([]time.Duration, 0, total+16)}
	queue := make(chan time.Time, c.maxInflight)
	var wg sync.WaitGroup
	for w := 0; w < c.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for due := range queue {
				ok, err := send(client, c)
				rec.add(time.Since(due), ok)
				if err != nil {
					rec.noteErr(err)
				}
			}
		}()
	}
	interval := time.Second / time.Duration(rate)
	start := time.Now()
	// Release every send that is due on each wake-up; timer granularity then
	// only batches sends, it does not lower the rate.
	for i := 0; i < total; {
		now := time.Now()
		for ; i < total; i++ {
			due := start.Add(time.Duration(i) * interval)
			if due.After(now) {
				break
			}
			select {
			case queue <- due:
			default:
				rec.dropped.Add(1)
			}
		}
		time.Sleep(200 * time.Microsecond)
	}
	close(queue)
	wg.Wait()
	if !measure {
		return stepResult{}
	}
	res := rec.result(time.Since(start), c)
	res.Mode, res.TargetRPS = "open", rate
	return res
}

// closedLoop runs n workers that send back-to-back for d.
func closedLoop(client *http.Client, c config, n int, d time.Duration) stepResult {
	rec := &recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				t := time.Now()
				ok, err := send(client, c)
				rec.add(time.Since(t), ok)
				if err != nil {
					rec.noteErr(err)
				}
			}
		}()
	}
	wg.Wait()
	res := rec.result(time.Since(start), c)
	res.Mode, res.Concurrency = "closed", n
	return res
}

func pct(s []time.Duration, p float64) float64 {
	i := int(float64(len(s)-1) * p)
	return ms(s[i])
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func splitInts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}
