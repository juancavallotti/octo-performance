package agent

import (
	"math"
	"testing"
)

func TestCPURateQuantumIsOneTickPerInterval(t *testing.T) {
	// The number that decides whether a CPU series is allowed to reject a measured
	// window. Getting it wrong in the optimistic direction — claiming a finer
	// resolution than the counter has — restores exactly the intermittent failure it
	// exists to prevent.
	for _, tc := range []struct {
		name     string
		tick     int
		interval string
		want     float64
	}{
		{"100Hz sampled every 200ms", 100, "200ms", 0.05},
		{"100Hz sampled every second", 100, "1s", 0.01},
		{"250Hz sampled every second", 250, "1s", 0.004},
		{"unknown tick rate", 0, "1s", 0},
		{"unparseable interval", 100, "", 0},
		{"zero interval", 100, "0s", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Collected{Static: Static{ClockTick: tc.tick}, Interval: tc.interval}
			if got := c.CPURateQuantum(); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("CPURateQuantum() = %v, want %v", got, tc.want)
			}
		})
	}
}
