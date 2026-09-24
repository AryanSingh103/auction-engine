# 005. Hybrid local development loop

## Context
The brief requires everything to be containerized from the start. Rebuilding the API image on every edit (10–30s) slows the inner loop for someone learning Go.

## Decision
Infrastructure (Postgres now; Redis and Redpanda later) always runs in compose. The API has two supported modes, both driven by the same `.env`:
- `make run`: `go run` on the host, for fast iteration.
- `make up`: the full stack, including the API container. It's verified at every milestone, so the container path never rots.

Published ports bind to `127.0.0.1` only.

## Alternatives considered
- **API always in compose:** closest to production, but the slowest loop.
- **Hot-reload tools (air) inside a container:** another moving part and more config to explain, for a small gain over `go run`.

## Consequences
One `.env` cannot hold a single `DATABASE_URL` that suits both modes. The host reaches Postgres at `127.0.0.1:$POSTGRES_HOST_PORT`, while the container uses `postgres:5432`. The M1 plan is for compose's `environment:` to override `DATABASE_URL` for the `api` service.
