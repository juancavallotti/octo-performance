// Package exec runs processes and moves bytes on a host.
//
// It is the boundary between local iteration and a real campaign. The orchestrator
// never shells out directly; it holds two [Runner] values, one per machine, and the
// same campaign code drives a laptop or a pair of GCP VMs depending on what those
// values are.
//
// Three rules this package exists to enforce:
//
// A command is argv, never a shell string. [Cmd.Args] is a slice and no implementation
// may interpolate it into a shell. Every quoting bug in the old lab/bin/ — and there
// were several, in the paths that assembled octo's flags — is unrepresentable here.
//
// A non-zero exit status is data, not an error. k6 exits 99 when a threshold is
// breached, which is a finding about the run and not a failure of the harness; the old
// lab wrote that 99 to k6-exit.txt in every published cell and nothing ever read it.
// [Runner.Run] returns an error only when the command could not be run at all, and
// puts the status in [Result.ExitCode] where a gate can see it. Callers that genuinely
// require success say so with [Result.Err].
//
// Output is captured under a bound. A process that floods stderr must not take the
// orchestrator down with it, so capture stops at [Cmd.CaptureLimit] and records that it
// did.
package exec
