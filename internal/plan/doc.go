// Package plan expands a campaign spec into the ordered list of cells to execute.
//
// It is a pure function over the spec, and the ordering it produces is the control
// that makes a comparison mean anything. METHODOLOGY.md called interleaving mandatory
// for the entire life of the previous harness, whose run-bench.sh ran all of arm A and
// then all of arm B — so every "gain" that lab ever published is confounded with
// time-in-session and chassis temperature.
//
// # Why cyclic rotation rather than simple alternation
//
// Plain A,B,A,B still hands arm A every position that immediately follows a cooldown —
// the ordinal that inherits the most thermal and socket state. Instead the arm order
// rotates by one each repetition:
//
//	rep 1:  A B C
//	rep 2:  B C A
//	rep 3:  C A B
//
// Over n repetitions every arm occupies every position exactly once, which is a cyclic
// Latin square. For two arms it reduces to A B / B A / A B, giving the sequence
// A B B A A B B A A B for five repetitions.
//
// Scenarios are not interleaved. Switching scenario means restaging config and possibly
// restarting dependencies, so each scenario's cells stay contiguous; the confound
// interleaving addresses is between arms, and arms alternate within a scenario.
//
// # Identity
//
// A plan carries a content hash over the resolved spec and the expanded cells. It names
// the campaign directory and is the key a resumed run matches against, so a spec edited
// mid-campaign cannot silently continue into the previous run's output.
package plan
