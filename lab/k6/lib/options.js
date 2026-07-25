// Shared k6 option presets.
//
// Every scenario builds its options from these so that executor choice, threshold
// shape, and environment handling stay consistent across the lab.
//
// The load tests use ARRIVAL-RATE executors on purpose. Under a closed model
// (fixed VUs looping), a slow server simply receives fewer requests and
// throughput self-limits into a number that looks stable while hiding the
// problem. Under an open model, load is offered at a fixed rate regardless of how
// the server copes, so degradation surfaces honestly as rising latency and
// non-zero dropped_iterations.

/** Read an env var with a default. */
export function env(name, fallback) {
  const v = __ENV[name];
  return v === undefined || v === '' ? fallback : v;
}

export function envInt(name, fallback) {
  const v = env(name, undefined);
  return v === undefined ? fallback : parseInt(v, 10);
}

/** The URL under test, assembled from the host profile and scenario route. */
export function targetUrl() {
  const base = env('BASE_URL', 'http://localhost:8080').replace(/\/+$/, '');
  const route = env('ROUTE', '/');
  return base + route;
}

/**
 * Constant arrival rate — the headline comparison.
 *
 * preAllocatedVUs is generous relative to the rate: k6 can only start an
 * iteration if a VU is free, and starving the pool would show up as dropped
 * iterations caused by the generator rather than by the server.
 */
export function steadyOptions({ rate, duration, thresholds }) {
  const vus = Math.max(50, Math.ceil(rate * 0.6));
  return {
    discardResponseBodies: true,
    scenarios: {
      steady: {
        executor: 'constant-arrival-rate',
        rate,
        timeUnit: '1s',
        duration,
        preAllocatedVUs: vus,
        maxVUs: vus * 4,
        gracefulStop: '10s',
      },
    },
    thresholds: thresholds || defaultThresholds(),
  };
}

/**
 * Ramping arrival rate — locate the knee of the curve.
 *
 * Thresholds here are not pass/fail gates so much as markers: the run is
 * expected to push past what the server can sustain, and where it breaks is the
 * answer being looked for.
 */
export function capacityOptions({ startRate, stages, thresholds }) {
  const peak = stages.reduce((m, s) => Math.max(m, s.target), startRate);
  const vus = Math.max(100, Math.ceil(peak * 0.6));
  return {
    discardResponseBodies: true,
    scenarios: {
      capacity: {
        executor: 'ramping-arrival-rate',
        startRate,
        timeUnit: '1s',
        stages,
        preAllocatedVUs: vus,
        maxVUs: vus * 4,
        gracefulStop: '10s',
      },
    },
    thresholds: thresholds || { http_req_failed: ['rate<0.01'] },
  };
}

/** Smoke: correctness only. Bodies are kept so checks can inspect them. */
export function smokeOptions({ iterations }) {
  return {
    discardResponseBodies: false,
    scenarios: {
      smoke: {
        executor: 'shared-iterations',
        vus: 1,
        iterations,
        maxDuration: '1m',
      },
    },
    // Any failure at all fails the gate.
    thresholds: {
      checks: ['rate==1.0'],
      http_req_failed: ['rate==0'],
    },
  };
}

export function defaultThresholds() {
  return {
    // A dropped iteration means the offered rate exceeded what could be started;
    // the run is then a saturation measurement, not a latency measurement.
    dropped_iterations: ['count==0'],
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
  };
}
