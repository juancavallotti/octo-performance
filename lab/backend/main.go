// A deliberately boring backend for proxy-shaped benchmarks.
//
// a commercial platform's HTTP Proxy benchmark puts a Vert.x server behind the runtime that
// "adds a 70 milliseconds delay to emulate more realistic conditions and returns a
// payload of a given size". This is that server. It exists so scenario 005 measures
// the runtime's ability to hold concurrent in-flight requests rather than the
// runtime's ability to generate bytes.
//
// Two properties matter and both are deliberate:
//
//   - The delay is a sleep, not work. A backend that burned CPU would compete with
//     the runtime under test on a shared host and quietly become the bottleneck.
//   - Payloads are generated once at startup and served from memory. Allocating a
//     1 MB response per request would make this process, not the runtime, the thing
//     the benchmark measures.
//
// Usage: backend -addr :9090
//
//	GET /payload?size=1024&delay=70ms   JSON body of about `size` bytes
//	GET /health                          readiness, no delay
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	addr         = flag.String("addr", ":9090", "listen address")
	defaultSize  = flag.Int("size", 1024, "default payload size in bytes")
	defaultDelay = flag.Duration("delay", 70*time.Millisecond, "default response delay")
)

// Payloads are memoised by size: a benchmark sweeps a handful of sizes and then
// requests each of them millions of times.
var (
	mu     sync.RWMutex
	cached = map[int][]byte{}
)

func payload(size int) []byte {
	mu.RLock()
	if b, ok := cached[size]; ok {
		mu.RUnlock()
		return b
	}
	mu.RUnlock()

	// A realistic-ish record, padded to the requested size. The padding is a single
	// field so that a runtime parsing this JSON does bounded structural work
	// regardless of size — what grows is the bytes, which is the point.
	base := map[string]any{
		"id":        "acct-00000000",
		"status":    "ACTIVE",
		"region":    "us-east-1",
		"updatedAt": "2026-07-25T00:00:00Z",
		"pad":       "",
	}
	head, _ := json.Marshal(base)
	padLen := size - len(head)
	if padLen < 0 {
		padLen = 0
	}
	base["pad"] = strings.Repeat("x", padLen)
	b, _ := json.Marshal(base)

	mu.Lock()
	cached[size] = b
	mu.Unlock()
	return b
}

func main() {
	flag.Parse()

	// Warm the common sizes so the first request of a run is not the one that pays
	// for allocation.
	for _, s := range []int{1024, 102400, 1048576} {
		payload(s)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	mux.HandleFunc("/payload", func(w http.ResponseWriter, r *http.Request) {
		size := *defaultSize
		if v := r.URL.Query().Get("size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				size = n
			}
		}
		delay := *defaultDelay
		if v := r.URL.Query().Get("delay"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= 0 {
				delay = d
			}
		}

		if delay > 0 {
			// Honour cancellation: when the caller gives up, so should we, or a
			// saturated run leaves this process holding thousands of dead sleeps.
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}

		b := payload(size)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	})

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Generous: the whole point is holding many slow connections open.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("backend listening on %s (default size=%d delay=%s)", *addr, *defaultSize, *defaultDelay)
	log.Fatal(srv.ListenAndServe())
}
