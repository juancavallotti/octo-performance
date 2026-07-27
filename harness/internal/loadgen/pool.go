// Package loadgen drives the load generator and parses what it produced.
//
// The pool sizing below is the part that matters most, and it is why this logic lives
// in Go rather than inside a k6 script. See docs/LEARNINGS.md (L2): poolFor() used to
// live in lab/k6/lib/options.js, computed silently at script start, and its output
// appeared in no result file. The one number that discriminates a healthy run from a
// worthless one was not merely unpublished — it could not be recovered afterwards.
package loadgen

import (
	"fmt"
	"math"
	"time"
)

// Pool is the virtual-user allocation for one load pass, and what actually happened
// to it.
type Pool struct {
	// PreAllocatedVUs is what Little's law says the offered rate needs.
	PreAllocatedVUs int
	// MaxVUs is the ceiling the generator may grow to.
	MaxVUs int
	// ObservedMaxVUs is how far it actually grew, read back from the run.
	ObservedMaxVUs int
	// Cap is the hard limit configured for the campaign, if any.
	Cap int
}

// Growth is how far past its allocation the generator had to scale.
//
// 1.0 means it stayed within the allocation. In the recorded evidence a healthy run
// sits at 1.0 (1,600 of 1,600) and the collapsed run reaches 4.4 (7,113 of 1,600) —
// the extra goroutines competing with the subject for the same cores.
func (p Pool) Growth() float64 {
	if p.PreAllocatedVUs <= 0 {
		return 0
	}
	return float64(p.ObservedMaxVUs) / float64(p.PreAllocatedVUs)
}

// SizePool computes the VU allocation for an offered rate.
//
// Little's law: concurrency = rate x latency. The headroom multiplier covers the tail,
// because sizing from the median leaves the pool starved exactly when latency spikes —
// and a starved pool is what turns a slow moment into a collapsed run.
//
// The floor keeps very low rates from allocating a pool too small to absorb any jitter
// at all.
func SizePool(rate int, expectedLatency time.Duration, hardCap int) Pool {
	const (
		headroom  = 4  // multiple of the Little's-law figure to pre-allocate
		floorVUs  = 20 // never allocate less than this
		maxFactor = 10 // how far above the allocation growth is permitted
	)

	pre := floorVUs
	if rate > 0 && expectedLatency > 0 {
		need := math.Ceil(float64(rate) * expectedLatency.Seconds() * headroom)
		if int(need) > pre {
			pre = int(need)
		}
	}

	p := Pool{PreAllocatedVUs: pre, MaxVUs: pre * maxFactor, Cap: hardCap}
	if hardCap > 0 {
		if p.PreAllocatedVUs > hardCap {
			p.PreAllocatedVUs = hardCap
		}
		if p.MaxVUs > hardCap {
			p.MaxVUs = hardCap
		}
	}
	return p
}

func (p Pool) String() string {
	return fmt.Sprintf("pre=%d max=%d observed=%d growth=%.2fx",
		p.PreAllocatedVUs, p.MaxVUs, p.ObservedMaxVUs, p.Growth())
}
