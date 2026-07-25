// Steady-state load for scenario 004 — the baseline-vs-tuned comparison.

import http from 'k6/http';
import { steadyOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { JOB_BODY, JOB_PARAMS } from './payload.js';

const RATE = envInt('RATE', envInt('STEADY_RATE', 3000));
const DURATION = env('DURATION', env('STEADY_DURATION', '60s'));

export const options = steadyOptions({ rate: RATE, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.post(URL, JOB_BODY, JOB_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'steady', offeredRate: RATE });
}
