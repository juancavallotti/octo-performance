package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/agent"
	"github.com/juancavallotti/octo-performance/harness/internal/campaign"
	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/loadgen"
	"github.com/juancavallotti/octo-performance/harness/internal/plan"
	"github.com/juancavallotti/octo-performance/harness/internal/report"
	"github.com/juancavallotti/octo-performance/harness/internal/result"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	campaignPath := fs.String("campaign", "", "path to the campaign spec")
	scenariosDir := fs.String("scenarios", "scenarios", "directory holding the scenarios")
	outDir := fs.String("out", "campaigns/out", "where to write the campaign directory")
	versionsDir := fs.String("versions", "", "directory holding the octo releases (default ~/.octo-versions)")
	subjectHost := fs.String("subject", "127.0.0.1", "address the runner reaches the subject on")
	workloadPort := fs.Int("port", 8080, "the workload port")
	adminPort := fs.Int("admin-port", 39999, "the runtime's admin port")
	k6Bin := fs.String("k6", "k6", "the load generator binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *campaignPath == "" {
		return errors.New("run needs --campaign")
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

	// The plan is written before anything runs. A campaign interrupted at cell three
	// of seventy must leave behind what it intended to do, not only what it managed.
	dir := filepath.Join(*outDir, fmt.Sprintf("%s-%s-%s",
		time.Now().Format("2006-01-02"), c.Name, p.Short()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := result.WriteJSON(filepath.Join(dir, "plan.json"), p); err != nil {
		return err
	}

	local := exec.NewLocal()
	r := campaign.New(campaign.Config{
		Hosts:         campaign.Hosts{Runner: local, Subject: local},
		Endpoints:     campaign.Endpoints{Host: *subjectHost, WorkloadPort: *workloadPort, AdminPort: *adminPort},
		Dir:           dir,
		ResolveBinary: campaign.DefaultResolver(*versionsDir),
		LoadGen:       loadgen.NewK6(local, *k6Bin),
		SubjectSource: agent.LocalSource(),
		RunnerSource:  agent.LocalSource(),
		Gates:         gate.Default(gateConfig(c.Gates)),
		ObserveOnly:   c.Gates.ObserveOnly,
		Cooldown:      c.Cooldown,
		Log:           func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})

	// Ctrl-C stops after the current cell rather than in the middle of one, so the
	// campaign resumes from a clean boundary instead of leaving a half-measured cell
	// that reads like a complete one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("%s — %d cells into %s\n\n", c.Name, len(p.Cells), dir)

	var peers []gate.Peer
	var cells []*result.Cell
	var completed, excluded int
	for _, cell := range p.Cells {
		out, err := r.RunCell(ctx, cell, peers)
		if err != nil {
			// One cell failing is a finding about that cell. Abandoning the other
			// sixty-nine because of it is a decision the operator makes, not one
			// the harness makes for them.
			fmt.Fprintf(os.Stderr, "cell %s failed: %v\n", cell.ID, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		completed++
		cells = append(cells, out)
		if out.Verdict.Excluded() {
			excluded++
		}
		peers = append(peers, campaign.Peer(out))
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted: stopping at a cell boundary")
			break
		}
	}

	// The roll-up and the report are written from whatever completed. A campaign cut
	// short still answers what it managed to measure, and the ledger says how much
	// that was — which is more than the old lab could do for a run that finished.
	rolled := result.Rollup(rollupInput(c, p, cells), cells)
	if err := result.WriteJSON(filepath.Join(dir, "campaign.json"), rolled); err != nil {
		return err
	}
	html, err := report.Render(rolled)
	if err != nil {
		return err
	}
	reportPath := filepath.Join(dir, "report.html")
	if err := os.WriteFile(reportPath, html, 0o644); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}

	fmt.Printf("\n%s\n\n", rolled.Verdict)
	fmt.Printf("%d of %d cells completed", completed, len(p.Cells))
	if excluded > 0 {
		fmt.Printf(", %d excluded by a gate", excluded)
	}
	fmt.Printf("\nreport: %s\n", reportPath)
	if completed < len(p.Cells) {
		return fmt.Errorf("%d cells did not complete", len(p.Cells)-completed)
	}
	return nil
}

// rollupInput carries what the cells cannot: the campaign's own declarations, and the
// topology fact that qualifies every number in the report.
func rollupInput(c *spec.Campaign, p *plan.Plan, cells []*result.Cell) result.RollupInput {
	in := result.RollupInput{
		Name:        c.Name,
		Question:    c.Question,
		PlanHash:    p.Hash,
		Reps:        c.Reps,
		Order:       string(c.Order),
		OrderReason: c.OrderReason,
		ObserveOnly: c.Gates.ObserveOnly,
		Notes:       c.Notes,
		Planned:     len(p.Cells),
		Seed:        uint64(c.Seed) + 1,
		Routes:      map[string]string{},
	}
	for i, a := range c.Arms {
		in.Arms = append(in.Arms, a.Name)
		if i == 0 {
			// The first declared arm is the baseline: a comparison needs something
			// to be a comparison against, and picking it by declaration order means
			// the spec says which rather than the renderer guessing.
			in.Baseline = a.Name
		}
	}
	for _, cell := range p.Cells {
		in.Routes[cell.ID.Scenario] = cell.Scenario.Route
	}
	// Colocation is the fact that qualifies everything else, so it is derived from
	// the topology rather than left for a reader to infer from the machine names.
	in.Colocated = c.Topology.Runner.Local() && c.Topology.Subject.Local()
	return in
}

// gateConfig maps the campaign's thresholds onto the gate suite's.
func gateConfig(g spec.Gates) gate.Config {
	return gate.Config{
		RunnerCPUCeilingPct:  g.RunnerCPUCeilingPct,
		PoolGrowthSuspect:    g.PoolGrowthSuspect,
		PoolGrowthInvalid:    g.PoolGrowthInvalid,
		PeerPoolRatioInvalid: g.PeerPoolRatioInvalid,
		MaxFailedRate:        g.MaxFailedRate,
		MaxDroppedIterations: g.MaxDroppedIterations,
		AchievedVsOfferedMin: g.AchievedVsOfferedMin,
		MaxClockDriftMs:      g.MaxClockDriftMs,
	}
}
