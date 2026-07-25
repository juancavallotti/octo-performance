// Capacity probe for scenario 001 — find the knee.
//
// Ramps the offered rate in equal steps up to the peak. The run is EXPECTED to
// push past what the server can sustain: the answer being looked for is where
// latency departs and where dropped iterations begin. Use the result to choose
// STEADY_RATE in scenario.env.

import http from 'k6/http';
import { capacityOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

const START = envInt('CAPACITY_START_RATE', 100);
const PEAK = envInt('CAPACITY_PEAK_RATE', 4000);
const STEPS = envInt('CAPACITY_STEPS', 8);
const STEP_DURATION = env('CAPACITY_STEP_DURATION', '20s');

// Equal steps from START to PEAK. Each stage ramps to its target and holds it,
// so the summary reflects sustained rates rather than a single sweep upward.
const stages = [];
for (let i = 1; i <= STEPS; i++) {
  stages.push({ target: Math.round(START + ((PEAK - START) * i) / STEPS), duration: STEP_DURATION });
}

export const options = capacityOptions({ startRate: START, stages });

const URL = targetUrl();

export default function () {
  http.get(URL);
}

export function handleSummary(data) {
  // No single offered rate applies to a ramp; the report reads the ramp from here.
  return summaryHandler(data, { test: 'capacity', offeredRate: PEAK });
}
