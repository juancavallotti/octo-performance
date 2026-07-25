// Steady-state load for scenario 001 — the headline baseline-vs-tuned comparison.
//
// Open model (constant-arrival-rate): load is offered at a fixed rate whether or
// not the server keeps up, so degradation shows as rising latency and dropped
// iterations instead of quietly reduced throughput.

import http from 'k6/http';
import { steadyOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

const RATE = envInt('RATE', envInt('STEADY_RATE', 500));
const DURATION = env('DURATION', env('STEADY_DURATION', '60s'));

export const options = steadyOptions({ rate: RATE, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.get(URL);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'steady', offeredRate: RATE });
}
