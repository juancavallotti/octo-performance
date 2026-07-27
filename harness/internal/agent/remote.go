package agent

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
)

// Remote samples another machine's /proc through an [exec.Runner].
//
// One command per sample, reading every file the sample needs in a single round trip.
// That is the whole design constraint: at 1 Hz over a multiplexed SSH connection a
// round trip costs single-digit milliseconds, and six separate reads would cost six
// times that — on the machine whose spare capacity the entire experiment depends on.
//
// It shares [sampleFrom] with the local source rather than parsing separately, because
// two parsers of the same format agree until one of them is fixed.
type Remote struct {
	runner exec.Runner

	staticOnce sync.Once
	static     Static
	staticErr  error

	// pageSize is the SUBJECT's page size, not the harness's. A 16 KB-page arm64
	// subject sampled from a 4 KB-page runner would report four times the resident
	// set, and the number would look entirely plausible.
	pageSize int
}

// NewRemote returns a source that samples through r.
func NewRemote(r exec.Runner) *Remote { return &Remote{runner: r, pageSize: 4096} }

// Name implements Source.
func (r *Remote) Name() string { return "procfs@" + r.runner.Name() }

// Fidelity implements Source.
//
// Full: it is the same /proc the local source reads. What differs is the transport,
// and the transport does not change what a counter means.
func (*Remote) Fidelity() Fidelity { return Full }

// Available reports whether the far side has a usable /proc.
func (r *Remote) Available(ctx context.Context) bool {
	res, err := r.runner.Run(ctx, exec.Cmd{Path: "test", Args: []string{"-r", "/proc/stat"}})
	return err == nil && res.ExitCode == 0
}

// staticScript reads everything that does not change while a cell runs.
const staticScript = `
printf '@@hostname\n'; hostname 2>/dev/null || true
printf '@@kernel\n';   uname -r 2>/dev/null || true
printf '@@pagesize\n'; getconf PAGESIZE 2>/dev/null || true
printf '@@uptime\n';   cat /proc/uptime 2>/dev/null || true
printf '@@cpuinfo\n';  cat /proc/cpuinfo 2>/dev/null || true
`

// Static implements Source. It is read once and cached: nothing in it changes while a
// cell runs, and paying a round trip per sample for a constant would be the sampler
// competing with the thing it samples.
func (r *Remote) Static() (Static, error) {
	r.staticOnce.Do(func() {
		res, err := r.runner.Run(context.Background(), shell(staticScript))
		if err != nil {
			r.staticErr = fmt.Errorf("agent: reading %s static: %w", r.runner.Name(), err)
			return
		}
		if res.ExitCode != 0 {
			r.staticErr = fmt.Errorf("agent: reading %s static: exit %d: %s",
				r.runner.Name(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
			return
		}
		secs := sections(res.Stdout)

		st := Static{ClockTick: userHZ, PageSize: 4096}
		st.Hostname = text(secs["hostname"])
		st.Kernel = text(secs["kernel"])
		if n, err := strconv.Atoi(text(secs["pagesize"])); err == nil && n > 0 {
			st.PageSize = n
		}
		if b := secs["cpuinfo"]; len(b) > 0 {
			st.CPUModel = firstFieldValue(b, "model name")
			if st.CPUModel == "" {
				st.CPUModel = firstFieldValue(b, "Model")
			}
			st.Cores = countPrefix(b, "processor")
		}
		if f := strings.Fields(text(secs["uptime"])); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				st.BootTime = time.Now().Add(-time.Duration(up * float64(time.Second)))
			}
		}
		r.static, r.pageSize = st, st.PageSize
	})
	return r.static, r.staticErr
}

// Sample implements Source.
func (r *Remote) Sample(pid int) (Sample, error) {
	// Static first, so the page size is the subject's before any RSS is converted.
	if _, err := r.Static(); err != nil {
		return Sample{}, err
	}

	p := strconv.Itoa(pid)
	// One command, one round trip, and the pid's own stat is the only read allowed to
	// fail the sample: everything else is corroboration, and a process that exits
	// between two reads should still contribute what it did produce.
	script := "set -e\n" +
		"printf '@@stat\\n';   cat /proc/" + p + "/stat\n" +
		"set +e\n" +
		"printf '\\n@@status\\n'; cat /proc/" + p + "/status 2>/dev/null\n" +
		"printf '@@host\\n';   cat /proc/stat 2>/dev/null\n" +
		"printf '@@load\\n';   cat /proc/loadavg 2>/dev/null\n" +
		"printf '@@fd\\n';     ls /proc/" + p + "/fd 2>/dev/null | wc -l\n"

	at := time.Now()
	res, err := r.runner.Run(context.Background(), shell(script))
	if err != nil {
		return Sample{}, fmt.Errorf("agent: sampling %s: %w", r.runner.Name(), err)
	}
	if res.ExitCode != 0 {
		return Sample{}, fmt.Errorf("agent: sampling pid %d on %s: exit %d: %s",
			pid, r.runner.Name(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	secs := sections(res.Stdout)
	snap := procSnapshot{
		pidStat:   secs["stat"],
		pidStatus: secs["status"],
		hostStat:  secs["host"],
		loadavg:   secs["load"],
		pageSize:  r.pageSize,
	}
	if n, err := strconv.Atoi(text(secs["fd"])); err == nil {
		snap.openFDs = n
	}
	if len(snap.pidStat) == 0 {
		return Sample{}, fmt.Errorf("agent: pid %d on %s produced no stat", pid, r.runner.Name())
	}
	return sampleFrom(snap, at)
}

// shell wraps a script for the far side.
//
// This is the one place in the harness that hands a shell a string, and it is confined
// to scripts written here as constants — never to anything derived from a spec, a
// scenario or a filename. The rule everywhere else is that argv is a slice, because
// assembling octo's flags as text is what let --metrics reach a build that could not
// parse it.
func shell(script string) exec.Cmd {
	return exec.Cmd{Path: "/bin/sh", Args: []string{"-c", script}}
}

// sections splits @@name-delimited output.
func sections(b []byte) map[string][]byte {
	out := map[string][]byte{}
	name := ""
	var body []byte
	flush := func() {
		if name != "" {
			out[name] = body
		}
		body = nil
	}
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		if trimmed := bytes.TrimRight(line, "\n"); bytes.HasPrefix(trimmed, []byte("@@")) {
			flush()
			name = string(trimmed[2:])
			continue
		}
		body = append(body, line...)
	}
	flush()
	return out
}

func text(b []byte) string { return strings.TrimSpace(string(b)) }
