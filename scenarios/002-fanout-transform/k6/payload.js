// Shared request payload for scenario 002.
//
// Built once at module scope, not per iteration: the point of this scenario is to
// load the server, and re-serialising the same object on every VU iteration would
// spend the generator's CPU on work that tells us nothing.

import { envInt } from '../../../lab/k6/lib/options.js';

const LINES = envInt('ORDER_LINES', 8);

// JSON numbers decode to CEL doubles, so qty and price are written as decimals —
// CEL will not multiply an int by a double.
function buildOrder(lines) {
  const items = [];
  for (let i = 0; i < lines; i++) {
    items.push({
      sku: `SKU-${1000 + i}`,
      qty: (i % 4) + 1.0,
      price: 5.5 + i * 3.25,
    });
  }
  return {
    customer: 'acme-industrial',
    channel: 'web',
    lines: items,
  };
}

export const ORDER_BODY = JSON.stringify(buildOrder(LINES));

export const ORDER_PARAMS = {
  headers: { 'Content-Type': 'application/json' },
};

export const ORDER_LINE_COUNT = LINES;
