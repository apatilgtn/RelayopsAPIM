// Command mockupstream is a tiny backend for exercising the gateway locally.
//
//	GET  /anything...        echo request as JSON
//	GET  /delay/{ms}         respond after a delay
//	GET  /status/{code}      respond with the given status code
//	GET  /stream             Server-Sent-Events tick stream (10 events)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

func main() {
	addr := flag.String("addr", ":7070", "listen address")
	name := flag.String("name", "mock-upstream", "service name echoed in responses")
	jitter := flag.Int("jitter-ms", 25, "maximum random latency added to default responses (0 disables)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /delay/{ms}", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.PathValue("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		echo(w, r, *name, http.StatusOK)
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil || code < 100 || code > 599 {
			code = http.StatusBadRequest
		}
		echo(w, r, *name, code)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "" {
			req.Model = "gpt-4o"
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			f, ok := w.(http.Flusher)
			if ok {
				f.Flush()
			}
			chunks := []string{"Hello", " from", " RelayOps", " AI", " Gateway!"}
			for _, chunk := range chunks {
				delta := map[string]any{
					"id":      "chatcmpl-mock",
					"object":  "chat.completion.chunk",
					"model":   req.Model,
					"choices": []any{map[string]any{"delta": map[string]string{"content": chunk}}},
				}
				b, _ := json.Marshal(delta)
				fmt.Fprintf(w, "data: %s\n\n", string(b))
				if ok {
					f.Flush()
				}
				time.Sleep(30 * time.Millisecond)
			}
			// Final usage chunk with provider_reported token accounting
			finalChunk := map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion.chunk",
				"model":   req.Model,
				"choices": []any{},
				"usage": map[string]int{
					"prompt_tokens":     18,
					"completion_tokens": 12,
					"total_tokens":      30,
				},
			}
			b, _ := json.Marshal(finalChunk)
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if ok {
				f.Flush()
			}
			return
		}

		// Non-streaming JSON response
		resp := map[string]any{
			"id":      "chatcmpl-mock-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Hello! I am a simulated response from " + req.Model + " proxied through the RelayOps AI Gateway.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     24,
				"completion_tokens": 36,
				"total_tokens":      60,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(w, "data: {\"tick\":%d,\"at\":%q}\n\n", i, time.Now().Format(time.RFC3339Nano))
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// a little natural latency jitter makes the dashboard interesting
		if *jitter > 0 {
			time.Sleep(time.Duration(rand.Intn(*jitter)) * time.Millisecond)
		}
		echo(w, r, *name, http.StatusOK)
	})
	log.Printf("%s listening on %s", *name, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func echo(w http.ResponseWriter, r *http.Request, name string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service": name, "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
		"headers": r.Header, "host": r.Host, "status": status,
	})
}
