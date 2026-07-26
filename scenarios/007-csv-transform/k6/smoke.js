// Correctness gate for scenario 007: the CSV must actually be parsed, and parsed
// correctly. A decoder written as an expression can fail in ways a compiled one
// cannot — silently keeping the header row, or leaving every field a string — and
// both of those would benchmark faster than doing the work.

import http from 'k6/http';
import { check } from 'k6';
import { smokeOptions, targetUrl, env } from '../../../lab/k6/lib/options.js';
import { summaryHandler } from '../../../lab/k6/lib/summary.js';
import { TRANSFORM_BODY, TRANSFORM_PARAMS, RECORD_COUNT } from './payload.js';

export const options = smokeOptions({ iterations: 20 });

const URL = targetUrl();
const RICH = env('ROUTE', '/transform').indexOf('rich') >= 0;

function accounts(r) {
  return JSON.parse(r.body).accounts;
}

const checks = {
  'status is 200': (r) => r.status === 200,

  // Every data row in, every record out — and the header row is not one of them.
  // Off by one here is the classic failure of an index-filtered parse.
  'every record survived, header excluded': (r) => {
    try {
      return JSON.parse(r.body).meta.count === RECORD_COUNT;
    } catch (e) {
      return false;
    }
  },
  'the header row was not parsed as data': (r) => {
    try {
      return accounts(r)[0].name.toLowerCase() !== 'name';
    } catch (e) {
      return false;
    }
  },

  // CSV carries no types. If the projection did not convert, `amount` is the
  // string "137.5" and arithmetic on it never happened.
  'fields were typed, not left as strings': (r) => {
    try {
      const a = accounts(r)[1];
      return typeof a.balance.amount === 'number' &&
             typeof a.balance.withTax === 'number' &&
             typeof a.active === 'boolean' &&
             ['gold', 'silver', 'bronze'].includes(a.tier);
    } catch (e) {
      return false;
    }
  },
  'the projection reshaped the row': (r) => {
    try {
      const a = accounts(r)[1];
      return a.location.region !== undefined && a.location.country !== undefined;
    } catch (e) {
      return false;
    }
  },
};

// The rich route is the one that exercises the rest of the vocabulary, so it gets
// the checks that prove each library ran rather than being optimised away.
if (RICH) {
  checks['regex pulled the id out of the reference'] = (r) => {
    try {
      return /^\d+$/.test(accounts(r)[1].accountId);
    } catch (e) {
      return false;
    }
  };
  checks['strings folded case and formatted a label'] = (r) => {
    try {
      const a = accounts(r)[1];
      return a.name === a.name.toLowerCase() && /\(\w\w\) \d+\.\d\d /.test(a.label);
    } catch (e) {
      return false;
    }
  };
  checks['lists, encoders, math and sets ran'] = (r) => {
    try {
      const a = accounts(r)[1];
      return a.location.zone.split('.').length === 3 &&
             /^[A-Za-z0-9+/]+=*$/.test(a.idempotencyKey) &&
             Math.abs(a.balance.withTax * 100 - Math.round(a.balance.withTax * 100)) < 1e-9 &&
             typeof a.active === 'boolean';
    } catch (e) {
      return false;
    }
  };
}

export default function () {
  const res = http.post(URL, TRANSFORM_BODY, TRANSFORM_PARAMS);
  check(res, checks);
}

export function handleSummary(data) {
  return summaryHandler(data, { test: 'smoke' });
}
