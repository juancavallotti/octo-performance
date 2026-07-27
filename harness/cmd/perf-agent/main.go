// Command perf-agent runs on the subject VM, supervises the runtime under test, and
// samples it.
//
// It speaks NDJSON over stdin/stdout across a single SSH session — no listener, no
// authentication story, nothing to secure. Terraform installs the pinned release at
// boot on both machines, and perf refuses to proceed if the agent's protocol version
// does not match its own.
//
// Being the runtime's parent process is the point. wait4 yields whole-lifetime user and
// system CPU together with peak RSS directly, which retires the /usr/bin/time wrapper,
// the pgrep -P child hunt and the wrapper-pid file the old harness needed — a mechanism
// that was also a genuine race.
//
// The protocol server is Phase 1. This binary currently reports its identity so the
// release pipeline and the terraform startup scripts have something real to install.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/juancavallotti/octo-performance/harness/internal/buildinfo"
)

// ProtocolVersion is the agent wire contract. perf refuses to drive an agent that
// reports a different number, because a silently mismatched sampler produces plausible
// data rather than an error.
const ProtocolVersion = 1

func main() {
	asJSON := flag.Bool("json", false, "print identity as JSON, which is how perf reads it")
	flag.Parse()

	info := buildinfo.Get()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			buildinfo.Info
			Protocol int `json:"protocolVersion"`
		}{info, ProtocolVersion}); err != nil {
			fmt.Fprintln(os.Stderr, "perf-agent:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Printf("perf-agent %s  protocol %d\n", info, ProtocolVersion)
}
