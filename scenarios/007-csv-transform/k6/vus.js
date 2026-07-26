// Closed-model VU step for scenario 007. Parsing is CPU-bound, so the knee arrives
// at low concurrency — the same shape as scenario 006. See COMPARISON.md.

import http from 'k6/http';
import { vuStepOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS, RECORD_COUNT } from './payload.js';

const VUS = envInt('VUS', 10);
const DURATION = env('DURATION', '30s');

export const options = vuStepOptions({ vus: VUS, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
}

export function handleSummary(data) {
  return summaryHandler(data, {
    test: 'vus',
    loadModel: 'closed',
    vus: VUS,
    payload: { records: RECORD_COUNT, bytes: TRANSFORM_BODY.length },
  });
}
