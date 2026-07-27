// Package agent samples a process and the machine it runs on.
//
// It is the measurement half of perf-agent, and it is a library rather than a program
// so that the parsing can be tested against committed /proc fixtures instead of being
// untested awk. The old lab's equivalent was a Python script reading ps output with a
// BSD-versus-GNU flag branch, and nothing verified either branch.
//
// Two things it is careful about.
//
// Counters are cumulative and are differenced afterwards. Reading an instantaneous
// %cpu understates a short window, because it is itself an average over an interval
// nobody chose. Every CPU figure here is a monotonically increasing total; the rate
// comes from series.Rate over the measured window, which is exact.
//
// Fidelity is recorded, not assumed. A Linux subject yields per-process CPU, RSS,
// thread count, open file descriptors and context switches from /proc. A macOS laptop
// running a local campaign yields CPU and RSS from ps at coarser resolution and
// nothing else. Both are useful; only one of them supports a statement about context
// switches, so the sample says which one it is rather than leaving a reader to assume.
package agent
