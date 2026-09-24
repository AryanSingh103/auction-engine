# 011. Bidding rules

## Context
The invariants left four questions open (R5): the first-bid minimum, self-raises, zero-bid auctions, and a reserve price.

## Decision
Decided by the owner:
- The **first bid must be ≥ `starting_price`**. Every later bid must be ≥ `current_price + min_increment`.
- The **current leader cannot outbid themselves** (409 `self_outbid`).
- An auction with **zero bids closes with no winner and no invoice**. Invariant 1 therefore reads "at most one winner; exactly one if any bid was accepted".
- **There is no reserve price.**
- A bid is valid only while `start_at ≤ now < end_at`, judged by the database clock after the auction row lock is held.

Go (`validateBid`) and the guard trigger implement the rules in the same order, so both report the same reason.

## Alternatives considered
- First bid ≥ `starting_price + min_increment`: an unusual experience for users.
- Allowing self-raises: it lets a leader inflate their own price.
- A reserve price: extra state and close-path rules with little systems value.

## Consequences
Table tests and a fuzz test (13.5M executions, no failures) pin the Go rules. The schema tests pin the trigger.
