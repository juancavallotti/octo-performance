// Steady-state load for scenario 007 — the baseline-vs-tuned comparison.

import http from 'k6/http';
import { steadyOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS, RECORD_COUNT } from './payload.js';

const RATE = envInt('RATE', envInt('STEADY_RATE', 2000));
const DURATION = env('DURATION', env('STEADY_DURATION', '60s'));

export const options = steadyOptions({ rate: RATE, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
}

export function handleSummary(data) {
  // The record count travels with the result: this scenario's ladder is sized in
  // records rather than bytes, so a summary without it cannot be placed on the
  // ladder — or next to scenario 006's rung of the same size.
  return summaryHandler(data, {
    test: 'steady',
    offeredRate: RATE,
    payload: { records: RECORD_COUNT, bytes: TRANSFORM_BODY.length },
  });
}
