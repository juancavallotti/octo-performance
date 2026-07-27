package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// CellID identifies one measured repetition.
type CellID struct {
	Campaign string
	Scenario string
	Arm      string
	Rep      int
}

// Slug is the on-disk directory name for the cell.
func (id CellID) Slug() string {
	return fmt.Sprintf("%s__%s__rep%d", id.Scenario, sanitise(id.Arm), id.Rep)
}

func (id CellID) String() string {
	return fmt.Sprintf("%s/%s/rep%d", id.Scenario, id.Arm, id.Rep)
}

// sanitise makes an arm name safe for a path without losing its identity.
func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Cell is one execution: a scenario, an arm, a repetition, at a known position in the
// campaign's sequence.
type Cell struct {
	ID CellID
	// Ordinal is the position in the whole campaign, from zero. It is what the
	// order-effect check regresses against, so a delta that merely tracks position
	// can be identified as an artifact.
	Ordinal int
	// PositionInRep is the slot within this repetition's arm rotation. Recorded so a
	// reader can confirm the rotation actually balanced.
	PositionInRep int

	Scenario *spec.Scenario
	Arm      spec.Arm
	// Load is fully resolved: scenario defaults with campaign overrides applied.
	Load spec.Load
}

// Plan is the ordered execution list.
type Plan struct {
	Campaign string
	// Hash identifies this plan's content. It names the output directory and is what
	// a resumed run must match.
	Hash  string
	Order spec.Order
	Cells []Cell
}

// Expand turns a validated campaign into its ordered cells.
//
// scenarios must contain every scenario the campaign names; a missing one is an error
// rather than a skipped cell, because a campaign that silently measures less than it
// claimed is how the old results index came to list 34 rows against 40 directories.
func Expand(c *spec.Campaign, scenarios map[string]*spec.Scenario) (*Plan, error) {
	if c == nil {
		return nil, fmt.Errorf("plan: nil campaign")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	p := &Plan{Campaign: c.Name, Order: c.Order}
	ordinal := 0

	for _, id := range c.Scenarios {
		sc, ok := scenarios[id]
		if !ok {
			return nil, fmt.Errorf("plan: campaign names scenario %q but it was not loaded", id)
		}
		load := sc.Load.Merge(c.Load)
		// The resolved load is the first point at which the specification is whole,
		// so it is the first point at which it can be checked.
		if err := load.Validate(); err != nil {
			return nil, fmt.Errorf("plan: scenario %s load: %w", id, err)
		}

		for rep := 1; rep <= c.Reps; rep++ {
			for pos, armIdx := range armOrder(len(c.Arms), rep, c.Order, c.Seed) {
				arm := c.Arms[armIdx]
				p.Cells = append(p.Cells, Cell{
					ID: CellID{
						Campaign: c.Name,
						Scenario: id,
						Arm:      arm.Name,
						Rep:      rep,
					},
					Ordinal:       ordinal,
					PositionInRep: pos,
					Scenario:      sc,
					Arm:           arm,
					Load:          load,
				})
				ordinal++
			}
		}
	}

	p.Hash = hashPlan(c, p.Cells)
	return p, nil
}

// armOrder returns the arm indices to run for one repetition.
//
// Interleaved rotates by (rep-1)+seed so that over n repetitions each arm occupies each
// position exactly once. Blocked returns the declared order every time, which is only
// reachable with a stated reason.
func armOrder(n, rep int, order spec.Order, seed int64) []int {
	out := make([]int, n)
	if order == spec.Blocked {
		for i := range out {
			out[i] = i
		}
		return out
	}
	shift := (int64(rep-1) + seed) % int64(n)
	if shift < 0 {
		shift += int64(n)
	}
	for i := range out {
		out[i] = (i + int(shift)) % n
	}
	return out
}

// Remaining returns the cells not yet completed, in order. This is what makes an
// interrupted campaign resumable instead of leaving the half-finished directories the
// old lab accumulated with no way to tell them from valid ones.
func (p *Plan) Remaining(done map[CellID]bool) []Cell {
	out := make([]Cell, 0, len(p.Cells))
	for _, c := range p.Cells {
		if !done[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// Sequence renders the execution order compactly, e.g. "A B B A A B". It is what
// `perf plan` prints so an eight-hour campaign is reviewed rather than discovered.
func (p *Plan) Sequence(scenario string) string {
	var names []string
	for _, c := range p.Cells {
		if scenario == "" || c.ID.Scenario == scenario {
			names = append(names, c.ID.Arm)
		}
	}
	return strings.Join(names, " ")
}

// ArmOrdinals returns each arm's execution positions within one scenario, which is what
// the order-effect check consumes.
func (p *Plan) ArmOrdinals(scenario, arm string) []int {
	var out []int
	for _, c := range p.Cells {
		if c.ID.Scenario == scenario && c.ID.Arm == arm {
			out = append(out, c.Ordinal)
		}
	}
	return out
}

// hashPlan is a content hash over everything that decides what will be measured.
//
// Deliberately not a hash of the YAML text: reformatting the spec must not invalidate a
// resume, and a semantically identical spec must produce the same campaign directory.
func hashPlan(c *spec.Campaign, cells []Cell) string {
	h := sha256.New()
	w := func(format string, args ...any) { fmt.Fprintf(h, format, args...) }

	w("campaign=%s\norder=%s\nreps=%d\nseed=%d\n", c.Name, c.Order, c.Reps, c.Seed)
	for _, cell := range cells {
		w("cell=%s|%s|%s|%d|%s|%s\n",
			cell.ID.Scenario, cell.ID.Arm, cell.Arm.Binary.String(), cell.ID.Rep,
			cell.Arm.Config.Mode, knobString(cell.Arm.Config.Knobs))
		w("  load=%s|%s|rate=%d|calib=%v|vus=%d|dur=%s|warm=%s|cap=%d\n",
			cell.Load.Test, cell.Load.Model, cell.Load.Rate, cell.Load.Calibrate,
			cell.Load.VUs, cell.Load.Duration, cell.Load.Warmup, cell.Load.VUCap)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func knobString(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// Short is the first eight characters of the hash, used in the campaign directory name.
func (p *Plan) Short() string {
	if len(p.Hash) < 8 {
		return p.Hash
	}
	return p.Hash[:8]
}
