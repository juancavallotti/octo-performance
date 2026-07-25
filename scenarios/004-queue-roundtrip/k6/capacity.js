// Capacity probe for scenario 004 — find the knee.
//
// Expected to push past what the queue round trip sustains. Where latency departs
// and dropped iterations begin is the answer; use it to choose STEADY_RATE.

import http from 'k6/http';
import { capacityOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { JOB_BODY, JOB_PARAMS } from './payload.js';

const START = envInt('CAPACITY_START_RATE', 500);
const PEAK = envInt('CAPACITY_PEAK_RATE', 15000);
const STEPS = envInt('CAPACITY_STEPS', 10);
const STEP_DURATION = env('CAPACITY_STEP_DURATION', '15s');

const stages = [];
for (let i = 1; i <= STEPS; i++) {
  stages.push({ target: Math.round(START + ((PEAK - START) * i) / STEPS), duration: STEP_DURATION });
}

export const options = capacityOptions({ startRate: START, stages });

const URL = targetUrl();

export default function () {
  http.post(URL, JOB_BODY, JOB_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'capacity', offeredRate: PEAK });
}
