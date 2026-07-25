// Correctness gate for scenario 004.
//
// A request/reply queue can fail in a way that still returns 200: if the reply is
// never folded back, the response is shaped from an empty body and every field is
// simply missing. The checks assert the consumer's arithmetic actually came back,
// which is the only proof the round trip completed rather than timing out quietly.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { JOB_BODY, JOB_PARAMS, EXPECTED_SCORE, EXPECTED_BAND } from './payload.js';

export const options = smokeOptions({ iterations: envInt('SMOKE_ITERATIONS', 30) });

const URL = targetUrl();

export default function () {
  const res = http.post(URL, JOB_BODY, JOB_PARAMS);

  let doc = null;
  try {
    doc = res.json();
  } catch (e) {
    doc = null;
  }

  check(res, {
    'status is 200': (r) => r.status === 200,
    'body parses as json': () => doc !== null,
    'flow completed': () => doc && doc.status === 'ok',
    // Producer side: the job id was bound before dispatch.
    'producer bound a job id': () => doc && typeof doc.jobId === 'string' && doc.jobId.length > 0,
    // Consumer side: proves a worker actually ran, not just that the enqueue returned.
    'consumer handled the job': () => doc && doc.handled === true,
    // The reply was folded back into the producer's message.
    'reply folded back — score': () =>
      doc && typeof doc.score === 'number' && Math.abs(doc.score - EXPECTED_SCORE) < 0.001,
    'reply folded back — band': () => doc && doc.band === EXPECTED_BAND,
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
