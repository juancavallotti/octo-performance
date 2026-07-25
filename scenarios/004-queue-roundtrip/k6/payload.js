// Shared request payload for scenario 004.
//
// Built once at module scope: the cost under test is the queue round trip, so
// re-serialising an identical object every iteration would only steal generator
// capacity from the thing being measured.

export const JOB_BODY = JSON.stringify({
  tenant: 'acme-industrial',
  // JSON numbers decode to CEL doubles; units is multiplied by weight in the
  // consumer, and CEL will not multiply an int by a double.
  units: 12.0,
  weight: 18.5,
});

export const JOB_PARAMS = {
  headers: { 'Content-Type': 'application/json' },
};

// weight * units * 1.37 = 18.5 * 12 * 1.37 = 304.14 -> "medium"
export const EXPECTED_SCORE = 18.5 * 12.0 * 1.37;
export const EXPECTED_BAND = 'medium';
