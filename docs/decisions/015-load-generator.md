# 015. Load generator that verifies itself

## Context
A load test that accidentally measures nothing is the brief's named failure mode. M2's numbers must be quotable.

## Decision
`cmd/loadgen` (flags, since it's a one-shot tool):
- closed-loop bidders, one HTTP connection per worker, and exact percentiles from every sample
- unknown outcomes (503, 500, transport errors) are **retried with the same idempotency key** (R14)
- five checks run after every run, with a non-zero exit if any fail:
  - the server's request delta equals the client's responses
  - the client's accepted count equals the bids in the database
  - prices moved
  - the invariants hold
  - the error rate is within its limit

A test proves a stub returning 201 without storing anything fails them.

**Topology:** it runs inside compose. From the host, colima's port forwarder might become the thing measured.

## Alternatives considered
**k6:** can't reconcile against the database or the invariants without more glue than it saves.

## Consequences
Loadgen shares the VM's 4 vCPU with the API and Postgres, so every recorded number says so. A `/healthz` control run shows the harness's own ceiling.
