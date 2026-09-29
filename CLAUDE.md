# CLAUDE.md

Context for Claude Code sessions on this repo. The full original spec is `docs/PROJECT_BRIEF.md`. If this file and the brief disagree on the spec (stack, invariants, mechanisms, milestones), the brief wins. On the **working agreement**, this file wins: it records changes the owner made after the brief was written.

## What this is

auction-engine is a real-time auction engine in Go, themed as storage-unit auctions. The theme is cosmetic. The point is distributed systems engineering: concurrency correctness under contention, at-least-once messaging with idempotency, backpressure, and real deployed infrastructure. It is a portfolio project, and the owner (a CS student, new to Go, Kafka, Terraform and AWS) expects to be interviewed in depth about every line.

Module path: `github.com/AryanSingh103/auction-engine`

## Stack (decided; do not re-litigate)

- Go (latest stable; currently 1.27)
- HTTP: net/http with chi (see `docs/decisions/002-chi-router.md`)
- PostgreSQL as the single source of truth (currently 18), pgx driver, migrations via golang-migrate or goose (chosen in M1)
- Redis for caching, rate limiting, and pub/sub fan-out between API instances
- Kafka for async work. Locally that means Redpanda, with segmentio/kafka-go or franz-go (chosen in M4).
- WebSockets for live browser updates
- Docker Compose for local development, containerized from the start
- All configuration from environment variables. Every variable is required and there are no defaults in code. No hostnames or `localhost` strings in the code.
- Prometheus and Grafana for metrics
- Terraform on AWS (ECS on EC2, RDS, S3, ALB) for deployment
- GitHub Actions: tests from M1, deployment from M6
- Frontend: one plain, deliberately ugly HTML file with vanilla JS. Spend no effort here.

## Working agreement

Updated 2026-09-28. This replaces rules 2 and 4 of the brief.

1. **Small slices, one commit per component.** Since 2026-09-28, to save usage, Claude commits **and pushes** once per finished component (each commit green), with a short, specific message (e.g. `add auctions table migration`). Three separate things are three commits, never one batch. Claude does not hand git commands to the owner.
2. **Run each milestone straight through.** Claude does not stop between components for approval. It stops and asks only for:
   - a real decision that belongs to the owner
   - anything needing the owner's password or a browser
   - anything that costs money, such as creating AWS resources
3. **Explain every design decision,** including what was rejected and why. For example, if choosing pessimistic locking, explain what optimistic locking would have done differently and when it would be the better choice.
4. **Never leave code the owner can't explain.** There is no per-component quiz and, since 2026-09-28, no interview-questions subagent: the owner removed it to save usage.
5. **Don't scaffold ahead.** Add files only when they become necessary.
6. **If something asked for is a bad idea, say so directly.**
7. **Record every significant decision** in `docs/decisions/NNN-short-title.md`, covering context, decision, alternatives considered, and consequences. Keep each under 200 words.
8. **Assume every line will be asked about in an interview.**
9. **Plan, then go.** Since 2026-09-24 (M3), the owner has waived the plan-approval round trip because their time is limited. State the milestone plan in a few lines and start immediately, without waiting for approval. Every commit must leave the repo buildable and green, and be pushed, so nothing is lost if work stops mid-milestone. Before presenting a plan, verify versions, image tags and paths against live sources rather than memory.

## Honesty rules (these matter more than anything else)

- Never say something works unless it was actually run. Run the code, run the tests, and show the real output.
- Never make a failing test pass by weakening the assertion, adding a sleep, skipping the test, or lowering a threshold. If a test fails, either the code is wrong or the test is wrong. Say which, and why.
- When something fails, explain the cause before proposing a fix. No shotgun debugging.
- If a benchmark number looks suspiciously good, be suspicious. Verify that the load test actually exercises what it claims to. A load test that accidentally measures nothing is a common and embarrassing failure.
- If unsure, say so instead of guessing confidently.

## The invariants (these ARE the spec)

1. Exactly one winner per auction, ever.
2. The winner is the highest bidder.
3. Every accepted bid exceeds the previous accepted bid by at least min_increment.
4. No bid is ever accepted after the auction has closed.
5. Exactly one charge per closed auction, even if the settlement message is delivered many times.
6. No bid exists without its outbox event, and no outbox event exists without its bid.
7. All of the above hold while processes are being killed mid-operation.

Enforce these with database constraints and explicit mechanisms, not hopeful application logic. Push whatever you can into the schema. Known gaps in the wording and in the mechanisms are tracked in `docs/open-questions.md`.

## Domain conventions (decided up front)

