// Closed-model VU step for scenario 005.
//
// Run by lab/bin/run-vuramp.sh, one execution per VU level. This is the only
// closed-model test in the lab; it exists so this scenario's curve can be read
// next to published benchmarks, which are almost all closed-model. See
// COMPARISON.md.

import http from 'k6/http';
import { vuStepOptions, targetUrl, env, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';

const VUS = envInt('VUS', 10);
const DURATION = env('DURATION', '30s');

export const options = vuStepOptions({ vus: VUS, duration: DURATION });

const URL = targetUrl();

export default function () {
  http.get(URL);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'vus', loadModel: 'closed', vus: VUS });
}
