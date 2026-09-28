# 023. A payment simulator with durable idempotency and realistic faults

## Context
Exactly one charge per auction (invariant 5) needs a provider that honors idempotency keys (R3). The brief asks for 10% failures and occasional 5s hangs, so retries and the circuit breaker have something real to handle.

## Decision
- **`cmd/paysim`** charges under an `Idempotency-Key` (the invoice id). A repeat returns the original charge; a different request under the same key gets 422.
- **Charges live in Postgres** (a `paysim` schema the simulator owns), so a restart cannot turn a retry into a second charge.
- **Faults:**
  - half the failures reject before charging (503)
  - half charge, then answer 500 (a lost response)
  - hangs also come after charging
  - amounts ending in 13 cents are always declined (402), which tests the dead-letter path

## Alternatives
- **In-memory idempotency:** lost on restart.
- **Faults only before charging:** would never test lost responses.

## Consequences
Settlement must treat 5xx and timeouts as "unknown outcome" and retry with the same key, and treat 402/422 as final. The invariant checker audits `paysim.charges`, as a reconciliation job would.
