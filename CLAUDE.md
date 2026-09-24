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

Updated 2026-09-24. This replaces rules 2 and 4 of the brief.

1. **Small slices, one commit per logical change.** Claude commits **and pushes** after every individual logical change, with a short, specific message (e.g. `add auctions table migration`). Three separate things are three commits, never one batch. Claude does not hand git commands to the owner.
2. **Run each milestone straight through.** Claude does not stop between components for approval. It stops and asks only for:
   - a real decision that belongs to the owner
   - anything needing the owner's password or a browser
   - anything that costs money, such as creating AWS resources
3. **Explain every design decision,** including what was rejected and why. For example, if choosing pessimistic locking, explain what optimistic locking would have done differently and when it would be the better choice.
4. **Never leave code the owner can't explain.** There is no per-component quiz. The end-of-milestone interview-questions subagent (below) is the knowledge check.
5. **Don't scaffold ahead.** Add files only when they become necessary.
6. **If something asked for is a bad idea, say so directly.**
7. **Record every significant decision** in `docs/decisions/NNN-short-title.md`, covering context, decision, alternatives considered, and consequences. Keep each under 200 words.
8. **Assume every line will be asked about in an interview.**
9. **Plan mode first.** Each milestone starts in plan mode, and the owner approves the plan. Before presenting a plan, verify versions, image tags and paths against live sources rather than memory.

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
- An **interview-questions subagent** reads the milestone's diff and writes 5 senior-level questions. The owner gets them **without answers**, answers them, and is then told where they were wrong or vague. Never skip this.

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

## Current status

Milestone 0 is in progress.

## Local environment

- macOS on Apple M3. Docker runs through **colima** (4 CPU, 6 GiB, 40 GiB disk), which does not auto-start after a reboot: run `colima start`.
- `psql` comes from Homebrew's keg-only `libpq`, which is on PATH via `~/.zshrc`.
