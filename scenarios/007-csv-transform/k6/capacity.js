// Capacity ramp for scenario 007 — locate the knee under an open model.

import http from 'k6/http';
import { capacityOptions, targetUrl, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS, RECORD_COUNT } from './payload.js';

const START = envInt('CAPACITY_START_RATE', 200);
const PEAK = envInt('CAPACITY_PEAK_RATE', 6000);
const STEPS = envInt('CAPACITY_STEPS', 10);
const STEP = __ENV.CAPACITY_STEP_DURATION || '15s';

const stages = [];
for (let i = 1; i <= STEPS; i++) {
  stages.push({ duration: STEP, target: Math.round(START + ((PEAK - START) * i) / STEPS) });
}

export const options = capacityOptions({ startRate: START, stages });

const URL = targetUrl();

export default function () {
  http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, {
    test: 'capacity',
    offeredRate: PEAK,
    payload: { records: RECORD_COUNT, bytes: TRANSFORM_BODY.length },
  });
}
