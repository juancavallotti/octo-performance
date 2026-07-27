package campaign

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// DepsHostPlaceholder is what a scenario writes where a dependency's address goes.
//
// It exists because of what went wrong without it. Scenarios 003 and 005 reached their
// dependencies through host.docker.internal, a name that resolves only from inside a
// container on a developer laptop, and METHODOLOGY.md calls the resulting numbers "not
// merely indicative — misleading". The address of a dependency is a property of the
// topology, not of the scenario, and writing it into the scenario is what welded the two
// together.
const DepsHostPlaceholder = "${DEPS_HOST}"

// Deps is a scenario's external dependency, standing.
type Deps struct {
	Scenario string            `json:"scenario"`
	Host     string            `json:"host"`
	Env      map[string]string `json:"env,omitempty"`
	Note     string            `json:"note,omitempty"`

	teardown string
	dir      string
	runner   exec.Runner
	log      func(string, ...any)
}

// StartDeps stands up whatever a scenario needs, once, before its first cell.
//
// Once per scenario and not once per cell: scenario 003's Postgres is the reason. A cold
// database would put schema creation and connection establishment inside the measured
// window, and the scenario would report the cost of connecting rather than the cost of
// querying. The setup scripts are written to be idempotent for the same reason.
func (r *Runner) StartDeps(ctx context.Context, sc *spec.Scenario) (*Deps, error) {
	if !sc.Deps.Declared() {
		return nil, nil
	}
	host := r.cfg.Endpoints.DepsHost
	if host == "" {
		host = "127.0.0.1"
	}

	d := &Deps{
		Scenario: sc.ID,
		Host:     host,
		Env:      resolveDepsEnv(sc.Deps.Env, host),
		Note:     strings.TrimSpace(sc.Deps.Note),
		teardown: sc.Deps.Teardown,
		dir:      sc.Dir,
		runner:   r.depsRunner(),
		log:      r.cfg.Log,
	}

	r.deps[sc.ID] = d
	d.log("deps for %s: %s on %s", sc.ID, filepath.Base(sc.Deps.Setup), d.runner.Name())
	res, err := d.runner.Run(ctx, exec.Cmd{
		Path: filepath.Join(sc.Dir, sc.Deps.Setup),
		Dir:  sc.Dir,
		Env:  d.Env,
	})
	if err != nil {
		return nil, fmt.Errorf("campaign: %s setup: %w", sc.ID, err)
	}
	if res.ExitCode != 0 {
		// A dependency that did not come up is not a cell-level finding. Every cell of
		// the scenario would fail identically, and the failure has nothing to do with
		// the runtime under test.
		return nil, fmt.Errorf("campaign: %s setup exited %d: %s",
			sc.ID, res.ExitCode, lastLines(string(res.Stderr)+string(res.Stdout), 8))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stderr)), "\n") {
		if line != "" {
			d.log("  %s", line)
		}
	}
	return d, nil
}

// Stop tears the dependency down. It is called after the scenario's last cell, and it
// reports rather than returns an error: a teardown failure must never mask the result
// the campaign just spent an hour producing.
func (d *Deps) Stop(ctx context.Context) {
	if d == nil || d.teardown == "" {
		return
	}
	res, err := d.runner.Run(ctx, exec.Cmd{
		Path: filepath.Join(d.dir, d.teardown),
		Dir:  d.dir,
		Env:  d.Env,
	})
	switch {
	case err != nil:
		d.log("  deps teardown for %s: %v", d.Scenario, err)
	case res.ExitCode != 0:
		d.log("  deps teardown for %s exited %d", d.Scenario, res.ExitCode)
	}
}

// SubjectEnv is what the runtime under test needs in its environment to reach the
// dependency. It is the same map the setup script got, which is the point: an address
// that differs between the two sides of a connection is a class of failure that costs a
// whole cell to discover.
func (d *Deps) SubjectEnv() map[string]string {
	if d == nil {
		return nil
	}
	return d.Env
}

func (r *Runner) depsRunner() exec.Runner {
	if r.cfg.Hosts.Deps != nil {
		return r.cfg.Hosts.Deps
	}
	// The runner, not the subject. The scenario's setup script lives in the repository
	// and the repository is on the machine driving the campaign; the subject host is
	// deliberately kept to one job.
	return r.cfg.Hosts.Runner
}

// resolveDepsEnv substitutes the deps host into the addresses a scenario declared.
func resolveDepsEnv(in map[string]string, host string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = strings.ReplaceAll(v, DepsHostPlaceholder, host)
	}
	return out
}

// mergeEnv layers over onto base without mutating either. The arm's own environment
// wins: a campaign that names a value has said something more specific than a scenario
// default.
func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
