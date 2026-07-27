// Command fakeocto is an octo-alike for testing the harness without a runtime.
//
// It exists so the whole cell procedure — probe capabilities, assemble argv, start,
// await readiness, scrape identity, sample, load, stop, gate — runs in an ordinary
// `go test` in seconds. Everything it does, it does for real: it parses the flags octo
// parses, refuses the ones the build it is impersonating would refuse, binds ports,
// takes time to become ready, and exits on SIGTERM.
//
// It is configured through the environment rather than through flags, because its
// flags have to be octo's:
//
//	FAKEOCTO_VERSION    version to report, default 0.6.0
//	FAKEOCTO_CAPS       admin | metrics | blocks | none, default metrics
//	FAKEOCTO_ADDR       workload listen address, default 127.0.0.1:8080
//	FAKEOCTO_COLD_START how long until /readyz answers, default 75ms
//	FAKEOCTO_LATENCY    per-request service time, default 400µs
//	FAKEOCTO_CAPACITY   in-flight requests before latency grows, default 64
//	FAKEOCTO_ERROR_RATE share of requests answered 500, default 0
//
// It is never released. Only tests build it.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/fake"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakeocto:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := configFromEnv()

	cmd := "run"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "version":
		fmt.Printf("octo %s (built %s)\n", cfg.Version, cfg.BuildDate.UTC().Format(time.RFC3339))
		return nil
	case "run":
		return runServe(cfg, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func isFlag(s string) bool { return len(s) > 1 && s[0] == '-' }

// runServe parses the flags exactly as strictly as octo does.
//
// The strictness is the point. Passing --metrics to a build without it is a hard
// parse failure at start-up, not a flag that is ignored, and a harness that guesses
// capabilities from a version string discovers this by losing a cell. The fake has to
// fail the same way or that guard would never be tested.
func runServe(cfg fake.Config, args []string) error {
	var configPath string

	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--help", "-h":
			fmt.Print(helpText(cfg.Caps))
			return nil

		case "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("flag --config needs a value")
			}
			i++
			configPath = args[i]

		case "--watch":

		case "--observability-addr":
			if !cfg.Caps.Observability {
				return fmt.Errorf("unknown flag: %s", a)
			}
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s needs a value", a)
			}
			i++
			cfg.AdminAddr = normaliseAddr(args[i])

		case "--metrics":
			if !cfg.Caps.Metrics {
				return fmt.Errorf("unknown flag: %s", a)
			}

		case "--metrics-blocks":
			if !cfg.Caps.MetricsBlocks {
				return fmt.Errorf("unknown flag: %s", a)
			}
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s needs a value", a)
			}
			i++

		default:
			return fmt.Errorf("unknown flag: %s", a)
		}
	}

	if configPath == "" {
		return fmt.Errorf("--config is required")
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	o, err := fake.Start(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("octo %s serving %s", cfg.Version, o.WorkloadAddr())
	if a := o.AdminAddr(); a != "" {
		fmt.Printf(" admin %s", a)
	}
	fmt.Println()

	// Shut down on SIGTERM the way a real runtime does, so the harness's graceful
	// stop is exercised rather than its kill escalation.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch

	return o.Close()
}

// normaliseAddr turns octo's ":39999" into something a test can reach.
func normaliseAddr(a string) string {
	if len(a) > 0 && a[0] == ':' {
		return "127.0.0.1" + a
	}
	return a
}

func configFromEnv() fake.Config {
	cfg := fake.DefaultConfig()
	cfg.WorkloadAddr = "127.0.0.1:8080"
	cfg.AdminAddr = "127.0.0.1:39999"

	if v := os.Getenv("FAKEOCTO_VERSION"); v != "" {
		cfg.Version = v
	}
	if v := os.Getenv("FAKEOCTO_CAPS"); v != "" {
		cfg.Caps = fake.ParseCaps(v)
	}
	if v := os.Getenv("FAKEOCTO_ADDR"); v != "" {
		cfg.WorkloadAddr = v
	}
	if d, ok := envDuration("FAKEOCTO_COLD_START"); ok {
		cfg.ColdStart = d
	}
	if d, ok := envDuration("FAKEOCTO_LATENCY"); ok {
		cfg.BaseLatency = d
		cfg.Jitter = d / 2
	}
	if v := os.Getenv("FAKEOCTO_CAPACITY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Capacity = n
		}
	}
	if v := os.Getenv("FAKEOCTO_ERROR_RATE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.ErrorRate = f
		}
	}
	return cfg
}

func envDuration(key string) (time.Duration, bool) {
	v := os.Getenv(key)
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false
	}
	return d, true
}

// helpText mirrors the shape of octo's own help, including the property the harness
// reads it for: the observability section is absent from a build without one.
func helpText(caps fake.Caps) string {
	s := `octo — run and invoke octo integration flows

Usage:
  octo [run] --config <path> [--watch]                       Start connectors and flows (default)

Run flags:
  --config <path>   path to the runtime config (file or directory)
  --watch           reload the config when it changes
`
	if caps.Observability {
		s += `
Observability flags (octo run):
  --observability=false
                     do not serve probes at all (default: serve them)
  --observability-addr <addr>
                     admin listen address (default :39999)
`
	}
	if caps.Metrics {
		s += `
  --metrics          serve Prometheus metrics (default: off)
`
	}
	if caps.MetricsBlocks {
		s += `  --metrics-blocks <addrs>
                     comma-separated block addresses to report per-block timings for
`
	}
	if caps.Observability {
		s += `
    GET /healthz     200 while the process is alive
    GET /readyz      200 once every connector and flow started
`
	}
	if caps.Metrics {
		s += `    GET /metrics     Prometheus exposition, with --metrics
`
	}
	return s
}
