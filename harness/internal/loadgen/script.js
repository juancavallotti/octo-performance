// The load script. One file, driven entirely by the environment, archived per cell.
//
// It deliberately computes almost nothing. The old lab's options.js sized the virtual
// user pool at script start with a poolFor() helper, and that number — the one value
// that discriminates a healthy run from a collapsed one — appeared in no result file
// and could not be recovered afterwards. Everything decided here would have the same
// problem, so the harness decides it in Go, records it, and passes it in.
//
// PERF_MODEL      open | closed
// PERF_RATE       requests per second, open model
// PERF_VUS        virtual users, closed model
// PERF_PRE_VUS    pre-allocated pool, from Little's law in Go
// PERF_MAX_VUS    ceiling the pool may grow to
// PERF_DURATION   how long to hold it
// PERF_STAGES     JSON stages for a capacity ramp, instead of a steady rate
// PERF_START_RATE starting rate for a ramp
// PERF_SUMMARY    where to write the end-of-run summary

import http from 'k6/http';

const url = __ENV.PERF_URL;
const model = __ENV.PERF_MODEL || 'open';
const method = (__ENV.PERF_METHOD || 'GET').toUpperCase();
const bodyText = __ENV.PERF_BODY || null;
const contentType = __ENV.PERF_CONTENT_TYPE || '';

const rate = Number(__ENV.PERF_RATE || 0);
const vus = Number(__ENV.PERF_VUS || 0);
const preVUs = Number(__ENV.PERF_PRE_VUS || 20);
const maxVUs = Number(__ENV.PERF_MAX_VUS || 200);
const duration = __ENV.PERF_DURATION || '30s';
const startRate = Number(__ENV.PERF_START_RATE || 0);
const stages = __ENV.PERF_STAGES ? JSON.parse(__ENV.PERF_STAGES) : null;

function executor() {
  if (stages) {
    // A capacity probe. It ramps until something gives, which is how the steady
    // rate gets chosen rather than remembered from a laptop run a day earlier.
    return {
      executor: 'ramping-arrival-rate',
      startRate: startRate,
      timeUnit: '1s',
      preAllocatedVUs: preVUs,
      maxVUs: maxVUs,
      stages: stages,
    };
  }
  if (model === 'closed') {
    // A fixed population looping. A slow server simply receives fewer requests, so
    // throughput self-limits into a number that looks stable while hiding the
    // problem. Used only for cross-vendor comparability, never for a regression.
    return { executor: 'constant-vus', vus: vus, duration: duration };
  }
  // Open model: offer a fixed rate whatever the server is doing, so degradation
  // surfaces as rising latency and dropped iterations instead of as a lower number
  // that looks like a result.
  return {
    executor: 'constant-arrival-rate',
    rate: rate,
    timeUnit: '1s',
    duration: duration,
    preAllocatedVUs: preVUs,
    maxVUs: maxVUs,
  };
}

export const options = {
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
  scenarios: { load: executor() },
  // No thresholds. A breach makes k6 exit 99 and, if it is configured to abort on
  // failure, cuts the run short — which changes the measurement in exactly the
  // situation worth measuring. Validity is decided afterwards, by the gates,
  // against the evidence a complete run produced.
  thresholds: {},
};

const params = contentType ? { headers: { 'Content-Type': contentType } } : {};

export default function () {
  if (bodyText === null) {
    http.request(method, url, null, params);
  } else {
    http.request(method, url, bodyText, params);
  }
}

export function handleSummary(data) {
  const out = {};
  if (__ENV.PERF_SUMMARY) {
    out[__ENV.PERF_SUMMARY] = JSON.stringify(data, null, 1);
  }
  return out;
}
