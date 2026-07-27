// Package gate decides whether a measured cell is allowed to become a number.
//
// Validity here is a value, not a rendering. The previous harness printed a validity
// table in each report and then printed the numbers anyway, and the index that anyone
// actually read stripped the warning out entirely — 61 rows of point estimates drawn
// from runs that were, without exception, saturated. Nothing structural stopped it.
//
// So a [Gate] is a pure function from [Evidence] to findings. It performs no I/O, it
// never mutates a measurement, and it never aborts a run. [Evaluate] folds the findings
// into a [Verdict] that travels with the cell, and no aggregate anywhere may include a
// cell whose verdict is Invalid.
//
// # Observe mode
//
// Nobody yet knows the right runner-CPU ceiling or pool-growth ratio, because the old
// lab never measured them. Every gate therefore records its evidence unconditionally
// from the first campaign, and [Config.ObserveOnly] caps escalation at Suspect so the
// thresholds can be calibrated from the observed distribution rather than guessed. A
// gate suite that marks everything invalid gets ignored within a week, which is exactly
// how the warnings-in-a-table approach failed.
//
// # Invalid does not mean aborted
//
// An invalid cell is excluded from the headline, kept on disk, and shown in the
// validity ledger. A badly chosen threshold must cost a re-read, never an afternoon on
// hardware you are paying for.
package gate

import (
	"fmt"
	"sort"
)

// Level is how much a finding costs the cell.
type Level int

const (
	// Valid: nothing found.
	Valid Level = iota
	// Suspect: worth reading before believing, but not excluded.
	Suspect
	// Invalid: excluded from every aggregate.
	Invalid
)

func (l Level) String() string {
	switch l {
	case Valid:
		return "valid"
	case Suspect:
		return "suspect"
	case Invalid:
		return "invalid"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

// MarshalText makes the level readable in verdict.json.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// UnmarshalText reads a level back.
func (l *Level) UnmarshalText(b []byte) error {
	switch string(b) {
	case "valid":
		*l = Valid
	case "suspect":
		*l = Suspect
	case "invalid":
		*l = Invalid
	default:
		return fmt.Errorf("gate: unknown level %q", b)
	}
	return nil
}

// Finding is one gate's observation about one cell.
//
// Evidence is machine-readable on purpose: the calibration section of the report reads
// it across every cell in a campaign to show what the observed distribution of each
// threshold actually is.
type Finding struct {
	Gate     string         `json:"gate"`
	Level    Level          `json:"level"`
	Summary  string         `json:"summary"`
	Detail   string         `json:"detail,omitempty"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

// Verdict is the cell's validity.
type Verdict struct {
	Level    Level     `json:"level"`
	Findings []Finding `json:"findings,omitempty"`
}

// Excluded reports whether this cell may contribute to an aggregate.
func (v Verdict) Excluded() bool { return v.Level == Invalid }

// Reasons returns the summaries of findings at the verdict's level.
func (v Verdict) Reasons() []string {
	var out []string
	for _, f := range v.Findings {
		if f.Level == v.Level && f.Level != Valid {
			out = append(out, f.Summary)
		}
	}
	return out
}

func (v Verdict) String() string {
	if len(v.Findings) == 0 {
		return v.Level.String()
	}
	return fmt.Sprintf("%s: %v", v.Level, v.Reasons())
}

// Gate inspects one cell's evidence.
//
// Implementations must be pure. A gate that reached out to a process or a file could
// not be replayed against the frozen corpus, and replaying against real specimens is
// the only way anyone can tell a working gate from a plausible one.
type Gate interface {
	ID() string
	Check(Evidence) []Finding
}

// Evaluate runs every gate and folds the results. The worst level wins.
//
// ObserveOnly caps escalation at Suspect while still recording every finding, which is
// how the first campaigns calibrate thresholds without discarding their own data.
func Evaluate(gates []Gate, e Evidence, observeOnly bool) Verdict {
	var v Verdict
	for _, g := range gates {
		for _, f := range g.Check(e) {
			if observeOnly && f.Level > Suspect {
				f.Level = Suspect
				f.Detail = appendNote(f.Detail, "capped at suspect: gates are in observe mode")
			}
			v.Findings = append(v.Findings, f)
			if f.Level > v.Level {
				v.Level = f.Level
			}
		}
	}
	sort.SliceStable(v.Findings, func(i, j int) bool {
		if v.Findings[i].Level != v.Findings[j].Level {
			return v.Findings[i].Level > v.Findings[j].Level
		}
		return v.Findings[i].Gate < v.Findings[j].Gate
	})
	return v
}

func appendNote(detail, note string) string {
	if detail == "" {
		return note
	}
	return detail + "; " + note
}

// Default returns the gate suite a campaign runs.
func Default(cfg Config) []Gate {
	cfg = cfg.withDefaults()
	return []Gate{
		GeneratorSaturation{cfg},
		ErrorRate{cfg},
		RateGap{cfg},
		SteadyState{},
		Identity{},
		Fingerprint{},
		ClockDrift{cfg},
	}
}
