// Correctness gate for scenario 005: the proxy must actually reach the backend and
// return what it returned.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

export const options = smokeOptions({ iterations: 20 });

const URL = targetUrl();
const EXPECTED_SIZE = envInt('BACKEND_SIZE', 1024);

export default function () {
  const res = http.get(URL);
  check(res, {
    'status is 200': (r) => r.status === 200,
    'json content type': (r) =>
      (r.headers['Content-Type'] || '').includes('application/json'),
    // Proves the response travelled through the backend rather than being
    // synthesised by the flow — a proxy that answers without proxying would
    // otherwise benchmark beautifully.
    'body came from the backend': (r) => {
      try {
        return JSON.parse(r.body).status === 'ACTIVE';
      } catch (e) {
        return false;
      }
    },
    // Guards the payload-size ladder: a truncated response would silently make the
    // 1 MB case measure something smaller.
    'payload is about the requested size': (r) =>
      Math.abs(r.body.length - EXPECTED_SIZE) <= 64,
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