- Money is `BIGINT` integer cents (Go `int64`). Never floating point.
- Timestamps are `timestamptz`.
- The database is the only clock that matters for ordering. Time checks in the bid and close paths use `clock_timestamp()`, never `now()`. `now()` is the transaction *start* time, so a transaction that waited on a row lock would see a stale time (open question R1).

## Milestone ritual and definition of done

At the end of each milestone:
- An **adversarial-review subagent** reviews the work as a skeptical senior backend engineer. It hunts for race conditions, concurrency bugs, missing error handling, unhandled failure modes, resource leaks, and any path that violates an invariant, and it tries to construct a specific breaking interleaving. It reports findings ranked by severity. Don't defend the code: fix what is real (each fix its own commit), and report what was dismissed and why.

A milestone is done only when all of these hold:
- All tests pass, and `go test -race ./...` is clean.
- Where applicable, the invariant checker runs green after a load test.
- Every significant decision is written up in `docs/decisions/`.
- `docs/benchmarks.md` records any numbers produced, with the date, machine specs (including the colima VM size), and the exact config used.
- This file is updated if anything structural changed.
- Everything is committed and pushed.
- The adversarial review has run and its real findings are fixed.

## Milestones (do not jump ahead)

0. Repo, Go module, multi-stage Dockerfile, docker compose with Postgres, /healthz, slog, env config, Makefile.
1. Schema and migrations, the bid path with row locking, an integration test of 1000 concurrent bids proving no lost update, and GitHub Actions CI.
2. Prometheus metrics, a Grafana dashboard, a load generator, first honest numbers, and a pessimistic vs optimistic locking benchmark.
3. WebSocket fan-out, Redis caching, Redis pub/sub across instances, rate limiting.
3.5. Throwaway trial deploy: minimal Terraform for /healthz on AWS, then destroy.
4. Redpanda, transactional outbox, settlement consumer, flaky payment simulator, retries with jittered backoff, circuit breaker, DLQ.
5. Closer worker, advisory-lock leader election, anti-snipe, and provable correctness of the close-vs-bid race.
6. Full Terraform, a GitHub Actions deploy pipeline, and a real deploy.
7. Chaos testing, README with architecture diagram and benchmark graphs, design doc.

## Commands

Run `make help` for the full list. The ones you'll use most:

- `cp .env.example .env`: one-time setup. `.env` is git-ignored, and every variable in it is required.
- `make run`: run the API on the host (fast loop), with config from `.env`. It needs Postgres running (`docker compose up -d postgres`) and migrated (`make migrate`).
- `make up` / `make down`: the full stack in docker compose (postgres, then the one-shot migrate, then api). `make nuke` also **deletes the database volume**.
- `make migrate`, `make migrate-status`: goose migrations against `.env`'s `DATABASE_URL`.
- `make test`, `make test-race`, `make vet`, `make fmt-check`, `make lint`: all must be clean before a milestone closes. The tests **need Docker running**, because integration tests start Postgres through testcontainers. The Makefile points testcontainers at the active docker context (colima), so a plain `go test` outside make needs `DOCKER_HOST` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` set the same way.
- `make test-page`: the live page's client-contract test (needs Node). CI runs it too.
- `make psql`: a psql shell inside the Postgres container. `make logs`: follow every compose service.
- **Two API instances** in compose: `api` on :8080 and `api2` on :8081. The live page is at `http://localhost:8080/`.
- **Workers in compose (M4):**
  - `redpanda`: Kafka API on :19092 from the host, `redpanda:9092` inside the network
  - `relay` + `relay2`: the outbox publishers (one batch at a time, whoever holds the lock)
  - `settler` + `settler2`: one settlement consumer group
  - `paysim`: the flaky payment provider, internal only
  - `closer` + `closer2` (M5): one leads by advisory lock and closes ended auctions
- **Inspect Kafka:** `docker compose exec redpanda rpk topic consume auction-events -o start`. The dead-letter topic is `settlement-dlq`.
- **Observability:**
  - Grafana at `http://localhost:3000`. Dashboards are viewable without login; admin credentials are in `.env`.
  - Prometheus at `http://localhost:9090`.
  - The API's metrics are on its separate listener (`:9091` in the container), never on the public port.
- **Load:**
  - `make bench-smoke`: a 10s verified run inside compose.
  - `scripts/bench.sh <dir> [dur] [warmup] [reps]`: the full matrix (about 40 min at 30/5/3; keep the machine idle). `scripts/bench_summary.py <dir>` produces the tables.
  - A load run that fails its checks exits non-zero, and its numbers must not be used.

