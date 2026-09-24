# 009. Idempotency keys on bid creation

## Context
Clients retry. A retry must not bid twice, and should learn the original outcome.

## Decision
- An `Idempotency-Key` header is required. The key is stored on the bid, with `UNIQUE (user_id, idempotency_key)`, scoped per user as in Stripe's API.
- The key is looked up **after** the auction lock is taken. A test caught the bug in doing it before: two copies of one request both missed the lookup, and the second was then rejected as "self-outbid" instead of replayed.
- A replay with the same auction and amount returns the original bid (**200**, `Idempotent-Replayed: true`). A different payload gets **422**.
- Same-key races across different auctions: the unique constraint makes the loser roll back and answer 422.
- Only accepted bids are stored. A retried rejection is evaluated again, which is safe because rejections have no side effects.

## Alternatives considered
- **A separate idempotency table caching every response:** replays rejections exactly, but costs another write and a retention policy.
- **Checking before the lock:** faster replays, and wrong (see above).

## Consequences
Replays queue behind live bids on the same auction. 100 concurrent retries yield exactly one bid.
