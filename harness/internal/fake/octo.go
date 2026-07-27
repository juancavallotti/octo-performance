package fake

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Caps is what a fake build presents itself as offering.
type Caps struct {
	// Observability serves /healthz and /readyz. Absent before octo 0.5.0.
	Observability bool
	// Metrics serves /metrics. Requires Observability.
	Metrics bool
	// MetricsBlocks accepts --metrics-blocks.
	MetricsBlocks bool
}

// CapsPre050 is what 0.4.2 and 0.4.3 look like: a workload port and nothing else.
func CapsPre050() Caps { return Caps{} }

// CapsFull is what 0.5.0 and later look like.
func CapsFull() Caps { return Caps{Observability: true, Metrics: true, MetricsBlocks: true} }

// ParseCaps reads a comma-separated capability list, as the FAKEOCTO_CAPS environment
// variable carries it: "admin,metrics,blocks", or "none".
func ParseCaps(s string) Caps {
	var c Caps
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(part) {
		case "admin", "observability":
			c.Observability = true
		case "metrics":
			c.Observability, c.Metrics = true, true
		case "blocks", "metrics-blocks":
			c.Observability, c.Metrics, c.MetricsBlocks = true, true, true
		}
	}
	return c
}

func (c Caps) String() string {
	var parts []string
	if c.Observability {
		parts = append(parts, "admin")
	}
	if c.Metrics {
		parts = append(parts, "metrics")
	}
	if c.MetricsBlocks {
		parts = append(parts, "blocks")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// Config describes one fake runtime.
type Config struct {
	Version   string
	BuildDate time.Time
	Caps      Caps

	// Flow names the flow in the exposition, matching the scenario under test.
	Flow string

	// ColdStart is how long the runtime takes to answer /readyz. A runtime that is
	// ready the instant it starts would let a broken readiness probe pass.
	ColdStart time.Duration

	// BaseLatency is what one request costs with no contention.
	BaseLatency time.Duration
	// Jitter is added uniformly to BaseLatency.
	Jitter time.Duration

	// Capacity is how many requests can be in flight before latency starts to grow.
	// Past it, each additional in-flight request adds BaseLatency — the knee that
	// makes a capacity probe find something rather than scaling forever.
	Capacity int

	// ErrorRate is the share of requests answered 500, in [0,1). Fast failures look
	// like fast successes to a load generator that only reads latency, so a harness
	// that ignores the error rate needs something to fail against.
	ErrorRate float64

	// WorkloadAddr and AdminAddr are listen addresses. An empty port lets the kernel
	// choose, and Octo.Endpoints reports what it got.
	WorkloadAddr string
	AdminAddr    string
}

// DefaultConfig is a fast, well-behaved runtime with a plausible knee.
func DefaultConfig() Config {
	return Config{
		Version:      "0.6.0",
		BuildDate:    time.Date(2026, 7, 26, 20, 23, 46, 0, time.UTC),
		Caps:         CapsFull(),
		Flow:         "page",
		ColdStart:    75 * time.Millisecond,
		BaseLatency:  400 * time.Microsecond,
		Jitter:       200 * time.Microsecond,
		Capacity:     64,
		WorkloadAddr: "127.0.0.1:0",
		AdminAddr:    "127.0.0.1:0",
	}
}

// Octo is a running fake runtime.
type Octo struct {
	cfg Config

	workload net.Listener
	admin    net.Listener
	servers  []*http.Server

	startedAt time.Time
	ready     atomic.Bool

	inFlight atomic.Int64
	rng      *rand.Rand
	rngMu    sync.Mutex

	mu        sync.Mutex
	completed map[string]int64 // outcome -> count
	durations []float64        // seconds, for the histogram
	sumByOut  map[string]float64
}

// Start brings a fake runtime up. It returns as soon as the listeners are bound;
// readiness follows after ColdStart, exactly as a real start-up does.
func Start(cfg Config) (*Octo, error) {
	if cfg.Flow == "" {
		cfg.Flow = "page"
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = 64
	}

	o := &Octo{
		cfg:       cfg,
		startedAt: time.Now(),
		rng:       rand.New(rand.NewPCG(0x0c70, 0x8ea1)),
		completed: map[string]int64{},
		sumByOut:  map[string]float64{},
	}

	wl, err := net.Listen("tcp", addrOr(cfg.WorkloadAddr, "127.0.0.1:0"))
	if err != nil {
		return nil, fmt.Errorf("fake: binding workload port: %w", err)
	}
	o.workload = wl
	o.serve(wl, o.workloadMux())

	if cfg.Caps.Observability {
		ad, err := net.Listen("tcp", addrOr(cfg.AdminAddr, "127.0.0.1:0"))
		if err != nil {
			wl.Close()
			return nil, fmt.Errorf("fake: binding admin port: %w", err)
		}
		o.admin = ad
		o.serve(ad, o.adminMux())
	}

	go func() {
		time.Sleep(cfg.ColdStart)
		o.ready.Store(true)
	}()
	return o, nil
}

func addrOr(a, fallback string) string {
	if a == "" {
		return fallback
	}
	return a
}

func (o *Octo) serve(l net.Listener, h http.Handler) {
	s := &http.Server{Handler: h}
	o.servers = append(o.servers, s)
	go s.Serve(l) //nolint:errcheck // Serve always returns an error at shutdown
}

// WorkloadAddr is where the workload is listening.
func (o *Octo) WorkloadAddr() string { return o.workload.Addr().String() }

// AdminAddr is where the admin port is listening, empty when there is none.
func (o *Octo) AdminAddr() string {
	if o.admin == nil {
		return ""
	}
	return o.admin.Addr().String()
}

// Close stops the runtime.
func (o *Octo) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var errs []error
	for _, s := range o.servers {
		if err := s.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (o *Octo) workloadMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", o.handleWorkload)
	return mux
}

func (o *Octo) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !o.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "starting")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	if o.cfg.Caps.Metrics {
		mux.HandleFunc("/metrics", o.handleMetrics)
	}
	return mux
}

