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
		if out.Verdict.Excluded() {
			excluded++
		}
		peers = append(peers, campaign.Peer(out))
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted: stopping at a cell boundary")
			break
		}
	}

	fmt.Printf("\n%d of %d cells completed", completed, len(p.Cells))
	if excluded > 0 {
		fmt.Printf(", %d excluded by a gate", excluded)
	}
	fmt.Printf("\nartifacts: %s\n", dir)
	if completed < len(p.Cells) {
		return fmt.Errorf("%d cells did not complete", len(p.Cells)-completed)
	}
	return nil
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
