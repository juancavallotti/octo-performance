// Package fake provides the doubles that make the orchestrator testable without a VM.
//
// The centre of it is an octo-alike: a real HTTP server, started as a real process,
// speaking the routes and the Prometheus exposition the runtime actually serves. It is
// deliberately not a stub that returns canned answers. It binds ports, takes time to
// become ready, degrades under concurrency, and shuts down on SIGTERM, because those
// are the behaviours the harness's cell procedure has to get right and none of them
// can be verified against a mock that skips them.
//
// It also lies on request. FAKEOCTO_CAPS makes it present itself as a build with no
// admin port, which is what every octo before 0.5.0 is, so the comparison this lab
// exists to make — an arm that can serve metrics against one that cannot — can be
// exercised end to end on a laptop.
//
// Nothing here ships in a release. cmd/fakeocto is built by tests when they need it.
package fake
