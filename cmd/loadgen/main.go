// Command loadgen drives traffic through the gateway so the live dashboard has
// something to show.
//
//	go run ./cmd/loadgen -url http://localhost:8080/demo/orders -rps 50 -key rk_xxx
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	urls := flag.String("url", "http://localhost:8080/demo/hello", "comma-separated target URLs (picked at random)")
	rps := flag.Int("rps", 20, "requests per second")
	dur := flag.Duration("d", 0, "duration (0 = forever)")
	key := flag.String("key", "", "API key sent as X-API-Key")
	bearer := flag.String("bearer", "", "bearer token sent in Authorization")
	flag.Parse()

	targets := strings.Split(*urls, ",")
	client := &http.Client{Timeout: 30 * time.Second}
	var ok, fail atomic.Int64
	codes := make(map[int]*atomic.Int64)
	for _, c := range []int{200, 401, 403, 404, 429, 500, 502, 504} {
		codes[c] = &atomic.Int64{}
	}

	go func() {
		for range time.Tick(2 * time.Second) {
			var parts []string
			for c, n := range codes {
				if v := n.Load(); v > 0 {
					parts = append(parts, fmt.Sprintf("%d=%d", c, v))
				}
			}
			log.Printf("sent ok=%d fail=%d  %s", ok.Load(), fail.Load(), strings.Join(parts, " "))
		}
	}()

	var deadline <-chan time.Time
	if *dur > 0 {
		deadline = time.After(*dur)
	}
	tick := time.NewTicker(time.Second / time.Duration(*rps))
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			log.Printf("done ok=%d fail=%d", ok.Load(), fail.Load())
			return
		case <-tick.C:
			go func() {
				req, _ := http.NewRequest(http.MethodGet, targets[rand.Intn(len(targets))], nil)
				if *key != "" {
					req.Header.Set("X-API-Key", *key)
				}
				if *bearer != "" {
					req.Header.Set("Authorization", "Bearer "+*bearer)
				}
				resp, err := client.Do(req)
				if err != nil {
					fail.Add(1)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				ok.Add(1)
				if c, found := codes[resp.StatusCode]; found {
					c.Add(1)
				}
			}()
		}
	}
}
