// Correctness gate for scenario 003.
//
// A CRUD flow can return 200 while quietly doing nothing — an INSERT that failed
// silently, a SELECT that matched no row, a DELETE that removed nothing. The
// checks assert that each statement actually took effect.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { ORDER_BODY, ORDER_PARAMS } from './payload.js';

export const options = smokeOptions({ iterations: envInt('SMOKE_ITERATIONS', 30) });

const URL = targetUrl();

export default function () {
  const res = http.post(URL, ORDER_BODY, ORDER_PARAMS);

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
    // The id is the message eventID, so a present, non-empty id proves the
    // INSERT bound its arguments rather than writing nulls.
    'insert bound an id': () => doc && typeof doc.orderId === 'string' && doc.orderId.length > 0,
    // The read-back returned the row we just wrote.
    'select read the row back': () => doc && doc.customer === 'acme-industrial',
    // rowsAffected proves the DELETE matched, so the table stays bounded.
    'delete removed exactly one row': () => doc && doc.deleted === 1,
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
