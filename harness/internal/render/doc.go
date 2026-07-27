// Package render derives a campaign arm's config from a scenario's single
// integration.yaml.
//
// One scenario declares exactly one integration document. Both arms are derived from
// it, so they cannot drift:
//
//   - baseline has every declared tunable stripped, so the runtime falls back to
//     whatever it actually ships with. Writing workers: 8 into a baseline would freeze
//     it at today's default and silently stop tracking the real one; stripping means
//     that if a future version changes a default, this arm follows — which is exactly
//     the regression the lab exists to catch.
//   - tuned has the knobs rewritten to literals, so the config archived beside the
//     result records the value that ran rather than the fact that it came from
//     somewhere.
//
// # Nodes, not lines
//
// The predecessor matched a regex against lines, and AGENTS.md documents the landmine
// that created: "Declare each knob in exactly one place, or the renderer — which
// rewrites every occurrence of a name — will set them all together." The actual rule is
// that workers, buffer and pool are root-flow only and sub-flows inherit, which no line
// regex can express.
//
// So selectors carry a node path — flows[*].workers, connectors[*].settings.maxOpenConns,
// flows[*].source.settings.listeners — and this package walks the document.
//
// # Provenance moves off whitespace
//
// yaml.v3 re-emits with its own indentation, so the rendered file is not byte-identical
// to the source in untouched regions. Provenance therefore lives in [Result.Changes],
// archived as config.render.json: source digest, selector, concrete node path,
// before to after, and source line. That says what changed and where, which the
// rendered YAML alone never could.
//
// # Verification is part of rendering
//
// [Render] verifies its own output before returning it. A baseline that still declares
// a tunable anywhere in the document is an error, not a warning.
package render
