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
 * Size the VU pool from Little's law: concurrency = arrival rate × service time.
 *
 * This matters more than it looks. k6 initialises every pre-allocated VU up front,
 * and on a host shared with the server under test an over-sized pool steals CPU
 * and memory from the thing being measured — biasing the result downward. A naive
 * "VUs = rate × 0.6" allocates 7200 VUs at 12k req/s for a workload that actually
 * needs about 15.
 *
 * The pool is still generously over-provisioned (4× headroom, and maxVUs allows a
 * 10× latency excursion) because starving it would surface as dropped iterations
 * caused by the generator rather than by the server — which would be a lie in the
 * opposite direction.
 */
function poolFor(rate) {
  const latencySeconds = envInt('EXPECTED_LATENCY_MS', 5) / 1000;
  const needed = rate * latencySeconds;
  const preAllocatedVUs = Math.max(20, Math.ceil(needed * 4));
  return { preAllocatedVUs, maxVUs: Math.max(200, preAllocatedVUs * 10) };
}

/** Constant arrival rate — the headline comparison. */
export function steadyOptions({ rate, duration, thresholds }) {
  return {
    discardResponseBodies: true,
    scenarios: {
      steady: {
        executor: 'constant-arrival-rate',
        rate,
        timeUnit: '1s',
        duration,
        ...poolFor(rate),
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
  return {
    discardResponseBodies: true,
    scenarios: {
      capacity: {
        executor: 'ramping-arrival-rate',
        startRate,
        timeUnit: '1s',
        stages,
        ...poolFor(peak),
        gracefulStop: '10s',
      },
    },
    thresholds: thresholds || { http_req_failed: ['rate<0.01'] },
  };
}

/**
 * Constant virtual users — a CLOSED model, used only for cross-vendor comparison.
 *
 * Everything else in this lab is deliberately open-model, for the reasons at the
 * top of this file. This executor exists because the published numbers we want to
 * stand beside are closed-model: a commercial platform's charts plot TPS and CPU% against JMeter
 * virtual users and define the "knee point" as the VU count where TPS stops rising.
 * That knee is an artifact of the closed model — under an open model the same
 * server has no knee, it just accumulates latency — so reproducing their x-axis is
 * the only way to produce a number that means the same thing theirs does.
 *
 * The harness runs one k6 execution per VU level rather than ramping through them,
 * so each level gets its own clean CPU sample and no ramp transient bleeds across.
 *
 * Never compare a number from here with a number from steadyOptions(). The load
 * model is recorded in the summary so the report can refuse to.
 */
export function vuStepOptions({ vus, duration, thresholds }) {
  return {
    discardResponseBodies: true,
    scenarios: {
      vustep: {
        executor: 'constant-vus',
        vus,
        duration,
        gracefulStop: '5s',
      },
    },
    // The run is expected to be pushed past saturation — that is the measurement.
    // Only outright failure is a threshold.
    thresholds: thresholds || { http_req_failed: ['rate<0.05'] },
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

/**
 * Thresholds are per-scenario: a bound loose enough for every workload is a bound
 * that catches nothing. Set THRESHOLD_P95_MS / THRESHOLD_P99_MS in scenario.env to
 * something the scenario should actually hold at its steady rate.
 */
export function defaultThresholds() {
  return {
    // A dropped iteration means the offered rate exceeded what could be started;
    // the run is then a saturation measurement, not a latency measurement.
    dropped_iterations: ['count==0'],
    http_req_failed: ['rate<0.001'],
    http_req_duration: [
      `p(95)<${envInt('THRESHOLD_P95_MS', 250)}`,
      `p(99)<${envInt('THRESHOLD_P99_MS', 500)}`,
    ],
  };
}
