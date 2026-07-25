// Steady-state load for scenario 005 — the baseline-vs-tuned comparison.
//
// Open model, like the rest of the lab: the rate is offered regardless of whether
// the server can take it, so the out-of-the-box arm's inability to keep up shows up
// as dropped iterations rather than as a quietly smaller number.

import http from 'k6/http';
import { steadyOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

const RATE = envInt('RATE', envInt('STEADY_RATE', 800));
const DURATION = env('DURATION', env('STEADY_DURATION', '60s'));

export const options = steadyOptions({ rate: RATE, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.get(URL);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'steady', offeredRate: RATE });
}
