# 010. Bidder identity from an X-User-ID header (no real auth)

## Context
Bids, per-user rate limits (M3) and invoices all need a user, but the brief defines no authentication (R6). Real auth (sessions, OAuth, JWT) is a large subsystem that serves none of the project's distributed-systems goals.

## Decision
Decided by the owner. Requests name the bidder in an `X-User-ID` header holding a positive integer user id:
- a missing or malformed header gets 401 `missing_user`
- an id not in `users` gets 401 `unknown_user`, checked inside the bid transaction before the auction lock is taken

This is documented everywhere as **no real authentication, out of scope**. Any client can claim any user.

## Alternatives considered
- **API keys** (random, stored hashed, sent as bearer tokens): realistic, but another subsystem to build, test and explain.
- **JWT from an identity provider:** external setup and key rotation, with nothing gained for the invariants.

## Consequences
The API must never be exposed as if it were secure. Before the M6 deploy, it sits behind the ALB with the demo framing made explicit. If auth is ever added, only the header parsing changes; the service already takes a `UserID`.
