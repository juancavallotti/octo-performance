// Request payload for scenario 006, generated once per VU at init time.
//
// PAYLOAD_BYTES picks a rung on the size ladder. a commercial platform benchmarks payload transformation at a
// fixed shape and separately benchmarks the proxy at 1 KB and 1 MB; this scenario
// sweeps the size so that "how does transformation cost scale with payload" has an
// answer rather than an anecdote.
//
// Generating the body once and reusing it is deliberate: building a 1 MB JSON
// document per iteration would make k6, not the runtime, the thing under test.

import { envInt } from '../../../lab/k6/lib/options.js';

const TARGET_BYTES = envInt('PAYLOAD_BYTES', 1024);

const REGIONS = ['us-east-1', 'eu-west-1', 'ap-south-1', 'sa-east-1'];
const COUNTRIES = ['US', 'IE', 'IN', 'BR'];
const CURRENCIES = ['USD', 'EUR', 'INR', 'BRL'];

function record(i) {
  return {
    id: `acct-${String(i).padStart(8, '0')}`,
    name: `Account Holder ${i}`,
    region: REGIONS[i % REGIONS.length],
    country: COUNTRIES[i % COUNTRIES.length],
    amount: (i % 100) * 137.5,
    currency: CURRENCIES[i % CURRENCIES.length],
    status: i % 7 === 0 ? 'CLOSED' : 'ACTIVE',
  };
}

// Grow the record count until the serialised document reaches the target size, so
// every rung of the ladder is the same shape at a different scale.
function build() {
  const records = [];
  let body = '';
  for (let i = 0; ; i++) {
    records.push(record(i));
    if (i % 8 === 0 || records.length < 2) {
      body = JSON.stringify({ records });
      if (body.length >= TARGET_BYTES) break;
    }
    if (records.length > 20000) break; // hard stop, never reached at 1 MB
  }
  return JSON.stringify({ records });
}

export const TRANSFORM_BODY = build();
export const RECORD_COUNT = JSON.parse(TRANSFORM_BODY).records.length;
export const TRANSFORM_PARAMS = {
  headers: { 'Content-Type': 'application/json' },
};
