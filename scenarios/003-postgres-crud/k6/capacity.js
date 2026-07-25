// Capacity probe for scenario 003 — find the knee.
//
// Expected to push past what the database sustains. Where latency departs and
// dropped iterations begin is the answer; use it to choose STEADY_RATE.

import http from 'k6/http';
import { capacityOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { ORDER_BODY, ORDER_PARAMS } from './payload.js';

const START = envInt('CAPACITY_START_RATE', 200);
const PEAK = envInt('CAPACITY_PEAK_RATE', 6000);
const STEPS = envInt('CAPACITY_STEPS', 10);
const STEP_DURATION = env('CAPACITY_STEP_DURATION', '15s');

const stages = [];
for (let i = 1; i <= STEPS; i++) {
  stages.push({ target: Math.round(START + ((PEAK - START) * i) / STEPS), duration: STEP_DURATION });
}

export const options = capacityOptions({ startRate: START, stages });

const URL = targetUrl();

export default function () {
  http.post(URL, ORDER_BODY, ORDER_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'capacity', offeredRate: PEAK });
}