// handleWorkload answers one request, taking longer the more of them are in flight.
func (o *Octo) handleWorkload(w http.ResponseWriter, r *http.Request) {
	if !o.ready.Load() {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}

	n := o.inFlight.Add(1)
	defer o.inFlight.Add(-1)

	d := o.cfg.BaseLatency
	if o.cfg.Jitter > 0 {
		d += time.Duration(o.float64() * float64(o.cfg.Jitter))
	}
	// Past capacity every extra in-flight request costs another base latency. This
	// is the knee: throughput stops rising and latency starts to, which is what a
	// capacity probe is looking for.
	if over := n - int64(o.cfg.Capacity); over > 0 {
		d += time.Duration(over) * o.cfg.BaseLatency
	}

	start := time.Now()
	time.Sleep(d)
	elapsed := time.Since(start)

	outcome := "completed"
	if o.cfg.ErrorRate > 0 && o.float64() < o.cfg.ErrorRate {
		outcome = "failed"
	}

	o.record(outcome, elapsed.Seconds())

	if outcome == "failed" {
		http.Error(w, "flow failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<html><body>%s %s</body></html>", o.cfg.Flow, r.URL.Path)
}

func (o *Octo) float64() float64 {
	o.rngMu.Lock()
	defer o.rngMu.Unlock()
	return o.rng.Float64()
}

func (o *Octo) record(outcome string, seconds float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.completed[outcome]++
	o.sumByOut[outcome] += seconds
	// Bounded: enough to shape a histogram, not a transcript of the run.
	if len(o.durations) < 1<<16 {
		o.durations = append(o.durations, seconds)
	}
}

// buckets are Prometheus's defaults, which is what the runtime uses.
//
// The lowest edge is 5 ms and a healthy flow finishes in hundreds of microseconds, so
// essentially every observation lands in the first bucket. That is not a flaw in the
// fake — it is the reason promx refuses to interpolate a quantile below the first edge,
// and the fake has to reproduce it or that refusal would never be exercised.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func (o *Octo) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	o.mu.Lock()
	completed := map[string]int64{}
	sums := map[string]float64{}
	for k, v := range o.completed {
		completed[k] = v
	}
	for k, v := range o.sumByOut {
		sums[k] = v
	}
	durations := append([]float64(nil), o.durations...)
	o.mu.Unlock()

	ready := 0
	if o.ready.Load() {
		ready = 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# HELP octo_build_info Always 1; the labels carry which binary is running.\n")
	fmt.Fprintf(&b, "# TYPE octo_build_info gauge\n")
	fmt.Fprintf(&b, "octo_build_info{build_date=%q,services_module=\"standalone\",version=%q} 1\n",
		o.cfg.BuildDate.UTC().Format(time.RFC3339), o.cfg.Version)

	fmt.Fprintf(&b, "# HELP octo_ready 1 when the runtime is serving, 0 otherwise. The same signal /readyz answers.\n")
	fmt.Fprintf(&b, "# TYPE octo_ready gauge\n")
	fmt.Fprintf(&b, "octo_ready %d\n", ready)

	fmt.Fprintf(&b, "# HELP octo_flows Flows in the running configuration.\n")
	fmt.Fprintf(&b, "# TYPE octo_flows gauge\n")
	fmt.Fprintf(&b, "octo_flows 1\n")
	fmt.Fprintf(&b, "# HELP octo_connectors Connectors in the running configuration.\n")
	fmt.Fprintf(&b, "# TYPE octo_connectors gauge\n")
	fmt.Fprintf(&b, "octo_connectors 1\n")

	fmt.Fprintf(&b, "# HELP octo_flow_in_flight Messages currently being processed by a flow.\n")
	fmt.Fprintf(&b, "# TYPE octo_flow_in_flight gauge\n")
	fmt.Fprintf(&b, "octo_flow_in_flight{flow=%q} %d\n", o.cfg.Flow, o.inFlight.Load())

	fmt.Fprintf(&b, "# HELP octo_flow_messages_total Messages a flow finished with, by outcome.\n")
	fmt.Fprintf(&b, "# TYPE octo_flow_messages_total counter\n")
	for _, outcome := range sortedKeys(completed) {
		fmt.Fprintf(&b, "octo_flow_messages_total{flow=%q,outcome=%q} %d\n",
			o.cfg.Flow, outcome, completed[outcome])
	}

	fmt.Fprintf(&b, "# HELP octo_flow_duration_seconds How long a flow took to process a message, including its error path.\n")
	fmt.Fprintf(&b, "# TYPE octo_flow_duration_seconds histogram\n")
	for _, outcome := range sortedKeys(completed) {
		writeHistogram(&b, o.cfg.Flow, outcome, durations, completed[outcome], sums[outcome])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

// writeHistogram emits cumulative buckets, a sum and a count.
//
// observed is a bounded sample of the run's durations, so its bucket shape is scaled
// up to this outcome's true count. The invariants a consumer relies on are the ones
// worth preserving: buckets are non-decreasing, none exceeds the count, and the +Inf
// bucket equals the count.
func writeHistogram(b *strings.Builder, flow, outcome string, observed []float64, count int64, sum float64) {
	counts := make([]int64, len(buckets))
	for _, d := range observed {
		for i, edge := range buckets {
			if d <= edge {
				counts[i]++
			}
		}
	}
	scale := 1.0
	if len(observed) > 0 {
		scale = float64(count) / float64(len(observed))
	}
	for i, edge := range buckets {
		n := int64(math.Round(float64(counts[i]) * scale))
		if n > count {
			n = count
		}
		fmt.Fprintf(b, "octo_flow_duration_seconds_bucket{flow=%q,outcome=%q,le=%q} %d\n",
			flow, outcome, fmt.Sprintf("%g", edge), n)
	}
	fmt.Fprintf(b, "octo_flow_duration_seconds_bucket{flow=%q,outcome=%q,le=\"+Inf\"} %d\n", flow, outcome, count)
	fmt.Fprintf(b, "octo_flow_duration_seconds_sum{flow=%q,outcome=%q} %g\n", flow, outcome, sum)
	fmt.Fprintf(b, "octo_flow_duration_seconds_count{flow=%q,outcome=%q} %d\n", flow, outcome, count)
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
