package subject

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures under testdata/help are the verbatim output of the four octo binaries
// this lab actually compares. They are the artifacts' own statements about what they
// accept, which is the only evidence this package is allowed to reason from.
//
// The interesting property of the set is that it straddles the change: 0.4.2 and 0.4.3
// have no admin port at all, 0.5.0 introduced one, and passing --metrics to either of
// the first two is a hard flag-parse failure rather than a flag that is ignored.

func helpFixture(t *testing.T, version string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "help", "octo-"+version+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCapabilitiesComeFromTheArtifactNotTheVersionString(t *testing.T) {
	for _, tc := range []struct {
		version                        string
		observability, metrics, blocks bool
	}{
		{"0.4.2", false, false, false},
		{"0.4.3", false, false, false},
		{"0.5.0", true, true, true},
		{"0.6.0", true, true, true},
	} {
		help := helpFixture(t, tc.version)
		flags := parseFlags(help)
		c := Caps{Flags: flags}

		if got := c.Has("observability-addr"); got != tc.observability {
			t.Errorf("%s: --observability-addr detected = %v, want %v", tc.version, got, tc.observability)
		}
		if got := c.Has("metrics"); got != tc.metrics {
			t.Errorf("%s: --metrics detected = %v, want %v", tc.version, got, tc.metrics)
		}
		if got := c.Has("metrics-blocks"); got != tc.blocks {
			t.Errorf("%s: --metrics-blocks detected = %v, want %v", tc.version, got, tc.blocks)
		}
		// Every build accepts these; if they are missing the parser is broken in a
		// way the feature flags above would not reveal.
		if !c.Has("config") || !c.Has("watch") {
			t.Errorf("%s: --config/--watch not found; parseFlags is not reading the help", tc.version)
		}
	}
}

func TestFlagDetectionMatchesWholeTokens(t *testing.T) {
	// "--metrics" is a substring of "--metrics-blocks". A build offering only
	// per-block timings must not be recorded as offering Prometheus exposition,
	// because acting on that would pass a flag the artifact rejects at start-up.
	c := Caps{Flags: parseFlags("  --metrics-blocks <addrs>   per-block timings\n")}
	if c.Has("metrics") {
		t.Error("--metrics inferred from --metrics-blocks by substring")
	}
	if !c.Has("metrics-blocks") {
		t.Error("--metrics-blocks not detected")
	}
}

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  string
		dated bool
	}{
		{"octo 0.4.2", "0.4.2", false},
		{"octo 0.6.0 (built 2026-07-26T20:23:46Z)", "0.6.0", true},
		{"octo 0.4.3 (built 2026-07-25T16:36:22Z)", "0.4.3", true},
		{"octo v1.2.3", "1.2.3", false},
		{"octo 0.4.3-dev.7e5d950.dirty", "0.4.3-dev.7e5d950.dirty", false},
		{"", "", false},
		{"octo dev", "", false},
		{"unknown", "", false},
	} {
		got, built := parseVersion(tc.raw)
		if got != tc.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if built.IsZero() == tc.dated {
			t.Errorf("parseVersion(%q) build date present = %v, want %v", tc.raw, !built.IsZero(), tc.dated)
		}
	}
}

func TestParseVersionAgainstTheRealBinaries(t *testing.T) {
	for _, v := range []string{"0.4.2", "0.4.3", "0.5.0", "0.6.0"} {
		b, err := os.ReadFile(filepath.Join("testdata", "help", "octo-"+v+".version.txt"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := parseVersion(strings.TrimSpace(string(b)))
		if got != v {
			t.Errorf("octo %s reports %q, parsed as %q", v, strings.TrimSpace(string(b)), got)
		}
	}
}

func TestBuildDateIsParsedWhenOffered(t *testing.T) {
	_, built := parseVersion("octo 0.6.0 (built 2026-07-26T20:23:46Z)")
	want := time.Date(2026, 7, 26, 20, 23, 46, 0, time.UTC)
	if !built.Equal(want) {
		t.Errorf("build date = %v, want %v", built, want)
	}
}

func TestArgvWithholdsFlagsTheArtifactDoesNotAccept(t *testing.T) {
	// Passing --metrics to 0.4.3 is a flag-parse failure at start-up, not a flag
	// that is ignored. The old harness guarded this with a shell function; here the
	// guard is in the one place argv is assembled.
	old := Caps{Binary: "/opt/octo-0.4.3", Version: "0.4.3", Flags: parseFlags(helpFixture(t, "0.4.3"))}
	old.Observability, old.Metrics = old.Has("observability-addr"), old.Has("metrics")

	args, withheld := Argv(StartRequest{
		Caps: old, ConfigPath: "/tmp/cell/config.yaml", AdminAddr: ":39999", Metrics: true,
	})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--metrics") {
		t.Errorf("argv = %q; --metrics reached a build that cannot parse it", joined)
	}
	if strings.Contains(joined, "--observability-addr") {
		t.Errorf("argv = %q; --observability-addr reached a build without an admin port", joined)
	}
	if want := "run --config /tmp/cell/config.yaml"; joined != want {
		t.Errorf("argv = %q, want %q", joined, want)
	}
	// Withheld is recorded rather than silent: an arm that could not serve metrics
	// is not comparable to one that did, and the report has to be able to say so.
	if len(withheld) != 2 {
		t.Errorf("withheld = %v, want both flags recorded", withheld)
	}
}

func TestArgvPassesFlagsTheArtifactAccepts(t *testing.T) {
	newer := Caps{Binary: "/opt/octo-0.6.0", Version: "0.6.0", Flags: parseFlags(helpFixture(t, "0.6.0"))}
	newer.Observability, newer.Metrics = newer.Has("observability-addr"), newer.Has("metrics")
	newer.MetricsBlocks = newer.Has("metrics-blocks")

	args, withheld := Argv(StartRequest{
		Caps:       newer,
		ConfigPath: "/tmp/cell/config.yaml",
		AdminAddr:  ":39999",
		Metrics:    true,
		ExtraFlags: []string{"--watch"},
	})

	want := "run --config /tmp/cell/config.yaml --observability-addr :39999 --metrics --watch"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if len(withheld) != 0 {
		t.Errorf("withheld = %v on a build that accepts everything asked for", withheld)
	}
}

func TestArgvKeepsPerBlockTimingsOffUnlessAsked(t *testing.T) {
	// Watching a block puts its timing on the flow's own goroutine, so it changes
	// what is being measured. It is never a default.
	newer := Caps{Binary: "/opt/octo", Version: "0.6.0", Metrics: true, MetricsBlocks: true, Observability: true}
	args, _ := Argv(StartRequest{Caps: newer, ConfigPath: "/c.yaml", Metrics: true})
	if strings.Contains(strings.Join(args, " "), "--metrics-blocks") {
		t.Error("per-block timings were enabled without being asked for")
	}
}

func TestCapsSummaryNamesTheArtifactAndWhatItOffers(t *testing.T) {
	c := Caps{Version: "0.6.0", SHA256: "abcdef0123456789", Observability: true, Metrics: true}
	got := c.Summary()
	for _, want := range []string{"0.6.0", "abcdef012345", "admin-port", "metrics"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q, missing %q", got, want)
		}
	}
	if got := (Caps{Version: "0.4.3"}).Summary(); !strings.Contains(got, "no admin port") {
		t.Errorf("Summary() = %q, want it to state the absence", got)
	}
}
