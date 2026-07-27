// Package subject starts, probes and stops the runtime under test.
//
// One Target, parameterised by an exec.Runner, replaces the target-native.sh and
// target-docker.sh dispatch the old lab carried. Where the subject runs is a property
// of the Runner, not a branch in this package.
//
// The rule this package exists to hold is: ask the artifact, never its version string.
//
// A source build reports the version constant of the release it branched from, so a
// build with a feature and a build without one can claim the same version. Worse in
// the other direction: passing --metrics to a build that does not accept it is a hard
// flag-parse failure at start-up, so an optimistic guess does not degrade, it fails
// the cell. Capabilities therefore come from `octo run --help`, which is the artifact's
// own statement of what it accepts, and the raw text is archived so a later reader can
// check the inference rather than trust it.
//
// Detection alone is still a grep of someone else's help text, so it is confirmed
// positively: if the help says there is an admin port, the admin port has to answer
// /healthz before the cell is allowed to produce a number. Being wrong in the
// optimistic direction is the dangerous direction.
package subject
