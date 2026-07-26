// Request payload for scenario 007, generated once per VU at init time.
//
// The records are byte-for-byte the same records scenario 006 sends, written as
// CSV instead of JSON. That is the point: at the same record count the two
// scenarios do the same work on the same data, so the difference between them is
// the cost of the input format and nothing else.
//
// PAYLOAD_RECORDS therefore sizes the ladder, not PAYLOAD_BYTES. Equal bytes would
// mean unequal records — CSV carries no keys, quotes or braces, so it holds a
// record in roughly half the space — and the record count is what the transform
// cost actually tracks.
//
// Generating the body once and reusing it is deliberate: building the document per
// iteration would make k6, not the runtime, the thing under test.

import { envInt } from '../../../lab/k6/lib/options.js';

const RECORDS = envInt('PAYLOAD_RECORDS', 9);

const REGIONS = ['us-east-1', 'eu-west-1', 'ap-south-1', 'sa-east-1'];
const COUNTRIES = ['US', 'IE', 'IN', 'BR'];
const CURRENCIES = ['USD', 'EUR', 'INR', 'BRL'];

const HEADER = 'id,name,region,country,amount,currency,status';

// Identical to scenario 006's record(i), flattened into a row. No field contains a
// comma or a quote, which is what lets the runtime's naive CEL reader be correct
// here — see the note in integration.yaml.
function row(i) {
  return [
    `acct-${String(i).padStart(8, '0')}`,
    `Account Holder ${i}`,
    REGIONS[i % REGIONS.length],
    COUNTRIES[i % COUNTRIES.length],
    (i % 100) * 137.5,
    CURRENCIES[i % CURRENCIES.length],
    i % 7 === 0 ? 'CLOSED' : 'ACTIVE',
  ].join(',');
}

function build() {
  const lines = [HEADER];
  for (let i = 0; i < RECORDS; i++) lines.push(row(i));
  return lines.join('\n');
}

export const TRANSFORM_BODY = build();
export const RECORD_COUNT = RECORDS;
export const TRANSFORM_PARAMS = {
  headers: { 'Content-Type': 'text/csv' },
};