CI (`.github/workflows/ci.yml`) runs lint, `make fmt-check vet test-race` and an image build on every push. Check a run with `gh run list` / `gh run watch`.

## Layout

- `cmd/api/`: API entrypoint. `run()` holds config, logger, the pgx pool, server lifecycle and graceful shutdown. The server drains first, then the pool closes.
- `cmd/migrate/`: one-shot `up|down|status` migration command (ADR 006).
- `migrations/`: embedded goose SQL, plus `schema_test.go`, which pins every constraint and trigger with exact SQLSTATEs.
- `internal/config/`: env loading and validation, including cross-field rules (ADR 001).
- `internal/auction/`: domain rules (`validateBid`, pure, table- and fuzz-tested) and the bid path (`Service.PlaceBid`, ADR 007/009). `BID_LOCKING` selects pessimistic (the default, ADR 016) or optimistic (`optimistic.go`, ADR 014). The correctness tests run against both.
- `internal/invariants/`: SQL audit queries for the invariants. Every check is proven to detect a planted violation.
- `internal/httpapi/`: chi router, handlers and middleware. The order is RequestID, request logger, metrics, recoverer, request deadline.
- `internal/metrics/`: Prometheus registry, HTTP middleware (labels are route *patterns*), the pool collector, and the `auction.Observer` implementation (ADR 013).
- `internal/loadgen/` + `cmd/loadgen/`: the self-verifying load generator (ADR 015).
- `internal/postgres/`: pool construction; every connection gets `idle_in_transaction_session_timeout`.
- **Redis is an accelerator only (ADR 017):**
  - `internal/redisclient` (short timeouts, no retries)
  - `internal/ratelimit` (Lua token bucket, fail-open, ADR 018)
  - `internal/cache` (version-guarded auction cache, ADR 019)
  - `internal/live` (the hub that drops slow clients, and the Redis pub/sub bus, ADR 020)
  - `internal/testredis` (the test harness)
- The WebSocket handler is `internal/httpapi/live.go`, and the plain page is `internal/httpapi/web/index.html`.
- `deploy/`: Prometheus config, and Grafana provisioning plus the dashboard JSON (change dashboards here, not in the UI).
- `deploy/terraform/trial/`: the destroyed M3.5 stack, kept for reference. Its state is local and git-ignored. Variables have no defaults, so pass them with `-var`.
- `docs/benchmarks.md` + `docs/benchmarks/<date>/`: recorded numbers, with their raw data and environment.
- **Outbox and settlement (M4):**
  - `internal/outbox` + `cmd/relay`: publishes the outbox to Kafka, keyed by auction id. Each batch runs under a transaction-level advisory lock (ADR 022).
  - `internal/kafkaclient`: franz-go clients and idempotent topic creation. `internal/testkafka`: the Redpanda harness.
  - `internal/auction/close.go`: `CloseAuction`, which writes the `auction_closed` event. M5's closer will call it.
  - `internal/paysim` + `cmd/paysim`: the fake provider, with durable idempotency and injected faults (ADR 023).
  - `internal/settlement` + `cmd/settler`: invoice, charge once, retry unknown outcomes, dead-letter (ADR 024). `internal/breaker`: the circuit breaker.
  - `internal/httpapi/shed.go`: bid load shedding (ADR 025).
  - `internal/metrics/workers.go`: the relay's, settler's and closer's metrics.
- **Closer and anti-snipe (M5):**
  - `internal/closer` + `cmd/closer`: session advisory-lock leader on a hijacked connection; closes via `CloseAuction` (ADR 027). Leadership is only an optimization: closing is safe under split brain.
  - Anti-snipe is per auction (`extend_window`, `extend_by`), applied in `writeBid`'s head UPDATE and checked exactly by the update guard (ADR 026).
  - `auctions.version` is bumped by the trigger on every update and is the cache version. `closed_at` is set by the trigger.
  - `internal/auction/race_test.go`: the close-vs-bid race proofs. `testdb.EndAuctionNow` is the only way a fixture may end an auction early.
- `internal/testdb/`: testcontainers harness. One container per test binary, and a fresh database cloned from a migrated template per test.
- `docs/decisions/`: ADRs, numbered `NNN-short-title.md`. `docs/open-questions.md`: spec gaps and risks R1–R18. `docs/interview/`: interview questions for M0–M4 (no longer generated).

