// Correctness gate for scenario 006: the transformation must actually happen.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS, RECORD_COUNT } from './payload.js';

export const options = smokeOptions({ iterations: 20 });

const URL = targetUrl();

export default function () {
  const res = http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
  check(res, {
    'status is 200': (r) => r.status === 200,
    // Every record in, every record out: a transform that silently dropped rows
    // would benchmark faster than one that did the work.
    'every record survived': (r) => {
      try {
        return JSON.parse(r.body).meta.count === RECORD_COUNT;
      } catch (e) {
        return false;
      }
    },
    // Proves the reshape ran rather than the body being echoed.
    'fields were reshaped': (r) => {
      try {
        const a = JSON.parse(r.body).accounts[0];
        return a.accountId !== undefined &&
               a.location.region !== undefined &&
               a.balance.withTax !== undefined &&
               ['gold', 'silver', 'bronze'].includes(a.tier);
      } catch (e) {
        return false;
      }
    },
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
