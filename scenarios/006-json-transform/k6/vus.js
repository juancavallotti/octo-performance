// Closed-model VU step for scenario 006 — comparable to a commercial platform's payload transformation charts,
// which plot TPS against virtual users and find the knee at 10 VUs for every
// instance size, transformation being CPU-bound.

import http from 'k6/http';
import { vuStepOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS } from './payload.js';

const VUS = envInt('VUS', 10);
const DURATION = env('DURATION', '30s');

export const options = vuStepOptions({ vus: VUS, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'vus', loadModel: 'closed', vus: VUS });
}
