// Shared handleSummary: emit a stable, minimal JSON shape the report generator
// can rely on, instead of k6's full internal summary structure.
//
// The file path comes from the SUMMARY_OUT environment variable so the harness
// decides where each cell's data lands.

/** Pull the trend percentiles the methodology reports on. */
function trend(m) {
  if (!m || !m.values) return {};
  const v = m.values;
  return {
    avg: v.avg,
    min: v.min,
    med: v.med,
    p90: v['p(90)'],
    p95: v['p(95)'],
    p99: v['p(99)'],
    max: v.max,
  };
}

function counter(m) {
  if (!m || !m.values) return {};
  return { count: m.values.count, rate: m.values.rate };
}

function rate(m) {
  if (!m || !m.values) return {};
  return { rate: m.values.rate, passes: m.values.passes, fails: m.values.fails };
}

function gauge(m) {
  if (!m || !m.values) return {};
  return { value: m.values.value, max: m.values.max };
}

/** Flatten k6's nested group/check tree into pass/fail totals. */
function collectChecks(root) {
  let passes = 0;
  let fails = 0;
  const walk = (node) => {
    if (!node) return;
    for (const c of node.checks || []) {
      passes += c.passes || 0;
      fails += c.fails || 0;
    }
    for (const g of node.groups || []) walk(g);
  };
  walk(root);
  return { passes, fails };
}

function collectThresholds(metrics) {
  const out = {};
  for (const [name, m] of Object.entries(metrics || {})) {
    if (!m.thresholds) continue;
    out[name] = {};
    for (const [expr, res] of Object.entries(m.thresholds)) {
      // k6 reports either {ok: bool} or a bare boolean depending on version.
      out[name][expr] = typeof res === 'object' ? !!res.ok : !!res;
    }
  }
  return out;
}

/**
 * @param {object} data      k6 summary data
 * @param {object} extra     { test, offeredRate, loadModel, vus }
 *
 * `loadModel` is recorded rather than inferred because it is the one property that
 * makes two throughput numbers incomparable no matter how alike they look. An open
 * model reports what the server was *asked* for; a closed model reports what a
 * fixed population of clients could *extract*. Defaulting to "open" is safe: every
 * test in the lab is open except the cross-vendor VU steps, which set it.
 */
export function buildSummary(data, extra = {}) {
  const m = data.metrics || {};
  const durationMs = (data.state && data.state.testRunDurationMs) || 0;

  return {
    test: extra.test || __ENV.TEST_NAME || 'unknown',
    loadModel: extra.loadModel || 'open',
    vus: extra.vus !== undefined ? extra.vus : null,
    offeredRate: extra.offeredRate !== undefined ? extra.offeredRate : null,
    durationSeconds: durationMs / 1000,
    metrics: {
      http_reqs: counter(m.http_reqs),
      http_req_duration: trend(m.http_req_duration),
      http_req_waiting: trend(m.http_req_waiting),
      http_req_connecting: trend(m.http_req_connecting),
      http_req_failed: rate(m.http_req_failed),
      iterations: counter(m.iterations),
      dropped_iterations: counter(m.dropped_iterations),
      data_received: counter(m.data_received),
      data_sent: counter(m.data_sent),
      vus_max: gauge(m.vus_max),
    },
    thresholds: collectThresholds(m),
    checks: collectChecks(data.root_group),
  };
}

/**
 * Build the object k6 expects from handleSummary. Always writes the human
 * summary to stdout (captured into k6.log) plus the JSON the harness reads.
 */
export function summaryHandler(data, extra = {}) {
  const out = {
    stdout: textSummaryFallback(data),
  };
  const target = __ENV.SUMMARY_OUT;
  if (target) {
    out[target] = JSON.stringify(buildSummary(data, extra), null, 2);
  }
  return out;
}

// k6's textSummary helper lives on a remote module; the lab is offline-friendly,
// so render the handful of lines that matter ourselves.
function textSummaryFallback(data) {
  const m = data.metrics || {};
  const line = (k, v) => `  ${k.padEnd(28)} ${v}\n`;
  const num = (v, d = 2) => (v === undefined || v === null ? 'n/a' : v.toFixed(d));

  let s = '\n';
  if (m.http_reqs) {
    s += line('http_reqs', `${m.http_reqs.values.count} (${num(m.http_reqs.values.rate)}/s)`);
  }
  if (m.http_req_duration) {
    const v = m.http_req_duration.values;
    s += line('http_req_duration', `med=${num(v.med)}ms p95=${num(v['p(95)'])}ms p99=${num(v['p(99)'])}ms max=${num(v.max)}ms`);
  }
  if (m.http_req_waiting) {
    s += line('http_req_waiting', `p95=${num(m.http_req_waiting.values['p(95)'])}ms`);
  }
  if (m.http_req_failed) {
    s += line('http_req_failed', `${num(m.http_req_failed.values.rate * 100, 3)}%`);
  }
  if (m.dropped_iterations) {
    s += line('dropped_iterations', `${m.dropped_iterations.values.count}`);
  }
  if (m.vus_max) {
    s += line('vus_max', `${m.vus_max.values.max ?? m.vus_max.values.value}`);
  }
  const checks = collectChecks(data.root_group);
  s += line('checks', `${checks.passes} passed, ${checks.fails} failed`);
  return s + '\n';
}
