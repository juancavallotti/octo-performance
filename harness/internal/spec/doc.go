// Package spec is the declarative description of a campaign and of a scenario.
//
// A campaign spec is the thing that replaces scripts scattered across a bin directory.
// It is checked in, it is archived verbatim beside the result, and it *is* the record
// of what a run was trying to find out. The old lab encoded that intent as free text in
// a directory name — `-mON`, `-mOFF`, `-probesonly`, `-metricson`, five labels covering
// two axes, two of them synonyms — and the only place the mapping was written down was
// four lines of a log file nothing linked to.
//
// # Defaulting
//
// A scenario carries load defaults; a campaign may override them. [Load.Merge] applies
// campaign over scenario over zero, and the resolved value is what gets archived, so a
// reader never has to reconstruct which layer won.
//
// # Validation refuses rather than guesses
//
// [Campaign.Validate] rejects a spec that cannot produce a defensible result: fewer
// than two arms, an arm whose binary cannot be resolved, a blocked execution order with
// no stated reason. Two runs in the old results are stamped `vunknown-dev` and were
// published anyway; a campaign that cannot name what it is testing does not start.
package spec
