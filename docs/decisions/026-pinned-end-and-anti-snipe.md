# 026. Pinned end_at, anti-snipe, and a row version

## Context
R19: nothing stopped `UPDATE auctions SET end_at = <past>` followed by a close, which bypassed AE012. R17: the cache version (2·head + closed) missed any change that kept the head, and anti-snipe changes `end_at`.

## Decision
- Anti-snipe is per auction (`extend_window`, `extend_by`; zero disables). A bid accepted within `extend_window` of the end moves it to bid time + `extend_by` (if later). The bid path computes this in the same UPDATE that advances the head, from the bid's recorded time, under the row lock.
- The update guard recomputes that exact value and rejects any other `end_at` (AE018). So `end_at` changes only by this rule: it only grows, only with a bid, and only while open.
- A closed row never changes, and the auction's terms never change (AE019). A new row must be open, with no head (AE017).
- The trigger sets `closed_at` from `clock_timestamp()` and bumps `version` on every UPDATE. The cache orders states by `version`.

## Alternatives
- Checking the rule only in Go: the schema could not catch a bug or a manual edit.
- Global anti-snipe config: fewer columns, but it could not vary per auction, and the trigger would need the config.

## Consequences
Test fixtures that end an auction early must switch triggers off (`testdb.EndAuctionNow`).
