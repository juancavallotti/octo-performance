package campaign

import (
	"math"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
)

func TestParseEpochReadsWhatDateActuallyPrints(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     time.Time
		ok       bool
	}{
		{"gnu nanoseconds", "1785200000.123456789", time.Unix(1785200000, 123456789), true},
		{"whole seconds", "1785200000", time.Unix(1785200000, 0), true},
		// busybox and some BSD dates print %N literally. Parsing "N" as nanoseconds
		// would be a silent, plausible-looking error; falling back to whole seconds is
		// a real measurement at lower resolution.
		{"unsupported %N", "1785200000.N", time.Unix(1785200000, 0), true},
		{"short fraction", "1785200000.12", time.Unix(1785200000, 0), true},
		{"not a stamp", "date: illegal option", time.Time{}, false},
		{"empty", "", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseEpoch(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOneMachineHasOneClock(t *testing.T) {
	// A gate that raises a finding about a discrepancy that cannot exist is a gate
	// people learn to skip, and then it is not there when it matters.
	local := exec.NewLocal()
	defer local.Close()
	r := New(Config{Hosts: Hosts{Runner: local, Subject: local}})
	c := r.clockFor(t.Context())

	if !c.Measured {
		t.Error("a colocated topology reported its clock as unmeasured")
	}
	if c.OffsetMs != 0 || c.DriftMs != 0 {
		t.Errorf("a single clock reported an offset: %+v", c)
	}
}

func TestDriftIsTheMovementBetweenTwoEstimates(t *testing.T) {
	start := gate.Clock{Measured: true, OffsetMs: 12.0, UncertaintyMs: 0.4}
	end := gate.Clock{Measured: true, OffsetMs: 12.6, UncertaintyMs: 1.1}

	got := driftBetween(start, end)
	if math.Abs(got.DriftMs-0.6) > 1e-9 {
		t.Errorf("drift = %v, want 0.6", got.DriftMs)
	}
	// The drift is only as trustworthy as the less certain endpoint.
	if got.UncertaintyMs != 1.1 {
		t.Errorf("uncertainty = %v, want the wider of the two", got.UncertaintyMs)
	}
	if got.OffsetMs != 12.0 {
		t.Errorf("the reported offset moved: %v", got.OffsetMs)
	}
}

func TestAnUnmeasuredEndpointMakesTheWholeThingUnmeasured(t *testing.T) {
	// Half a measurement is not a measurement. Reporting the start offset with no
	// drift would assert that the clock held, which is exactly what was not observed.
	for _, tc := range []struct{ start, end gate.Clock }{
		{gate.Clock{Measured: true, OffsetMs: 12}, gate.Clock{}},
		{gate.Clock{}, gate.Clock{Measured: true, OffsetMs: 12}},
		{gate.Clock{}, gate.Clock{}},
	} {
		if got := driftBetween(tc.start, tc.end); got.Measured {
			t.Errorf("driftBetween(%+v, %+v) reported a measurement", tc.start, tc.end)
		}
	}
}

func TestClockTextSaysWhenNothingWasMeasured(t *testing.T) {
	if got := clockText(gate.Clock{}); got != "unmeasured" {
		t.Errorf("clockText of an unmeasured clock = %q", got)
	}
	got := clockText(gate.Clock{Measured: true, OffsetMs: -3.25, UncertaintyMs: 0.4, DriftMs: 0.1})
	if got == "unmeasured" || got == "" {
		t.Errorf("clockText = %q", got)
	}
}
