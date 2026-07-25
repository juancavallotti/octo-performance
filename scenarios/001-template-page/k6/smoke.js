// Correctness gate for scenario 001.
//
// This is not a measurement. It exists so that a load run can never be recorded
// against an endpoint that is 404ing, returning JSON instead of HTML, or serving
// a template that failed to interpolate. A fast server returning the wrong thing
// is not a result (AGENTS.md rule 6).

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

export const options = smokeOptions({ iterations: envInt('SMOKE_ITERATIONS', 30) });

const URL = targetUrl();

// The route's last segment is what the template should echo back.
const EXPECTED_NAME = env('ROUTE', '/page/octo').split('/').filter(Boolean).pop();

export default function () {
  const res = http.get(URL);

  check(res, {
    'status is 200': (r) => r.status === 200,
    'content-type is html': (r) =>
      String(r.headers['Content-Type'] || '').toLowerCase().includes('text/html'),
    'body is a full html document': (r) =>
      r.body && r.body.includes('<!doctype html>') && r.body.includes('</html>'),
    // Proves the template engine actually ran rather than echoing the source.
    'path parameter was interpolated': (r) => r.body && r.body.includes(EXPECTED_NAME),
    'no unrendered placeholders left': (r) => r.body && !r.body.includes('{{'),
    // Guards against the flow silently falling back to a JSON response.
    'not a json envelope': (r) => r.body && !r.body.trimStart().startsWith('{'),
    'body is a realistic size': (r) => r.body && r.body.length > 1000,
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