Database error codes: the guard trigger raises `AE001`–`AE006` and the deferred outbox check raises `AE007`. `internal/auction` maps them to domain errors wrapped with `ErrRejectedByDatabaseGuard`. Only one case is expected in correct operation: `ErrAuctionEnded` when `end_at` falls between the Go and trigger clock reads (`IsExpectedGuardRejection`). Any other guard rejection means the Go rules missed something. The hardening migrations add `AE008` (bids append-only), `AE009` (outbox append-only except marking published), `AE010` (status only open→closed) and `AE011` (head only advances one link). M4 adds:
- `AE012`: no close before `end_at`
- `AE013`: close without its event fails at commit
- `AE014`: no close event for an open auction
- `AE015`: an invoice must match the closed result
- `AE016`: invoice status only moves pending→paid or pending→failed (both final; migration 15), and invoices are never deleted

M5 adds (migration 16):
- `AE017`: a new auction starts open, headless, at version 0
- `AE018`: `end_at` only moves by the anti-snipe rule, with a new head
- `AE019`: a closed auction never changes; an auction's terms never change

`internal/invariants` has a `SafetyMode` (holds at every instant) and a `DrainedMode` (it also checks the eventual settlement properties, so use it only once nothing is in flight).

**Lock order:** always lock the auction row first. Every path that touches several of auctions, users, bids and outbox must follow it (R14).

## Current status

**Milestone 5 work is built (2026-09-28); its adversarial review has not run yet.** At the owner's request (usage limits), the review was deferred to a later session. Run it before calling M5 done. Built in M5 (ADRs 026–027):
- migration 16: pinned `end_at`, anti-snipe, the row version (resolving the R17 and R19 must-fixes), and guards AE017–AE019
- the closer worker with advisory-lock leader election (two copies in compose)
- the close-vs-bid race proofs, under both locking strategies
- the close-time invariant audit

Deferred items are in R20. **Waiting for the owner:** ADR 025 dropped the lag-based relay throttle from the chosen R4 option. Still open: R15, R16, R17 (the rest), R19 (the rest), R20. Interview questions for M0–M4 are in `docs/interview/`.

**Next:** the M5 adversarial review, then M6.

Notes for whoever picks this up:
- **`README.md` runs ahead of the code.** At the owner's request (2026-09-25) it describes the finished system: the outbox publisher, settlement, the closer, load shedding, the AWS deploy and fault injection are written up as done, although they are M4–M7 work. Use this status section and `docs/open-questions.md` for what is actually built; never treat the README as evidence that something exists. Its only numbers are real M2 benchmark results, and it must never gain invented ones.
- `make run` sources `.env` in the shell. The compose `api` service gets an explicit variable list, not the whole `.env`, always listens on `:8080` inside the container, and uses an in-network `DATABASE_URL` built from the `POSTGRES_*` variables.
- **Settlement statuses mean what the provider guarantees:** a timeout or exhausted retries leave an invoice `pending`, never `failed`, because a charge may exist. Never "simplify" that.
- **Tests that need to stop a worker must wait for its commit first:** a Kafka ack is not a marked outbox row.
- **When piping a gate command, check its exit code:** `make lint | tail` and `go test | grep` hide failures (this happened in M4). Use `if go test ...; then commit; fi`.
- `SHUTDOWN_TIMEOUT` bounds the **whole** graceful shutdown (API drain, metrics server, live handlers). It must stay below compose's `stop_grace_period` (20s) and the ECS `stopTimeout` (20s in the trial).
- **AWS:** the IAM user `aryan-cli` in us-east-2 has a $10 budget. Push to ECR with `aws ecr get-login-password | docker login --password-stdin`, so the token never appears in output. Creating anything billable still needs the owner's go-ahead (R10).
- Colima only shares `$HOME` into its VM. Bind mounts from `/tmp` or `/private/tmp` show up empty inside containers.
- In zsh, `$VAR` holding a command with spaces does not word-split. Use a shell function.
- A plain `go test` needs `DOCKER_HOST=$(docker context inspect -f '{{.Endpoints.docker.Host}}') TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock` (make sets both).
- The Redis-backed features must **degrade, never fail**, when Redis is down: the cache falls back to Postgres, the limiter allows the request, and live updates are skipped. The bid path must never read Redis.
- Prometheus does not reload `deploy/prometheus/prometheus.yml` on its own: `docker compose restart prometheus` after editing it.
- `docker compose run` and `up` do not rebuild images; `scripts/bench.sh` builds first. Use `--build` elsewhere, or you may test stale code.

## Local environment

- macOS on Apple M3. Docker runs through **colima** (4 CPU, 6 GiB, 40 GiB disk), which does not auto-start after a reboot: run `colima start`.
- `psql` comes from Homebrew's keg-only `libpq`, which is on PATH via `~/.zshrc`.
