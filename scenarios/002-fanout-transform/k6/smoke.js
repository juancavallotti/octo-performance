// Correctness gate for scenario 002.
//
// Every composite in the flow has a way of silently doing nothing — a fork branch
// that errors is joined over, a flow-ref that no-ops leaves the variable unset —
// so the checks assert the *output* of each stage, not just a 200.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, envInt } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { ORDER_BODY, ORDER_PARAMS, ORDER_LINE_COUNT } from './payload.js';

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
    // multi-transform ran: it is what sets lineCount and gross.
    'multi-transform applied': () => doc && doc.lineCount === ORDER_LINE_COUNT,
    'gross was computed': () => doc && typeof doc.gross === 'number' && doc.gross > 0,
    // flow-ref reached score-risk on the main message, so riskBand is real.
    'flow-ref score-risk ran': () => doc && doc.riskBand && doc.riskBand !== 'unscored',
    'risk band is valid': () => doc && ['low', 'medium', 'high'].includes(doc.riskBand),
    // flow-ref reached finalize, which is what builds this response shape.
    'flow-ref finalize ran': () => doc && doc.status === 'accepted',
    'order id echoed': () => doc && typeof doc.orderId === 'string' && doc.orderId.length > 0,
  });
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
