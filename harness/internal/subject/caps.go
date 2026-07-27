package subject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
)

// Caps is what one artifact says it can do, established by asking it.
type Caps struct {
	// Binary is the path the artifact was probed at, on the subject host.
	Binary string `json:"binary"`
	// SHA256 identifies the artifact itself. Two arms with the same digest are the
	// same binary whatever their names say, which is how the old lab's 2.3x swing on
	// "different" runs was eventually recognised as one binary measured twice.
	SHA256 string `json:"sha256"`

	// Version is parsed from `octo version`. An artifact that cannot state its
	// version does not run: a result that cannot be attributed is not a result.
	Version   string    `json:"version"`
	BuildDate time.Time `json:"buildDate,omitzero"`

	// Flags is every long flag named anywhere in `octo run --help`.
	Flags []string `json:"flags"`

	// Observability reports an admin port serving /healthz, /readyz and, with
	// --metrics, /metrics. Absent before 0.5.0, which is exactly the comparison this
	// lab exists to make, so nothing may assume it.
	Observability bool `json:"observability"`
	// Metrics reports that Prometheus exposition can be turned on.
	Metrics bool `json:"metrics"`
	// MetricsBlocks reports per-block timings. Never on by default: watching a block
	// puts its measurement on the flow's own goroutine, which changes what is being
	// measured.
	MetricsBlocks bool `json:"metricsBlocks"`

	// Help and Version output are archived verbatim, so the inference above can be
	// re-checked against its source rather than believed.
	Help       string `json:"-"`
	HelpSHA256 string `json:"helpSha256"`
	VersionRaw string `json:"versionRaw"`

	ProbedAt time.Time `json:"probedAt"`
}

// Has reports whether the artifact names a flag.
func (c Caps) Has(flag string) bool {
	flag = strings.TrimPrefix(flag, "--")
	for _, f := range c.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

// Summary is a one-line description for logs and the report.
func (c Caps) Summary() string {
	feat := []string{}
	if c.Observability {
		feat = append(feat, "admin-port")
	}
	if c.Metrics {
		feat = append(feat, "metrics")
	}
	if len(feat) == 0 {
		feat = append(feat, "no admin port")
	}
	return fmt.Sprintf("%s (%s) — %s", c.Version, shortDigest(c.SHA256), strings.Join(feat, ", "))
}

func shortDigest(d string) string {
	if len(d) < 12 {
		return d
	}
	return d[:12]
}

// Prober asks artifacts what they accept, and remembers the answer.
//
// The cache is keyed by content digest rather than by path, so two arms pointing at
// the same file are probed once and two different files at the same path are never
// confused for each other.
type Prober struct {
	mu    sync.Mutex
	cache map[string]Caps
}

// NewProber returns an empty prober.
func NewProber() *Prober { return &Prober{cache: map[string]Caps{}} }

// Probe reads the artifact's digest, version and accepted flags.
func (p *Prober) Probe(ctx context.Context, r exec.Runner, binary string) (Caps, error) {
	digest, err := digestOf(ctx, r, binary)
	if err != nil {
		return Caps{}, err
	}

	p.mu.Lock()
	if c, ok := p.cache[digest]; ok {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := probe(ctx, r, binary, digest)
	if err != nil {
		return Caps{}, err
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]Caps{}
	}
	p.cache[digest] = c
	p.mu.Unlock()
	return c, nil
}

func probe(ctx context.Context, r exec.Runner, binary, digest string) (Caps, error) {
	c := Caps{Binary: binary, SHA256: digest, ProbedAt: time.Now()}

	// `run --help` is the artifact's own statement of which flags it accepts. Some
	// builds print help to stderr, so both streams count.
	help, err := r.Run(ctx, exec.Cmd{Path: binary, Args: []string{"run", "--help"}})
	if err != nil {
		return Caps{}, fmt.Errorf("subject: asking %s for its flags: %w", binary, err)
	}
	c.Help = string(help.Stdout) + string(help.Stderr)
	if strings.TrimSpace(c.Help) == "" {
		return Caps{}, fmt.Errorf("subject: %s printed no help, so nothing about it can be established", binary)
	}
	sum := sha256.Sum256([]byte(c.Help))
	c.HelpSHA256 = hex.EncodeToString(sum[:])
	c.Flags = parseFlags(c.Help)
	c.Observability = c.Has("observability") || c.Has("observability-addr")
	c.Metrics = c.Has("metrics")
	c.MetricsBlocks = c.Has("metrics-blocks")

	ver, err := r.Run(ctx, exec.Cmd{Path: binary, Args: []string{"version"}})
	if err != nil {
		return Caps{}, fmt.Errorf("subject: asking %s for its version: %w", binary, err)
	}
	c.VersionRaw = strings.TrimSpace(string(ver.Stdout) + string(ver.Stderr))
	c.Version, c.BuildDate = parseVersion(c.VersionRaw)
	if c.Version == "" {
		// The old lab published cells stamped vunknown-dev. A number nobody can
		// attribute to an artifact is not a measurement of anything.
		return Caps{}, fmt.Errorf(
			"subject: %s does not state a version (%q); an arm that cannot name its artifact does not run",
			binary, c.VersionRaw)
	}
	return c, nil
}

// digestOf hashes the artifact through the runner, so the same code identifies a local
// file and a file on a subject VM.
func digestOf(ctx context.Context, r exec.Runner, path string) (string, error) {
	rc, err := r.Get(ctx, path)
	if err != nil {
		return "", fmt.Errorf("subject: reading %s: %w", path, err)
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("subject: hashing %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// flagPattern matches a long flag as it appears in help text.
var flagPattern = regexp.MustCompile(`--([a-zA-Z][a-zA-Z0-9-]*)`)

// parseFlags collects every long flag named in help text.
//
// Whole tokens, never substrings. Searching the text for "--metrics" finds it inside
// "--metrics-blocks", so a build offering only per-block timings would be recorded as
// offering Prometheus exposition — and the run would then fail at start-up on a flag
// the artifact does not accept.
func parseFlags(help string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range flagPattern.FindAllStringSubmatch(help, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// versionPattern matches "octo 0.6.0" and "octo 0.4.3-dev.7e5d950.dirty".
var versionPattern = regexp.MustCompile(`(?m)\bocto\s+v?([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.\-+]*)`)

// buildDatePattern matches the "(built <RFC3339>)" suffix.
var buildDatePattern = regexp.MustCompile(`\(built\s+([0-9T:\-Z+.]+)\)`)

// parseVersion reads `octo version` output. An unrecognisable line yields an empty
// version, which the caller turns into a refusal to run.
func parseVersion(raw string) (string, time.Time) {
	var version string
	if m := versionPattern.FindStringSubmatch(raw); m != nil {
		version = m[1]
	}
	var built time.Time
	if m := buildDatePattern.FindStringSubmatch(raw); m != nil {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			built = t
		}
	}
	return version, built
}
