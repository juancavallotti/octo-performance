// Shared request payload for scenario 003.
//
// Built once at module scope: the server-side cost is three Postgres round trips,
// so spending generator CPU re-serialising an identical object every iteration
// would only steal capacity from the thing under test.

export const ORDER_BODY = JSON.stringify({
  customer: 'acme-industrial',
  channel: 'web',
  // JSON numbers decode to CEL doubles; the column is numeric(12,2).
  amount: 249.95,
});

export const ORDER_PARAMS = {
  headers: { 'Content-Type': 'application/json' },
};
