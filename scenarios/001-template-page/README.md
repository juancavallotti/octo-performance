# 001 — Template page

An HTTP route that renders a template resource as an HTML page.

## The integration

```
GET /page/{name}  →  http source  →  template-resource  →  text/html
```

One root flow, one block. A request arrives on the `http` server connector; the
`http` route source matches `/page/{name}`, lifts the path parameter into the message
variables, and hands the message to a flow worker; the `template-resource` block
evaluates every `{{ CEL }}` placeholder in `octo/templates/page.tmpl` and writes the
result as a raw-content body, which the source returns verbatim with
`Content-Type: text/html; charset=utf-8`.

| | |
|---|---|
| Connectors | `http` (HTTP Server) |
| Sources | `http` route, `/page/{name}` |
| Blocks | `template-resource` (raw body, `text/html`) |
| Resources | one template, ~4 KB rendered |
| External dependencies | none |

## Why this scenario

It is the shortest useful path through the runtime — inbound HTTP, routing, path
parameter extraction, flow dispatch, template evaluation, response write — with no
database, no outbound call, and deliberately no `log` block in the hot path.

That makes it the **regression canary**. If this scenario gets slower between two Octo
versions, something in the core request path got slower, and there is nowhere else for
the cost to hide. Scenarios that add connectors come later and are interpreted against
this floor.

The template is intentionally not a constant string: it interpolates the path
parameter several times, calls `size()`, `upperAscii()`, `lowerAscii()`, `string()`,
and concatenation, and renders `now`. A trivial template would measure the HTTP stack
alone and quietly exclude the templating engine.

## What is compared

| Arm | Config | `workers` / `buffer` / `pool` |
|---|---|---|
| baseline | `octo/baseline.yaml` | absent — Octo defaults (8 / 64 / 8) |
| tuned | `octo/tuned.yaml` | set explicitly, supplied from the environment |

The two files are identical apart from those keys, which `lab/bin/assert-variants.py`
enforces before every run.

## Running it

```bash
task smoke SCENARIO=001-template-page                 # correctness gate
task capacity SCENARIO=001-template-page              # find the knee, then set STEADY_RATE
task sweep SCENARIO=001-template-page                 # grid-search the knobs
task bench SCENARIO=001-template-page \
  TUNED_WORKERS=16 TUNED_BUFFER=256 TUNED_POOL=8      # publish baseline vs tuned
```

Add `TARGET=docker` to run the same thing against the published runtime image.

## Load profile

Set in `scenario.env`.

- **steady** — `constant-arrival-rate` at `STEADY_RATE` for `STEADY_DURATION`. The
  headline comparison.
- **capacity** — `ramping-arrival-rate` from `CAPACITY_START_RATE` to
  `CAPACITY_PEAK_RATE` in `CAPACITY_STEPS` equal steps. Expected to push past what the
  server sustains; where it breaks is the point.
- **smoke** — 1 VU, 30 iterations, seven checks covering status, content type,
  document shape, interpolation, and the absence of unrendered placeholders.

`STEADY_RATE` starts at a placeholder value. Run the capacity test first on a given
host and set it just below the knee, so a healthy baseline drops no iterations and
there is headroom for tuning to show.

## Checks in the smoke gate

The gate fails the whole run if any of these fail:

- status is 200
- `Content-Type` contains `text/html`
- body is a complete HTML document
- the path parameter appears in the body (the template actually interpolated)
- no `{{` remains in the body (nothing failed to render)
- the response is not a JSON envelope (raw-content mode is really in effect)
- body is larger than 1 KB (not a truncated or error page)
