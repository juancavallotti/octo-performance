// Command perf drives a performance campaign.
//
// It runs on the runner VM and orchestrates everything: rendering each arm's config,
// starting and probing the subject over its agent, offering load, sampling three
// machines onto one timeline, gating every cell for validity, and emitting a single
// self-contained report.
//
// See docs/ARCHITECTURE.md for the design and docs/LEARNINGS.md for why it is shaped
// this way.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/juancavallotti/octo-performance/harness/internal/buildinfo"
	"github.com/juancavallotti/octo-performance/harness/internal/plan"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

const usage = `perf — the Octo performance lab harness

usage:
  perf plan     --campaign <file> [--scenarios <dir>]   show the execution order and what it will cost
  perf version                                          print the binary's identity

An eight-hour campaign should be reviewed as a plan, not discovered as a mistake.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "version":
		fmt.Println(buildinfo.Get())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "perf: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "perf:", err)
		os.Exit(1)
	}
}

func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	campaignPath := fs.String("campaign", "", "path to the campaign spec")
	scenariosDir := fs.String("scenarios", "scenarios", "directory holding the scenarios")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *campaignPath == "" {
		return fmt.Errorf("plan needs --campaign")
	}

	c, err := spec.LoadCampaign(*campaignPath)
	if err != nil {
		return err
	}

	scenarios := map[string]*spec.Scenario{}
	for _, id := range c.Scenarios {
		s, err := spec.LoadScenario(filepath.Join(*scenariosDir, id))
		if err != nil {
			return err
		}
		scenarios[id] = s
	}

	p, err := plan.Expand(c, scenarios)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n%s\n\n", c.Name, c.Question)
	fmt.Printf("plan %s — %d cells, %d scenarios x %d arms x %d reps, %s order\n",
		p.Short(), len(p.Cells), len(c.Scenarios), len(c.Arms), c.Reps, p.Order)
	if c.Order == spec.Blocked {
		fmt.Printf("  !! blocked order: %s\n", c.OrderReason)
	}
	if c.Gates.ObserveOnly {
		fmt.Printf("  gates are in observe mode: evidence recorded, nothing excluded\n")
	}
	fmt.Println()

	for _, id := range c.Scenarios {
		sc := scenarios[id]
		fmt.Printf("%s  %s\n", id, sc.Route)
		load := sc.Load.Merge(c.Load)
		if load.Calibrate {
			fmt.Printf("  rate: measured per campaign, at %.0f%% of the knee\n",
				load.CalibrateFraction*100)
		} else {
			fmt.Printf("  rate: %d req/s (fixed)\n", load.Rate)
		}
		fmt.Printf("  order: %s\n", p.Sequence(id))
		fmt.Println()
	}

	// Every arm must occupy every position equally, and saying so is cheaper than
	// asking a reader to verify it from the sequence.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "arm\tcells\tordinals")
	for _, arm := range c.Arms {
		var ords []string
		var n int
		for _, id := range c.Scenarios {
			for _, o := range p.ArmOrdinals(id, arm.Name) {
				ords = append(ords, fmt.Sprint(o))
				n++
			}
		}
		sort.Strings(ords)
		fmt.Fprintf(w, "%s\t%d\t%s\n", arm.Name, n, strings.Join(ords, " "))
	}
	w.Flush()

	fmt.Printf("\nestimated wall clock: %s (load + warm-up + cooldown only; excludes calibration and start-up)\n",
		estimate(c, scenarios))
	return nil
}

// estimate is deliberately a floor, and says so. Start-up, readiness, smoke passes and
// calibration all add to it, and a campaign that claims to be shorter than it is would
// be worse than one that admits it does not know.
func estimate(c *spec.Campaign, scenarios map[string]*spec.Scenario) string {
	var total float64
	for _, id := range c.Scenarios {
		load := scenarios[id].Load.Merge(c.Load)
		per := load.Duration.Seconds() + load.Warmup.Seconds() + c.Cooldown.Seconds()
		total += per * float64(c.Reps*len(c.Arms))
	}
	h := int(total) / 3600
	m := (int(total) % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("at least %dh %dm", h, m)
	}
	return fmt.Sprintf("at least %dm", m)
}
